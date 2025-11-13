#!/usr/bin/env python3
"""
RTMP to LL-HLS Converter
Accepts RTMP streams from cameras and converts them to Low-Latency HLS for browser streaming
"""

import os
import subprocess
import threading
import logging
from pathlib import Path
from flask import Flask, send_from_directory, render_template_string, Response, request
from flask_cors import CORS
import json
import signal
import sys

# Configure logging
logging.basicConfig(
    level=logging.INFO,
    format='%(asctime)s - %(name)s - %(levelname)s - %(message)s',
    handlers=[
        logging.StreamHandler(),
        logging.FileHandler('rtmp_to_hls.log')
    ]
)
logger = logging.getLogger(__name__)

app = Flask(__name__)
CORS(app)

# Configuration
RTMP_PORT = 1935
HTTP_PORT = 8080
HLS_OUTPUT_DIR = Path("hls_output")
RECORDINGS_DIR = Path("recordings")
FRAMES_DIR = Path("frames")  # Directory for AI frame analysis
HLS_SEGMENT_DURATION = 1  # seconds (short for low latency)
HLS_PLAYLIST_SIZE = 6  # number of segments in playlist
PART_DURATION = 0.2  # LL-HLS part duration in seconds
RECORDING_SEGMENT_DURATION = 300  # 5 minutes in seconds
FRAME_EXTRACTION_FPS = 1  # Extract 1 frame per second for AI analysis

# Store active streams
active_streams = {}
ffmpeg_processes = {}
frame_callback_function = None  # Global frame callback for AI processing


