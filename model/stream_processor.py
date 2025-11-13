#!/usr/bin/env python3
"""
High-level stream processing service for ingesting authenticated RTMP feeds,
remuxing them to Low-Latency HLS (LL-HLS), running the locally-exported
Roboflow `rf-der-seg` model via Ultralytics YOLO, and emitting contextual alerts.

Key features
------------
* Lightweight authentication via signed stream keys and optional IP allowLists
* Dedicated FFmpeg worker per camera stream (RTMP -> LL-HLS + analysis frames)
* Asynchronous local inference on sampled frames (configurable FPS)
* Alert routing with state deduplication and webhook delivery
* FastAPI control-plane that serves LL-HLS manifests/segments and alert history

This module is intended to be executed as:

    python model/stream_processor.py --config config/streams.yaml

See `Config` dataclass and example YAML for configuration details.
"""
from __future__ import annotations

import contextlib

import argparse
import asyncio
import json
import logging
import os
import random
import signal
import string
import sys
from collections import deque
from contextlib import asynccontextmanager
from dataclasses import dataclass, field
from datetime import datetime, timezone
from enum import Enum
from pathlib import Path
from typing import Any, AsyncGenerator, Deque, Dict, Iterable, List, Optional, Set, Tuple

import cv2
import httpx
import numpy as np
import uvicorn
from fastapi import FastAPI, HTTPException, Request, Response, status
from fastapi.responses import FileResponse, StreamingResponse
from fastapi.staticfiles import StaticFiles
from pydantic import BaseModel, Field
from ultralytics import YOLO


LOG = logging.getLogger("stream-processor")


# ---------------------------------------------------------------------------
# Configuration models
# ---------------------------------------------------------------------------


def _default_hls_dir() -> Path:
    return Path(os.getenv("HLS_OUTPUT_ROOT", "./var/hls")).resolve()


def _default_analysis_dir() -> Path:
    return Path(os.getenv("ANALYSIS_CACHE_ROOT", "./var/cache")).resolve()


def _random_key(length: int = 32) -> str:
    return "".join(random.choice(string.ascii_letters + string.digits) for _ in range(length))


def _default_weights_path() -> Path:
    return Path(os.getenv("MODEL_WEIGHTS_PATH", "./model/weights.pt")).resolve()


@dataclass
class StreamAuth:
    """Configuration for authenticating camera devices."""

    device_id: str
    device_secret: str
    stream_key: str
    allowed_ips: Set[str] = field(default_factory=set)

    def matches(self, presented_key: str, remote_ip: str | None = None) -> bool:
        """Verify stream key (and optionally remote IP) for ingestion."""
        if presented_key != self.stream_key:
            return False
        if self.allowed_ips and remote_ip:
            return remote_ip in self.allowed_ips
        return True

    def rotate_stream_key(self, new_key: Optional[str] = None) -> str:
        self.stream_key = new_key or _random_key()
        return self.stream_key


@dataclass
class StreamConfig:
    """Complete per-stream configuration."""

    camera_id: str
    description: str
    ffmpeg_listen_port: int
    stream_app: str
    auth: StreamAuth
    hls_output_dir: Path
    public_ingest_host: Optional[str] = None
    public_ingest_port: Optional[int] = None
    segment_duration: float = 1.0
    hls_list_size: int = 10
    llhls_partial_count: int = 3
    ffmpeg_preset: str = "veryfast"
    ffmpeg_crf: int = 22
    analysis_fps: float = 1.0
    analysis_scale_width: int = 640
    roboflow_min_confidence: float = 0.4
    alert_cooldown_seconds: int = 8
    webhook_url: Optional[str] = None

    @property
    def bind_url(self) -> str:
        return f"rtmp://0.0.0.0:{self.ffmpeg_listen_port}/{self.stream_app}/{self.auth.stream_key}"

    def ingest_url(self, host: Optional[str] = None, port: Optional[int] = None) -> str:
        target_host = host or self.public_ingest_host or "localhost"
        target_port = port or self.public_ingest_port or self.ffmpeg_listen_port
        return f"rtmp://{target_host}:{target_port}/{self.stream_app}/{self.auth.stream_key}"


