package stream

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"porteight-camera-ai/internal/db"

	"github.com/datarhei/joy4/av"
	"github.com/datarhei/joy4/codec/h264parser"
	"github.com/datarhei/joy4/format/flv"
	"github.com/datarhei/joy4/format/rtmp"
)

type Server struct {
	rtmpServer          *rtmp.Server
	addr                string
	hlsDir              string
	recDir              string
	streams             map[string]*StreamSession
	mu                  sync.RWMutex
	onRecordingStart    func(key string, session string, localDir string)
	onRecordingComplete func(key string, session string, localDir string)
}

type StreamSession struct {
	Key        string
	Active     bool
	StartTime  time.Time
	LiveCmd    *exec.Cmd
	ArchiveCmd *exec.Cmd
	Stdin      io.WriteCloser
}

// StreamMetadata is written to a JSON file for the player to read
type StreamMetadata struct {
	Key       string `json:"key"`
	StartTime int64  `json:"startTime"`
	Active    bool   `json:"active"`
}

// Recording represents a saved recording
type Recording struct {
	Key       string  `json:"key"`
	Filename  string  `json:"filename"`  // session folder name (timestamp)
	Playlist  string  `json:"playlist"`  // URL path (/static/hls/<key>/recordings/<session>/index.m3u8)
	StartTime int64   `json:"startTime"` // Unix timestamp
	EndTime   int64   `json:"endTime"`   // Unix timestamp
	Size      int64   `json:"size"`
	Duration  float64 `json:"duration"` // seconds
	IsLive    bool    `json:"isLive"`
}

// StreamKeyInfo represents a stream key with its recording summary
type StreamKeyInfo struct {
	Key             string  `json:"key"`
	TotalRecordings int     `json:"totalRecordings"`
	TotalDuration   float64 `json:"totalDuration"`
	TotalSize       int64   `json:"totalSize"`
	IsLive          bool    `json:"isLive"`
	EarliestRecord  int64   `json:"earliestRecord"`
	LatestRecord    int64   `json:"latestRecord"`
}

func NewServer(addr, hlsDir, recDir string) *Server {
	s := &Server{
		addr:    addr,
		hlsDir:  hlsDir,
		recDir:  recDir,
		streams: make(map[string]*StreamSession),
	}

	s.rtmpServer = &rtmp.Server{
		Addr:          addr,
		HandlePublish: s.handlePublish,
		HandlePlay:    s.handlePlay,
	}

	return s
}

func (s *Server) SetOnRecordingComplete(fn func(key string, session string, localDir string)) {
	s.onRecordingComplete = fn
}

func (s *Server) SetOnRecordingStart(fn func(key string, session string, localDir string)) {
	s.onRecordingStart = fn
}

func (s *Server) Start() error {
	os.MkdirAll(s.hlsDir, 0755)
	os.MkdirAll(s.recDir, 0755)

	log.Printf("RTMP Server starting on %s", s.addr)
	return s.rtmpServer.ListenAndServe()
}

func (s *Server) handlePlay(conn *rtmp.Conn) {
	conn.Close()
}

func stripAnnexBStartCode(nalu []byte) []byte {
	if len(nalu) >= 4 && bytes.Equal(nalu[:4], []byte{0, 0, 0, 1}) {
		return nalu[4:]
	}
	if len(nalu) >= 3 && bytes.Equal(nalu[:3], []byte{0, 0, 1}) {
		return nalu[3:]
	}
	return nalu
}

func extractH264SpsPps(pktData []byte) (sps []byte, pps []byte) {
	nalus, _ := h264parser.SplitNALUs(pktData)
	for _, n := range nalus {
		n = stripAnnexBStartCode(n)
		if len(n) == 0 {
			continue
		}
		typ := n[0] & 0x1f
		switch typ {
		case 7: // SPS
			if sps == nil {
				sps = n
			}
		case 8: // PPS
			if pps == nil {
				pps = n
			}
		}
		if sps != nil && pps != nil {
			return sps, pps
		}
	}
	return sps, pps
}

