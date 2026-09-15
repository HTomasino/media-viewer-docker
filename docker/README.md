# Docker / Proxmox Deployment Runbook

Package the Media Viewer Go backend + `Deployable/web` frontend as a Linux
Docker image and run it on a Proxmox host (LXC with Docker, or a small VM).

```
repo root
├─ backend/Dockerfile                 # multi-stage image build
├─ .dockerignore                      # build-context exclusions
└─ docker/
   ├─ compose.yaml                    # media-viewer + watchtower stack
   ├─ config.docker.json.example      # annotated container config template
   ├─ build.sh                        # build helper (tags latest)
   └─ README.md                       # this file
```

---

## 1. Architecture

```
                     Proxmox host
  ┌─────────────────────────────────────────────────────┐
  │  LXC (nesting=1) or VM with Docker CE               │
  │  ┌───────────────┐   ┌───────────────────────────┐  │
  │  │ media-viewer  │   │ watchtower (sidecar)      │  │
  │  │ :3000         │   │ docker.sock (opt-in only) │  │
  │  └───────┬───────┘   └───────────────────────────┘  │
  │          │                                          │
  │   bind mounts (ro)          named volume             │
  │   /media/images  ◄─ NAS/    media-data: /app/data    │
  │   /media/manga   ◄─ NFS/SMB   (.thumbnails/ index/)  │
  │   /media/h-manga ◄─ share    ./config.docker.json    │
  │                              bind-mounted config     │
  └─────────────────────────────────────────────────────┘
          ▲
          │ port 3000 (or reverse proxy → TLS)
          LAN clients (browsers / PWA)
```

Image layout (relative config paths resolve against the exe dir `/app`):

| Path | What | Writable? |
|---|---|---|
| `/app/server` | static Go binary (CGO disabled) | no |
| `/app/web/` | frontend served by NoRoute handler | no |
| `/app/config.json` | server config — **read AND written** by the server (`saveConfig()` on gotify/discord toggles) | bind mount |
| `/app/data/` | `media-data` named volume: `.thumbnails/`, `index/`, optional `gotify-data/` | yes |
| `/media/*` | media libraries (bind mounts of NAS/NFS/SMB) | ro |

Key invariants:

- **Media mounts are read-only.** The server never writes to media dirs;
  thumbnails and index go to `/app/data`.
- **`config.json` lives on a bind mount, never baked into the image** — the
  server rewrites it when notifiers are toggled via the API.
- **`media-data` named volume survives image updates** — watchtower recreates
  the container but the volume and config are untouched.

## 2. Prerequisites

### 2.1 Proxmox host prep

**Option A (recommended): LXC container.** Debian 12 template, 2-4 vCPU,
2-4 GB RAM, small disk (thumbnails + index only; media stays on the share).

1. Create the LXC and enable nesting (required for Docker-in-LXC):
   ```
   pct set <vmid> --features nesting=1
   ```
2. Pass media into the LXC — either an `mpX` bind mount in the LXC config:
   ```
   pct set <vmid> -mp0 /mnt/pve/media,mp=/mnt/pve/media
   ```
   or mount NFS/SMB inside the container (`/etc/fstab` or `mount`).
3. Install Docker CE inside the LXC (Debian 12):
   ```
   apt-get update && apt-get install -y ca-certificates curl gnupg
   install -m 0755 -d /etc/apt/keyrings
   curl -fsSL https://download.docker.com/linux/debian/gpg | gpg --dearmor -o /etc/apt/keyrings/docker.gpg
   echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.gpg] https://download.docker.com/linux/debian bookworm stable" > /etc/apt/sources.list.d/docker.list
   apt-get update && apt-get install -y docker-ce docker-ce-cli containerd.io docker-compose-plugin
   ```

**Option B: small VM** (2 vCPU / 4 GB) with Docker — no nesting quirks,
simplest storage drivers.

**Sizing guidance:** 2-4 vCPU, 2-4 GB RAM (ffmpeg transcode spikes); disk =
thumbnails + index growth only (~small) — media stays on the NAS/share.

### 2.2 Firewall / access

