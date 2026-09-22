# Media Viewer Go Backend

## Overview
A complete Go-based backend server for the Media Viewer application. Single executable file with all dependencies embedded - no runtime dependencies required.

## Build Output
- **File**: `backend/media-server.exe` (Windows) or `media-server` (Linux/macOS)
- **Size**: ~8.5 MB (with debug symbols)
- **Optimized**: ~6 MB with `-ldflags "-s -w"`
- **Windows tray mode**: build with `-ldflags "-s -w -H windowsgui"` to produce a
  GUI-subsystem binary with no console window â€” the app runs entirely in the
  system tray (no taskbar item, no close button that can kill it). Logs go to
  `server.log` next to the executable; use the tray's "Show Window" to open an
  on-demand console for live log viewing. Use `-notray` for a normal console app.

## Features
- Single executable - no runtime dependencies
- REST API compatible with existing frontend
- In-memory database (can be upgraded to SQLite)
- File scanning for images, manga, h-manga
- Tag extraction and filtering
- **Async thumbnail generation** with worker pool
- **Rate limiting** and connection limiting
- **Security hardening** (path validation, CORS, security headers)
- **Monitoring** metrics and health checks
- **Daemon mode** with file logging
- **Lazy loading** support for manga long strip mode
- **Responsive image delivery** with size bucketing
- **Mobile preloading** with prev/next image hints
- **HTTP/2 Link headers** for push optimization
- CORS enabled for browser access

## Directory Structure

### Complete Deployment Structure
```
media-server-deployment/
â”œâ”€â”€ media-server.exe          # Main server executable
â”œâ”€â”€ config.json               # Server configuration
â”œâ”€â”€ .thumbnails/              # Thumbnail cache (auto-created)
â”‚   â”œâ”€â”€ abc123.jpg           # Legacy base-tier thumbnails
â”‚   â””â”€â”€ responsive/          # Responsive image cache
â”‚       â”œâ”€â”€ 640/            # 640px bucket
â”‚       â”œâ”€â”€ 1024/           # 1024px bucket
â”‚       â””â”€â”€ 1920/           # 1920px bucket
â”œâ”€â”€ logs/                     # Log files (if using -daemon)
â”‚   â””â”€â”€ server.log
â””â”€â”€ web/                      # Frontend files (optional, for static serving)
    â”œâ”€â”€ index.html
    â”œâ”€â”€ styles.css
    â”œâ”€â”€ script.js
    â””â”€â”€ ...

# Media directories (configured in config.json)
Z:/Downloads/
â”œâ”€â”€ Other/                    # Images section
â”œâ”€â”€ Manga/                    # Manga section
â””â”€â”€ H-Manga/                  # H-Manga section
```