class RTMPtoHLSConverter:
    """Handles conversion of RTMP streams to LL-HLS"""
    
    def __init__(self, stream_key, output_dir, frame_callback=None):
        self.stream_key = stream_key
        self.output_dir = output_dir / stream_key
        self.output_dir.mkdir(parents=True, exist_ok=True)
        self.recordings_dir = RECORDINGS_DIR / stream_key
        self.recordings_dir.mkdir(parents=True, exist_ok=True)
        self.frames_dir = FRAMES_DIR / stream_key
        self.frames_dir.mkdir(parents=True, exist_ok=True)
        self.process = None
        self.recording_process = None
        self.frame_process = None
        self.rtmp_url = None
        self.should_run = True
        self.monitor_thread = None
        self.frame_monitor_thread = None
        self.last_output_time = None
        self.session_start_time = None
        self.frame_callback = frame_callback  # User-provided callback for AI processing
        self.last_processed_frame = None
        # Create dedicated log file for this stream's ffmpeg output
        self.ffmpeg_log_file = self.output_dir / f"ffmpeg_{stream_key}.log"
        self.ffmpeg_logger = self._setup_stream_logger()
    
    def _setup_stream_logger(self):
        """Setup dedicated logger for this stream's FFmpeg output"""
        stream_logger = logging.getLogger(f"ffmpeg.{self.stream_key}")
        stream_logger.setLevel(logging.DEBUG)
        
        # Remove any existing handlers
        stream_logger.handlers = []
        
        # File handler for FFmpeg output
        fh = logging.FileHandler(self.ffmpeg_log_file)
        fh.setLevel(logging.DEBUG)
        formatter = logging.Formatter('%(asctime)s - %(levelname)s - %(message)s')
        fh.setFormatter(formatter)
        stream_logger.addHandler(fh)
        
        # Also log to console at INFO level
        ch = logging.StreamHandler()
        ch.setLevel(logging.INFO)
        ch.setFormatter(formatter)
        stream_logger.addHandler(ch)
        
        return stream_logger
        
    def start(self, rtmp_url):
        """Start FFmpeg process to convert RTMP to LL-HLS"""
        import datetime
        self.rtmp_url = rtmp_url
        self.should_run = True
        self.session_start_time = datetime.datetime.now()
        
        # Clean any old live stream data for fresh start
        self._clean_live_stream_data()
        
        # Start the HLS FFmpeg process
        if not self._start_ffmpeg():
            return False
        
        # Start the recording FFmpeg process (5-minute segments)
        if not self._start_recording():
            self.ffmpeg_logger.warning("Failed to start recording process, but HLS will continue")
        
        # Start the frame extraction process for AI analysis
        if not self._start_frame_extraction():
            self.ffmpeg_logger.warning("Failed to start frame extraction, but streaming/recording will continue")
        
        # Start monitoring thread
        self.monitor_thread = threading.Thread(
            target=self._monitor_process,
            daemon=True
        )
        self.monitor_thread.start()
        
        # Start frame monitoring thread if callback is provided
        if self.frame_callback:
            self.frame_monitor_thread = threading.Thread(
                target=self._monitor_frames,
                daemon=True
            )
            self.frame_monitor_thread.start()
            self.ffmpeg_logger.info("Frame monitoring for AI analysis enabled")
        
        return True
    
    def _start_ffmpeg(self):
        """Internal method to start FFmpeg"""
        playlist_path = self.output_dir / "playlist.m3u8"
        
        # FFmpeg command for LL-HLS conversion with detailed logging
        cmd = [
            'ffmpeg',
            '-loglevel', 'verbose',  # Increased verbosity for debugging
            '-stats',  # Show encoding stats
            '-fflags', '+igndts',
            '-i', self.rtmp_url,
            '-c:v', 'libx264',  # Video codec
            '-preset', 'ultrafast',  # Fast encoding for low latency
            '-tune', 'zerolatency',  # Zero latency tuning
            '-g', '30',  # GOP size
            '-sc_threshold', '0',  # Disable scene change detection
            '-c:a', 'aac',  # Audio codec
            '-b:a', '128k',  # Audio bitrate
            '-ar', '44100',  # Audio sample rate
            '-f', 'hls',  # HLS format
            '-hls_time', str(HLS_SEGMENT_DURATION),  # Segment duration
            '-hls_list_size', str(HLS_PLAYLIST_SIZE),  # Playlist size
            '-hls_flags', 'delete_segments+append_list',  # HLS flags
            '-start_number', '0',
            str(playlist_path)
        ]
        
        self.ffmpeg_logger.info("=" * 80)
        self.ffmpeg_logger.info(f"Starting FFmpeg for stream: {self.stream_key}")
        self.ffmpeg_logger.info(f"RTMP Input: {self.rtmp_url}")
        self.ffmpeg_logger.info(f"HLS Output: {playlist_path}")
        self.ffmpeg_logger.info(f"Output Directory: {self.output_dir}")
        self.ffmpeg_logger.info("=" * 80)
        self.ffmpeg_logger.info(f"FFmpeg Command: {' '.join(cmd)}")
        self.ffmpeg_logger.info("=" * 80)
        
        try:
            self.process = subprocess.Popen(
                cmd,
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
                universal_newlines=True,
                bufsize=1  # Line buffered
            )
            
            self.ffmpeg_logger.info(f"FFmpeg process started with PID: {self.process.pid}")
            
            # Start threads to read both stdout and stderr
            threading.Thread(
                target=self._read_ffmpeg_output,
                args=(self.process.stderr, "STDERR"),
                daemon=True
            ).start()
            
            threading.Thread(
                target=self._read_ffmpeg_output,
                args=(self.process.stdout, "STDOUT"),
                daemon=True
            ).start()
            
            self.last_output_time = threading.Event()
            self.ffmpeg_logger.info("FFmpeg output monitoring threads started")
            return True
        except FileNotFoundError:
            self.ffmpeg_logger.error("FFmpeg not found! Please install FFmpeg first.")
            self.ffmpeg_logger.error("Install: brew install ffmpeg (macOS) or apt-get install ffmpeg (Linux)")
            return False
        except Exception as e:
            self.ffmpeg_logger.error(f"Failed to start FFmpeg: {e}")
            import traceback
            self.ffmpeg_logger.error(traceback.format_exc())
            return False
    
    def _start_recording(self):
        """Start FFmpeg process to record stream in 5-minute segments"""
        import datetime
        
        # Create session directory for this recording session
        session_timestamp = self.session_start_time.strftime("%Y%m%d_%H%M%S")
        session_dir = self.recordings_dir / session_timestamp
        session_dir.mkdir(parents=True, exist_ok=True)
        
        recording_path = session_dir / "segment_%03d.mp4"
        
        # FFmpeg command for recording in 5-minute segments
        cmd = [
            'ffmpeg',
            '-loglevel', 'info',
            '-fflags', '+genpts+igndts',  # Handle timestamp issues
            '-i', self.rtmp_url,
            '-c:v', 'copy',  # Copy video codec (no re-encoding for recording)
            '-c:a', 'copy',  # Copy audio codec (no re-encoding for recording)
            '-f', 'segment',  # Segment muxer
            '-segment_time', str(RECORDING_SEGMENT_DURATION),  # 5 minutes
            '-reset_timestamps', '1',  # Reset timestamps for each segment
            '-segment_format', 'mp4',  # Output format
            str(recording_path)
        ]
        
        self.ffmpeg_logger.info("=" * 80)
        self.ffmpeg_logger.info(f"Starting Recording FFmpeg for stream: {self.stream_key}")
        self.ffmpeg_logger.info(f"Recording to: {session_dir}")
        self.ffmpeg_logger.info(f"Segment duration: {RECORDING_SEGMENT_DURATION}s (5 minutes)")
        self.ffmpeg_logger.info("=" * 80)
        
        try:
            self.recording_process = subprocess.Popen(
                cmd,
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
                universal_newlines=True,
                bufsize=1
            )
            
            self.ffmpeg_logger.info(f"Recording FFmpeg process started with PID: {self.recording_process.pid}")
            
            # Start threads to read output
            threading.Thread(
                target=self._read_recording_output,
                args=(self.recording_process.stderr, "RECORDING-STDERR"),
                daemon=True
            ).start()
            
            return True
        except Exception as e:
            self.ffmpeg_logger.error(f"Failed to start recording FFmpeg: {e}")
            import traceback
            self.ffmpeg_logger.error(traceback.format_exc())
            return False
    
    def _start_frame_extraction(self):
        """Start FFmpeg process to extract frames for AI analysis"""
        # Clean old frames
        self._clean_frames()
        
        frame_pattern = self.frames_dir / "frame_%08d.jpg"
        
        # FFmpeg command for frame extraction at 1 fps
        cmd = [
            'ffmpeg',
            '-loglevel', 'error',  # Only log errors for frame extraction
            '-fflags', '+genpts+igndts',
            '-i', self.rtmp_url,
            '-vf', f'fps={FRAME_EXTRACTION_FPS}',  # Extract 1 frame per second
            '-q:v', '2',  # JPEG quality (2 is high quality)
            '-f', 'image2',  # Image output format
            '-update', '1',  # Overwrite the same file (keeps only latest frame)
            str(self.frames_dir / "latest.jpg")  # Always write to same file
        ]
        
        self.ffmpeg_logger.info("=" * 80)
        self.ffmpeg_logger.info(f"Starting Frame Extraction for AI analysis: {self.stream_key}")
        self.ffmpeg_logger.info(f"Extraction rate: {FRAME_EXTRACTION_FPS} fps")
        self.ffmpeg_logger.info(f"Output: {self.frames_dir / 'latest.jpg'}")
        self.ffmpeg_logger.info("=" * 80)
        
        try:
            self.frame_process = subprocess.Popen(
                cmd,
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
                universal_newlines=True,
                bufsize=1
            )
            
            self.ffmpeg_logger.info(f"Frame extraction FFmpeg started with PID: {self.frame_process.pid}")
            
            # Start thread to read output
            threading.Thread(
                target=self._read_frame_output,
                args=(self.frame_process.stderr, "FRAME-STDERR"),
                daemon=True
            ).start()
            
            return True
        except Exception as e:
            self.ffmpeg_logger.error(f"Failed to start frame extraction: {e}")
            import traceback
            self.ffmpeg_logger.error(traceback.format_exc())
            return False
    
    def _monitor_frames(self):
        """Monitor for new frames and call user callback for AI processing"""
        import time
        
        latest_frame_path = self.frames_dir / "latest.jpg"
        self.ffmpeg_logger.info("Frame monitor thread started - will call AI callback for each new frame")
        
        while self.should_run:
            try:
                # Wait a bit
                time.sleep(1.0 / FRAME_EXTRACTION_FPS)
                
                # Check if frame file exists and has been modified
                if latest_frame_path.exists():
                    # Get modification time
                    mtime = latest_frame_path.stat().st_mtime
                    
                    # Only process if it's a new frame
                    if self.last_processed_frame is None or mtime > self.last_processed_frame:
                        self.last_processed_frame = mtime
                        
                        # Call user's AI processing callback
                        if self.frame_callback:
                            try:
                                # Read frame data
                                with open(latest_frame_path, 'rb') as f:
                                    frame_data = f.read()
                                
                                # Call callback with stream_key and frame data
                                self.frame_callback(self.stream_key, latest_frame_path, frame_data)
                            except Exception as e:
                                self.ffmpeg_logger.error(f"Error in frame callback: {e}")
                                import traceback
                                self.ffmpeg_logger.debug(traceback.format_exc())
            
            except Exception as e:
                self.ffmpeg_logger.error(f"Error in frame monitor: {e}")
                time.sleep(1)
        
        self.ffmpeg_logger.info("Frame monitor thread exiting")
    
    def _clean_frames(self):
        """Clean old frame files"""
        try:
            for frame_file in self.frames_dir.glob("*.jpg"):
                try:
                    frame_file.unlink()
                except:
                    pass
            self.ffmpeg_logger.debug("Cleaned old frame files")
        except Exception as e:
            self.ffmpeg_logger.warning(f"Error cleaning frames: {e}")
    
    def _clean_live_stream_data(self):
        """Clean live stream data for fresh start"""
        try:
            # Remove old .ts segments
            for seg_file in self.output_dir.glob("*.ts"):
                try:
                    seg_file.unlink()
                    self.ffmpeg_logger.debug(f"Removed old segment: {seg_file.name}")
                except Exception as e:
                    self.ffmpeg_logger.warning(f"Could not remove {seg_file.name}: {e}")
            
            # Remove old playlist
            playlist_file = self.output_dir / "playlist.m3u8"
            if playlist_file.exists():
                try:
                    playlist_file.unlink()
                    self.ffmpeg_logger.debug("Removed old playlist")
                except Exception as e:
                    self.ffmpeg_logger.warning(f"Could not remove playlist: {e}")
            
            self.ffmpeg_logger.info("Cleaned live stream data for fresh start")
        except Exception as e:
            self.ffmpeg_logger.error(f"Error cleaning live stream data: {e}")
    
    def _read_recording_output(self, pipe, pipe_name):
        """Read recording FFmpeg output for logging"""
        try:
            while self.recording_process and self.recording_process.poll() is None:
                line = pipe.readline()
                if line:
                    line_stripped = line.strip()
                    
                    # Log important recording events
                    if any(keyword in line_stripped.lower() for keyword in 
                            ['error', 'failed', 'segment', 'opening', 'closing']):
                        self.ffmpeg_logger.info(f"[{pipe_name}] {line_stripped}")
                    else:
                        self.ffmpeg_logger.debug(f"[{pipe_name}] {line_stripped}")
        except Exception as e:
            self.ffmpeg_logger.error(f"Error reading recording output: {e}")
    
    def _read_frame_output(self, pipe, pipe_name):
        """Read frame extraction FFmpeg output for logging"""
        try:
            while self.frame_process and self.frame_process.poll() is None:
                line = pipe.readline()
                if line:
                    line_stripped = line.strip()
                    # Only log errors for frame extraction
                    if line_stripped:
                        self.ffmpeg_logger.warning(f"[{pipe_name}] {line_stripped}")
        except Exception as e:
            self.ffmpeg_logger.error(f"Error reading frame extraction output: {e}")
    
    def _read_ffmpeg_output(self, pipe, pipe_name):
        """Read FFmpeg output for logging"""
        self.ffmpeg_logger.debug(f"Started monitoring {pipe_name}")
        line_count = 0
        
        try:
            while self.process and self.process.poll() is None:
                line = pipe.readline()
                if line:
                    line_stripped = line.strip()
                    line_count += 1
                    
                    # Log errors and warnings prominently
                    if 'error' in line_stripped.lower():
                        self.ffmpeg_logger.error(f"[{pipe_name}] {line_stripped}")
                    elif 'warning' in line_stripped.lower():
                        self.ffmpeg_logger.warning(f"[{pipe_name}] {line_stripped}")
                    # Log important connection/stream info
                    elif any(keyword in line_stripped.lower() for keyword in 
                            ['failed', 'refused', 'timeout', 'could not', 'unable to',
                             'invalid', 'not found', 'no such', 'cannot']):
                        self.ffmpeg_logger.error(f"[{pipe_name}] {line_stripped}")
                    # Log stream information
                    elif any(keyword in line_stripped.lower() for keyword in 
                            ['input #', 'output #', 'stream #', 'duration:', 'start:', 
                             'bitrate:', 'video:', 'audio:', 'metadata:']):
                        self.ffmpeg_logger.info(f"[{pipe_name}] {line_stripped}")
                    # Log frame processing (shows activity)
                    elif 'frame=' in line_stripped:
                        # This indicates FFmpeg is actively processing
                        if self.last_output_time:
                            self.last_output_time.set()
                        # Log every 100 frames to avoid spam
                        if line_count % 100 == 0:
                            self.ffmpeg_logger.info(f"[{pipe_name}] {line_stripped}")
                        else:
                            self.ffmpeg_logger.debug(f"[{pipe_name}] {line_stripped}")
                    # Log opening/closing connections
                    elif any(keyword in line_stripped.lower() for keyword in 
                            ['opening', 'closing', 'connected', 'disconnected']):
                        self.ffmpeg_logger.info(f"[{pipe_name}] {line_stripped}")
                    else:
                        # Log everything else at DEBUG level
                        self.ffmpeg_logger.debug(f"[{pipe_name}] {line_stripped}")
                        
            self.ffmpeg_logger.warning(f"{pipe_name} monitoring ended - process terminated")
        except Exception as e:
            self.ffmpeg_logger.error(f"Error reading {pipe_name}: {e}")
            import traceback
            self.ffmpeg_logger.error(traceback.format_exc())
    
    def _monitor_process(self):
        """Monitor FFmpeg process health and restart if needed"""
        import time
        restart_count = 0
        max_restarts = 5
        
        self.ffmpeg_logger.info("Process monitor thread started")
        
        while self.should_run:
            time.sleep(10)  # Check every 10 seconds
            
            if not self.should_run:
                break
                
            # Check if process is still running
            if self.process and self.process.poll() is not None:
                exit_code = self.process.returncode
                self.ffmpeg_logger.error(f"FFmpeg process died with exit code: {exit_code}")
                
                # Log common exit codes
                if exit_code == 1:
                    self.ffmpeg_logger.error("Exit code 1: Generic error (check logs above for details)")
                elif exit_code == 255:
                    self.ffmpeg_logger.error("Exit code 255: Connection refused or stream not available")
                elif exit_code == -15:
                    self.ffmpeg_logger.info("Exit code -15: Process terminated normally (SIGTERM)")
                
                if restart_count < max_restarts and exit_code not in [-15, 0]:
                    restart_count += 1
                    self.ffmpeg_logger.warning(f"Attempting restart {restart_count}/{max_restarts}")
                    time.sleep(2)  # Wait before restart
                    if self._start_ffmpeg():
                        self.ffmpeg_logger.info("Successfully restarted FFmpeg")
                        restart_count = 0  # Reset counter on successful restart
                    else:
                        self.ffmpeg_logger.error("Failed to restart FFmpeg")
                else:
                    if exit_code not in [-15, 0]:
                        self.ffmpeg_logger.error(f"Max restart attempts reached or process terminated normally")
                    break
            
            # Check if playlist file exists
            playlist_path = self.output_dir / "playlist.m3u8"
            if self.process and self.process.poll() is None and not playlist_path.exists():
                self.ffmpeg_logger.warning("Process running but no playlist file found yet (still waiting for stream?)")
            elif playlist_path.exists():
                # Check segment files
                segment_files = list(self.output_dir.glob("*.ts"))
                if len(segment_files) > 0:
                    self.ffmpeg_logger.debug(f"Health check: {len(segment_files)} segment files found")
        
        self.ffmpeg_logger.info("Process monitor thread exiting")
    
    def stop(self):
        """Stop the FFmpeg processes and clean up"""
        self.should_run = False
        
        # Stop HLS streaming process
        if self.process:
            self.ffmpeg_logger.info(f"Stopping HLS FFmpeg process (PID: {self.process.pid})")
            self.process.terminate()
            try:
                self.process.wait(timeout=5)
                self.ffmpeg_logger.info("HLS FFmpeg process terminated gracefully")
            except subprocess.TimeoutExpired:
                self.ffmpeg_logger.warning("HLS FFmpeg didn't terminate gracefully, forcing kill")
                self.process.kill()
                self.ffmpeg_logger.info("HLS FFmpeg process killed")
            self.process = None
        
        # Stop recording process
        if self.recording_process:
            self.ffmpeg_logger.info(f"Stopping Recording FFmpeg process (PID: {self.recording_process.pid})")
            self.recording_process.terminate()
            try:
                self.recording_process.wait(timeout=5)
                self.ffmpeg_logger.info("Recording FFmpeg process terminated gracefully")
            except subprocess.TimeoutExpired:
                self.ffmpeg_logger.warning("Recording FFmpeg didn't terminate gracefully, forcing kill")
                self.recording_process.kill()
                self.ffmpeg_logger.info("Recording FFmpeg process killed")
            self.recording_process = None
        
        # Stop frame extraction process
        if self.frame_process:
            self.ffmpeg_logger.info(f"Stopping Frame Extraction FFmpeg process (PID: {self.frame_process.pid})")
            self.frame_process.terminate()
            try:
                self.frame_process.wait(timeout=5)
                self.ffmpeg_logger.info("Frame Extraction FFmpeg process terminated gracefully")
            except subprocess.TimeoutExpired:
                self.ffmpeg_logger.warning("Frame Extraction FFmpeg didn't terminate gracefully, forcing kill")
                self.frame_process.kill()
                self.ffmpeg_logger.info("Frame Extraction FFmpeg process killed")
            self.frame_process = None
        
        # Log recording location
        if self.session_start_time:
            session_timestamp = self.session_start_time.strftime("%Y%m%d_%H%M%S")
            session_dir = self.recordings_dir / session_timestamp
            if session_dir.exists():
                recording_files = list(session_dir.glob("*.mp4"))
                if recording_files:
                    total_size = sum(f.stat().st_size for f in recording_files)
                    self.ffmpeg_logger.info("=" * 80)
                    self.ffmpeg_logger.info(f"Recordings saved to: {session_dir}")
                    self.ffmpeg_logger.info(f"Number of segments: {len(recording_files)}")
                    self.ffmpeg_logger.info(f"Total size: {total_size / (1024 * 1024):.2f} MB")
                    self.ffmpeg_logger.info(f"Segments: {', '.join(sorted([f.name for f in recording_files]))}")
                    self.ffmpeg_logger.info("=" * 80)
        
        # Clean live stream data for next session
        self._clean_live_stream_data()
        
        self.ffmpeg_logger.info("=" * 80)
        self.ffmpeg_logger.info(f"Stream {self.stream_key} stopped - ready for new session")
        self.ffmpeg_logger.info("=" * 80)