Expose port 3000 on the Proxmox LAN (pve firewall rule for the LXC/VM, or the
host-level firewall). Optional reverse proxy (nginx-proxy-manager / Traefik /
caddy) for TLS + hostname — when used, add the proxy IP to `trusted_proxies`
in the config so gin's client-IP logic stays correct.

**Service-worker note:** SW registration works over plain HTTP only for
`localhost`/`127.0.0.1`. Remote LAN access needs HTTPS (via reverse proxy)
for full SW caching; without it the reader still works via the network-only
fallback — just no offline caching.

## 3. Configure

```bash
cp docker/config.docker.json.example docker/config.docker.json
# edit: media paths, trusted_proxies, TZ-relevant tuning, gotify/discord secrets
```

The example matches the backend `Config` schema (`backend/main.go`). Notable
decisions already baked in:

- `mode: release` — no gin debug spam.
- `index_path: /app/data/index`, `thumbnail_dir: /app/data/.thumbnails` —
  both live on the `media-data` volume.
- `gotify.enabled: false` — the bundled gotify child process is Windows-only
  (the official Linux release binary is glibc-linked and cannot run in the
  musl media-viewer image). For push notifications in Docker: uncomment the
  `gotify` sidecar service in compose, then set in `config.docker.json`:
  `gotify.enabled: true`, `server_url: "http://gotify:8080"`, matching
  `admin_user`/`admin_pass`, `app_token: ""` (the app token is created
  automatically via admin credentials and persisted back into the config).
  `data_dir` must stay on the `/app/data` volume (it holds the
  notified-chapters tracker).
- The `MV_*_DIR` env vars (`MV_IMAGES_DIR`, `MV_MANGA_DIR`, `MV_HMANGA_DIR`)
  override `directories` if you prefer not to edit the config file.
- There is **no `first_byte_grace_sec` field** — the write-rate middleware
  uses `write_timeout_sec` as its initial grace period (see
  `MinWriteRateMiddleware` in `backend/main.go`).

## 4. Build & run

```bash
cd docker
./build.sh              # -> media-viewer:latest
docker compose up -d
docker compose ps       # wait for `healthy`
```

Manual (no compose) equivalent:

```bash
docker build -f backend/Dockerfile -t media-viewer:latest .
docker run -d --name media-viewer -p 3000:3000 \
  -v ./config.docker.json:/app/config.json \
  -v /mnt/pve/media/images:/media/images:ro \
  -v /mnt/pve/media/manga:/media/manga:ro \
  -v /mnt/pve/media/h-manga:/media/h-manga:ro \
  -v media-data:/app/data \
  -e TZ=America/New_York \
  --restart unless-stopped \
  media-viewer:latest
```

First boot: the in-memory index rebuilds (scans `/media/*`); with empty
indexes expect a full scan. `index_on_startup: always` (default) loads saved
indexes fast and verifies in the background. If the NAS mount is slow to
appear at boot, the periodic rescan (`watch_directories`, 10 min interval)
picks it up; the container healthcheck covers the transition.

## 5. Updates (watchtower)

`docker/compose.yaml` ships a watchtower sidecar that only manages containers
carrying the `com.centurylinklabs.watchtower.enable: "true"` label (the
media-viewer service has it; unrelated containers on the same Docker host are
not touched).

- Schedule: daily at 04:00 (`WATCHTOWER_SCHEDULE`), cleanup of old images on.
- Update flow: `./docker/build.sh <tag>` (build + push to GHCR) → watchtower
  pulls the newer `latest` and recreates only the labeled container →
  `media-data` volume and the bind-mounted config survive; the in-memory
  index rebuilds on boot.

**Registry requirement:** watchtower pulls from a registry. The compose file
already points at the published image (`ghcr.io/htomasino/media-viewer:latest`),
so watchtower works as-is. Note that a locally rebuilt image with the same tag
shadows the registry one until `docker compose pull` runs.

**Registry:** the image is published to GHCR (`ghcr.io/htomasino/media-viewer`)
— watchtower pulls it from there; no extra config needed. To publish a new
build yourself (requires `docker login ghcr.io` with a PAT that has
`write:packages`):

