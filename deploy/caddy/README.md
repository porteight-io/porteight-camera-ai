# Caddy (HTTPS) for PortEight Camera AI

This adds an HTTPS reverse-proxy in front of the Go web server (`:8080`).

## What it does
- Terminates TLS (automatic Let's Encrypt by default)
- Proxies all HTTP(S) traffic to `127.0.0.1:8080`

## Required env vars
- `CAMERA_AI_DOMAIN`: your public domain (example: `camera.example.com`)
- `CADDY_EMAIL`: email used for Let's Encrypt registration/renewal

## Notes
- RTMP ingest still happens directly on `:1935` (Caddy does not proxy RTMP unless you use extra plugins).
- When running behind HTTPS, set `SESSION_SECURE=true` on the camera-ai service so cookies are marked Secure.

## Example run

```bash
export CAMERA_AI_DOMAIN="camera.example.com"
export CADDY_EMAIL="ops@example.com"

caddy run --config ./deploy/caddy/Caddyfile
```

## Using your own certificate
Edit `deploy/caddy/Caddyfile` and uncomment the `tls /path/to/fullchain.pem /path/to/privkey.pem` line.