@dataclass
class ModelConfig:
    weights_path: Path = field(default_factory=_default_weights_path)
    device: Optional[str] = None
    confidence: float = 0.4


@dataclass
class ServiceConfig:
    """Top-level service configuration."""

    model: ModelConfig
    streams: List[StreamConfig]
    hls_root: Path = field(default_factory=_default_hls_dir)
    analysis_cache_root: Path = field(default_factory=_default_analysis_dir)
    rtmp_public_host: Optional[str] = None
    rtmp_public_port: Optional[int] = None
    api_host: str = "0.0.0.0"
    api_port: int = 8000
    api_debug: bool = False


# ---------------------------------------------------------------------------
# Utilities
# ---------------------------------------------------------------------------


def ensure_directory(path: Path) -> None:
    path.mkdir(parents=True, exist_ok=True)


def utcnow() -> datetime:
    return datetime.now(timezone.utc)


def timestamp() -> str:
    return utcnow().isoformat()


class GracefulExit(SystemExit):
    pass


async def read_json_config(path: Path) -> Dict[str, Any]:
    text = path.read_text()
    if path.suffix.lower() in {".yaml", ".yml"}:
        import yaml

        return yaml.safe_load(text)
    return json.loads(text)


def build_service_config(raw: Dict[str, Any]) -> ServiceConfig:
    """Parse dictionary into dataclasses with sensible defaults & validation."""
    model_cfg_raw = raw.get("model") or {}
    weights_candidate = (
        model_cfg_raw.get("weights_path")
        or os.getenv("MODEL_WEIGHTS_PATH")
        or str(_default_weights_path())
    )
    weights_path = Path(weights_candidate).expanduser().resolve()
    if not weights_path.exists():
        raise ValueError(
            f"Model weights not found at {weights_path}. "
            "Set config.model.weights_path or MODEL_WEIGHTS_PATH env variable."
        )
    device = model_cfg_raw.get("device") or os.getenv("MODEL_DEVICE")
    if device:
        device = str(device)
    confidence = float(model_cfg_raw.get("confidence", 0.4))
    model_cfg = ModelConfig(weights_path=weights_path, device=device, confidence=confidence)

    rtmp_cfg = raw.get("rtmp") or {}
    rtmp_public_host = rtmp_cfg.get("public_host")
    rtmp_public_port = rtmp_cfg.get("public_port")

    stream_cfgs: List[StreamConfig] = []
    for entry in raw.get("streams", []):
        device = entry.get("device") or {}
        stream_key = device.get("stream_key") or _random_key()
        allowed_ips = set(device.get("allowed_ips") or [])
        device_secret = device.get("device_secret")
        if not device_secret:
            env_var = device.get("device_secret_env")
            if env_var:
                device_secret = os.getenv(env_var)
                if not device_secret:
                    raise ValueError(
                        f"Device secret environment variable {env_var} not set for camera {entry['camera_id']}"
                    )
        if not device_secret:
            raise ValueError(
                f"Device secret missing for camera {entry['camera_id']} (set device.device_secret or device.device_secret_env)"
            )
        public_host = entry.get("public_ingest_host") or rtmp_public_host
        public_port = entry.get("public_ingest_port") or rtmp_public_port
        auth = StreamAuth(
            device_id=device.get("device_id") or entry["camera_id"],
            device_secret=device_secret,
            stream_key=stream_key,
            allowed_ips=allowed_ips,
        )

        stream = StreamConfig(
            camera_id=entry["camera_id"],
            description=entry.get("description", entry["camera_id"]),
            ffmpeg_listen_port=int(entry.get("ffmpeg_listen_port", 1935)),
            stream_app=entry.get("stream_app", "live"),
            auth=auth,
            hls_output_dir=_default_hls_dir() / entry["camera_id"],
            public_ingest_host=public_host,
            public_ingest_port=int(public_port) if public_port else None,
            segment_duration=float(entry.get("segment_duration", 1.0)),
            hls_list_size=int(entry.get("hls_list_size", 6)),
            llhls_partial_count=int(entry.get("llhls_partial_count", 3)),
            ffmpeg_preset=entry.get("ffmpeg_preset", "veryfast"),
            ffmpeg_crf=int(entry.get("ffmpeg_crf", 22)),
            analysis_fps=float(entry.get("analysis_fps", 1.0)),
            analysis_scale_width=int(entry.get("analysis_scale_width", 640)),
            roboflow_min_confidence=float(entry.get("roboflow_min_confidence", 0.4)),
            alert_cooldown_seconds=int(entry.get("alert_cooldown_seconds", 8)),
            webhook_url=entry.get("webhook_url"),
        )
        stream_cfgs.append(stream)

    return ServiceConfig(
        model=model_cfg,
        streams=stream_cfgs,
        hls_root=_default_hls_dir(),
        analysis_cache_root=_default_analysis_dir(),
        rtmp_public_host=rtmp_public_host,
        rtmp_public_port=int(rtmp_public_port) if rtmp_public_port else None,
        api_host=raw.get("api", {}).get("host", "0.0.0.0"),
        api_port=int(raw.get("api", {}).get("port", 8000)),
        api_debug=bool(raw.get("api", {}).get("debug", False)),
    )