```bash
./docker/build.sh 0.2.0   # builds, pushes ghcr.io/...:0.2.0 and :latest
```

Manual update instead of watchtower:

```bash
docker compose pull && docker compose up -d
```

```bash
docker compose pull && docker compose up -d
```

**Rollback:** watchtower's `WATCHTOWER_CLEANUP=true` removes old images, so
for instant rollback either (a) keep the last 2 tags and pin `image:` in the
compose file, or (b) retag the previous image and `docker compose up -d`:

```bash
docker tag <previous-tag-or-sha> media-viewer:latest
docker compose up -d
```

**Sidecar security:** the docker.sock mount is root-equivalent on the
LXC/VM — acceptable on a single-purpose appliance; on a shared host use a
socket proxy (`tecnativa/docker-socket-proxy`, read + restart only) or Diun
(image-update *notifier*, read-only socket) and apply updates manually.

## 6. Migrating existing Windows state

| State | Migrate? | How |
|---|---|---|
| `Deployable/.thumbnails/` | optional (regenerates) | copy into the `media-data` volume at `/app/data/.thumbnails/` (`docker cp` or copy directly into the volume path on the Proxmox host) |
| `Deployable/index/` | **not recommended** — formats are stable but the server rebuilds cheaply | skip; note the rescan-on-first-boot expectation |
| `config.json` | hand-port | copy gotify/discord tokens as-is into `config.docker.json`; change paths (see §3) |
| Browser SW caches | nothing to do | SW caches are per-origin; a new host/port = fresh origin, first load refetches everything |

Volume permissions: the container runs as uid 1000 (`appuser`). The
`media-data` volume dir is owned correctly out of the box; if you place the
volume on a bind path instead, `chown -R 1000:1000` it. Read-only media
mounts sidestep permission issues there.

Config file permissions: the server rewrites `/app/config.json` in place
(when notifiers are toggled via the API), so the host-side
`docker/config.docker.json` must be writable by uid 1000 — a root-owned copy
makes `saveConfig()` fail with permission denied and API toggle changes are
lost on restart:

```bash
chown 1000:1000 docker/config.docker.json   # in the LXC/VM, after cp
```

## 7. Logs, health, verification

```bash
docker logs -f media-viewer          # stdout logs (-notray console mode)
docker inspect media-viewer --format '{{.State.Health.Status}}'
curl http://<host>:3000/api/health   # 200 healthy / 503 degraded + checks
curl http://<host>:3000/api/metrics  # request/thumbnail counters
```

Log rotation is configured in compose (`json-file`, max 10 MB × 3).

### Verification checklist

| # | Check | Expectation | Status |
|---|---|---|---|
| 1 | `docker build` on Linux | clean, no CGO, static binary | ✅ verified (smoke-tested) |
| 2 | Container starts, `/api/health` within start_period | 200 `healthy` | ✅ verified |
| 3 | ffmpeg detected in container | log line `[FFMPEG] Detected ffmpeg=/usr/bin/ffmpeg` | ✅ verified |
| 4 | `/api/folders?section=manga` returns folders for a mounted share | JSON, `X-Cache` semantics unchanged | ✅ verified (empty share → `null`; populated share → array) |
| 5 | Thumbnail endpoint returns image, `X-Cache` header set | `MISS`→`HIT` across two calls | ☐ on Proxmox |
| 6 | Responsive WebP streaming terminates on slow min-rate | <30s w/ chunked terminator | ☐ on Proxmox |
| 7 | Index persists across `docker compose down && up` | `media-data` volume retains `index/`, second boot loads it | ☐ on Proxmox |
| 8 | Restart policy works | daemon reboot → auto-restart | ☐ on Proxmox |
| 9 | Remote client loads UI + reader | phone on LAN | ☐ on Proxmox |
| 10 | Log rotation | json-file rotated, `max-size` honored | ✅ configured in compose |
| 11 | Watchtower update cycle | new tag → recreate → data intact, health 200 | ☐ on Proxmox |
| 12 | Rollback path | retag previous image → `up -d` | ☐ on Proxmox |