class RTMPServer:
    """Simple RTMP server manager using FFmpeg"""
    
    def __init__(self, port=RTMP_PORT):
        self.port = port
        self.process = None
    
    def start(self):
        """Start the RTMP server (using nginx-rtmp or SRS)"""
        # Note: This requires nginx with rtmp module or SRS (Simple Realtime Server)
        # For simplicity, we'll document this in the README
        logger.info(f"RTMP server should be listening on port {self.port}")
        logger.info("Please ensure nginx-rtmp or SRS is configured and running")


def register_stream(stream_key, rtmp_input_url=None):
    """Register a new stream and start conversion"""
    if stream_key in active_streams:
        logger.warning(f"Stream {stream_key} already exists")
        return False
    
    if rtmp_input_url is None:
        rtmp_input_url = f"rtmp://localhost:{RTMP_PORT}/live/{stream_key}"
    
    # Create converter with frame callback if set
    converter = RTMPtoHLSConverter(stream_key, HLS_OUTPUT_DIR, frame_callback=frame_callback_function)
    if converter.start(rtmp_input_url):
        active_streams[stream_key] = {
            'rtmp_url': rtmp_input_url,
            'hls_url': f"/stream/{stream_key}/playlist.m3u8"
        }
        ffmpeg_processes[stream_key] = converter
        logger.info(f"Stream {stream_key} registered and started")
        return True
    return False


