package aimonitor

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"porteight-camera-ai/internal/storage"
)

// ──────────────────────────── Types ────────────────────────────

// VLMResponse is the structured JSON the vision model returns.
type VLMResponse struct {
	HumanIntrusion struct {
		Present     bool   `json:"present"`
		Description string `json:"description"`
	} `json:"human_intrusion"`
	VisualObstruction struct {
		Present     bool   `json:"present"`
		Description string `json:"description"`
	} `json:"visual_obstruction"`
	CameraTampering struct {
		Present     bool   `json:"present"`
		Description string `json:"description"`
	} `json:"camera_tampering"`
	OverallStatus string `json:"overall_status"` // "safe" | "warning" | "critical"
}

// AnalysisResult is the complete record sent to Tinybird.
type AnalysisResult struct {
	StreamKey        string      `json:"stream_key"`
	Timestamp        time.Time   `json:"timestamp"`
	SnapshotBase64   string      `json:"snapshot_base64,omitempty"` // kept in-memory for API, NOT sent to Tinybird
	SnapshotS3Key    string      `json:"snapshot_s3_key,omitempty"` // S3 object key for the snapshot
	SnapshotURL      string      `json:"snapshot_url,omitempty"`    // presigned URL for the snapshot
	VLMResponse      *VLMResponse `json:"vlm_response,omitempty"`
	OverallStatus    string      `json:"overall_status"` // safe | warning | critical | error
	InputTokens      int         `json:"input_tokens"`
	OutputTokens     int         `json:"output_tokens"`
	APIResponseMs    int64       `json:"api_response_ms"`
	APIError         string      `json:"api_error,omitempty"`
	ClipS3Key        string      `json:"clip_s3_key,omitempty"`
	Model            string      `json:"model"`
}

// AlertClip references a 10-minute clip saved to S3 after an alert.
type AlertClip struct {
	StreamKey  string    `json:"stream_key"`
	Timestamp  time.Time `json:"timestamp"`
	S3Bucket   string    `json:"s3_bucket"`
	S3Key      string    `json:"s3_key"`
	DurationS  float64   `json:"duration_seconds"`
	Status     string    `json:"status"` // overall_status that triggered it
}

// Config for the AI monitor.
type Config struct {
	SiliconFlowAPIKey string
	SiliconFlowModel  string // default: Qwen/Qwen3-VL-32B-Instruct
	TinybirdToken     string
	TinybirdBaseURL   string // e.g. https://api.eu-central-1.aws.tinybird.co
	TinybirdDSName    string // data source name (default: camera_ai_analysis)
	HLSDir            string
	S3Store           *storage.S3Store
	// How often to take a snapshot (default 60s)
	SnapshotInterval time.Duration
	// Cooldown after alert detection (default 5min) – skip analysis, record clip instead
	AlertCooldown time.Duration
}

// streamState tracks per-stream monitoring state.
type streamState struct {
	cancel        context.CancelFunc
	lastAlertTime time.Time
	alertActive   bool
}

// Monitor orchestrates AI analysis of live RTMP streams.
type Monitor struct {
	cfg     Config
	mu      sync.Mutex
	streams map[string]*streamState // key -> state
	results []AnalysisResult         // ring buffer of recent results (for API)
	alerts  []AlertClip              // ring buffer of recent alert clips
}

const (
	defaultSnapshotInterval = 60 * time.Second
	defaultAlertCooldown    = 5 * time.Minute
	defaultModel            = "Qwen/Qwen3-VL-32B-Instruct"
	maxRecentResults        = 500
	maxRecentAlerts         = 100
)