# ---------------------------------------------------------------------------
# Local model inference
# ---------------------------------------------------------------------------


class ModelInferenceError(RuntimeError):
    pass


class LocalYoloModel:
    def __init__(self, config: ModelConfig) -> None:
        self._cfg = config
        self._model = YOLO(str(config.weights_path))
        self._device = config.device or "auto"
        self._confidence = config.confidence
        self._lock = asyncio.Lock()

    async def infer(self, image_bytes: bytes, confidence: Optional[float] = None) -> Dict[str, Any]:
        async with self._lock:
            loop = asyncio.get_running_loop()
            return await loop.run_in_executor(None, self._infer_sync, image_bytes, confidence)

    def _infer_sync(self, image_bytes: bytes, confidence: Optional[float]) -> Dict[str, Any]:
        array = np.frombuffer(image_bytes, dtype=np.uint8)
        frame = cv2.imdecode(array, cv2.IMREAD_COLOR)
        if frame is None:
            raise ModelInferenceError("Failed to decode frame for inference")

        conf = confidence if confidence is not None else self._confidence
        conf = max(0.01, min(0.99, float(conf)))

        try:
            results = self._model.predict(
                frame,
                device=self._device,
                conf=conf,
                verbose=False,
                stream=False,
            )
        except Exception as exc:
            raise ModelInferenceError(f"Ultralytics prediction failed: {exc}") from exc

        predictions: List[Dict[str, Any]] = []
        if results:
            res = results[0]
            names = res.names or {}
            boxes = getattr(res, "boxes", None)
            if boxes is not None and len(boxes) > 0:
                for cls_tensor, conf_tensor in zip(boxes.cls, boxes.conf):
                    cls_id = int(cls_tensor.item())
                    conf_val = float(conf_tensor.item())
                    label = names.get(cls_id, str(cls_id))
                    predictions.append({"class": label, "confidence": conf_val})

        return {"predictions": predictions}

    async def close(self) -> None:
        # Placeholder for symmetry; Ultralytics models do not require explicit close.
        return None


# ---------------------------------------------------------------------------
# Alerting
# ---------------------------------------------------------------------------


class AlertLevel(str, Enum):
    TAMPERING = "tampering"
    THEFT = "theft"
    CLEAR = "clear"


class AlertPayload(BaseModel):
    camera_id: str
    level: AlertLevel
    detail: Dict[str, Any] = Field(default_factory=dict)
    observed_at: datetime = Field(default_factory=utcnow)


class AuthTokenRequest(BaseModel):
    device_id: str
    device_secret: str


class AuthTokenResponse(BaseModel):
    camera_id: str
    stream_key: str
    rtmp_url: str
    llhls_url: str
    issued_at: datetime = Field(default_factory=utcnow)
    allowed_ips: List[str] = Field(default_factory=list)
    note: Optional[str] = None