def set_frame_callback(callback_function):
    """
    Set the global frame callback function for AI processing
    
    The callback function will be called for each frame (1 fps) with:
        - stream_key: str - The stream identifier
        - frame_path: Path - Path to the frame image file
        - frame_data: bytes - The JPEG image data
    
    Example:
        def my_ai_processor(stream_key, frame_path, frame_data):
            print(f"Processing frame from {stream_key}")
            # Your AI processing here
            
        set_frame_callback(my_ai_processor)
    """
    global frame_callback_function
    frame_callback_function = callback_function
    logger.info("Frame callback function registered for AI processing")


def unregister_stream(stream_key):
    """Stop and unregister a stream"""
    if stream_key in ffmpeg_processes:
        ffmpeg_processes[stream_key].stop()
        del ffmpeg_processes[stream_key]
        del active_streams[stream_key]
        logger.info(f"Stream {stream_key} unregistered")
        return True
    return False


# Flask routes

@app.route('/')
def index():
    """Home page with stream information"""
    html = """
    <!DOCTYPE html>
    <html>
    <head>
        <title>RTMP to LL-HLS Streaming Server</title>
        <style>
            body {
                font-family: Arial, sans-serif;
                max-width: 1200px;
                margin: 0 auto;
                padding: 20px;
                background-color: #f5f5f5;
            }
            h1 {
                color: #333;
            }
            .container {
                background: white;
                padding: 20px;
                border-radius: 8px;
                box-shadow: 0 2px 4px rgba(0,0,0,0.1);
                margin-bottom: 20px;
            }
            .stream-list {
                list-style: none;
                padding: 0;
            }
            .stream-item {
                padding: 10px;
                margin: 10px 0;
                background: #f9f9f9;
                border-left: 4px solid #4CAF50;
                border-radius: 4px;
            }
            .stream-item a {
                color: #2196F3;
                text-decoration: none;
                font-weight: bold;
            }
            .stream-item a:hover {
                text-decoration: underline;
            }
            .code {
                background: #f4f4f4;
                padding: 10px;
                border-radius: 4px;
                font-family: monospace;
                overflow-x: auto;
            }
            .btn {
                background: #4CAF50;
                color: white;
                padding: 10px 20px;
                border: none;
                border-radius: 4px;
                cursor: pointer;
                margin: 5px;
            }
            .btn:hover {
                background: #45a049;
            }
            .btn-danger {
                background: #f44336;
            }
            .btn-danger:hover {
                background: #da190b;
            }
        </style>
    </head>
    <body>
        <h1>🎥 RTMP to LL-HLS Streaming Server</h1>
        
        <div class="container">
            <h2>Active Streams</h2>
            <ul class="stream-list" id="streamList">
                {% if streams %}
                    {% for key, info in streams.items() %}
                    <li class="stream-item">
                        <strong>Stream Key:</strong> {{ key }}<br>
                        <strong>HLS URL:</strong> <a href="{{ info.hls_url }}" target="_blank">{{ info.hls_url }}</a><br>
                        <a href="/player/{{ key }}" target="_blank">
                            <button class="btn">▶ Watch Stream</button>
                        </a>
                        <button class="btn btn-danger" onclick="stopStream('{{ key }}')">⏹ Stop Stream</button>
                    </li>
                    {% endfor %}
                {% else %}
                    <li>No active streams</li>
                {% endif %}
            </ul>
        </div>
        
        <div class="container">
            <h2>Start New Stream</h2>
            <form id="startStreamForm">
                <label>Stream Key: <input type="text" id="streamKey" required></label><br><br>
                <label>RTMP URL (optional): <input type="text" id="rtmpUrl" placeholder="rtmp://localhost:1935/live/stream_key"></label><br><br>
                <button type="submit" class="btn">Start Stream</button>
            </form>
        </div>
        
        <div class="container">
            <h2>📡 How to Stream</h2>
            <p>Use OBS, FFmpeg, or any RTMP-capable software to stream to:</p>
            <div class="code">
                rtmp://localhost:1935/live/YOUR_STREAM_KEY
            </div>
            <p><strong>Example with FFmpeg:</strong></p>
            <div class="code">
                ffmpeg -re -i input.mp4 -c:v libx264 -preset veryfast -maxrate 3000k -bufsize 6000k -pix_fmt yuv420p -g 50 -c:a aac -b:a 160k -ar 44100 -f flv rtmp://localhost:1935/live/YOUR_STREAM_KEY
            </div>
        </div>
        
        <script>
            document.getElementById('startStreamForm').addEventListener('submit', async (e) => {
                e.preventDefault();
                const streamKey = document.getElementById('streamKey').value;
                const rtmpUrl = document.getElementById('rtmpUrl').value;
                
                const response = await fetch('/api/stream/start', {
                    method: 'POST',
                    headers: { 'Content-Type': 'application/json' },
                    body: JSON.stringify({ stream_key: streamKey, rtmp_url: rtmpUrl || null })
                });
                
                if (response.ok) {
                    alert('Stream started!');
                    location.reload();
                } else {
                    alert('Failed to start stream');
                }
            });
            
            async function stopStream(streamKey) {
                if (!confirm(`Stop stream "${streamKey}"?`)) return;
                
                const response = await fetch('/api/stream/stop', {
                    method: 'POST',
                    headers: { 'Content-Type': 'application/json' },
                    body: JSON.stringify({ stream_key: streamKey })
                });
                
                if (response.ok) {
                    alert('Stream stopped!');
                    location.reload();
                } else {
                    alert('Failed to stop stream');
                }
            }
        </script>
    </body>
    </html>
    """
    return render_template_string(html, streams=active_streams)