var systemPrompt = `You are a specialized anti-theft surveillance AI analyzing images from a CCTV camera mounted to monitor the rear/bed of a loaded truck. The camera is installed specifically to prevent cargo theft.

─── SCENE CONTEXT ───
- The camera faces the truck bed/cargo area from a fixed position (typically mounted on the truck cabin looking backward, or on a pole near the truck).
- The truck bed is usually loaded with goods covered by tarpaulins, sheets, or open cargo.
- The environment can be a warehouse, parking lot, roadside, construction site, or any stop point.
- Images may be captured in daylight, low-light, night-vision (IR/grayscale), or artificial lighting. Night shots will appear grainy, monochrome, or greenish — this is NORMAL and NOT an obstruction.

─── STEP 1: SCENE VALIDATION ───
Before analyzing threats, verify the camera is functioning correctly:
- Can you see the truck bed or cargo area (even partially)? If YES, proceed to threat analysis.
- If the entire frame is black, blurred beyond recognition, shows a single solid color, or shows something physically pressed against the lens (cloth, hand, tape, paint, paper), this is camera tampering/obstruction.
- A dark or grainy image with visible shapes, outlines, or IR illumination is a NORMAL night shot — NOT obstruction.
- Rain, fog, condensation, or dust on the lens causing partial blur is visual_obstruction (warning level, not critical unless the truck bed is completely invisible).
- An image that shows a completely different scene (sky only, ground only, wall only, no truck visible at all) indicates the camera has been repositioned — this is camera_tampering.

─── STEP 2: HUMAN INTRUSION DETECTION ───
ANY person visible in or near the truck bed area is suspicious. This is a restricted zone — no one should be there.

Mark human_intrusion as PRESENT (true) if you see ANY of:
- A person standing on, climbing onto, sitting on, or leaning over the truck bed
- A person reaching into the truck bed or touching cargo
- A person standing immediately beside the truck bed (within arm's reach)
- A person walking along the side of the truck near the cargo area
- Human body parts visible: hands, arms, legs, feet, head, torso — even partially visible
- A silhouette, shadow, or outline of a human figure on or near the truck bed (even in night/IR shots)
- A person crouching, hiding, or lying flat on the truck bed
- A person lifting, pulling, dragging, or carrying items from the truck bed area
- A person using tools (cutting tarpaulin, opening latches, removing ropes/straps)
- Multiple people coordinating near the truck (especially at night)
- A person wearing a face covering, hoodie, or attempting to conceal their identity near the truck

Mark human_intrusion as NOT present (false) ONLY if:
- No human figure, body part, silhouette, or shadow of a person is visible anywhere near the truck bed
- Only the truck, cargo, tarpaulins, and environment (trees, buildings, sky, ground) are visible

IMPORTANT: Do NOT dismiss a detection just because the person might be a driver or worker. Any person near the truck bed triggers an alert — it is not your job to determine intent, only presence.

─── STEP 3: VISUAL OBSTRUCTION DETECTION ───
Someone may try to cover the camera to hide theft activity.

Mark visual_obstruction as PRESENT (true) if:
- A cloth, fabric, bag, or sheet is draped over or in front of the lens
- Tape, sticker, paper, or paint covers part or all of the lens
- A hand or fingers are blocking the camera view
- An object has been placed directly in front of the camera (box, board, bag)
- Spray paint or liquid smeared on the lens
- Sudden heavy blur that wasn't there before (vaseline or grease on lens)
- Part of the frame shows the truck bed normally but a section is blocked by something close to the lens

Mark visual_obstruction as NOT present (false) if:
- The image is dark/grainy but shapes are visible (normal night vision)
- Light rain droplets or mild condensation (minor, does not block view significantly)
- Normal environmental conditions (fog in the distance, dust in the air)
- The truck bed and surroundings are clearly visible even if image quality is low

─── STEP 4: CAMERA TAMPERING DETECTION ───
Someone may physically move, disconnect, or damage the camera.

Mark camera_tampering as PRESENT (true) if:
- The camera angle has shifted dramatically (truck bed is no longer visible, camera points at sky/ground/wall)
- The image is completely black with no IR illumination (camera may be powered off or disconnected)
- The image shows heavy static, rolling lines, or digital artifacts suggesting physical interference
- The camera appears to be shaking or vibrating unnaturally (motion blur in a parked-truck scenario)
- The view has changed significantly compared to what a truck-rear camera should show (no truck visible at all)

Mark camera_tampering as NOT present (false) if:
- The image is dark but IR/night-vision shapes are visible (normal night operation)
- Minor vibrations from wind or passing vehicles
- The truck bed is visible even if at a slightly different angle (trucks can shift when loaded/unloaded)

─── SEVERITY CLASSIFICATION ───
- "safe": No issues detected. Truck bed visible, no people, no obstruction, no tampering.
- "warning": Minor concern. Examples: slight lens obstruction but truck bed still partially visible, distant figure that might be a person, minor camera shift.
- "critical": Immediate threat. Examples: person detected on/near truck bed, camera fully obstructed, camera tampered/repositioned, complete blackout, multiple issues simultaneously.

When in doubt between "safe" and "warning", choose "warning".
When in doubt between "warning" and "critical", choose "critical".
If ANY human is detected near the truck bed, minimum severity is "critical".
If the camera view is fully blocked or tampered, severity is "critical".

─── OUTPUT FORMAT ───
Respond ONLY with the following strict JSON (no extra text, no explanation outside JSON):

{
  "human_intrusion": {
    "present": true | false,
    "description": "Short factual explanation of what you see"
  },
  "visual_obstruction": {
    "present": true | false,
    "description": "Short factual explanation of what you see"
  },
  "camera_tampering": {
    "present": true | false,
    "description": "Short factual explanation of what you see"
  },
  "overall_status": "safe" | "warning" | "critical"
}

─── CRITICAL RULES ───
1. Keep descriptions short (1-2 sentences max) and factual — describe what you SEE, not what you assume.
2. ANY person near the truck bed = critical. No exceptions. Do not try to determine if the person is authorized.
3. Night/IR/grayscale images are NORMAL — do not flag low-light images as obstruction or tampering.
4. If you cannot see the truck bed at all, something is wrong — flag it.
5. If multiple issues exist simultaneously, overall_status MUST be "critical".
6. Err on the side of caution — a false alert is better than a missed theft.
7. Do not output anything outside the JSON object.`