class AlertManager:
    def __init__(self) -> None:
        self._recent_state: Dict[str, Tuple[AlertLevel, datetime]] = {}
        self._history: Deque[AlertPayload] = deque(maxlen=500)
        self._subscribers: Set[asyncio.Queue[AlertPayload]] = set()
        self._lock = asyncio.Lock()

    async def publish(self, alert: AlertPayload, cooldown: int, webhook: Optional[str] = None) -> None:
        async with self._lock:
            last_level, last_at = self._recent_state.get(alert.camera_id, (None, datetime.min.replace(tzinfo=timezone.utc)))
            now = utcnow()
            if last_level == alert.level and (now - last_at).total_seconds() < cooldown:
                LOG.debug("Skipping duplicate alert for %s (%s within cooldown)", alert.camera_id, alert.level)
                return

            self._recent_state[alert.camera_id] = (alert.level, now)
            self._history.append(alert)

        LOG.warning("ALERT %s -> %s (%s)", alert.camera_id, alert.level, alert.detail)
        await self._notify(alert)
        if webhook:
            await self._post_webhook(alert, webhook)

    async def _notify(self, alert: AlertPayload) -> None:
        for queue in list(self._subscribers):
            try:
                queue.put_nowait(alert)
            except asyncio.QueueFull:
                LOG.debug("Dropping alert for subscriber due to full queue")

    async def _post_webhook(self, alert: AlertPayload, webhook: str) -> None:
        try:
            async with httpx.AsyncClient(timeout=5) as client:
                await client.post(webhook, json=alert.dict())
        except Exception as exc:
            LOG.error("Failed to POST alert to webhook %s: %s", webhook, exc)

    def history(self) -> List[AlertPayload]:
        return list(self._history)

    @asynccontextmanager
    async def subscribe(self) -> AsyncGenerator[asyncio.Queue[AlertPayload], None]:
        queue: asyncio.Queue[AlertPayload] = asyncio.Queue(maxsize=100)
        self._subscribers.add(queue)
        try:
            yield queue
        finally:
            self._subscribers.discard(queue)


# ---------------------------------------------------------------------------
# FFmpeg ingest & analysis worker
# ---------------------------------------------------------------------------