## API Endpoints

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/api/config` | GET | Server configuration |
| `/api/health` | GET | Health check with metrics |
| `/api/metrics` | GET | Performance metrics |
| `/api/scan/:section` | POST | Trigger scan (images/manga/h-manga) |
| `/api/scan/status` | GET | Current scan status |
| `/api/logs?after=&limit=` | GET | Recent server log lines (ring buffer; cursor polling for the Console view) |
| `/api/files?section=` | GET | List files in section |
| `/api/folders?section=` | GET | List manga folders |
| `/api/tags` | GET | All tags with file associations |
| `/api/thumbnail/*path` | GET | Get thumbnail (async with caching, supports responsive sizing) |
| `/api/media/*path` | GET | Stream media file with proper headers |

## Configuration

### Config File (JSON)
```json
{
  "port": 3000,
  "directories": {
    "images": "Z:/Downloads/Other",
    "manga": "Z:/Downloads/Manga",
    "h-manga": "Z:/Downloads/H-Manga"
  },
  "thumbnail_dir": "./.thumbnails",
  "watch_directories": true,
  "mode": "release",
  "rate_limit_rps": 100,
  "rate_limit_burst": 20,
  "max_concurrent": 50,
  "thumbnail_workers": 5,
  "thumb_scale_factor": 0.5,
  "thumb_max_width": 400,
  "thumb_max_height": 300,
  "request_timeout_sec": 30,
  "max_request_mb": 10,
  "trusted_proxies": ["127.0.0.1", "::1", "192.168.1.50"]
}
```

### Configuration Options

| Option | Default | Description |
|--------|---------|-------------|
| `port` | 3000 | Server port |
| `directories` | `{}` | Map of section â†’ directory paths |
| `thumbnail_dir` | "./.thumbnails" | Thumbnail cache directory |
| `watch_directories` | true | Enable file watching |
| `mode` | "release" | "debug" or "release" mode |
| `rate_limit_rps` | 100 | Rate limit requests per second |
| `rate_limit_burst` | 20 | Rate limit burst size |
| `max_concurrent` | 50 | Maximum concurrent connections |
| `thumbnail_workers` | 5 | Number of thumbnail generator workers |
| `thumb_scale_factor` | 0.5 | Thumbnail scale factor (0.5 = 50%) |
| `thumb_max_width` | 400 | Thumbnail max width in pixels |
| `thumb_max_height` | 300 | Thumbnail max height in pixels |
| `request_timeout_sec` | 30 | Request timeout in seconds |
| `max_request_mb` | 10 | Maximum request body size in MB |
| `max_thumbnail_mb` | 100 | Maximum file size for thumbnail generation |
| `trusted_proxies` | `["127.0.0.1", "::1"]` | IPs of trusted reverse proxies |
| `responsive_buckets` | `[320, 640, 768, 1024, 1280, 1536, 1920, 2048]` | Size buckets for responsive images |
| `enable_preloading` | `true` | Enable prev/next image preloading for mobile |

### Responsive Image Delivery

The server automatically delivers optimally-sized images based on the client's screen dimensions:

**Client Request:**
```
GET /api/thumbnail/path/to/image.jpg?width=390&height=844&dpr=2&mobile=1&index=5&total=24
```

**Parameters:**
- `width`, `height`: Client screen dimensions
- `dpr`: Device pixel ratio (e.g., 2.0 for iPhone retina)
- `mobile`: Flag to enable mobile optimizations
- `index`, `total`: Current position in sequence (for preloading)

**Server Response Headers:**
```
X-Image-Bucket: 768
X-Cache: HIT
X-Preload-Previous: /api/thumbnail/.../img_04.jpg?...
X-Preload-Next: /api/thumbnail/.../img_06.jpg?...
Link: </api/thumbnail/.../img_06.jpg?>; rel=preload; as=image
```

**Cache Structure:**
```
.thumbnails/
â”œâ”€â”€ abc123.jpg                 # Legacy base-tier thumbnails
â””â”€â”€ responsive/
    â”œâ”€â”€ 640/
    â”‚   â””â”€â”€ abc123.jpg        # 640px bucket
    â”œâ”€â”€ 1024/
    â”‚   â””â”€â”€ abc123.jpg        # 1024px bucket
    â””â”€â”€ 1920/
        â””â”€â”€ abc123.jpg        # 1920px bucket
```

**Bucket Calculation:**
The server uses the minimum of width/height to handle orientation changes gracefully. If an exact bucket doesn't exist, the next larger bucket is used as fallback.

### Mobile Preloading

When reading manga on mobile devices, the server automatically:
1. Detects mobile from User-Agent or `mobile=1` flag
2. Generates `X-Preload-Previous` and `X-Preload-Next` headers
3. Adds `Link` headers for HTTP/2 push hints
4. Pre-generates thumbnails for adjacent images

The frontend uses these hints to prefetch images via `<link rel="prefetch">` elements for instant navigation.

## Deployment Guide

### Windows Deployment

#### Basic Setup
```powershell
# 1. Create deployment directory
mkdir C:\MediaServer
cd C:\MediaServer

# 2. Copy files
copy Z:\path\to\media-server.exe .
copy Z:\path\to\config.json .

# 3. Edit config.json with your paths
notepad config.json

# 4. Run server
.\media-server.exe -config=config.json
```

#### As Windows Service
```powershell
# Using nssm (Non-Sucking Service Manager)
nssm install MediaServer C:\MediaServer\media-server.exe
nssm set MediaServer AppDirectory C:\MediaServer
nssm set MediaServer AppParameters "-config=config.json"
nssm start MediaServer
```

#### Auto-start on Boot (Registry)
```powershell
# Add to registry for auto-start
reg add "HKCU\Software\Microsoft\Windows\CurrentVersion\Run" /v MediaServer /t REG_SZ /d "C:\MediaServer\media-server.exe -config=C:\MediaServer\config.json" /f
```

### Linux Deployment

#### systemd Service
```bash
# 1. Copy binary
sudo cp media-server /usr/local/bin/
sudo chmod +x /usr/local/bin/media-server

# 2. Create config directory
sudo mkdir -p /etc/media-server
sudo cp config.json /etc/media-server/
sudo mkdir -p /var/lib/media-server/.thumbnails
sudo mkdir -p /var/log/media-server

# 3. Create service file
sudo tee /etc/systemd/system/media-server.service << 'SERVICE'
[Unit]
Description=Media Viewer Server
After=network.target

[Service]
Type=simple
User=media-server
Group=media-server
WorkingDirectory=/var/lib/media-server
ExecStart=/usr/local/bin/media-server -config=/etc/media-server/config.json
Restart=always
RestartSec=5
StandardOutput=journal
StandardError=journal
SyslogIdentifier=media-server

[Install]
WantedBy=multi-user.target
SERVICE

# 4. Create user
sudo useradd -r -s /bin/false media-server
sudo chown -R media-server:media-server /var/lib/media-server
sudo chown -R media-server:media-server /var/log/media-server

# 5. Enable and start
sudo systemctl daemon-reload
sudo systemctl enable media-server
sudo systemctl start media-server
```

#### Docker Deployment

The canonical container build lives at [`backend/Dockerfile`](Dockerfile) and the
deployment stack (compose file, config template, hourly watchtower, runbook) at
[`docker/`](../docker/README.md). Build context must be the repository root:

```bash
docker build -f backend/Dockerfile -t ghcr.io/htomasino/media-viewer:latest .
docker run -d -p 3000:3000 \
  -v ./config.json:/app/config.json \
  -v /path/to/media/images:/media/images:ro \
  -v media-data:/app/data \
  ghcr.io/htomasino/media-viewer:latest
```

See `docker/README.md` for the full Proxmox/Docker runbook.
### Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `MV_SCAN_TIMEOUT_SEC` | `300` | Per-scan context timeout in seconds — raise (e.g. `7200`) for large libraries on CIFS/NFS |
| `MV_IMAGES_DIR` / `MV_MANGA_DIR` / `MV_HMANGA_DIR` | config `directories` | Section directory overrides (container deployments) |
| `ALLOWED_ORIGINS` | *(none)* | Extra CORS origins; plain `http://` accepted for private (RFC1918) IPv4 hosts only |

### macOS Deployment

#### LaunchAgent (Auto-start)
```bash
# 1. Create plist
mkdir -p ~/Library/LaunchAgents
cat > ~/Library/LaunchAgents/com.mediaserver.plist << 'PLIST'
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>com.mediaserver</string>
    <key>ProgramArguments</key>
    <array>
        <string>/usr/local/bin/media-server</string>
        <string>-config=/usr/local/etc/media-server/config.json</string>
    </array>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
    <key>StandardOutPath</key>
    <string>/usr/local/var/log/media-server/server.log</string>
    <key>StandardErrorPath</key>
    <string>/usr/local/var/log/media-server/error.log</string>
</dict>
</plist>
PLIST

# 2. Load service
launchctl load ~/Library/LaunchAgents/com.mediaserver.plist
```

### Nginx Reverse Proxy Setup

```nginx
# /etc/nginx/sites-available/media-server
server {
    listen 80;
    server_name media.local;
    
    # Security headers
    add_header X-Frame-Options "SAMEORIGIN" always;
    add_header X-Content-Type-Options "nosniff" always;
    add_header X-XSS-Protection "1; mode=block" always;

    location / {
        proxy_pass http://localhost:3000;
        proxy_http_version 1.1;
        
        proxy_set_header Ho
