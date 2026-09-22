# Media Viewer Deployment

## Quick Start

```powershell
.\media-server.exe
```

The server runs as a **system tray application** - it has no console window and no taskbar entry. Look for the Media Viewer icon in the system tray (notification area) to control it:

- **Open in Browser** - open `http://localhost:3000` in your default browser (enabled only once the server is ready to accept connections).
- **Show Window** - open an on-demand console window showing live server logs.
- **Hide Window** - hide the console window (the app keeps running in the tray).
- **Quit** - shut the server down gracefully.

Server logs are written to `server.log` next to the executable. Use **Show Window** for live log viewing. (`server.log` is rotated to `server.log.old` when it exceeds 10 MB.)

On the very first run, a one-time dialog appears explaining the tray-only behavior; it is not shown again on subsequent runs (gated by a `.tray_notice_shown` marker file next to the executable).

> To run as a normal console application instead, launch with `-notray`: `.\media-server.exe -notray`

Then open your browser to `http://localhost:3000`

## Directory Structure

```
./
â”œâ”€â”€ media-server.exe        # Main server executable
â”œâ”€â”€ config.json             # Server configuration
â”œâ”€â”€ README.md               # This file
â”œâ”€â”€ tools/
â”‚   â””â”€â”€ gotify-server.exe   # Push notification server (bundled)
â””â”€â”€ web/                    # Frontend files
    â”œâ”€â”€ index.html
    â”œâ”€â”€ tag-management.html
    â”œâ”€â”€ manifest.json
    â”œâ”€â”€ sw.js
    â”œâ”€â”€ css/
    â””â”€â”€ js/
```

## Configuration

Edit `config.json` to set your media directories:

```json
{
  "port": 3000,
  "directories": {
    "images": "C:/Users/YourName/Pictures",
    "manga": "C:/Users/YourName/Manga",
    "h-manga": "C:/Users/YourName/H-Manga"
  }
}
```

### Main Configuration Options

| Option | Default | Description |
|--------|---------|-------------|
| `port` | `3000` | Server port |
| `directories` | - | Paths to images, manga, h-manga folders |
| `watch_directories` | `true` | Enable periodic directory rescans |
| `rescan_interval_sec` | `600` | Seconds between rescans (10 min) |
| `mode` | `release` | "debug" or "release" |
| `rate_limit_rps` | `100` | Requests per second limit |
| `rate_limit_burst` | `20` | Rate limit burst size |
| `max_concurrent` | `50` | Maximum concurrent API connections |
| `conn_limit_acquire_timeout_sec` | `5` | Seconds to wait for a connection slot before 503 |
| `thumbnail_workers` | `5` | Thumbnail generation workers |
| `thumb_scale_factor` | `0.5` | Thumbnail scale factor |
| `thumb_max_width` | `400` | Maximum thumbnail width |
| `thumb_max_height` | `300` | Maximum thumbnail height |
| `thumbnail_dir` | `./.thumbnails` | Thumbnail cache directory |
| `max_thumbnail_mb` | `100` | Max file size (MB) eligible for thumbnail generation |
| `request_timeout_sec` | `30` | Request timeout in seconds |
| `write_timeout_sec` | `30` | Initial write deadline in seconds; extended while client downloads fast enough |
| `min_write_rate_bytes` | `512000` | Minimum bytes/sec to keep connection alive (500 KB/s). Connections slower than this are terminated. |
| `max_request_mb` | `10` | Max request body size in MB |
| `index_on_startup` | `always` | Index strategy: "always", "fallback", or "never" |
| `index_path` | `./index` | Directory for saved index files |
| `responsive_buckets` | `[320, 640, 768, 1024, 1280, 1536, 1920, 2048]` | Size buckets for responsive image delivery |
| `enable_preloading` | `true` | Enable prev/next image preloading hints |
| `preload_desktop_default` | `3` | Default number of images to preload on desktop |
| `preload_mobile_default` | `1` | Default number of images to preload on mobile |
| `max_preload_count` | `5` | Maximum allowed preload count |
| `trusted_proxies` | `[127.0.0.1, ::1]` | IPs of trusted reverse proxies |