class FFMpegProcess:
    def __init__(self, config: StreamConfig) -> None:
        self._cfg = config
        self._proc: Optional[asyncio.subprocess.Process] = None
        self._analysis_task: Optional[asyncio.Task[None]] = None
        self._analysis_queue: asyncio.Queue[bytes] = asyncio.Queue(maxsize=16)
        self._stdout_reader_task: Optional[asyncio.Task[None]] = None
        self._lock = asyncio.Lock()

    @property
    def analysis_queue(self) -> asyncio.Queue[bytes]:
        return self._analysis_queue

    def _flush_analysis_queue(self) -> None:
        try:
            while True:
                self._analysis_queue.get_nowait()
        except asyncio.QueueEmpty:
            pass

    async def restart(self) -> None:
        async with self._lock:
            await self._stop_unlocked()
            await asyncio.sleep(0.2)
            await self._start_unlocked()

    async def start(self) -> None:
        async with self._lock:
            await self._start_unlocked()

    async def _start_unlocked(self) -> None:
        ensure_directory(self._cfg.hls_output_dir)
        self._flush_analysis_queue()

        filter_expr = (
            f"[0:v]split=2[vmain][vraw];"
            f"[vraw]fps={self._cfg.analysis_fps},"
            f"scale={self._cfg.analysis_scale_width}:-1,"
            f"format=yuv420p[vframes]"
        )

        segment_pattern = self._cfg.hls_output_dir / "segment_%05d.m4s"
        hls_index = self._cfg.hls_output_dir / "index.m3u8"
        init_time = max(0.1, self._cfg.segment_duration / max(1, self._cfg.llhls_partial_count))

        cmd = [
            "ffmpeg",
            "-hide_banner",
            "-loglevel",
            os.getenv("FFMPEG_LOGLEVEL", "info"),
            "-fflags",
            "nobuffer",
            "-listen",
            "1",
            "-rtmp_listen",
            "1",
            "-i",
            self._cfg.bind_url,
            "-filter_complex",
            filter_expr,
            # HLS output
            "-map",
            "[vmain]",
            "-map",
            "0:a?",
            "-c:v:0",
            "libx264",
            "-preset:v:0",
            self._cfg.ffmpeg_preset,
            "-tune:v:0",
            "zerolatency",
            "-profile:v:0",
            "main",
            "-pix_fmt",
            "yuv420p",
            "-crf",
            str(self._cfg.ffmpeg_crf),
            "-g",
            str(max(15, int(self._cfg.segment_duration * 30))),
            "-keyint_min",
            str(max(15, int(self._cfg.segment_duration * 30))),
            "-sc_threshold",
            "0",
            "-force_key_frames",
            f"expr:gte(t,n_forced*{self._cfg.segment_duration})",
            "-c:a:0",
            "aac",
            "-ar",
            "48000",
            "-b:a",
            "128k",
            "-f",
            "hls",
            "-hls_time",
            str(self._cfg.segment_duration),
            "-hls_list_size",
            str(self._cfg.hls_list_size),
            "-hls_init_time",
            f"{init_time:.3f}",
            "-hls_flags",
            "delete_segments+program_date_time+append_list+independent_segments+split_by_time+omit_endlist+temp_file",
            "-hls_segment_type",
            "fmp4",
            "-hls_fmp4_init_filename",
            "init.mp4",
            "-hls_allow_cache",
            "0",
            "-lhls",
            "1",
            "-hls_write_prft",
            "1",
            "-hls_start_number_source",
            "datetime",
            "-hls_segment_filename",
            str(segment_pattern),
            str(hls_index),
            # Analysis output (JPEG frames)
            "-map",
            "[vframes]",
            "-f",
            "image2pipe",
            "-vcodec",
            "mjpeg",
            "pipe:1",
        ]

        LOG.info("Starting FFmpeg for camera %s", self._cfg.camera_id)
        self._proc = await asyncio.create_subprocess_exec(
            *cmd,
            stdout=asyncio.subprocess.PIPE,
            stderr=asyncio.subprocess.PIPE,
            limit=10_000_000,
        )
        assert self._proc.stdout and self._proc.stderr

        loop = asyncio.get_running_loop()
        self._stdout_reader_task = loop.create_task(self._read_analysis_stream(self._proc.stdout))
        loop.create_task(self._log_stderr(self._proc.stderr))

    async def stop(self) -> None:
        async with self._lock:
            await self._stop_unlocked()

    async def _stop_unlocked(self) -> None:
        if not self._proc:
            return
        LOG.info("Stopping FFmpeg for %s", self._cfg.camera_id)
        self._proc.terminate()
        try:
            await asyncio.wait_for(self._proc.wait(), timeout=5)
        except asyncio.TimeoutError:
            LOG.warning("FFmpeg process did not stop, killing...")
            self._proc.kill()
        if self._stdout_reader_task:
            self._stdout_reader_task.cancel()
            self._stdout_reader_task = None
        self._proc = None
        self._flush_analysis_queue()

    async def _log_stderr(self, stream: asyncio.StreamReader) -> None:
        try:
            while True:
                line = await stream.readline()
                if not line:
                    break
                LOG.debug("ffmpeg(%s): %s", self._cfg.camera_id, line.decode(errors="ignore").rstrip())
        except asyncio.CancelledError:
            pass

    async def _read_analysis_stream(self, stream: asyncio.StreamReader) -> None:
        """Parse concatenated MJPEG frames emitted by FFmpeg."""
        buffer = bytearray()
        soi = b"\xff\xd8"  # Start of Image
        eoi = b"\xff\xd9"  # End of Image
        try:
            while True:
                chunk = await stream.read(8192)
                if not chunk:
                    if self._proc and self._proc.returncode is not None:
                        return
                    await asyncio.sleep(0.05)
                    continue
                buffer.extend(chunk)
                while True:
                    start = buffer.find(soi)
                    if start == -1:
                        buffer.clear()
                        break
                    end = buffer.find(eoi, start)
                    if end == -1:
                        if start > 0:
                            del buffer[:start]
                        break
                    frame = bytes(buffer[start : end + 2])
                    del buffer[: end + 2]
                    try:
                        self._analysis_queue.put_nowait(frame)
                    except asyncio.QueueFull:
                        LOG.debug("Analysis queue full for %s, dropping frame", self._cfg.camera_id)
        except asyncio.CancelledError:
            LOG.debug("Analysis reader cancelled for %s", self._cfg.camera_id)
        except Exception as exc:
            LOG.exception("Error reading analysis stream for %s: %s", self._cfg.camera_id, exc)