### Troubleshooting

| Symptom | Cause | Fix |
|---|---|---|
| `[FFMPEG] ... not found` in logs / no video thumbs | ffmpeg missing | image bundles `ffmpeg` — check `docker exec media-viewer ffmpeg -version`; a custom build must not drop it |
| `[GOTIFY] Failed to start: gotify binary not found at /app/tools/gotify-server.exe` | bundled-child mode attempted on Linux | use external mode: gotify sidecar (compose) + `server_url` in config (see §3); bundled spawn is Windows-only |
| Gotify `enabled: true, running: false` forever | same as above | same fix |
| `[GOTIFY] ... 401/403` on every send after moving servers | app token belongs to the old gotify database | external mode validates the token on start and re-creates it via admin creds automatically; or POST `/api/gotify/reset` |
| `[THUMB GEN ERROR] ... open /app/.thumbnails/...: no such file or directory` / `generated: 0` forever | `thumbnail_dir` from the config file ignored (fixed in main.go mergeConfig; affects images built before the fix) | update the image; permanent workaround: mount a writable volume at `/app/.thumbnails` and `chown 1000:1000` it |
| `[CORS WARNING] Rejecting unsafe origin: http://<lan-ip>:3000` and every API call 403s | LAN http origin not in `ALLOWED_ORIGINS`, or image predates the private-IP CORS fix | set `ALLOWED_ORIGINS=http://<lan-ip>:3000` (plain http is accepted for private IPv4 hosts; public http/wildcards stay rejected) |
| `[STARTUP ERROR] Failed to scan images: context deadline exceeded` | 5-min default scan timeout too small for the library over CIFS/NFS | set `MV_SCAN_TIMEOUT_SEC=7200` (compose example) or lower `rescan_interval_sec` |
| `permission denied` writing `.thumbnails`/`index` | volume dir not owned by uid 1000 | `chown -R 1000:1000` the volume path (bind-path volumes only) |
| Media not appearing | share not mounted, or scanned before mount | verify `docker exec media-viewer ls /media/...`; rescan runs every `rescan_interval_sec`; restart container after mounting |
| Startup `[SCAN ERROR] Cannot access directory /media/...` | mount absent at boot | same as above — rescan covers transient absence |
| Streams killed on slow NAS path | min-rate middleware (`min_write_rate_bytes`) sees <500 KB/s | raise `write_timeout_sec` (grace period) or lower `min_write_rate_bytes` |
| Stale UI after update | browser/SW cache | hard-refresh; SW is versioned — new origin clients refetch automatically |
| Tray missing / crashes | n/a | never happens: container runs `-notray`; `-daemon` is also wrong for Docker (PID 1) |
| Watchtower updates nothing | label missing, or newer image not pushed to GHCR | keep `com.centurylinklabs.watchtower.enable: "true"` on the service; publish with `./docker/build.sh <tag>` (see §5) |

## 8. Tuning table

| Setting | Default | When to change |
|---|---|---|
| `rescan_interval_sec` (600) | 10 min | lower if media changes frequently |
| `write_timeout_sec` (30) | 30 s initial grace + rate window | raise if NAS is slow to first byte |
| `min_write_rate_bytes` (512000) | 500 KB/s | lower for slow client links |
| `thumbnail_workers` (5) | 5 | raise on 4+ vCPU hosts |
| `trusted_proxies` | loopback only | add reverse-proxy IP when TLS-terminating in front |
| `MV_SCAN_TIMEOUT_SEC` (300) | 5 min per image-section scan | raise (e.g. 7200) for large libraries on CIFS/NFS |
| `ALLOWED_ORIGINS` | unset (localhost only) | add LAN origins (http + private IPv4) or https origins; comma-separated |

## 9. Out of scope / future work

- CI/CD image builds (manual `docker build` + registry push; watchtower pulls tags)
- Docker secrets / env templating for tokens (bind-mounted config file suffices)
- In-process gotify on Linux (needs a musl-linked gotify build or a
  debian-based runtime image; external sidecar mode covers the need)