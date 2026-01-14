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

## Production (AWS Linux)

This app requires **FFmpeg** at runtime, and (by default) **SQLite with CGO enabled** at build time.

### Option A (recommended): native binary + systemd (Amazon Linux 2023 / Ubuntu)

1. Install dependencies on the build machine (Linux):

- Amazon Linux 2023:

```bash
sudo dnf install -y git gcc sqlite sqlite-devel ffmpeg
```

- Ubuntu/Debian:

```bash
sudo apt-get update
sudo apt-get install -y git gcc libc6-dev libsqlite3-dev ffmpeg
```

2. Build the server (CGO enabled):

```bash
CGO_ENABLED=1 GOOS=linux GOARCH=amd64 go build -o server ./cmd/server
```

3. Create an app user and persistent directories:

```bash
sudo useradd --system --create-home --shell /usr/sbin/nologin porteight || true
sudo mkdir -p /opt/porteight-camera-ai /var/lib/porteight-camera-ai
sudo chown -R porteight:porteight /opt/porteight-camera-ai /var/lib/porteight-camera-ai
```

4. Copy files:

```bash
sudo cp ./server /opt/porteight-camera-ai/server
sudo cp -R ./web /opt/porteight-camera-ai/web
sudo chown -R porteight:porteight /opt/porteight-camera-ai
```

5. Create a systemd service:

```bash
sudo cp ./deploy/systemd/porteight-camera-ai.service /etc/systemd/system/porteight-camera-ai.service
sudo systemctl daemon-reload
sudo systemctl enable --now porteight-camera-ai
sudo systemctl status porteight-camera-ai --no-pager
```

6. Open ports / security groups:

- **RTMP ingest**: TCP **1935**
- **Web UI/API**: TCP **8080** (or use nginx to expose 80/443)

### Option B: keep CGO disabled (not implemented by default)

If you must build with `CGO_ENABLED=0`, switch the DB driver to a pure-Go sqlite driver (e.g. `github.com/glebarez/sqlite`).

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