@app.route('/player/<stream_key>')
def player(stream_key):
    """Video player page for a specific stream"""
    if stream_key not in active_streams:
        return "Stream not found", 404
    
    html = """
    <!DOCTYPE html>
    <html>
    <head>
        <title>Stream Player - {{ stream_key }}</title>
        <script src="https://cdn.jsdelivr.net/npm/hls.js@latest"></script>
        <style>
            body {
                margin: 0;
                padding: 20px;
                background: #000;
                font-family: Arial, sans-serif;
            }
            .container {
                max-width: 1280px;
                margin: 0 auto;
            }
            h1 {
                color: white;
                text-align: center;
            }
            #video {
                width: 100%;
                max-width: 1280px;
                display: block;
                margin: 20px auto;
                background: #000;
            }
            .info {
                color: white;
                text-align: center;
                margin-top: 20px;
            }
            .stats {
                background: rgba(255,255,255,0.1);
                padding: 15px;
                border-radius: 8px;
                margin-top: 20px;
                color: white;
            }
        </style>
    </head>
    <body>
        <div class="container">
            <h1>🎥 Stream: {{ stream_key }}</h1>
            <video id="video" controls autoplay muted></video>
            <div class="stats">
                <div>Status: <span id="status">Loading...</span></div>
                <div>Latency: <span id="latency">-</span></div>
            </div>
            <div class="info">
                <p>HLS URL: <code>{{ hls_url }}</code></p>
                <a href="/" style="color: #4CAF50;">← Back to Stream List</a>
            </div>
        </div>
        
        <script>
            const video = document.getElementById('video');
            const statusEl = document.getElementById('status');
            const latencyEl = document.getElementById('latency');
            const hlsUrl = '{{ hls_url }}';
            
            if (Hls.isSupported()) {
                const hls = new Hls({
                    enableWorker: true,
                    lowLatencyMode: true,
                    backBufferLength: 90,
                    maxBufferLength: 30,
                    maxMaxBufferLength: 60,
                    maxBufferSize: 60 * 1000 * 1000,
                    maxBufferHole: 0.5,
                    highBufferWatchdogPeriod: 2,
                    nudgeOffset: 0.1,
                    nudgeMaxRetry: 3,
                    maxFragLookUpTolerance: 0.25,
                    liveSyncDurationCount: 3,
                    liveMaxLatencyDurationCount: 10,
                    liveDurationInfinity: true,
                    liveBackBufferLength: 0
                });
                
                hls.loadSource(hlsUrl);
                hls.attachMedia(video);
                
                hls.on(Hls.Events.MANIFEST_PARSED, function() {
                    statusEl.textContent = 'Ready';
                    video.play().catch(e => {
                        console.log('Autoplay prevented:', e);
                        statusEl.textContent = 'Click play to start';
                    });
                });
                
                hls.on(Hls.Events.ERROR, function(event, data) {
                    console.error('HLS Error:', data);
                    if (data.fatal) {
                        statusEl.textContent = 'Error: ' + data.type;
                        switch(data.type) {
                            case Hls.ErrorTypes.NETWORK_ERROR:
                                console.log('Network error, attempting recovery...');
                                hls.startLoad();
                                break;
                            case Hls.ErrorTypes.MEDIA_ERROR:
                                console.log('Media error, attempting recovery...');
                                hls.recoverMediaError();
                                break;
                            default:
                                statusEl.textContent = 'Fatal error';
                                hls.destroy();
                                break;
                        }
                    }
                });
                
                // Update latency
                setInterval(() => {
                    if (video.buffered.length > 0) {
                        const latency = video.buffered.end(0) - video.currentTime;
                        latencyEl.textContent = latency.toFixed(2) + 's';
                    }
                }, 1000);
                
            } else if (video.canPlayType('application/vnd.apple.mpegurl')) {
                video.src = hlsUrl;
                statusEl.textContent = 'Native HLS';
                video.addEventListener('loadedmetadata', function() {
                    video.play();
                });
            } else {
                statusEl.textContent = 'HLS not supported in this browser';
            }
        </script>
    </body>
    </html>
    """
    return render_template_string(
        html,
        stream_key=stream_key,
        hls_url=active_streams[stream_key]['hls_url']
    )