# ---------------------------------------------------------------------------
# Analysis pipeline
# ---------------------------------------------------------------------------


def _extract_classes(prediction: Dict[str, Any]) -> Set[str]:
    preds = prediction.get("predictions") or prediction.get("objects") or []
    return {item.get("class") or item.get("label") for item in preds}


class Analyzer:
    def __init__(
        self,
        stream_config: StreamConfig,
        ffmpeg_process: FFMpegProcess,
        model: LocalYoloModel,
        alerts: AlertManager,
    ) -> None:
        self._cfg = stream_config
        self._process = ffmpeg_process
        self._model = model
        self._alerts = alerts
        self._task: Optional[asyncio.Task[None]] = None

    def start(self) -> None:
        loop = asyncio.get_running_loop()
        self._task = loop.create_task(self._run())

    async def stop(self) -> None:
        if self._task:
            self._task.cancel()
            with contextlib.suppress(asyncio.CancelledError):
                await self._task

    async def _run(self) -> None:
        LOG.info("Starting analyzer for %s", self._cfg.camera_id)
        while True:
            frame_bytes = await self._process.analysis_queue.get()
            if not frame_bytes:
                continue
            try:
                alert = await self._evaluate_frame(frame_bytes)
                if alert:
                    await self._alerts.publish(alert, cooldown=self._cfg.alert_cooldown_seconds, webhook=self._cfg.webhook_url)
            except ModelInferenceError as exc:
                LOG.error("Model inference failed for %s: %s", self._cfg.camera_id, exc)
            except Exception as exc:
                LOG.exception("Error handling frame for %s: %s", self._cfg.camera_id, exc)

    async def _evaluate_frame(self, frame_bytes: bytes) -> Optional[AlertPayload]:
        prediction = await self._model.infer(frame_bytes, confidence=self._cfg.roboflow_min_confidence)
        classes = _extract_classes(prediction)
        detail = {"classes": sorted(c for c in classes if c)}
        if "truck_bed" not in classes:
            return AlertPayload(
                camera_id=self._cfg.camera_id,
                level=AlertLevel.TAMPERING,
                detail={**detail, "reason": "truck_bed_missing"},
            )
        if "person" in classes:
            return AlertPayload(
                camera_id=self._cfg.camera_id,
                level=AlertLevel.THEFT,
                detail={**detail, "reason": "person_detected"},
            )
        # Only truck_bed detected (no person)
        return AlertPayload(
            camera_id=self._cfg.camera_id,
            level=AlertLevel.CLEAR,
            detail={**detail, "reason": "truck_bed_without_person"},
        )


# ---------------------------------------------------------------------------
# FastAPI application
# ---------------------------------------------------------------------------


@dataclass
class APIState:
    config: ServiceConfig
    alerts: AlertManager
    workers: Dict[str, FFMpegProcess]