// ──────────────────────────── Constructor ────────────────────────────

func NewMonitor(cfg Config) *Monitor {
	if cfg.SnapshotInterval == 0 {
		cfg.SnapshotInterval = defaultSnapshotInterval
	}
	if cfg.AlertCooldown == 0 {
		cfg.AlertCooldown = defaultAlertCooldown
	}
	if cfg.SiliconFlowModel == "" {
		cfg.SiliconFlowModel = defaultModel
	}
	if cfg.TinybirdDSName == "" {
		cfg.TinybirdDSName = "camera_ai_analysis"
	}
	return &Monitor{
		cfg:     cfg,
		streams: make(map[string]*streamState),
	}
}

// ──────────────────────────── Public API ────────────────────────────

// StartStream begins monitoring a live stream.
func (m *Monitor) StartStream(key string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// If already running, skip.
	if _, ok := m.streams[key]; ok {
		return
	}

	if m.cfg.SiliconFlowAPIKey == "" {
		log.Printf("[AI Monitor] Skipping stream %s – SILICONFLOW_API_KEY not set", key)
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	st := &streamState{cancel: cancel}
	m.streams[key] = st

	log.Printf("[AI Monitor] Started monitoring stream: %s (interval=%s, cooldown=%s)", key, m.cfg.SnapshotInterval, m.cfg.AlertCooldown)
	go m.monitorLoop(ctx, key, st)
}

// StopStream stops monitoring a live stream.
func (m *Monitor) StopStream(key string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if st, ok := m.streams[key]; ok {
		st.cancel()
		delete(m.streams, key)
		log.Printf("[AI Monitor] Stopped monitoring stream: %s", key)
	}
}

// GetRecentResults returns the most recent analysis results (for the API).
func (m *Monitor) GetRecentResults(key string, limit int) []AnalysisResult {
	m.mu.Lock()
	defer m.mu.Unlock()

	if limit <= 0 {
		limit = 50
	}
	var out []AnalysisResult
	for i := len(m.results) - 1; i >= 0 && len(out) < limit; i-- {
		if key == "" || m.results[i].StreamKey == key {
			out = append(out, m.results[i])
		}
	}
	return out
}

// GetRecentAlerts returns the most recent alert clips.
func (m *Monitor) GetRecentAlerts(key string, limit int) []AlertClip {
	m.mu.Lock()
	defer m.mu.Unlock()

	if limit <= 0 {
		limit = 50
	}
	var out []AlertClip
	for i := len(m.alerts) - 1; i >= 0 && len(out) < limit; i-- {
		if key == "" || m.alerts[i].StreamKey == key {
			out = append(out, m.alerts[i])
		}
	}
	return out
}

// GetActiveStreams returns the list of streams being monitored.
func (m *Monitor) GetActiveStreams() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	keys := make([]string, 0, len(m.streams))
	for k := range m.streams {
		keys = append(keys, k)
	}
	return keys
}

// ──────────────────────────── Monitor Loop ────────────────────────────