@app.route('/stream/<stream_key>/<path:filename>')
def serve_hls(stream_key, filename):
    """Serve HLS playlist and segments"""
    stream_dir = HLS_OUTPUT_DIR / stream_key
    
    # Set appropriate headers for HLS
    if filename.endswith('.m3u8'):
        return send_from_directory(
            stream_dir,
            filename,
            mimetype='application/vnd.apple.mpegurl',
            headers={
                'Cache-Control': 'no-cache, no-store, must-revalidate',
                'Pragma': 'no-cache',
                'Expires': '0'
            }
        )
    elif filename.endswith('.ts') or filename.endswith('.mp4'):
        return send_from_directory(
            stream_dir,
            filename,
            mimetype='video/mp2t' if filename.endswith('.ts') else 'video/mp4'
        )
    else:
        return "Not found", 404


@app.route('/api/streams')
def list_streams():
    """API endpoint to list active streams"""
    return json.dumps(active_streams), 200, {'Content-Type': 'application/json'}


@app.route('/api/stream/start', methods=['POST'])
def start_stream():
    """API endpoint to start a new stream"""
    data = request.json
    stream_key = data.get('stream_key')
    rtmp_url = data.get('rtmp_url')
    
    if not stream_key:
        return json.dumps({'error': 'stream_key required'}), 400
    
    if register_stream(stream_key, rtmp_url):
        return json.dumps({'success': True, 'stream': active_streams[stream_key]}), 200
    else:
        return json.dumps({'error': 'Failed to start stream'}), 500


