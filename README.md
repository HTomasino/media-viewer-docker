# Media Viewer

Self-hosted media server and reader for image and manga libraries: Go backend
+ vanilla ES6 web frontend, packaged as a Linux Docker image for deployment on
a home server (e.g. a Proxmox LXC/VM).

```
media-viewer:latest  ──►  :3000  ◄──  browsers / PWA (LAN)
```

## Quick start (Docker)

```bash
git clone https://github.com/HTomasino/media-viewer.git
cd media-viewer

# 1. Build the image (build context = repo root)
docker build -f backend/Dockerfile -t media-viewer:latest .

# 2. Configure
cp docker/config.docker.json.example docker/config.docker.json
# edit: media paths, TZ, trusted_proxies
chown 1000:1000 docker/config.docker.json   # server rewrites this file

# 3. Run
cd docker
docker compose up -d        # or: docker run (see docker/README.md §4)
docker compose ps           # wait for `healthy`
```

Then open `http://<host>:3000`.

Full runbook — Proxmox LXC/VM prep, watchtower updates, state migration,
troubleshooting — is in [`docker/README.md`](docker/README.md).

## What lives where

| Path | Description |
|---|---|
| `backend/` | Go server (single binary; scans libraries, thumbnails, responsive images, tag API, notifier integrations) |
| `backend/Dockerfile` | Multi-stage image build (static binary + ffmpeg) |
| `Deployable/web/` | Web frontend served from `<exe_dir>/web/` |
| `docker/` | Compose stack, config template, build helper, runbook |

## Features

- **Sections**: Images, Manga, H-Manga with periodic rescan (`watch_directories`)
- **Reader**: long-strip / single / double page, mobile gestures, persistent progress
- **Responsive images**: server-side size bucketing + WebP, preload hints
- **Async thumbnails** with worker pool and disk cache
- **Health & metrics**: `/api/health`, `/api/metrics` (Docker HEALTHCHECK wired)
- **Notifications**: Gotify (bundled child process on Windows) and Discord webhook bot — both disabled by default; configure in `config.json`
- **PWA**: service worker caching for shell + read-only API endpoints

## Config

See `docker/config.docker.json.example` (annotated) and `backend/README.md`
for the full schema. Media directories are mounted into the container
read-only; thumbnails and the on-disk index live in the `media-data` volume.

## Security notes

- The server is intended for a small number of trusted LAN clients — put a
  reverse proxy with TLS in front before exposing beyond that.
- `config.json` holds notifier tokens; keep it out of version control.