func (m *Monitor) monitorLoop(ctx context.Context, key string, st *streamState) {
	ticker := time.NewTicker(m.cfg.SnapshotInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// Check if we're in alert cooldown
			if st.alertActive && time.Since(st.lastAlertTime) < m.cfg.AlertCooldown {
				log.Printf("[AI Monitor] %s – in alert cooldown (%.0fs remaining), skipping snapshot",
					key, (m.cfg.AlertCooldown - time.Since(st.lastAlertTime)).Seconds())
				continue
			}
			if st.alertActive {
				st.alertActive = false
				log.Printf("[AI Monitor] %s – alert cooldown ended, resuming analysis", key)
			}

			m.analyzeOnce(ctx, key, st)
		}
	}
}

func (m *Monitor) analyzeOnce(ctx context.Context, key string, st *streamState) {
	now := time.Now()

	// 1. Capture snapshot from live HLS
	imgBytes, err := m.captureSnapshot(key)
	if err != nil {
		log.Printf("[AI Monitor] %s – snapshot failed: %v", key, err)
		m.recordResult(AnalysisResult{
			StreamKey:     key,
			Timestamp:     now,
			OverallStatus: "error",
			APIError:      fmt.Sprintf("snapshot failed: %v", err),
			Model:         m.cfg.SiliconFlowModel,
		})
		return
	}

	b64 := base64.StdEncoding.EncodeToString(imgBytes)
	log.Printf("[AI Monitor] %s – captured snapshot (%d bytes)", key, len(imgBytes))

	// 1b. Upload snapshot to S3 (non-blocking for VLM, but we need the key)
	var snapshotS3Key string
	var snapshotURL string
	if m.cfg.S3Store != nil {
		snapshotS3Key = fmt.Sprintf("snapshots/%s/%s.jpg", key, now.Format("2006-01-02_15-04-05"))
		if err := m.cfg.S3Store.UploadBytes(ctx, imgBytes, snapshotS3Key, "image/jpeg"); err != nil {
			log.Printf("[AI Monitor] %s – snapshot S3 upload failed: %v", key, err)
			snapshotS3Key = ""
		} else {
			if presigned, err := m.cfg.S3Store.PresignGet(ctx, snapshotS3Key); err == nil {
				snapshotURL = presigned
			}
			log.Printf("[AI Monitor] %s – snapshot uploaded to S3: %s", key, snapshotS3Key)
		}
	}

	// 2. Send to SiliconFlow VLM
	apiStart := time.Now()
	vlmResp, inputTokens, outputTokens, apiErr := m.callVLM(ctx, b64)
	apiMs := time.Since(apiStart).Milliseconds()

	result := AnalysisResult{
		StreamKey:      key,
		Timestamp:      now,
		SnapshotBase64: b64, // kept for in-memory API access
		SnapshotS3Key:  snapshotS3Key,
		SnapshotURL:    snapshotURL,
		VLMResponse:    vlmResp,
		InputTokens:    inputTokens,
		OutputTokens:   outputTokens,
		APIResponseMs:  apiMs,
		Model:          m.cfg.SiliconFlowModel,
	}

	if apiErr != nil {
		result.OverallStatus = "error"
		result.APIError = apiErr.Error()
		log.Printf("[AI Monitor] %s – VLM API error: %v (took %dms)", key, apiErr, apiMs)
	} else {
		result.OverallStatus = vlmResp.OverallStatus
		log.Printf("[AI Monitor] %s – status=%s intrusion=%v obstruction=%v tampering=%v (took %dms, in=%d out=%d tokens)",
			key, vlmResp.OverallStatus,
			vlmResp.HumanIntrusion.Present,
			vlmResp.VisualObstruction.Present,
			vlmResp.CameraTampering.Present,
			apiMs, inputTokens, outputTokens)
	}

	// 3. If alert detected, save clip and enter cooldown
	hasAlert := vlmResp != nil && (vlmResp.OverallStatus == "warning" || vlmResp.OverallStatus == "critical")
	if hasAlert {
		st.alertActive = true
		st.lastAlertTime = now
		log.Printf("[AI Monitor] %s – ALERT detected (%s), saving 10min clip and entering cooldown", key, vlmResp.OverallStatus)

		go func() {
			clipKey, err := m.saveAlertClip(key, now)
			if err != nil {
				log.Printf("[AI Monitor] %s – failed to save alert clip: %v", key, err)
				// Still send to Tinybird without clip key
				go m.sendToTinybird(result)
				return
			}
			result.ClipS3Key = clipKey

			m.mu.Lock()
			clip := AlertClip{
				StreamKey:  key,
				Timestamp:  now,
				S3Bucket:   "",
				S3Key:      clipKey,
				DurationS:  600, // 10 minutes
				Status:     vlmResp.OverallStatus,
			}
			if m.cfg.S3Store != nil {
				clip.S3Bucket = m.cfg.S3Store.Bucket()
			}
			m.alerts = append(m.alerts, clip)
			if len(m.alerts) > maxRecentAlerts {
				m.alerts = m.alerts[len(m.alerts)-maxRecentAlerts:]
			}
			m.mu.Unlock()

			log.Printf("[AI Monitor] %s – alert clip saved: %s", key, clipKey)

			// Send to Tinybird AFTER clip is saved so clip_s3_key is included
			go m.sendToTinybird(result)
		}()
	}

	// 4. Record result and send to Tinybird (for non-alert results, send immediately)
	m.recordResult(result)
	if !hasAlert {
		go m.sendToTinybird(result)
	}
}

