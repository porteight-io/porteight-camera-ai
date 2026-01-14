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
	rtmpServer *rtmp.Server
	addr       string
	hlsDir     string
	recDir     string
	streams    map[string]*StreamSession
	mu         sync.RWMutex
}

type StreamSession struct {
	Key           string
	Active        bool
	StartTime     time.Time
	LiveCmd       *exec.Cmd
	RecordCmd     *exec.Cmd
	Stdin         io.WriteCloser
	RecordingFile string
}

// StreamMetadata is written to a JSON file for the player to read
type StreamMetadata struct {
	Key           string `json:"key"`
	StartTime     int64  `json:"startTime"`
	Active        bool   `json:"active"`
	RecordingFile string `json:"recordingFile,omitempty"`
}

// Recording represents a saved recording
type Recording struct {
	Key       string  `json:"key"`
	Filename  string  `json:"filename"`
	Path      string  `json:"path"`
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
	keyRecDir := filepath.Join(s.recDir, key)

	// Clear old LIVE HLS data
	os.RemoveAll(liveDir)
	os.MkdirAll(liveDir, 0755)
	os.MkdirAll(keyRecDir, 0755)

	// Recording filename with timestamp
	recordingFilename := fmt.Sprintf("%s.mp4", startTime.Format("2006-01-02_15-04-05"))
	recordingPath := filepath.Join(keyRecDir, recordingFilename)

	// Write stream metadata
	metadata := StreamMetadata{
		Key:           key,
		StartTime:     startTime.Unix(),
		Active:        true,
		RecordingFile: recordingFilename,
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

	// FFmpeg for Recording (re-encode to reduce size)
	// Switch to H.265 for better compression vs. H.264.
	// Adjust CRF/preset/scale to tune quality vs. CPU usage.
	recordArgs := []string{
		"-y",
		"-analyzeduration", "20M",
		"-probesize", "20M",
		"-fflags", "+igndts+genpts",
		"-use_wallclock_as_timestamps", "1",
		"-f", "flv", "-i", "pipe:0",
		// Video: H.265 (libx265) for higher compression efficiency
		"-c:v", "libx265",
		"-preset", "veryfast", // faster encode, larger files; change to "slow" for smaller
		"-crf", "28", // lower=better quality/larger size; x265 default ~28
		"-maxrate", "2M", // cap bitrate to avoid large spikes
		"-bufsize", "4M",
		"-vf", "scale=-2:720", // downscale to 720p while keeping aspect (-2 keeps width divisible by 2)
		"-tag:v", "hvc1", // improve MP4 compatibility on Apple devices
		// Audio: transcode to AAC at a modest bitrate
		"-c:a", "aac",
		"-b:a", "96k",
		"-movflags", "+faststart",
		recordingPath,
	}
	recordCmd := exec.Command("ffmpeg", recordArgs...)
	recordStdin, err := recordCmd.StdinPipe()
	if err != nil {
		log.Println("Failed to create record ffmpeg stdin:", err)
		liveCmd.Process.Kill()
		conn.Close()
		return
	}
	recordCmd.Stderr = os.Stderr
	if err := recordCmd.Start(); err != nil {
		log.Println("Failed to start record ffmpeg:", err)
		liveCmd.Process.Kill()
		conn.Close()
		return
	}

	// Record session
	session := &StreamSession{
		Key:           key,
		Active:        true,
		StartTime:     startTime,
		LiveCmd:       liveCmd,
		RecordCmd:     recordCmd,
		Stdin:         pipeWriter,
		RecordingFile: recordingFilename,
	}
	s.mu.Lock()
	s.streams[key] = session
	s.mu.Unlock()

	// Tee goroutine: read from pipe and write to both FFmpeg processes
	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, err := pipeReader.Read(buf)
			if err != nil {
				break
			}
			if n > 0 {
				liveStdin.Write(buf[:n])
				recordStdin.Write(buf[:n])
			}
		}
		liveStdin.Close()
		recordStdin.Close()
	}()

	// Create Muxer to pipe to our tee writer
	muxer := flv.NewMuxer(pipeWriter)
	err = muxer.WriteHeader(streams)
	if err != nil {
		log.Println("Failed to write header:", err)
		liveCmd.Process.Kill()
		recordCmd.Process.Kill()
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
	recordCmd.Wait()

	// Mark stream as inactive
	metadata.Active = false
	s.writeStreamMetadata(liveDir, metadata)

	s.mu.Lock()
	delete(s.streams, key)
	s.mu.Unlock()

	log.Printf("Recording saved: %s", recordingPath)
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
	keyRecDir := filepath.Join(s.recDir, key)

	// Check if directory exists
	if _, err := os.Stat(keyRecDir); os.IsNotExist(err) {
		return recordings
	}

	files, err := os.ReadDir(keyRecDir)
	if err != nil {
		log.Printf("Failed to read recordings dir: %v", err)
		return recordings
	}

	// Check if there's an active stream for this key (RTMP or HLS)
	isLive := s.IsStreamLive(key)

	// Get RTMP session info if available
	s.mu.RLock()
	activeSession, hasRTMPSession := s.streams[key]
	s.mu.RUnlock()

	for _, f := range files {
		if f.IsDir() || !strings.HasSuffix(f.Name(), ".mp4") {
			continue
		}

		info, err := f.Info()
		if err != nil {
			continue
		}

		// Parse timestamp from filename (format: 2006-01-02_15-04-05.mp4)
		name := strings.TrimSuffix(f.Name(), ".mp4")
		startTime, err := time.ParseInLocation("2006-01-02_15-04-05", name, time.Local)
		if err != nil {
			startTime = info.ModTime()
		}

		// Check if this specific recording is the live one
		isThisLive := false
		if isLive {
			if hasRTMPSession && activeSession.RecordingFile == f.Name() {
				isThisLive = true
			} else if !hasRTMPSession {
				// For non-RTMP streams, mark the most recent recording as live if being updated
				// Check if this file is being actively written (modified recently)
				if time.Since(info.ModTime()) < 30*time.Second {
					isThisLive = true
				}
			}
		}

		// Get duration (skip for live recordings as file is still being written)
		var duration float64
		var endTime int64

		if isThisLive {
			// For live, calculate duration from start time to now
			duration = time.Since(startTime).Seconds()
			endTime = time.Now().Unix()
		} else {
			// Get actual duration from file
			duration = getVideoDuration(filepath.Join(keyRecDir, f.Name()))
			endTime = startTime.Unix() + int64(duration)
		}

		recordings = append(recordings, Recording{
			Key:       key,
			Filename:  f.Name(),
			Path:      filepath.Join(keyRecDir, f.Name()),
			StartTime: startTime.Unix(),
			EndTime:   endTime,
			Size:      info.Size(),
			Duration:  duration,
			IsLive:    isThisLive,
		})
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

	entries, err := os.ReadDir(s.recDir)
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
	entries, err := os.ReadDir(s.recDir)
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
