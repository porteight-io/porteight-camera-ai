# PortEight Camera AI Streaming Server

This is a Go-based RTMP server that accepts multiple camera streams, records them, and restreams them via HLS to a web interface.

## Features
- **RTMP Ingest**: Accepts streams from cameras (OBS, FFmpeg, etc.).
- **HLS Output**: Low-latency HLS delivery to browsers.
- **Recording**: Automatically saves streams as MP4.
- **Stream Management**: Generate stream keys via Web UI.
- **High Performance**: Uses `joy4` for efficient RTMP handling and `ffmpeg` for optimized transmuxing.

## Prerequisites
- Go 1.18+
- **FFmpeg** (Must be installed and in your system PATH)
  - macOS: `brew install ffmpeg`
  - Linux: `sudo apt install ffmpeg`

## Setup
1. Initialize the project:
   ```bash
   go mod tidy
   ```

2. Run the server:
   ```bash
   go run cmd/server/main.go
   ```

3. Access the Web UI:
   - Open [http://localhost:8080](http://localhost:8080)

## Persistent storage (RunPod Volume Disk)

RunPod mounts persistent storage at **`/workspace`**. This app writes:

- **SQLite DB** (default: `camera_ai.db`)
- **Recordings** (default: `./recordings`)
- **Live HLS output** (default: `./web/static/hls`)

To store these on the Volume Disk, set:

```bash
export PERSIST_DIR=/workspace
```

This will default to:

- `DB_PATH=/workspace/camera_ai.db`
- `RECORDINGS_DIR=/workspace/recordings`
- `HLS_DIR=/workspace/hls`

You can also override individually:

```bash
export DB_PATH=/workspace/camera_ai.db
export RECORDINGS_DIR=/workspace/recordings
export HLS_DIR=/workspace/hls
```

Note: the UI still plays HLS at `/static/hls/...` — the server now maps that URL to whatever `HLS_DIR` is set to.

## How to Stream
1. Create a **Stream Key** in the Web UI.
2. Configure your camera or OBS:
   - **Server**: `rtmp://localhost:1935/live`
   - **Stream Key**: `<YOUR_GENERATED_KEY>`
3. Start streaming.
4. Watch live on the dashboard.

## Architecture
- **RTMP Server**: `joy4` (Go library)
- **Transmuxing**: `ffmpeg` (via stdin pipe)
- **Web Server**: `Gin`
- **Database**: `SQLite`