// ──────────────────────────── Snapshot ────────────────────────────

func (m *Monitor) captureSnapshot(key string) ([]byte, error) {
	// Use the live HLS stream to grab a JPEG frame
	m3u8Path := filepath.Join(m.cfg.HLSDir, key, "index.m3u8")
	if _, err := os.Stat(m3u8Path); err != nil {
		return nil, fmt.Errorf("live stream not found: %s", m3u8Path)
	}

	// ffmpeg: read HLS, grab 1 frame, output JPEG to stdout
	args := []string{
		"-y",
		"-i", m3u8Path,
		"-frames:v", "1",
		"-f", "image2",
		"-c:v", "mjpeg",
		"-q:v", "3", // quality (2-5 is good, lower=better)
		"pipe:1",
	}
	cmd := exec.Command("ffmpeg", args...)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("ffmpeg snapshot failed: %v stderr=%s", err, stderr.String())
	}
	if stdout.Len() == 0 {
		return nil, fmt.Errorf("ffmpeg produced empty snapshot")
	}
	return stdout.Bytes(), nil
}

// ──────────────────────────── SiliconFlow VLM ────────────────────────────

type siliconFlowRequest struct {
	Model       string                   `json:"model"`
	Messages    []siliconFlowMessage     `json:"messages"`
	MaxTokens   int                      `json:"max_tokens"`
	Temperature float64                  `json:"temperature"`
	Stream      bool                     `json:"stream"`
}

type siliconFlowMessage struct {
	Role    string        `json:"role"`
	Content interface{}   `json:"content"` // string or []contentPart
}

type contentPart struct {
	Type     string    `json:"type"`
	Text     string    `json:"text,omitempty"`
	ImageURL *imageURL `json:"image_url,omitempty"`
}

type imageURL struct {
	URL    string `json:"url"`
	Detail string `json:"detail,omitempty"`
}

type siliconFlowResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error,omitempty"`
}

func (m *Monitor) callVLM(ctx context.Context, base64Image string) (*VLMResponse, int, int, error) {
	reqBody := siliconFlowRequest{
		Model: m.cfg.SiliconFlowModel,
		Messages: []siliconFlowMessage{
			{
				Role:    "system",
				Content: systemPrompt,
			},
			{
				Role: "user",
				Content: []contentPart{
					{
						Type: "image_url",
						ImageURL: &imageURL{
							URL:    fmt.Sprintf("data:image/jpeg;base64,%s", base64Image),
							Detail: "low", // low to save tokens
						},
					},
					{
						Type: "text",
						Text: "Analyze this truck rear camera image. First verify the truck bed/cargo area is visible, then check for any human presence, visual obstruction, or camera tampering. Any person near the truck bed is suspicious — flag it immediately.",
					},
				},
			},
		},
		MaxTokens:   512,
		Temperature: 0.1,
		Stream:      false,
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", "https://api.siliconflow.com/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, 0, 0, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", m.cfg.SiliconFlowAPIKey))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("API call failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, 0, 0, fmt.Errorf("API returned %d: %s", resp.StatusCode, string(respBody))
	}

	var sfResp siliconFlowResponse
	if err := json.Unmarshal(respBody, &sfResp); err != nil {
		return nil, 0, 0, fmt.Errorf("unmarshal response: %w", err)
	}

	if sfResp.Error != nil {
		return nil, 0, 0, fmt.Errorf("API error: %s (%s)", sfResp.Error.Message, sfResp.Error.Type)
	}

	if len(sfResp.Choices) == 0 {
		return nil, 0, 0, fmt.Errorf("no choices in response")
	}

	content := sfResp.Choices[0].Message.Content
	inputTokens := sfResp.Usage.PromptTokens
	outputTokens := sfResp.Usage.CompletionTokens

	// Extract JSON from the response (it might be wrapped in markdown code blocks)
	jsonStr := extractJSON(content)

	var vlmResp VLMResponse
	if err := json.Unmarshal([]byte(jsonStr), &vlmResp); err != nil {
		return nil, inputTokens, outputTokens, fmt.Errorf("parse VLM JSON: %w (raw: %s)", err, content)
	}

	return &vlmResp, inputTokens, outputTokens, nil
}