func (s *Server) handlePublish(conn *rtmp.Conn) {
	parts := strings.Split(strings.Trim(conn.URL.Path, "/"), "/")
	if len(parts) < 2 {
		log.Println("Invalid path")
		conn.Close()
		return
	}
	key := parts[len(parts)-1]

	sk, err := db.GetStreamKey(key)
	if err != nil || sk == nil {
		log.Printf("Invalid stream key: %s", key)
		conn.Close()
		return
	}

	// Fetch streams *after* key validation and try to ensure we have usable codec config
	// (some cameras publish H264 without an AVC sequence header; FFmpeg then can't detect frame size).
	streams, _ := conn.Streams()

	log.Printf("RTMP publish request: key=%s from=%s path=%s streams=%d", key, conn.RemoteAddr(), conn.URL.Path, len(streams))
	for i, st := range streams {
		switch c := st.(type) {
		case av.VideoCodecData:
			log.Printf("  stream[%d] video codec=%v resolution=%dx%d", i, c.Type(), c.Width(), c.Height())
		case av.AudioCodecData:
			log.Printf("  stream[%d] audio codec=%v sample_rate=%dHz channels=%d sample_fmt=%v", i, c.Type(), c.SampleRate(), c.ChannelLayout().Count(), c.SampleFormat())
		default:
			log.Printf("  stream[%d] codec=%v type=%T", i, st.Type(), st)
		}
	}

	startTime := time.Now()
	log.Printf("Stream started: %s at %v", key, startTime)

	// Setup directories
	liveDir := filepath.Join(s.hlsDir, key)
	recordingsRoot := filepath.Join(liveDir, "recordings")
	sessionName := startTime.Format("2006-01-02_15-04-05")
	// Full HLS archive directory (this *is* the recording)
	archiveDir := filepath.Join(recordingsRoot, sessionName)

	// Ensure base dirs exist
	os.MkdirAll(liveDir, 0755)
	os.MkdirAll(recordingsRoot, 0755)
	os.MkdirAll(archiveDir, 0755)

	// Clear old LIVE HLS data (but keep recordings/)
	if entries, err := os.ReadDir(liveDir); err == nil {
		for _, e := range entries {
			if e.Name() == "recordings" {
				continue
			}
			_ = os.RemoveAll(filepath.Join(liveDir, e.Name()))
		}
	}

	if s.onRecordingStart != nil {
		// Start hooks should not block ingest.
		go s.onRecordingStart(key, sessionName, archiveDir)
	}

	// Write stream metadata
	metadata := StreamMetadata{
		Key:       key,
		StartTime: startTime.Unix(),
		Active:    true,
	}
	s.writeStreamMetadata(liveDir, metadata)

	// If H264 video is missing size/config, try to recover SPS/PPS from early packets (no transcoding).
	// We buffer a small number of packets and, if successful, rebuild the H264 codec config record.
	var prePkts []av.Packet
	var videoIdx = -1
	var needH264Repair bool
	for i, st := range streams {
		if v, ok := st.(av.VideoCodecData); ok && v.Type() == av.H264 {
			videoIdx = i
			if v.Width() == 0 || v.Height() == 0 {
				needH264Repair = true
				log.Printf("H264 stream missing resolution in codec header (stream[%d]); attempting SPS/PPS recovery from packets", i)
			}
			break
		}
	}

	if needH264Repair && videoIdx >= 0 {
		deadline := time.Now().Add(5 * time.Second)
		var sps, pps []byte
		for time.Now().Before(deadline) && (sps == nil || pps == nil) && len(prePkts) < 300 {
			pkt, err := conn.ReadPacket()
			if err != nil {
				log.Printf("Failed while probing packets for SPS/PPS: %v", err)
				break
			}
			prePkts = append(prePkts, pkt)

			// Only inspect video packets
			if int(pkt.Idx) != videoIdx || len(pkt.Data) == 0 {
				continue
			}

			ps, pp := extractH264SpsPps(pkt.Data)
			if sps == nil && ps != nil {
				sps = ps
			}
			if pps == nil && pp != nil {
				pps = pp
			}
		}

		if sps != nil && pps != nil {
			if cd, err := h264parser.NewCodecDataFromSPSAndPPS(sps, pps); err == nil {
				streams[videoIdx] = cd
				log.Printf("Recovered H264 SPS/PPS from packets; repaired codec header. resolution=%dx%d", cd.Width(), cd.Height())
			} else {
				log.Printf("Found SPS/PPS but failed to build H264 codec data: %v", err)
			}
		} else {
			log.Printf("Could not recover SPS/PPS from early packets. Camera may not send SPS/PPS/IDR; FFmpeg copy-mode will likely fail until it does.")
		}
	}

	// Create a pipe to tee the input to both FFmpeg processes
	pipeReader, pipeWriter := io.Pipe()

	// FFmpeg for LIVE HLS (low latency, small buffer)
	liveArgs := []string{
		"-y",
		"-analyzeduration", "20M",
		"-probesize", "20M",
		"-fflags", "+igndts+genpts",
		"-use_wallclock_as_timestamps", "1",
		"-f", "flv", "-i", "pipe:0",
		"-c:v", "copy", "-c:a", "copy",
		"-f", "hls",
		"-hls_time", "2",
		"-hls_list_size", "5",
		"-hls_flags", "delete_segments+independent_segments",
		"-hls_segment_type", "mpegts",
		"-hls_segment_filename", filepath.Join(liveDir, "seg_%06d.ts"),
		filepath.Join(liveDir, "index.m3u8"),
	}
	liveCmd := exec.Command("ffmpeg", liveArgs...)
	liveStdin, err := liveCmd.StdinPipe()
	if err != nil {
		log.Println("Failed to create live ffmpeg stdin:", err)
		conn.Close()
		return
	}
	liveCmd.Stderr = os.Stderr
	if err := liveCmd.Start(); err != nil {
		log.Println("Failed to start live ffmpeg:", err)
		conn.Close()
		return
	}

	// FFmpeg for FULL HLS ARCHIVE (keeps all segments until disconnect)
	archiveArgs := []string{
		"-y",
		"-analyzeduration", "20M",
		"-probesize", "20M",
		"-fflags", "+igndts+genpts",
		"-use_wallclock_as_timestamps", "1",
		"-f", "flv", "-i", "pipe:0",
		"-c:v", "copy", "-c:a", "copy",
		"-f", "hls",
		"-hls_time", "2",
		"-hls_list_size", "0",
		"-hls_playlist_type", "event",
		"-hls_flags", "independent_segments",
		"-hls_segment_type", "mpegts",
		"-hls_segment_filename", filepath.Join(archiveDir, "seg_%06d.ts"),
		filepath.Join(archiveDir, "index.m3u8"),
	}
	archiveCmd := exec.Command("ffmpeg", archiveArgs...)
	archiveStdin, err := archiveCmd.StdinPipe()
	if err != nil {
		log.Println("Failed to create archive ffmpeg stdin:", err)
		liveCmd.Process.Kill()
		conn.Close()
		return
	}
	archiveCmd.Stderr = os.Stderr
	if err := archiveCmd.Start(); err != nil {
		log.Println("Failed to start archive ffmpeg:", err)
		liveCmd.Process.Kill()
		conn.Close()
		return
	}

	// Record session
	session := &StreamSession{
		Key:        key,
		Active:     true,
		StartTime:  startTime,
		LiveCmd:    liveCmd,
		ArchiveCmd: archiveCmd,
		Stdin:      pipeWriter,
	}
	s.mu.Lock()
	s.streams[key] = session
	s.mu.Unlock()

	// Tee goroutine: read from pipe and write to both FFmpeg processes (live + archive)
	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, err := pipeReader.Read(buf)
			if err != nil {
				break
			}
			if n > 0 {
				liveStdin.Write(buf[:n])
				archiveStdin.Write(buf[:n])
			}
		}
		liveStdin.Close()
		archiveStdin.Close()
	}()

	// Create Muxer to pipe to our tee writer
	muxer := flv.NewMuxer(pipeWriter)
	err = muxer.WriteHeader(streams)
	if err != nil {
		log.Println("Failed to write header:", err)
		liveCmd.Process.Kill()
		archiveCmd.Process.Kill()
		conn.Close()
		return
	}

	// Write any pre-buffered packets first (from SPS/PPS recovery), then stream the rest.
	for _, pkt := range prePkts {
		if err := muxer.WritePacket(pkt); err != nil {
			log.Printf("Stream write error (prebuffer): %v", err)
			break
		}
	}
	for {
		pkt, err := conn.ReadPacket()
		if err != nil {
			if err != io.EOF {
				log.Printf("Stream read error: %v", err)
			}
			break
		}
		if err := muxer.WritePacket(pkt); err != nil {
			log.Printf("Stream write error: %v", err)
			break
		}
	}

	// Cleanup
	log.Printf("Stream ended: %s", key)
	pipeWriter.Close()

	liveCmd.Wait()
	archiveCmd.Wait()

	// Mark stream as inactive
	metadata.Active = false
	s.writeStreamMetadata(liveDir, metadata)

	s.mu.Lock()
	delete(s.streams, key)
	s.mu.Unlock()

	log.Printf("HLS recording saved: %s", archiveDir)

	if s.onRecordingComplete != nil {
		// Fire-and-forget (do not block RTMP handler shutdown)
		go s.onRecordingComplete(key, sessionName, archiveDir)
	}
}