---

## Docker / Proxmox Deployment

To run the server as a Linux Docker container on a Proxmox host (LXC or VM),
see [`docker/README.md`](../docker/README.md). That runbook covers the image
build (`backend/Dockerfile`), the compose stack with the watchtower update
sidecar (`docker/compose.yaml`), the container config template
(`docker/config.docker.json.example`), Proxmox host prep, state migration from
this Windows deployment, and troubleshooting.

---

## Frontend Features

The shipped `web/` frontend is an ES6 modular single-page application with a service worker:

- **Sections**: Images, Manga, and H-Manga navigation with separate folder structures.
- **Manga reader**: Long-strip, single-page, and double-page modes with mobile tap/swipe gestures, lazy/sequential loading, and persistent reading progress.
- **Read state**: Per-chapter reading progress is stored in IndexedDB. Series cards show a "Continue Ch.N" badge (aggregated across all books for h-manga artists), h-manga book cards show a green "Read ✓" badge when fully read, and the series splash highlights the "Continue Reading" chapter.
- **Media modal**: Image/video viewer with tag assignment, swipe navigation, responsive preloading, and blob-URL resource management.
- **Tag management**: Dedicated `tag-management.html` page with a **Console** tab (live server log tail with pause, manual refresh, clear, and auto-scroll), a Tag Browser for bulk tag operations and flushing, and a thumbnail status view.
- **Service worker**: Stale-while-revalidate caching for read-only API endpoints (`/api/tags`, `/api/tags/stats`) and shell assets. `/api/files` and `/api/folders` are deliberately network-only so folder/chapter lists reflect newly-added content after reading (stale lists corrupted the Continue badge's chapter index).
- **Favorites & progress**: Client-side IndexedDB stores favorites, reading progress, and cache handles for offline/fallback directory selection.
- **Responsive images**: The backend delivers viewport-sized images; the frontend requests `width`, `height`, `dpr`, and `nocache` parameters and uses `X-Preload-Previous` / `X-Preload-Next` headers to prefetch adjacent pages.
- **Resource management**: `blob:` URLs created for preloaded images are revoked when the modal/reader closes to prevent memory leaks.

---

## API Endpoints

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/api/config` | GET | Server mode and configured sections |
| `/api/health` | GET | Health check with metrics |
| `/api/metrics` | GET | Performance metrics |
| `/api/files?section=` | GET | List files in a section |
| `/api/folders?section=` | GET | List manga/h-manga folders |
| `/api/scan/:section` | POST | Full rescan of a section |
| `/api/reindex/:section` | POST | Incremental reindex (mtime-based) |
| `/api/scan/:section/:folder?mode=` | POST | Rescan a specific folder (`full` or `quick`) |
| `/api/scan/status` | GET | Current scan status per section |
| `/api/logs?after=&limit=` | GET | Recent server log lines for the Console view (incremental cursor polling) |
| `/api/tags` | GET | All tags with file associations |
| `/api/tags/stats?section=` | GET | Tag counts, optionally filtered by section |
| `/api/tags/:tag/files?section=` | GET | Files by tag |
| `/api/tags/:path` | POST | Update tags for a single file |
| `/api/tags/bulk` | POST | Bulk add/remove/set tags |
| `/api/tags/flush?section=` | POST | Flush selected or all tags |
| `/api/thumbnail/*path` | GET | Thumbnail (bucket 0, async generation) |
| `/api/thumbnail/*path` | HEAD | Preload header discovery |
| `/api/media/*path` | GET | Media streaming / responsive image |
| `/api/media/*path` | HEAD | Preload header discovery |
| `/api/thumbnails/status` | GET | Thumbnail audit status |
| `/api/thumbnails/generate?section=&limit=` | POST | Start missing thumbnail generation |
| `/api/thumbnails/generate/*path` | POST | Generate a specific thumbnail |
| `/api/thumbnails/cull-orphans` | POST | Delete orphaned thumbnail files |
| `/api/thumbnails/orphans` | GET | List orphaned thumbnail files |
| `/api/gotify/status` | GET | Gotify status and pending notifications |
| `/api/gotify/toggle` | POST | Enable/disable Gotify |
| `/api/gotify/config` | POST | Update Gotify priority/cooldown |
| `/api/gotify/reset` | POST | Reset/restart Gotify and recreate app token |
| `/api/gotify/pending` | GET | List pending chapter notifications |
| `/api/gotify/push-pending` | POST | Send pending notifications manually |
| `/api/discord/status` | GET | Discord status and pending notifications |
| `/api/discord/toggle` | POST | Enable/disable Discord |
| `/api/discord/config` | POST | Update Discord token/recipient/cooldown |
| `/api/discord/test` | POST | Send a test Discord DM |
| `/api/discord/pending` | GET | List pending chapter notifications |
| `/api/discord/push-pending` | POST | Send pending notifications manually |

---

## Command Line Options

```powershell
.\media-server.exe -port=8080                         # Custom port
.\media-server.exe -config=my.json                    # Custom config file
.\media-server.exe -daemon -logfile=server.log       # Daemon mode
.\media-server.exe -notray                           # Console mode (disable tray)
.\media-server.exe -skip-deps                        # Skip dependency check/install
```

---

## Frontend/Backend Compatibility

The canonical frontend source lives in `refactor/src/`. The `Deployable/web/` copy is produced by `refactor/build/deploy.sh`. After editing files under `refactor/src/`, run the deploy script to update `Deployable/web/`. Do not hand-edit `Deployable/web/` directly or the two copies will drift (for example, mismatched `app.js?v=` cache-bust versions or divergent modal cleanup logic).

---

## Push Notifications with Gotify

Media Viewer can send push notifications to your phone or browser whenever new manga chapters are detected during a periodic rescan. This uses [Gotify](https://gotify.net/), a self-hosted push notification server that is bundled with this deployment.

### How It Works

1. The media server launches `gotify-server.exe` as a child process on startup. (Windows tray/console deployments only — in Docker the bundled binary is unavailable; use an external Gotify server via `server_url`.)
2. On first run, it automatically creates a "Media Viewer" application in Gotify.
3. When the periodic directory rescan detects new chapters in manga or h-manga folders, a notification is sent to Gotify.
4. Your phone/computer receives the notification via the Gotify app.
5. When the media server shuts down, the Gotify process is stopped automatically.

Notifications are grouped by manga title - if a rescan finds 3 new chapters for the same series, you get one notification ("3 new chapters added to manga") instead of three. A configurable cooldown per title (default 1 hour) prevents spam from repeated rescans.

### Server-Side Setup

#### Step 1: Enable Gotify in config.json

Edit `config.json` and set `"enabled": true` in the `gotify` section:

```json
{
  "gotify": {
    "enabled": true,
    "binary_path": "./tools/gotify-server.exe",
    "port": 8180,
    "admin_user": "admin",
    "admin_pass": "admin",
    "data_dir": "./gotify-data",
    "cooldown_sec": 3600,
    "default_priority": 5
  }
}
```

#### Step 2: Start the media server

```powershell
.\media-server.exe
```

The server log will show Gotify startup progress:

```
[GOTIFY] Starting gotify server on port 8180 (data: ...\gotify-data)
Starting Gotify version 2.9.1
[GOTIFY] Server is ready
[GOTIFY] Created app token: AbCdEf123456
```

If you see `[GOTIFY] Created app token: ...`, the setup is working. The token was auto-generated and is now being used to send notifications.

#### Step 3: Secure the Gotify admin account (IMPORTANT)

On first launch, Gotify creates an admin account with the password specified in `admin_pass` (default: `admin`). **You should change this password immediately:**

1. Open `http://localhost:8180` in your browser.
2. Log in with the username and password from your config (default: `admin` / `admin`).
3. Click your username in the top-right corner.
4. Change the password to something secure.
5. Update `admin_pass` in `config.json` to match (this is only used for the initial account creation, but keeping it in sync avoids confusion).

After changing the password, you can also:
- Create additional user accounts (Users tab - "Add User").
- Delete the default admin account if you created a replacement.
- Disable new user registration (it is off by default in our config).

#### Step 4: Verify the "Media Viewer" application exists

1. In the Gotify web UI (`http://localhost:8180`), click the **Apps** tab in the top-right.
2. You should see an application named **"Media Viewer"** with a token like `AbCdEf123456`.
3. This application is what the media server uses to send notifications - do not delete it.

If you accidentally delete the "Media Viewer" app, the media server will get a 401 error on the next notification attempt. To fix this, either:
- Restart the media server (it will auto-create a new app on startup if the token is invalid).
- Manually recreate the app in the Gotify UI and update the `app_token` field in `config.json`.

#### Step 5: Test notifications manually

You can test that the notification pipeline works by sending a message directly via the API:

```powershell
# Replace TOKEN with the app token from the Gotify Apps tab
$token = "YOUR_APP_TOKEN_HERE"
$body = @{
    title    = "New Chapter: Test Manga"
    message  = "Chapter `"Ch.42`" has been added to manga"
    priority = 5
} | ConvertTo-Json

Invoke-WebRequest -Uri "http://localhost:8180/message?token=$token" `
    -Method POST `
    -Headers @{"Content-Type"="application/json"} `
    -Body $body `
    -UseBasicParsing
```

You should see the notification appear in the Gotify web UI under the "Messages" tab.

### Client-Side Setup (Receiving Notifications)

To receive push notifications on your phone or other devices:

#### Android

1. Install **Gotify** from the [Google Play Store](https://play.google.com/store/apps/details?id=com.github.gotify) or [F-Droid](https://f-droid.org/en/packages/com.github.gotify/).
2. Open the app and enter your Gotify server URL:
   - If on the same network: `http://YOUR_SERVER_IP:8180`
   - If using a reverse proxy: `https://your-domain.com`
3. Log in with your Gotify username and password.
4. The app will subscribe to all messages for your user account.
5. Notifications for "Media Viewer" messages will now appear as push notifications on your phone.

#### iOS / macOS

Gotify does not have a native iOS app. Use one of these alternatives:
- **Browser notifications**: In the Gotify web UI (`http://localhost:8180`), enable browser notifications in the Settings tab. This works on any desktop browser.
- **Gotify-to-NTFY bridge**: Use [gotify-to-ntfy](https://github.com/gotify/server) or a webhook plugin to forward to a notification service with iOS support.

#### Desktop (Windows/macOS/Linux)

Browser push notifications work directly:
1. Open `http://localhost:8180` in Chrome, Firefox, or Edge.
2. Log in with your Gotify credentials.
3. Click **Settings** (gear icon) - enable **Push Notifications**.
4. Grant browser notification permissions when prompted.
5. Desktop notifications will now appear even when the browser tab is in the background.

#### Gotify CLI (for scripting)

You can also monitor messages from the command line:

```powershell
# Install the Gotify CLI
go install github.com/gotify/cli/v2@latest

# Configure it
gotify init

# Then push a test message
gotify push -t "Test" "Hello from the CLI"
```

### Remote Access Setup

By default, Gotify only listens on `localhost`. To receive notifications outside your local network:

#### Option A: Port Forwarding

1. Forward the Gotify port (8180) on your router to the server machine.
2. Connect your phone using `http://YOUR_PUBLIC_IP:8180`.
3. **Warning**: Use HTTPS in production - see Option B.

#### Option B: Reverse Proxy with HTTPS (Recommended)

1. Set up a reverse proxy (nginx, Caddy, or Traefik) in front of Gotify.
2. Configure an HTTPS certificate (Let`s Encrypt or self-signed).
3. Point your domain (e.g., `gotify.yourdomain.com`) to the proxy.
4. Connect the mobile app using `https://gotify.yourdomain.com`.

Example Caddy config (`Caddyfile`):
```
gotify.yourdomain.com {
    reverse_proxy localhost:8180
}
```

Caddy automatically provisions HTTPS via Let`s Encrypt.

### Gotify Configuration Reference

| Option | Default | Description |
|--------|---------|-------------|
| `enabled` | `false` | Enable/disable push notifications |
| `binary_path` | `./tools/gotify-server.exe` | Path to the Gotify server binary (Windows bundled mode only) |
| `server_url` | *(empty)* | **External Gotify server URL** (e.g. `http://gotify:8080` for a gotify container). When set, the server never spawns the bundled binary and talks to this URL instead — the supported mode in Docker/Linux deployments. Stale app tokens are re-created automatically via the admin credentials on the first 401/403 send. |
| `port` | `8180` | Port for the Gotify server HTTP API |
| `admin_user` | `admin` | Admin username (used for initial setup only) |
| `admin_pass` | `admin` | Admin password (used for initial setup only) |
| `app_token` | *(auto)* | Application token - leave empty to auto-create on first run. Set manually if you recreated the app in the Gotify UI. |
| `data_dir` | `./gotify-data` | Directory for Gotify database, images, and plugins |
| `cooldown_sec` | `3600` | Minimum seconds between notifications for the same manga title (default: 1 hour) |
| `default_priority` | `5` | Notification priority (1 = low, 10 = urgent). The Gotify client can filter by priority. |

### How Notifications Are Triggered

Notifications are only sent when **new chapters** are detected, which happens during:
1. **Periodic rescan** - runs every `rescan_interval_sec` (default: 10 minutes) if `watch_directories` is `true`.
2. **Manual rescan** - triggered via the API (`POST /api/scan/manga` or `POST /api/scan/h-manga`).
3. **Verification scan** - runs after startup if the index was loaded from disk.

The detection logic:
1. Before each rescan, the server snapshots the current chapter list per series.
2. After the rescan completes, it diffs the new state against the snapshot.
3. Any new chapters in an **existing** series trigger a notification.
4. **New series** (series that did not exist in the previous snapshot) do **not** trigger notifications on the first scan - this prevents a flood of notifications on first startup.
5. Subsequent rescans that find new chapters in those series will trigger notifications normally.

### Notification Format

| Field | Example |
|-------|---------|
| **Title** | `New Chapter: [Artist] Manga Name` |
| **Message** (single chapter) | `Chapter "Ch.42" has been added to manga` |
| **Message** (multiple chapters) | `3 new chapters added to h-manga` |
| **Priority** | 5 (configurable) |


---

## Push Notifications with Discord

Media Viewer can also send new-chapter notifications directly to your Discord account as direct messages using a Discord bot. Unlike Gotify, this does **not** require a separate self-hosted server; it only needs a free Discord bot token.

### How It Works

1. You create a Discord bot and invite it to your server, or keep it private.
2. The media server calls the Discord REST API to open a DM channel with your Discord user ID.
3. When the periodic rescan detects new chapters, it sends a rich embed DM to that channel.
4. Notifications are grouped by manga title and respect the same per-title cooldown as Gotify.

### Creating a Discord Bot

1. Go to the [Discord Developer Portal](https://discord.com/developers/applications).
2. Click **New Application**, give it a name (for example Media Viewer), and create it.
3. In the left sidebar, click **Bot**.
4. Click **Reset Token** (or **Add Bot** if you haven't created one yet), then **Copy** the token. Treat this like a password - anyone with it can control the bot.
5. Scroll down and make sure these are enabled:
   - **MESSAGE CONTENT INTENT** - required for the bot to understand simple DM replies if you later add interactivity.
   - **PRESENCE INTENT** and **SERVER MEMBERS INTENT** are not needed for notifications.

### Finding Your Discord User ID

Your user ID is **not** your username or tag:

1. Open Discord (desktop or web) and go to **User Settings**.
2. Click **Advanced** in the left sidebar and enable **Developer Mode**.
3. Right-click your own avatar or username in any server/DM and choose **Copy User ID**.

Alternatively, from any Discord client, type `\@yourself` in a message and press Enter; the resulting number is your user ID.

### Allowing the Bot to DM You

Discord bots cannot DM arbitrary users unless:

- The user and the bot share at least one server, **or**
- The user has added the bot as a friend, **or**
- The user has previously DM'd the bot.

The simplest way to satisfy this:

1. In the Developer Portal, go to **OAuth2 -> URL Generator**.
2. Under **Scopes**, select `bot`.
3. Under **Bot Permissions**, select **Send Messages** (no other permissions are required for DMs).
4. Copy the generated URL and open it in your browser.
5. Select a private server (you can create an empty server just for this) and authorize the bot.

Once the bot is in a server with you, the media server can open a DM channel and send messages.

### Server-Side Setup

#### Step 1: Enable Discord in config.json

Edit `config.json` and add the `discord` section:

```json
{
  "discord": {
    "enabled": true,
    "bot_token": "YOUR_BOT_TOKEN_HERE",
    "recipient_id": "YOUR_DISCORD_USER_ID",
    "cooldown_sec": 3600
  }
}
```
| Option | Default | Description |
|--------|---------|-------------|
| `enabled` | `false` | Enable/disable Discord notifications |
| `bot_token` | `""` | Your Discord bot token |
| `recipient_id` | `""` | Discord user ID that will receive DMs |
| `cooldown_sec` | `3600` | Minimum seconds between notifications for the same manga title |

#### Step 2: Start the media server

```powershell
.\media-server.exe
```
If the bot token is valid and the bot can DM the recipient, you will see:

```
[DISCORD] Token validated (bot user 123456789012345678)
[DISCORD] DM channel cached: 987654321098765432
```
If the bot and recipient do not share a server, you will see an error about being unable to create a DM channel. Add the bot to a server that contains the recipient and restart the media server.

#### Step 3: Test from the Tag Management page

1. Open `http://localhost:3000/tag-management.html`.
2. Scroll to the **Discord Settings** panel.
3. Click **Test** to send a test direct message.
4. Check your Discord DMs for a message from the bot.

You can also toggle Discord on/off and push pending notifications from this panel.

### Configuring via the Web UI

Instead of editing config.json directly, you can configure Discord entirely in the browser:

1. Go to **Tag Management** â†’ **Discord Settings**.
2. Paste the bot token into **Bot Token**.
3. Paste your Discord user ID into **Recipient User ID**.
4. Adjust the cooldown if desired.
5. Click **Save Config**.
6. Click **Enable**, then **Test**.

The token is saved to `config.json`. For security it is not echoed back to the UI after saving; the status text will show whether a token is configured.

### Troubleshooting Discord

**"discord not configured" / start fails:**
- Verify `bot_token` is set and starts with a valid bot token (not a client secret or application ID).
- Verify `recipient_id` is a numeric Discord user ID.
- Make sure the bot and recipient share at least one Discord server, or the recipient has previously DM'd the bot.

**"failed to resolve DM channel":**
- Discord requires a shared server (or existing DM) before a bot can open a DM. Invite the bot to a server containing the recipient.
- The bot needs no special channel permissions; only the ability to DM the user.

**Test succeeds but scan notifications never arrive:**
- Confirm `enabled` is `true` and the Discord status panel shows **Ready**.
- Confirm `watch_directories` is `true` and `rescan_interval_sec` is reasonable.
- Remember that only **new chapters in existing series** trigger notifications; the first scan establishes a baseline.
- Check the server logs for rate-limit sleeps (`[DISCORD] Rate limited ...`). The server handles Discord rate limits automatically.

**No notifications on first scan:**
- This is by design, matching Gotify behavior. Subsequent rescans that find new chapters in existing series will notify.

**"Token validated" but no DMs:**
- Double-check that the copied user ID is actually yours and not a guild/server ID.
- Check Discord privacy settings; some users block DMs from server members.


### Troubleshooting Gotify

**"gotify binary not found" error at startup:**
- Ensure `tools/gotify-server.exe` exists in the same directory as `media-server.exe`.
- Check that `binary_path` in `config.json` points to the correct location.
- If the binary is missing, download it from [Gotify Releases](https://github.com/gotify/server/releases/latest) and place it in `tools/`.

**"gotify health check failed" error:**
- Check if port 8180 is already in use by another process.
- Change the port in `config.json` (`"port": 8181`) and try again.
- Look at the server logs for Gotify startup errors.

**Notifications not appearing on phone:**
- Ensure the Gotify app is connected to the correct server URL.
- Check that you logged in with the correct user account in the mobile app.
- Verify the "Media Viewer" application exists in the Gotify web UI.
- Test manually using the PowerShell script in Step 5 above.
- Ensure `watch_directories` is `true` and `rescan_interval_sec` is reasonable.

**Too many notifications:**
- Increase `cooldown_sec` (e.g., `7200` for 2 hours).
- Only one notification is sent per manga title per cooldown window, regardless of how many chapters are found.

**"failed to create app" warning at startup:**
- This means the admin credentials in config do not match the actual Gotify account.
- If you changed the admin password in the Gotify UI, update `admin_pass` in `config.json`.
- Or manually create a "Media Viewer" app in the Gotify UI and set `app_token` in config.

**First-time scan produces no notifications:**
- This is by design. The first scan establishes a baseline. Notifications only fire on subsequent rescans that find **new** chapters compared to the previous state.

---

## Environment Variables

- `MV_IMAGES_DIR` - Override images directory
- `MV_MANGA_DIR` - Override manga directory
- `MV_HMANGA_DIR` - Override h-manga directory
- `ALLOWED_ORIGINS` - Comma-separated CORS origins

## Other Troubleshooting

**Port already in use:** `.\media-server.exe -port=8080`

**Config not loading:** Check that `config.json` is in the same directory as the executable.

**Web UI not loading:** Ensure the `web/` directory is next to the executable.

**Stale frontend after update:** The service worker caches the shell. Bump the cache version in `sw.js` and the `app.js?v=` query string in `index.html` when deploying frontend changes, then reload the page.

**Service worker shows stale data:** Clear site data in browser DevTools (Application - Clear storage) or unregister the worker.

**Thumbnails not generating:** Check `thumbnail_dir` is writable and `max_thumbnail_mb` is large enough for your files. Use `POST /api/thumbnails/generate?section=manga` to start a background pass.

**High memory use in the browser:** Reduce `preload_desktop_default` / `preload_mobile_default` or disable `enable_preloading`.

**Tray icon missing / app seems frozen:** Use **Show Window** from the tray menu or run `.\media-server.exe -notray` to see live logs.


## Known Issues & Workarounds

| Issue | Workaround |
|-------|------------|
| **First-time scan produces no Gotify notifications** | By design. New series establish a baseline; only **new chapters in existing series** trigger notifications on later scans. |
| **Stale service-worker data after frontend update** | Bump `CACHE_NAME` in `sw.js` and the `app.js?v=` query string in `index.html`, then reload. Use DevTools to unregister/clear storage if needed. |
| **Thumbnails not generating for large files** | Raise `max_thumbnail_mb` or check that `thumbnail_dir` is writable. |
| **High browser memory use** | Reduce `preload_desktop_default` / `preload_mobile_default` or disable `enable_preloading`. |
| **Gotify 401 after deleting the Media Viewer app (or fresh external server)** | Self-healing in `server_url` external mode: the first send's 401/403 re-creates the app via the admin credentials and retries. Manual fix: POST `/api/gotify/reset`, or recreate the app and set `app_token` in `config.json`. |
| **Images "load and cache but display blank" in Docker** | Alpine ffmpeg's WebP pipe muxer emits placeholder RIFF/VP8 sizes Chromium rejects. Fixed in the current image (output written to a seekable temp file). Legacy workaround: none needed after updating. |
| **Read state / chapter count stale after reading** | Was caused by the service worker caching `/api/files` + `/api/folders`; these are now network-only. One hard refresh after updating drops the old cached lists. |