// extractJSON extracts JSON from a string that might be wrapped in markdown code blocks.
func extractJSON(s string) string {
	s = strings.TrimSpace(s)
	// Try to find JSON within markdown code blocks
	if idx := strings.Index(s, "```json"); idx != -1 {
		s = s[idx+7:]
		if end := strings.Index(s, "```"); end != -1 {
			s = s[:end]
		}
	} else if idx := strings.Index(s, "```"); idx != -1 {
		s = s[idx+3:]
		if end := strings.Index(s, "```"); end != -1 {
			s = s[:end]
		}
	}
	s = strings.TrimSpace(s)
	// Find the first '{' and last '}'
	start := strings.Index(s, "{")
	end := strings.LastIndex(s, "}")
	if start >= 0 && end > start {
		s = s[start : end+1]
	}
	return s
}

// ──────────────────────────── Alert Clip ────────────────────────────

// saveAlertClip concatenates the last 5 minutes + next 5 minutes from archive segments.
// Because next 5 minutes haven't happened yet, we wait and then extract.
func (m *Monitor) saveAlertClip(key string, alertTime time.Time) (string, error) {
	if m.cfg.S3Store == nil {
		return "", fmt.Errorf("S3 not configured, cannot save alert clip")
	}

	// Wait for 5 minutes after alert to capture the "next 5 min" footage
	log.Printf("[AI Monitor] %s – waiting 5 minutes to capture post-alert footage...", key)
	time.Sleep(5 * time.Minute)

	// Find the recordings directory
	hlsKeyDir := filepath.Join(m.cfg.HLSDir, key, "recordings")
	entries, err := os.ReadDir(hlsKeyDir)
	if err != nil {
		return "", fmt.Errorf("read recordings dir: %w", err)
	}

	// Find the session(s) that cover the 10-minute window
	clipStart := alertTime.Add(-5 * time.Minute)
	clipEnd := alertTime.Add(5 * time.Minute)

	var sessionDir string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		st, err := time.ParseInLocation("2006-01-02_15-04-05", e.Name(), time.Local)
		if err != nil {
			continue
		}
		// Check if this session overlaps with our clip window
		playlistPath := filepath.Join(hlsKeyDir, e.Name(), "index.m3u8")
		if _, err := os.Stat(playlistPath); err != nil {
			continue
		}
		// The session started before clip end and is a reasonable candidate
		if st.Before(clipEnd) {
			sessionDir = filepath.Join(hlsKeyDir, e.Name())
		}
	}

	if sessionDir == "" {
		return "", fmt.Errorf("no recording session found covering alert time")
	}

	playlistPath := filepath.Join(sessionDir, "index.m3u8")

	// Calculate offset into the recording
	// Parse session start from directory name
	sessionName := filepath.Base(sessionDir)
	sessionStart, err := time.ParseInLocation("2006-01-02_15-04-05", sessionName, time.Local)
	if err != nil {
		return "", fmt.Errorf("parse session start: %w", err)
	}

	startOffset := clipStart.Sub(sessionStart).Seconds()
	if startOffset < 0 {
		startOffset = 0
	}
	duration := clipEnd.Sub(clipStart).Seconds()

	// Use ffmpeg to extract the clip
	tmpFile := filepath.Join(os.TempDir(), fmt.Sprintf("porteight-alert-%s-%d.mp4", key, alertTime.Unix()))
	defer os.Remove(tmpFile)

	args := []string{
		"-y",
		"-ss", fmt.Sprintf("%.2f", startOffset),
		"-i", playlistPath,
		"-t", fmt.Sprintf("%.2f", duration),
		"-c", "copy",
		"-bsf:a", "aac_adtstoasc",
		"-movflags", "+faststart",
		tmpFile,
	}
	cmd := exec.Command("ffmpeg", args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		// Try re-encode fallback
		args = []string{
			"-y",
			"-ss", fmt.Sprintf("%.2f", startOffset),
			"-i", playlistPath,
			"-t", fmt.Sprintf("%.2f", duration),
			"-c:v", "libx264",
			"-preset", "veryfast",
			"-crf", "23",
			"-c:a", "aac",
			"-b:a", "96k",
			"-movflags", "+faststart",
			tmpFile,
		}
		cmd = exec.Command("ffmpeg", args...)
		output, err = cmd.CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("ffmpeg clip extraction failed: %v output=%s", err, string(output))
		}
	}

	// Verify file exists and has content
	fi, err := os.Stat(tmpFile)
	if err != nil || fi.Size() == 0 {
		return "", fmt.Errorf("clip extraction produced empty file")
	}

	// Upload to S3 under ai-alerts/ prefix
	s3Key := m.cfg.S3Store.RecordingPrefixForSession(key, "ai-alerts") +
		fmt.Sprintf("/%s.mp4", alertTime.Format("2006-01-02_15-04-05"))

	if err := m.cfg.S3Store.UploadFile(context.Background(), tmpFile, s3Key); err != nil {
		return "", fmt.Errorf("S3 upload failed: %w", err)
	}

	return s3Key, nil
}