func (s *Server) writeStreamMetadata(keyDir string, meta StreamMetadata) {
	metaPath := filepath.Join(keyDir, "stream.json")
	data, err := json.Marshal(meta)
	if err != nil {
		log.Printf("Failed to marshal metadata: %v", err)
		return
	}
	if err := os.WriteFile(metaPath, data, 0644); err != nil {
		log.Printf("Failed to write metadata: %v", err)
	}
}

// GetActiveStreams returns list of active stream keys
// Checks both RTMP sessions AND actively updating HLS directories
func (s *Server) GetActiveStreams() []string {
	s.mu.RLock()
	rtmpStreams := make(map[string]bool)
	for k := range s.streams {
		rtmpStreams[k] = true
	}
	s.mu.RUnlock()

	// Also check for actively updating HLS streams (ffmpeg running directly)
	activeKeys := make([]string, 0)

	// Add RTMP-tracked streams
	for k := range rtmpStreams {
		activeKeys = append(activeKeys, k)
	}

	// Check HLS directories for recent updates
	entries, err := os.ReadDir(s.hlsDir)
	if err == nil {
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			key := entry.Name()

			// Skip if already tracked via RTMP
			if rtmpStreams[key] {
				continue
			}

			// Check if m3u8 was updated in last 10 seconds
			m3u8Path := filepath.Join(s.hlsDir, key, "index.m3u8")
			if info, err := os.Stat(m3u8Path); err == nil {
				if time.Since(info.ModTime()) < 10*time.Second {
					activeKeys = append(activeKeys, key)
				}
			}
		}
	}

	return activeKeys
}