def build_app(state: APIState) -> FastAPI:
    app = FastAPI(title="Porteight Camera AI", version="1.0.0", debug=state.config.api_debug)

    @app.get("/health")
    async def health() -> Dict[str, Any]:
        return {"status": "ok", "ts": timestamp(), "cameras": [s.camera_id for s in state.config.streams]}

    @app.post("/auth/token", response_model=AuthTokenResponse)
    async def issue_token(payload: AuthTokenRequest, request: Request) -> AuthTokenResponse:
        stream_cfg = _resolve_stream_by_device(state.config, payload.device_id)
        if not stream_cfg:
            raise HTTPException(status_code=status.HTTP_403_FORBIDDEN, detail="Unknown device")
        if payload.device_secret != stream_cfg.auth.device_secret:
            raise HTTPException(status_code=status.HTTP_403_FORBIDDEN, detail="Invalid credentials")

        client_ip = request.client.host if request.client else None
        if stream_cfg.auth.allowed_ips and (client_ip not in stream_cfg.auth.allowed_ips):
            raise HTTPException(status_code=status.HTTP_403_FORBIDDEN, detail="IP not allowed")

        new_key = stream_cfg.auth.rotate_stream_key()
        worker = state.workers.get(stream_cfg.camera_id)
        if not worker:
            raise HTTPException(status_code=status.HTTP_500_INTERNAL_SERVER_ERROR, detail="Ingest worker unavailable")
        await worker.restart()

        base_host = stream_cfg.public_ingest_host or state.config.rtmp_public_host or request.base_url.hostname
        if not base_host:
            host_header = request.headers.get("host")
            base_host = host_header.split(":")[0] if host_header else "localhost"
        base_port = stream_cfg.public_ingest_port or state.config.rtmp_public_port or stream_cfg.ffmpeg_listen_port
        rtmp_url = stream_cfg.ingest_url(host=base_host, port=base_port)

        base_http = str(request.base_url).rstrip("/")
        llhls_url = f"{base_http}/streams/{stream_cfg.camera_id}/index.m3u8"

        LOG.info(
            "Issued RTMP stream key for camera=%s device=%s ip=%s host=%s port=%s",
            stream_cfg.camera_id,
            payload.device_id,
            client_ip,
            base_host,
            base_port,
        )

        note = "Previous stream key revoked; connect promptly using the returned RTMP URL."
        if stream_cfg.auth.allowed_ips:
            note += f" Allowed source IPs: {', '.join(sorted(stream_cfg.auth.allowed_ips))}."

        return AuthTokenResponse(
            camera_id=stream_cfg.camera_id,
            stream_key=new_key,
            rtmp_url=rtmp_url,
            llhls_url=llhls_url,
            issued_at=utcnow(),
            allowed_ips=sorted(stream_cfg.auth.allowed_ips),
            note=note,
        )

    @app.get("/streams/{camera_id}/index.m3u8")
    async def get_manifest(camera_id: str) -> Response:
        stream_cfg = _resolve_stream(state.config, camera_id)
        manifest = stream_cfg.hls_output_dir / "index.m3u8"
        if not manifest.exists():
            raise HTTPException(status_code=status.HTTP_404_NOT_FOUND, detail="Manifest not found")
        return FileResponse(
            manifest,
            media_type="application/vnd.apple.mpegurl",
            headers={"Cache-Control": "no-store", "Access-Control-Allow-Origin": "*"},
        )

    @app.get("/streams/{camera_id}/init.mp4")
    async def get_init_segment(camera_id: str) -> Response:
        stream_cfg = _resolve_stream(state.config, camera_id)
        init_segment = stream_cfg.hls_output_dir / "init.mp4"
        if not init_segment.exists():
            raise HTTPException(status_code=status.HTTP_404_NOT_FOUND, detail="Init segment missing")
        return FileResponse(init_segment, media_type="video/mp4", headers={"Cache-Control": "no-store"})

    @app.get("/streams/{camera_id}/{segment}")
    async def get_segment(camera_id: str, segment: str) -> Response:
        stream_cfg = _resolve_stream(state.config, camera_id)
        segment_path = (stream_cfg.hls_output_dir / segment).resolve()
        if stream_cfg.hls_output_dir not in segment_path.parents:
            raise HTTPException(status_code=status.HTTP_400_BAD_REQUEST, detail="Invalid segment path")
        if not segment_path.exists():
            raise HTTPException(status_code=status.HTTP_404_NOT_FOUND, detail="Segment not found")
        return FileResponse(segment_path, media_type="video/mp4", headers={"Cache-Control": "no-store"})

    @app.get("/alerts")
    async def alerts_history() -> Dict[str, Any]:
        return {"alerts": [alert.dict() for alert in state.alerts.history()]}

    @app.get("/alerts/stream")
    async def alerts_stream():
        async def event_stream() -> AsyncGenerator[bytes, None]:
            async with state.alerts.subscribe() as queue:
                while True:
                    alert = await queue.get()
                    payload = json.dumps(alert.dict()).encode("utf-8")
                    yield b"data: " + payload + b"\n\n"

        return StreamingResponse(event_stream(), media_type="text/event-stream")

    # Serve static HLS assets (for debugging)
    app.mount("/static", StaticFiles(directory=state.config.hls_root, check_dir=False), name="static")

    return app