// ──────────────────────────── Tinybird ────────────────────────────

func (m *Monitor) sendToTinybird(result AnalysisResult) {
	if m.cfg.TinybirdToken == "" || m.cfg.TinybirdBaseURL == "" {
		return
	}

	// Build NDJSON row — send S3 key instead of base64 blob
	row := map[string]interface{}{
		"stream_key":       result.StreamKey,
		"timestamp":        result.Timestamp.UTC().Format("2006-01-02 15:04:05"),
		"snapshot_s3_key":  result.SnapshotS3Key,
		"overall_status":   result.OverallStatus,
		"input_tokens":     result.InputTokens,
		"output_tokens":    result.OutputTokens,
		"api_response_ms":  result.APIResponseMs,
		"api_error":        result.APIError,
		"clip_s3_key":      result.ClipS3Key,
		"model":            result.Model,
	}

	if result.VLMResponse != nil {
		row["human_intrusion_present"] = result.VLMResponse.HumanIntrusion.Present
		row["human_intrusion_description"] = result.VLMResponse.HumanIntrusion.Description
		row["visual_obstruction_present"] = result.VLMResponse.VisualObstruction.Present
		row["visual_obstruction_description"] = result.VLMResponse.VisualObstruction.Description
		row["camera_tampering_present"] = result.VLMResponse.CameraTampering.Present
		row["camera_tampering_description"] = result.VLMResponse.CameraTampering.Description
	}

	body, err := json.Marshal(row)
	if err != nil {
		log.Printf("[AI Monitor] Tinybird marshal error: %v", err)
		return
	}

	url := fmt.Sprintf("%s/v0/events?name=%s", m.cfg.TinybirdBaseURL, m.cfg.TinybirdDSName)
	req, err := http.NewRequest("POST", url, bytes.NewReader(body))
	if err != nil {
		log.Printf("[AI Monitor] Tinybird request error: %v", err)
		return
	}
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", m.cfg.TinybirdToken))
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Printf("[AI Monitor] Tinybird send error: %v", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusAccepted {
		respBody, _ := io.ReadAll(resp.Body)
		log.Printf("[AI Monitor] Tinybird returned %d: %s", resp.StatusCode, string(respBody))
		return
	}

	log.Printf("[AI Monitor] %s – sent to Tinybird (status=%s)", result.StreamKey, result.OverallStatus)
}

// ──────────────────────────── Internal ────────────────────────────

func (m *Monitor) recordResult(r AnalysisResult) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.results = append(m.results, r)
	if len(m.results) > maxRecentResults {
		m.results = m.results[len(m.results)-maxRecentResults:]
	}
}

// GetAlertClipURL generates a presigned URL for an alert clip.
func (m *Monitor) GetAlertClipURL(s3Key string) (string, error) {
	if m.cfg.S3Store == nil {
		return "", fmt.Errorf("S3 not configured")
	}
	return m.cfg.S3Store.PresignGet(context.Background(), s3Key)
}