// getVideoDuration uses ffprobe to get video duration
func getVideoDuration(filepath string) float64 {
	cmd := exec.Command("ffprobe",
		"-v", "error",
		"-show_entries", "format=duration",
		"-of", "default=noprint_wrappers=1:nokey=1",
		filepath,
	)
	output, err := cmd.Output()
	if err != nil {
		return 0
	}

	duration := strings.TrimSpace(string(output))
	var d float64
	fmt.Sscanf(duration, "%f", &d)
	return d
}

// getHLSDurationSeconds parses an HLS playlist and sums EXTINF durations.
// This is more reliable than ffprobe for in-progress EVENT playlists.
func getHLSDurationSeconds(m3u8Path string) float64 {
	b, err := os.ReadFile(m3u8Path)
	if err != nil {
		return 0
	}
	var total float64
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "#EXTINF:") {
			continue
		}
		rest := strings.TrimPrefix(line, "#EXTINF:")
		// EXTINF:<duration>,[title]
		if idx := strings.Index(rest, ","); idx != -1 {
			rest = rest[:idx]
		}
		rest = strings.TrimSpace(rest)
		if rest == "" {
			continue
		}
		if v, err := strconv.ParseFloat(rest, 64); err == nil && v > 0 {
			total += v
		}
	}
	return total
}

// IsStreamLive checks if a stream is currently live (RTMP or HLS updating)
func (s *Server) IsStreamLive(key string) bool {
	// Check RTMP sessions
	s.mu.RLock()
	_, isRTMPLive := s.streams[key]
	s.mu.RUnlock()

	if isRTMPLive {
		return true
	}

	// Check if HLS is being actively updated
	m3u8Path := filepath.Join(s.hlsDir, key, "index.m3u8")
	if info, err := os.Stat(m3u8Path); err == nil {
		if time.Since(info.ModTime()) < 10*time.Second {
			return true
		}
	}

	return false
}