@app.route('/api/stream/stop', methods=['POST'])
def stop_stream():
    """API endpoint to stop a stream"""
    data = request.json
    stream_key = data.get('stream_key')
    
    if not stream_key:
        return json.dumps({'error': 'stream_key required'}), 400
    
    if unregister_stream(stream_key):
        return json.dumps({'success': True}), 200
    else:
        return json.dumps({'error': 'Stream not found'}), 404


@app.route('/api/stream/health/<stream_key>')
def stream_health(stream_key):
    """API endpoint to check stream health"""
    if stream_key not in active_streams:
        return json.dumps({'error': 'Stream not found'}), 404
    
    converter = ffmpeg_processes.get(stream_key)
    if not converter:
        return json.dumps({'error': 'Converter not found'}), 404
    
    # Check if FFmpeg process is running
    process_running = converter.process and converter.process.poll() is None
    
    # Check if output files exist
    playlist_exists = (converter.output_dir / "playlist.m3u8").exists()
    segment_files = list(converter.output_dir.glob("segment_*.ts"))
    
    health_info = {
        'stream_key': stream_key,
        'process_running': process_running,
        'process_pid': converter.process.pid if converter.process else None,
        'playlist_exists': playlist_exists,
        'segment_count': len(segment_files),
        'output_dir': str(converter.output_dir),
        'rtmp_url': converter.rtmp_url,
        'healthy': process_running and playlist_exists and len(segment_files) > 0
    }
    
    return json.dumps(health_info), 200