def _resolve_stream_by_device(config: ServiceConfig, device_id: str) -> Optional[StreamConfig]:
    for stream in config.streams:
        if stream.auth.device_id == device_id:
            return stream
    return None


def _resolve_stream(config: ServiceConfig, camera_id: str) -> StreamConfig:
    for stream in config.streams:
        if stream.camera_id == camera_id:
            return stream
    raise HTTPException(status_code=status.HTTP_404_NOT_FOUND, detail=f"Unknown camera {camera_id}")


# ---------------------------------------------------------------------------
# Bootstrap & lifecycle
# ---------------------------------------------------------------------------


async def run_service(config: ServiceConfig) -> None:
    ensure_directory(config.hls_root)
    ensure_directory(config.analysis_cache_root)

    model = LocalYoloModel(config.model)
    alert_manager = AlertManager()

    ffmpeg_workers: Dict[str, FFMpegProcess] = {}
    analyzers: Dict[str, Analyzer] = {}

    for stream in config.streams:
        ensure_directory(stream.hls_output_dir)
        worker = FFMpegProcess(stream)
        await worker.start()
        analyzer = Analyzer(stream, worker, model, alert_manager)
        analyzer.start()
        ffmpeg_workers[stream.camera_id] = worker
        analyzers[stream.camera_id] = analyzer

    api_state = APIState(config=config, alerts=alert_manager, workers=ffmpeg_workers)
    app = build_app(api_state)

    config_uvicorn = uvicorn.Config(
        app=app,
        host=config.api_host,
        port=config.api_port,
        log_level="debug" if config.api_debug else "info",
        timeout_keep_alive=5,
        use_colors=False,
        reload=False,
    )
    server = uvicorn.Server(config_uvicorn)

    async def _serve_api() -> None:
        await server.serve()

    api_task = asyncio.create_task(_serve_api())

    # Wait for termination signals
    stop_event = asyncio.Event()

    def _signal_handler() -> None:
        LOG.info("Termination signal received")
        stop_event.set()

    loop = asyncio.get_running_loop()
    for sig in (signal.SIGINT, signal.SIGTERM):
        loop.add_signal_handler(sig, _signal_handler)

    await stop_event.wait()

    LOG.info("Shutting down service")
    server.should_exit = True
    await api_task

    for analyzer in analyzers.values():
        await analyzer.stop()
    for worker in ffmpeg_workers.values():
        await worker.stop()
    await model.close()


# ---------------------------------------------------------------------------
# CLI
# ---------------------------------------------------------------------------


def parse_args(argv: Optional[Iterable[str]] = None) -> argparse.Namespace:
    parser = argparse.ArgumentParser(description="Porteight camera AI processor")
    parser.add_argument("--config", type=Path, required=True, help="Path to YAML/JSON config file")
    parser.add_argument("--log-level", default=os.getenv("LOG_LEVEL", "INFO"), help="Logging level")
    return parser.parse_args(argv)


def configure_logging(level: str) -> None:
    logging.basicConfig(
        level=getattr(logging, level.upper(), logging.INFO),
        format="%(asctime)s | %(levelname)-8s | %(name)s | %(message)s",
    )


def main(argv: Optional[Iterable[str]] = None) -> None:
    args = parse_args(argv)
    configure_logging(args.log_level)
    config_path: Path = args.config.resolve()
    if not config_path.exists():
        LOG.error("Config file %s does not exist", config_path)
        sys.exit(1)

    raw_config = asyncio.run(read_json_config(config_path))
    service_config = build_service_config(raw_config)

    LOG.info("Loaded %d camera stream configurations", len(service_config.streams))
    try:
        asyncio.run(run_service(service_config))
    except GracefulExit:
        LOG.info("Exited gracefully")


if __name__ == "__main__":
    main()