// GetRecordings returns all recordings for a stream key
func (s *Server) GetRecordings(key string) []Recording {
	recordings := []Recording{}
	keyRecDir := filepath.Join(s.hlsDir, key, "recordings")

	// Check if directory exists
	if _, err := os.Stat(keyRecDir); os.IsNotExist(err) {
		return recordings
	}

	dirs, err := os.ReadDir(keyRecDir)
	if err != nil {
		log.Printf("Failed to read recordings dir: %v", err)
		return recordings
	}

	// If stream is currently live, the most recently updated recording session is considered "live".
	isKeyLive := s.IsStreamLive(key)
	var mostRecentSession string
	var mostRecentMod time.Time

	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}

		sessionName := d.Name()
		playlistPath := filepath.Join(keyRecDir, sessionName, "index.m3u8")
		info, err := os.Stat(playlistPath)
		if err != nil {
			continue
		}

		// Parse timestamp from session folder name (format: 2006-01-02_15-04-05)
		startTime, err := time.ParseInLocation("2006-01-02_15-04-05", sessionName, time.Local)
		if err != nil {
			startTime = info.ModTime()
		}

		// Get duration from playlist by parsing EXTINF (works while still recording).
		duration := getHLSDurationSeconds(playlistPath)
		endTime := startTime.Unix() + int64(duration)
		if duration <= 0 {
			// Fallback to ffprobe for completed playlists that might omit EXTINF (rare)
			duration = getVideoDuration(playlistPath)
			endTime = startTime.Unix() + int64(duration)
		}

		// Track most recently updated session (for IsLive)
		if info.ModTime().After(mostRecentMod) {
			mostRecentMod = info.ModTime()
			mostRecentSession = sessionName
		}

		// Estimate size by summing files in session directory
		var totalSize int64
		if entries, err := os.ReadDir(filepath.Join(keyRecDir, sessionName)); err == nil {
			for _, e := range entries {
				if e.IsDir() {
					continue
				}
				if fi, err := e.Info(); err == nil {
					totalSize += fi.Size()
				}
			}
		}

		recordings = append(recordings, Recording{
			Key:       key,
			Filename:  sessionName,
			Playlist:  fmt.Sprintf("/static/hls/%s/recordings/%s/index.m3u8", key, sessionName),
			StartTime: startTime.Unix(),
			EndTime:   endTime,
			Size:      totalSize,
			Duration:  duration,
			IsLive:    false,
		})
	}

	// Mark the most recently updated session as live if the key is live and playlist is updating.
	if isKeyLive && mostRecentSession != "" && time.Since(mostRecentMod) < 30*time.Second {
		for i := range recordings {
			if recordings[i].Filename == mostRecentSession {
				recordings[i].IsLive = true
				// Also extend endTime to "now" so it displays even if EXTINF hasn't caught up
				recordings[i].EndTime = time.Now().Unix()
				if recordings[i].EndTime > recordings[i].StartTime {
					recordings[i].Duration = float64(recordings[i].EndTime - recordings[i].StartTime)
				}
				break
			}
		}
	}

	// Sort by start time ascending (oldest first for timeline)
	sort.Slice(recordings, func(i, j int) bool {
		return recordings[i].StartTime < recordings[j].StartTime
	})

	return recordings
}

// GetStreamKeys returns list of all stream keys with recording summaries
func (s *Server) GetStreamKeys() []StreamKeyInfo {
	keys := []StreamKeyInfo{}

	entries, err := os.ReadDir(s.hlsDir)
	if err != nil {
		return keys
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		key := entry.Name()
		recordings := s.GetRecordings(key)

		if len(recordings) == 0 {
			continue
		}

		var totalDuration float64
		var totalSize int64
		var earliest, latest int64

		// Check if stream is live (RTMP or HLS)
		isLive := s.IsStreamLive(key)

		for i, rec := range recordings {
			totalDuration += rec.Duration
			totalSize += rec.Size
			if i == 0 {
				earliest = rec.StartTime
				latest = rec.EndTime
			} else {
				if rec.StartTime < earliest {
					earliest = rec.StartTime
				}
				if rec.EndTime > latest {
					latest = rec.EndTime
				}
			}
		}

		keys = append(keys, StreamKeyInfo{
			Key:             key,
			TotalRecordings: len(recordings),
			TotalDuration:   totalDuration,
			TotalSize:       totalSize,
			IsLive:          isLive,
			EarliestRecord:  earliest,
			LatestRecord:    latest,
		})
	}

	return keys
}

// GetAllRecordings returns recordings for all stream keys
func (s *Server) GetAllRecordings() map[string][]Recording {
	allRecordings := make(map[string][]Recording)

	// Get all stream key directories
	entries, err := os.ReadDir(s.hlsDir)
	if err != nil {
		return allRecordings
	}

	for _, entry := range entries {
		if entry.IsDir() {
			key := entry.Name()
			recordings := s.GetRecordings(key)
			if len(recordings) > 0 {
				allRecordings[key] = recordings
			}
		}
	}

	return allRecordings
}

// GetRecDir returns the recordings directory path
func (s *Server) GetRecDir() string {
	return s.recDir
}
