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
- `gotify.enabled: false` — the bundled gotify binary is a Windows `.exe` and
  cannot run in the Linux image. To get push notifications later, run gotify
  as its own container and point the config at it (documented as future work).
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
- Update flow: build/push a new `media-viewer:<tag>` image → watchtower pulls
  and recreates only the labeled container → `media-data` volume and the
  bind-mounted config survive; the in-memory index rebuilds on boot.

**Registry requirement:** watchtower pulls from a registry — it cannot see
locally built images. With the compose default `image: media-viewer:latest`
(build-only, never pushed), watchtower resolves that name against Docker Hub
and updates never happen. For unattended updates, push the image to a registry
and point `image:` at the full registry path:

```bash
docker tag media-viewer:latest registry.lan/media-viewer:latest
docker push registry.lan/media-viewer:latest
# then set image: registry.lan/media-viewer:latest in compose.yaml
```

If you keep updates manual (no registry), delete the watchtower service from
the compose file and apply updates with `docker compose up -d` after a rebuild.

```bash
# manual update instead (or alongside watchtower):
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
| `permission denied` writing `.thumbnails`/`index` | volume dir not owned by uid 1000 | `chown -R 1000:1000` the volume path (bind-path volumes only) |
| Media not appearing | share not mounted, or scanned before mount | verify `docker exec media-viewer ls /media/...`; rescan runs every `rescan_interval_sec`; restart container after mounting |
| Startup `[SCAN ERROR] Cannot access directory /media/...` | mount absent at boot | same as above — rescan covers transient absence |
| Streams killed on slow NAS path | min-rate middleware (`min_write_rate_bytes`) sees <500 KB/s | raise `write_timeout_sec` (grace period) or lower `min_write_rate_bytes` |
| Stale UI after update | browser/SW cache | hard-refresh; SW is versioned — new origin clients refetch automatically |
| Tray missing / crashes | n/a | never happens: container runs `-notray`; `-daemon` is also wrong for Docker (PID 1) |
| Watchtower updates nothing | label missing, or image not in a registry | keep `com.centurylinklabs.watchtower.enable: "true"` on the service; push to a registry and set the full path in `image:` (see §5) |

## 8. Tuning table

| Setting | Default | When to change |
|---|---|---|
| `rescan_interval_sec` (600) | 10 min | lower if media changes frequently |
| `write_timeout_sec` (30) | 30 s initial grace + rate window | raise if NAS is slow to first byte |
| `min_write_rate_bytes` (512000) | 500 KB/s | lower for slow client links |
| `thumbnail_workers` (5) | 5 | raise on 4+ vCPU hosts |
| `trusted_proxies` | loopback only | add reverse-proxy IP when TLS-terminating in front |

## 9. Out of scope / future work

- CI/CD image builds (manual `docker build` + registry push; watchtower pulls tags)
- Native gotify sidecar container (notifications parity) — document as future work
- Docker secrets / env templating for tokens (bind-mounted config file suffices)