@app.route('/api/recordings/<stream_key>')
def list_recordings(stream_key):
    """API endpoint to list recordings for a stream"""
    recordings_path = RECORDINGS_DIR / stream_key
    
    if not recordings_path.exists():
        return json.dumps({'stream_key': stream_key, 'sessions': []}), 200
    
    # List all recording sessions
    sessions = []
    for session_dir in sorted(recordings_path.iterdir(), reverse=True):
        if session_dir.is_dir():
            recording_files = list(session_dir.glob("*.mp4"))
            total_size = sum(f.stat().st_size for f in recording_files)
            
            # Calculate total duration (approximately)
            total_duration_min = len(recording_files) * (RECORDING_SEGMENT_DURATION / 60)
            
            sessions.append({
                'session_id': session_dir.name,
                'segment_count': len(recording_files),
                'total_size_mb': round(total_size / (1024 * 1024), 2),
                'estimated_duration_min': round(total_duration_min, 1),
                'segments': [f.name for f in sorted(recording_files)]
            })
    
    return json.dumps({
        'stream_key': stream_key,
        'total_sessions': len(sessions),
        'sessions': sessions
    }), 200


@app.route('/recordings/<stream_key>/<session_id>/<filename>')
def serve_recording(stream_key, session_id, filename):
    """Serve recording file"""
    recording_path = RECORDINGS_DIR / stream_key / session_id
    
    if not recording_path.exists():
        return "Recording not found", 404
    
    return send_from_directory(
        recording_path,
        filename,
        mimetype='video/mp4'
    )


def cleanup():
    """Cleanup function to stop all streams on exit"""
    logger.info("Cleaning up...")
    for stream_key in list(ffmpeg_processes.keys()):
        unregister_stream(stream_key)


def signal_handler(sig, frame):
    """Handle SIGINT (Ctrl+C)"""
    logger.info("Received interrupt signal")
    cleanup()
    sys.exit(0)


def main():
    """Main entry point"""
    # Create output directories
    HLS_OUTPUT_DIR.mkdir(exist_ok=True)
    RECORDINGS_DIR.mkdir(exist_ok=True)
    FRAMES_DIR.mkdir(exist_ok=True)
    
    # Register signal handler
    signal.signal(signal.SIGINT, signal_handler)
    
    logger.info("=" * 60)
    logger.info("RTMP to LL-HLS Streaming Server with Recording & AI")
    logger.info("=" * 60)
    logger.info(f"HTTP Server: http://localhost:{HTTP_PORT}")
    logger.info(f"RTMP Input: rtmp://localhost:{RTMP_PORT}/live/STREAM_KEY")
    logger.info(f"Live HLS Output: {HLS_OUTPUT_DIR}")
    logger.info(f"Recordings: {RECORDINGS_DIR} (5-minute segments)")
    logger.info(f"AI Frames: {FRAMES_DIR} ({FRAME_EXTRACTION_FPS} fps)")
    logger.info("=" * 60)
    
    # Note about RTMP server
    logger.info("⚠️  Note: You need to have an RTMP server running (nginx-rtmp or SRS)")
    logger.info("    See README.md for setup instructions")
    logger.info("=" * 60)
    
    # Start Flask server
    try:
        app.run(
            host='0.0.0.0',
            port=HTTP_PORT,
            debug=False,
            threaded=True
        )
    except KeyboardInterrupt:
        logger.info("Server stopped by user")
    finally:
        cleanup()


if __name__ == '__main__':
    main()

