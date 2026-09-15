package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type GotifyConfig struct {
	Enabled bool `json:"enabled"`
	// ServerURL, when set, points at an EXTERNAL gotify server (e.g. its own
	// container) â€” the notifier then never spawns a bundled server child.
	// Empty = spawn the bundled binary (Windows-only deployment; the bundled
	// binary is a Windows .exe and the official Linux release is glibc-linked,
	// so it can't run inside the musl media-viewer image).
	ServerURL       string `json:"server_url,omitempty"`
	BinaryPath      string `json:"binary_path"`
	Port            int    `json:"port"`
	AdminUser       string `json:"admin_user"`
	AdminPass       string `json:"admin_pass"`
	AppToken        string `json:"app_token"`
	DataDir         string `json:"data_dir"`
	CooldownSec     int    `json:"cooldown_sec"`
	DefaultPriority int    `json:"default_priority"`
}

type cooldownEntry struct {
	lastSent time.Time
}

type NewChapter struct {
	Series  string
	Chapter string
	Section string
}

// NewArchive represents a newly-detected compressed archive file inside an
// h-manga artist directory. The notification system uses this (instead of
// NewChapter) for h-manga so a message contains the artist name and only
// fires when an archive file is added â€” book subdirectories inside an
// artist's folder are intentionally NOT notified.
type NewArchive struct {
	Artist  string // artist directory name (= top-level h-manga subdirectory)
	Archive string // archive filename (e.g. "Artist - Title.cbz")
	Section string // section identifier (always SectionHManga in practice)
}

type GotifyNotifier struct {
	config   GotifyConfig
	appToken string
	cooldown sync.Map
	cmd      *exec.Cmd
	cmdMu    sync.Mutex // guards cmd (set in Start, read/signaled/niled in Stop)
	baseURL  string
	tracker  *NotificationTracker
	// ready is 1 once Start() has completed successfully (gotify process up,
	// health check passed, app token resolved). Notification paths check this
	// before doing anything so a background Start() (which can block up to ~30s
	// on the health check) doesn't cause NotifyBatch to hit a half-started
	// gotify. atomic.Bool would need import; uint32 with atomic store/load is
	// sufficient and keeps imports minimal.
	ready uint32
	// startDone tracks the in-flight background Start() so callers that need to
	// stop/replace the notifier (toggle-disable, shutdown) can wait for Start
	// to finish before touching n.cmd, avoiding a Start/Stop race on n.cmd.
	startDone sync.WaitGroup
}

// isReady reports whether Start() has completed successfully. Safe to call
// concurrently with Start(); returns false until the background startup finishes.
func (n *GotifyNotifier) isReady() bool {
	return atomic.LoadUint32(&n.ready) == 1
}

func NewGotifyNotifier(cfg GotifyConfig, tracker *NotificationTracker) *GotifyNotifier {
	port := cfg.Port
	if port == 0 {
		port = 8180
	}

	if tracker == nil {
		dataDir := cfg.DataDir
		if dataDir == "" {
			dataDir = "./gotify-data"
		}
		tracker = NewNotificationTracker(filepath.Join(dataDir, "notified-chapters.json"))
	}

	return &GotifyNotifier{
		config:  cfg,
		baseURL: fmt.Sprintf("http://localhost:%d", port),
		tracker: tracker,
	}
}

func (n *GotifyNotifier) IsEnabled() bool {
	return n.config.Enabled
}

func (n *GotifyNotifier) Start() error {
	if !n.config.Enabled {
		return nil
	}

	// External-server mode: never spawn a child. Health-check the configured
	// URL, then resolve the app token exactly like the bundled flow does.
	if n.config.ServerURL != "" {
		url := strings.TrimRight(n.config.ServerURL, "/")
		if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
			return fmt.Errorf("gotify server_url must start with http:// or https://: %s", url)
		}
		n.baseURL = url
		log.Printf("[GOTIFY] External server mode: %s", url)
		if err := n.healthCheck(); err != nil {
			return fmt.Errorf("external gotify server unreachable: %w", err)
		}
		if n.config.AppToken == "" {
			token, err := n.createApp()
			if err != nil {
				return fmt.Errorf("could not create app on external gotify (check admin_user/admin_pass): %w", err)
			}
			n.appToken = token
			n.config.AppToken = token
			log.Printf("[GOTIFY] Created app token on external server: %s â€” persisting to config", token)
			if currentConfig != nil {
				currentConfig.Gotify.AppToken = token
				if err := saveConfig(); err != nil {
					log.Printf("[GOTIFY] Warning: failed to save app token to config: %v â€” token is in-memory only", err)
				}
			}
		} else {
			// Configured token: no side-effect-free validation route exists
			// (app tokens only authenticate /message, and probing it creates
			// a message). Validate lazily â€” sendNotification detects a
			// 401/403 (stale token: fresh server or wiped data dir) and
			// re-creates the app via admin credentials once, in-band.
			n.appToken = n.config.AppToken
			log.Printf("[GOTIFY] Using configured app token")
		}
		atomic.StoreUint32(&n.ready, 1)
		return nil
	}

	binaryPath := n.config.BinaryPath
	if binaryPath == "" {
		binaryPath = "./tools/gotify-server.exe"
	}
	// Bundled-child mode is Windows-only: the official gotify release binary
	// for Linux is glibc-linked and cannot run inside the musl-based container
	// image, and there is no Linux download in the dependency list. On Linux,
	// deployments must use external-server mode (gotify.server_url) with a
	// separate gotify container. Fail here with an actionable message instead
	// of the confusing exec error later.
	if runtime.GOOS != "windows" {
		return fmt.Errorf(
			"bundled gotify child process is not supported on %s â€” set gotify.server_url in config to point at an external gotify server (e.g. its own container)",
			runtime.GOOS)
	}

	absPath, err := filepath.Abs(binaryPath)
	if err != nil {
		return fmt.Errorf("failed to resolve gotify binary path: %w", err)
	}

	if _, err := os.Stat(absPath); err != nil {
		return fmt.Errorf("gotify binary not found at %s: %w", absPath, err)
	}

	dataDir := n.config.DataDir
	if dataDir == "" {
		dataDir = "./gotify-data"
	}
	absDataDir, err := filepath.Abs(dataDir)
	if err != nil {
		return fmt.Errorf("failed to resolve gotify data dir: %w", err)
	}
	if err := os.MkdirAll(absDataDir, 0750); err != nil {
		return fmt.Errorf("failed to create gotify data dir: %w", err)
	}

	port := n.config.Port
	if port == 0 {
		port = 8180
	}

	adminUser := n.config.AdminUser
	if adminUser == "" {
		adminUser = "admin"
	}
	adminPass := n.config.AdminPass
	if adminPass == "" {
		adminPass = "admin"
	}

	dbPath := filepath.Join(absDataDir, "gotify.db")
	imagesDir := filepath.Join(absDataDir, "images")
	pluginsDir := filepath.Join(absDataDir, "plugins")
	os.MkdirAll(imagesDir, 0750)
	os.MkdirAll(pluginsDir, 0750)

	n.cmdMu.Lock()
	n.cmd = exec.Command(absPath)
	n.cmd.Stdout = os.Stdout
	n.cmd.Stderr = os.Stderr
	n.cmd.Dir = absDataDir
	n.cmd.Env = append(os.Environ(),
		fmt.Sprintf("GOTIFY_SERVER_PORT=%d", port),
		fmt.Sprintf("GOTIFY_DATABASE_DIALECT=sqlite3"),
		fmt.Sprintf("GOTIFY_DATABASE_CONNECTION=%s", filepath.ToSlash(dbPath)),
		fmt.Sprintf("GOTIFY_DEFAULTUSER_NAME=%s", adminUser),
		fmt.Sprintf("GOTIFY_DEFAULTUSER_PASS=%s", adminPass),
		fmt.Sprintf("GOTIFY_UPLOADEDIMAGESDIR=%s", filepath.ToSlash(imagesDir)),
		fmt.Sprintf("GOTIFY_PLUGINSDIR=%s", filepath.ToSlash(pluginsDir)),
		fmt.Sprintf("GOTIFY_REGISTRATION=false"),
	)
	// gotify-server.exe is a console-subsystem binary; when launched from this
	// GUI-subsystem app it would allocate its own console window with a taskbar
	// entry that doesn't hide with the parent's on-demand console. Hide it so
	// only the media-server's tray icon is the user-visible surface.
	configureChildWindow(n.cmd)

	log.Printf("[GOTIFY] Starting gotify server on port %d (data: %s)", port, absDataDir)
	if err := n.cmd.Start(); err != nil {
		n.cmdMu.Unlock()
		return fmt.Errorf("failed to start gotify: %w", err)
	}
	n.cmdMu.Unlock()

	log.Printf("[GOTIFY] Waiting for gotify to become ready...")
	if err := n.healthCheck(); err != nil {
		// Clean up the child WITHOUT calling Stop(): Stop() does startDone.Wait(),
		// but we're running inside Start() which is being awaited on startDone
		// by the background goroutine â€” calling Stop() here would self-deadlock.
		// stopChild() does the signal/kill/wait without the WaitGroup wait.
		n.stopChild()
		return fmt.Errorf("gotify health check failed: %w", err)
	}
	log.Printf("[GOTIFY] Server is ready")

	if n.config.AppToken == "" {
		token, err := n.createApp()
		if err != nil {
			log.Printf("[GOTIFY] Warning: failed to create app: %v", err)
		} else {
			n.appToken = token
			n.config.AppToken = token
			log.Printf("[GOTIFY] Created app token: %s â€” persisting to config", token)
			// Persist the new token to config so we don't create a duplicate
			// app on restart. If the save fails the token is still kept in
			// memory and used for this session; retry once after a short delay
			// in case the failure was transient (e.g. a brief disk error).
			// Without persistence, a restart would orphan the gotify app and
			// create a duplicate on the next Start().
			if currentConfig != nil {
				currentConfig.Gotify.AppToken = token
				if err := saveConfig(); err != nil {
					log.Printf("[GOTIFY] Warning: failed to save app token to config: %v â€” retrying once", err)
					time.Sleep(2 * time.Second)
					if err := saveConfig(); err != nil {
						log.Printf("[GOTIFY] Warning: app token save retry also failed: %v â€” token is in-memory only and will be lost on restart", err)
					}
				}
			}
		}
	} else {
		// Use the stored token directly. Validation is lazy: if the stored
		// token is stale/invalid (e.g. gotify data dir was wiped), the first
		// sendNotification will return 401/403 and log the failure per-request,
		// at which point the user can trigger a reset via the API. We don't
		// probe gotify here because there's no side-effect-free app-token-
		// authenticated GET route â€” /message is POST-only (it creates
		// messages), so probing it would either 404 (always "invalid") or
		// create a spurious notification. Eager validation that always fails
		// would recreate the app on EVERY restart, orphaning the old one and
		// accumulating duplicates â€” the exact bug the token persistence above
		// exists to prevent.
		n.appToken = n.config.AppToken
		log.Printf("[GOTIFY] Using configured app token")
	}

	// Mark as ready so notification paths (NotifyBatch etc.) stop short-circuiting
	// during startup. atomic so isReady() can be read concurrently without a lock.
	atomic.StoreUint32(&n.ready, 1)
	return nil
}

func (n *GotifyNotifier) UpdateConfig(cfg GotifyConfig) {
	n.config = cfg
	// External mode: honor a changed server_url.
	if cfg.ServerURL != "" {
		n.baseURL = strings.TrimRight(cfg.ServerURL, "/")
	} else if cfg.Port != 0 {
		n.baseURL = fmt.Sprintf("http://localhost:%d", cfg.Port)
	}
	// If the data dir changed, recompute the notified-chapters path and reload
	// the persisted set from the new location so notifications aren't re-sent
	// for chapters the old install already pushed.
	dataDir := cfg.DataDir
	if dataDir == "" {
		dataDir = "./gotify-data"
	}
	newPath := filepath.Join(dataDir, "notified-chapters.json")
	n.tracker.SetFilePath(newPath)
}

// stopChild signals and waits for the gotify child process to exit, then nils
// n.cmd. It does NOT wait on startDone (so it is safe to call from inside
// Start()'s failure path, which runs in the same goroutine that holds
// startDone â€” calling Stop() there would self-deadlock on startDone.Wait()).
func (n *GotifyNotifier) stopChild() {
	// External-server mode: nothing to stop (no child process was spawned).
	if n.config.ServerURL != "" {
		atomic.StoreUint32(&n.ready, 0)
		return
	}
	// Snapshot the cmd under the lock, then signal/wait outside the lock so a
	// long Wait() doesn't block concurrent readers of n.cmd.
	n.cmdMu.Lock()
	cmd := n.cmd
	n.cmdMu.Unlock()
	if cmd != nil && cmd.Process != nil {
		log.Printf("[GOTIFY] Stopping gotify server (PID %d)", cmd.Process.Pid)

		if runtime.GOOS == "windows" {
			// The gotify child is launched with CREATE_NO_WINDOW (no attached
			// console), so os.Interrupt (CTRL_C_EVENT) has no console to reach.
			// Kill() directly is reliable for a child we own; skip the 10s
			// graceful-wait path that only adds latency on Windows.
			cmd.Process.Kill()
		} else if err := cmd.Process.Signal(os.Interrupt); err != nil {
			log.Printf("[GOTIFY] Interrupt signal failed, killing process: %v", err)
			cmd.Process.Kill()
		}

		done := make(chan error, 1)
		go func() {
			done <- cmd.Wait()
		}()

		// On Windows there's no graceful path, so wait only briefly for the
		// process to exit after Kill(); on other platforms give it up to 10s
		// for the interrupt to take effect before force-killing.
		waitTimeout := 10 * time.Second
		if runtime.GOOS == "windows" {
			waitTimeout = 2 * time.Second
		}
		select {
		case <-done:
			log.Printf("[GOTIFY] Server stopped gracefully")
		case <-time.After(waitTimeout):
			log.Printf("[GOTIFY] Force killing server after timeout")
			cmd.Process.Kill()
		}
		n.cmdMu.Lock()
		n.cmd = nil
		n.cmdMu.Unlock()
	}
	// Mark not-ready so NotifyBatch short-circuits while stopped.
	atomic.StoreUint32(&n.ready, 0)
}

func (n *GotifyNotifier) Stop() {
	// If a background Start() is still in flight, wait for it to finish before
	// touching n.cmd â€” otherwise Stop() could signal a process Start() hasn't
	// spawned yet, or race on the n.cmd field itself. The WaitGroup is Add()ed
	// by the goroutine launcher in main() before Start() runs. NOTE: Start()'s
	// own failure path calls stopChild() (not Stop()) to avoid self-deadlock,
	// so this Wait() never waits on the calling goroutine itself.
	n.startDone.Wait()
	n.stopChild()
}

func (n *GotifyNotifier) healthCheck() error {
	client := &http.Client{Timeout: 2 * time.Second}
	maxAttempts := 30
	for i := 0; i < maxAttempts; i++ {
		resp, err := client.Get(n.baseURL + "/health")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		// External servers are usually already up â€” fail fast instead of
		// waiting the full 30s retry window when the URL is simply wrong.
		if n.config.ServerURL != "" && i == 0 && err != nil {
			break
		}
		time.Sleep(1 * time.Second)
	}
	return fmt.Errorf("gotify did not become ready at %s after %d attempts", n.baseURL, maxAttempts)
}

// validateAppToken is intentionally NOT implemented: gotify app tokens only
// authenticate the side-effectful /message route (POST), so there is no
// probe route. Stale tokens are detected by postMessage's 401/403 and
// recovered in-band by sendNotification's re-create-and-retry path.

func (n *GotifyNotifier) createApp() (string, error) {
	adminUser := n.config.AdminUser
	if adminUser == "" {
		adminUser = "admin"
	}
	adminPass := n.config.AdminPass
	if adminPass == "" {
		adminPass = "admin"
	}

	auth := base64.StdEncoding.EncodeToString([]byte(adminUser + ":" + adminPass))

	body := map[string]interface{}{
		"name":        "Media Viewer",
		"description": "Push notifications for new manga chapters",
	}
	jsonBody, err := json.Marshal(body)
	if err != nil {
		return "", fmt.Errorf("marshal app body: %w", err)
	}

	client := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequest("POST", n.baseURL+"/application", bytes.NewReader(jsonBody))
	if err != nil {
		return "", fmt.Errorf("create app request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Basic "+auth)

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("create app request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("create app failed (status %d): %s", resp.StatusCode, string(respBody))
	}

	var result map[string]interface{}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", fmt.Errorf("parse app response: %w", err)
	}

	token, ok := result["token"].(string)
	if !ok || token == "" {
		return "", fmt.Errorf("no token in app response: %v", result)
	}

	return token, nil
}

func (n *GotifyNotifier) Notify(manga, chapter, section string) bool {
	if !n.config.Enabled || n.appToken == "" {
		log.Printf("[GOTIFY] Notify skipped: enabled=%v, token=%v", n.config.Enabled, n.appToken != "")
		return false
	}

	key := fmt.Sprintf("%s|%s", section, manga)
	if last, ok := n.cooldown.Load(key); ok {
		entry := last.(cooldownEntry)
		cooldownDur := time.Duration(n.config.CooldownSec) * time.Second
		if n.config.CooldownSec == 0 {
			cooldownDur = 3600 * time.Second
		}
		if time.Since(entry.lastSent) < cooldownDur {
			debugLog("[GOTIFY] Skipping notification for %s (cooldown)", manga)
			return false
		}
	}

	title := fmt.Sprintf("New Chapter: %s", manga)
	message := fmt.Sprintf("Chapter \"%s\" has been added to %s", chapter, section)
	err := n.sendNotification(title, message, n.config.DefaultPriority)
	if err != nil {
		log.Printf("[GOTIFY] Notify failed for %s chapter %q: %v â€” not marking as notified, will retry", manga, chapter, err)
		return false
	}

	log.Printf("[GOTIFY] Notification pushed for %s (%s): 1 new chapter", manga, section)
	// Only mark as notified and store cooldown after confirmed success
	n.cooldown.Store(key, cooldownEntry{lastSent: time.Now()})
	n.tracker.Mark(section, manga, []string{chapter})
	return true
}

func (n *GotifyNotifier) NotifyBatch(manga, section string, chapters []string) bool {
	if !n.isReady() {
		// Start() is still running in the background (health check can take up
		// to ~30s). Skip rather than hitting a half-started gotify; the chapters
		// will be picked up on the next scan once ready.
		return false
	}
	if !n.config.Enabled || n.appToken == "" {
		log.Printf("[GOTIFY] NotifyBatch skipped: enabled=%v, token=%v", n.config.Enabled, n.appToken != "")
		return false
	}

	key := fmt.Sprintf("%s|%s", section, manga)
	if last, ok := n.cooldown.Load(key); ok {
		entry := last.(cooldownEntry)
		cooldownDur := time.Duration(n.config.CooldownSec) * time.Second
		if n.config.CooldownSec == 0 {
			cooldownDur = 3600 * time.Second
		}
		if time.Since(entry.lastSent) < cooldownDur {
			debugLog("[GOTIFY] Skipping batch notification for %s (cooldown)", manga)
			return false
		}
	}

	title := fmt.Sprintf("New Chapter: %s", manga)
	var message string
	if len(chapters) == 1 {
		message = fmt.Sprintf("Chapter \"%s\" has been added to %s", chapters[0], section)
	} else {
		message = fmt.Sprintf("%d new chapters added to %s", len(chapters), section)
	}

	err := n.sendNotification(title, message, n.config.DefaultPriority)
	if err != nil {
		log.Printf("[GOTIFY] NotifyBatch failed for %s (%d chapters): %v â€” not marking as notified, will retry", manga, len(chapters), err)
		return false
	}

	log.Printf("[GOTIFY] Notification pushed for %s (%s): %d new chapter(s)", manga, section, len(chapters))
	// Only mark as notified and store cooldown after confirmed success
	n.cooldown.Store(key, cooldownEntry{lastSent: time.Now()})
	n.tracker.Mark(section, manga, chapters)
	return true
}

// NotifyArchiveBatch sends a Gotify message for new archives added inside an
// h-manga artist directory. It mirrors NotifyBatch's semantics (one
// notification per artist per cooldown window, marking archives as
// notified only on success) but uses an archive-specific title/message and
// records via MarkArchive so archive notifications never collide with
// chapter notifications for the same artist.
//
// `artist` is the top-level h-manga subdirectory name; `archives` is the
// list of archive filenames that were newly detected inside it. The
// notification payload contains the artist name as requested.
func (n *GotifyNotifier) NotifyArchiveBatch(artist, section string, archives []string) bool {
	if !n.isReady() {
		return false
	}
	if !n.config.Enabled || n.appToken == "" {
		log.Printf("[GOTIFY] NotifyArchiveBatch skipped: enabled=%v, token=%v", n.config.Enabled, n.appToken != "")
		return false
	}
	if len(archives) == 0 {
		return false
	}

	key := fmt.Sprintf("%s|archive|%s", section, artist)
	if last, ok := n.cooldown.Load(key); ok {
		entry := last.(cooldownEntry)
		cooldownDur := time.Duration(n.config.CooldownSec) * time.Second
		if n.config.CooldownSec == 0 {
			cooldownDur = 3600 * time.Second
		}
		if time.Since(entry.lastSent) < cooldownDur {
			debugLog("[GOTIFY] Skipping archive notification for %s (cooldown)", artist)
			return false
		}
	}

	title := fmt.Sprintf("New Archive: %s", artist)
	var message string
	if len(archives) == 1 {
		message = fmt.Sprintf("Archive \"%s\" has been added to artist %q in %s", archives[0], artist, section)
	} else {
		message = fmt.Sprintf("%d new archives have been added to artist %q in %s", len(archives), artist, section)
	}

	err := n.sendNotification(title, message, n.config.DefaultPriority)
	if err != nil {
		log.Printf("[GOTIFY] NotifyArchiveBatch failed for artist=%s (%d archives): %v â€” not marking as notified, will retry", artist, len(archives), err)
		return false
	}

	log.Printf("[GOTIFY] Notification pushed for %s (%s): %d new archive(s)", artist, section, len(archives))
	n.cooldown.Store(key, cooldownEntry{lastSent: time.Now()})
	n.tracker.MarkArchive(section, artist, archives)
	return true
}

func (n *GotifyNotifier) sendNotification(title, message string, priority int) error {
	_, err := n.postMessage(title, message, priority)
	// 401/403 with a configured token means the token is stale (fresh server
	// or wiped data dir â€” report #4's silent-401 trap). Re-create the app via
	// admin credentials and retry once, in-band. Guarded by a flag so a
	// persistently broken setup can't loop: each send attempt re-creates at
	// most one app.
	if err == nil {
		return nil
	}
	if n.config.ServerURL != "" && n.adminRecreateEnabled() && isAuthFailure(err) {
		log.Printf("[GOTIFY] Token rejected by external server (%v) â€” recreating app via admin credentials", err)
		token, createErr := n.createApp()
		if createErr != nil {
			return fmt.Errorf("token invalid and app re-create failed: %w", createErr)
		}
		n.appToken = token
		n.config.AppToken = token
		log.Printf("[GOTIFY] Created replacement app token: %s â€” persisting to config", token)
		if currentConfig != nil {
			currentConfig.Gotify.AppToken = token
			if err := saveConfig(); err != nil {
				log.Printf("[GOTIFY] Warning: failed to save app token to config: %v â€” token is in-memory only and will be lost on restart", err)
			}
		}
		// Retry the original send once with the fresh token.
		_, err = n.postMessage(title, message, priority)
		if err != nil {
			return err
		}
		return nil
	}
	return err
}

// adminRecreateEnabled reports whether auto-recreation is possible: it needs
// admin credentials (always configured in practice) and external mode (for
// the bundled server the token lifecycle is already self-healing via reset).
func (n *GotifyNotifier) adminRecreateEnabled() bool {
	return n.config.ServerURL != "" && (n.config.AdminUser != "" || n.config.AdminPass != "")
}

// isAuthFailure reports whether a postMessage error is an auth rejection
// (stale/unknown app token) rather than a network/serialization error.
func isAuthFailure(err error) bool {
	return strings.Contains(err.Error(), "status 401") || strings.Contains(err.Error(), "status 403")
}

func (n *GotifyNotifier) postMessage(title, message string, priority int) (map[string]interface{}, error) {
	if n.appToken == "" {
		return nil, fmt.Errorf("no app token configured")
	}

	if priority == 0 {
		priority = 5
	}

	body := map[string]interface{}{
		"title":    title,
		"message":  message,
		"priority": priority,
		"extras": map[string]interface{}{
			"client::display": map[string]interface{}{
				"contentType": "text/markdown",
			},
		},
	}
	jsonBody, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal notification: %w", err)
	}

	client := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequest("POST", n.baseURL+"/message?token="+n.appToken, bytes.NewReader(jsonBody))
	if err != nil {
		return nil, fmt.Errorf("create notification request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	log.Printf("[GOTIFY] Sending notification: title=%q, priority=%d", title, priority)

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("send notification request: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("notification rejected (status %d): %s", resp.StatusCode, string(respBody))
	}

	// Parse Gotify response to confirm the push was accepted with the correct priority
	var result map[string]interface{}
	if err := json.Unmarshal(respBody, &result); err == nil {
		msgID, _ := result["id"].(float64)
		respPriority, _ := result["priority"].(float64)
		log.Printf("[GOTIFY] Push accepted: id=%.0f, title=%q, priority=%.0f (requested=%d)", msgID, title, respPriority, priority)
	} else {
		log.Printf("[GOTIFY] Push accepted (status 200) but could not parse response: %s", string(respBody))
	}

	return result, nil
}

func getMangaChapterSnapshot(db *InMemoryDB, section string) map[string][]string {
	db.mu.RLock()
	defer db.mu.RUnlock()

	snapshot := make(map[string][]string)
	if folders, ok := db.mangaFolders[section]; ok {
		for _, series := range folders {
			var chapters []string
			for _, ch := range series.Chapters {
				chapters = append(chapters, ch.Name)
			}
			snapshot[series.Name] = chapters
		}
	}
	return snapshot
}

func diffMangaChapters(prev map[string][]string, current []Series, section string) []NewChapter {
	var result []NewChapter

	currMap := make(map[string][]string)
	for _, series := range current {
		for _, ch := range series.Chapters {
			currMap[series.Name] = append(currMap[series.Name], ch.Name)
		}
	}

	for series, chapters := range currMap {
		prevChapters := prev[series]

		// If the series is entirely new (not in previous snapshot),
		// report all its chapters as new rather than skipping them.
		if len(prevChapters) == 0 {
			for _, ch := range chapters {
				result = append(result, NewChapter{
					Series:  series,
					Chapter: ch,
					Section: section,
				})
			}
			continue
		}

		prevSet := make(map[string]bool, len(prevChapters))
		for _, ch := range prevChapters {
			prevSet[ch] = true
		}

		for _, ch := range chapters {
			if !prevSet[ch] {
				result = append(result, NewChapter{
					Series:  series,
					Chapter: ch,
					Section: section,
				})
			}
		}
	}

	// Console-view visibility: log each discovered chapter/series. New
	// series (first appearance in the snapshot) count as a book discovery.
	if len(result) > 0 {
		newSeries := make(map[string]bool)
		for series := range currMap {
			if len(prev[series]) == 0 {
				newSeries[series] = true
			}
		}
		for series := range newSeries {
			log.Printf("[DISCOVERY] New series in %s: %s", section, series)
		}
		for _, nc := range result {
			if !newSeries[nc.Series] {
				log.Printf("[DISCOVERY] New chapter in %s: %s — %s", nc.Section, nc.Series, nc.Chapter)
			}
		}
	}

	return result
}

// getHMangaArchiveSnapshot walks the h-manga root directory and returns a
// per-artist list of archive filenames (.zip / .cbz / .rar / .7z) found
// directly inside each artist directory. Unlike getMangaChapterSnapshot,
// this does NOT consult the in-memory DB â€” the archive set is purely
// disk-derived because archive files are not represented as chapters in
// the h-manga DB (IsMediaFile returns false for them). Used to detect
// newly-added archives between consecutive scans.
//
// Returns nil (NOT an empty map) when the root directory cannot be read,
// so callers can tell "snapshot failed, don't diff" apart from "snapshot
// succeeded, zero archives exist". This prevents a transient network-drive
// hiccup at snapshot time from causing a mass-notification when the drive
// recovers and every archive reappears in the next snapshot.
//
// Safe to call concurrently with disk mutation: it does not hold any
// locks and only reads directory entries.
func getHMangaArchiveSnapshot(rootDir string) map[string][]string {
	entries, err := os.ReadDir(rootDir)
	if err != nil {
		log.Printf("[NOTIFY] h-manga archive snapshot failed for %s: %v", rootDir, err)
		return nil
	}
	snapshot := make(map[string][]string)
	for _, artistEntry := range entries {
		if !artistEntry.IsDir() {
			continue
		}
		artistPath := filepath.Join(rootDir, artistEntry.Name())
		subEntries, err := os.ReadDir(artistPath)
		if err != nil {
			// Skip unreadable artist dirs (permission, transient failure)
			// but keep the rest of the snapshot â€” partial visibility is
			// better than no visibility, and the diff will not flag any
			// archives from the skipped dir as new because it isn't in
			// `current` for that artist.
			continue
		}
		var archives []string
		for _, sub := range subEntries {
			if sub.IsDir() {
				continue
			}
			if IsArchiveFile(sub.Name()) {
				archives = append(archives, sub.Name())
			}
		}
		if len(archives) > 0 {
			snapshot[artistEntry.Name()] = archives
		}
	}
	return snapshot
}

// diffHMangaArchives returns archives that exist in `current` but not in
// `prev`. The result is grouped by artist so each notification can carry
// the artist name. Order within each artist is unspecified (callers sort
// for display if they care).
//
// If either snapshot is nil (the snapshot read failed â€” typically a
// transient network-drive hiccup), returns nil so the caller can skip
// the diff entirely. Treating a failed read as "everything is new" would
// produce a mass notification when the drive recovers.
func diffHMangaArchives(prev, current map[string][]string) []NewArchive {
	if prev == nil || current == nil {
		return nil
	}
	var result []NewArchive
	for artist, currentArchives := range current {
		prevSet := make(map[string]bool, len(prev[artist]))
		for _, a := range prev[artist] {
			prevSet[a] = true
		}
		for _, a := range currentArchives {
			if !prevSet[a] {
				result = append(result, NewArchive{
					Artist:  artist,
					Archive: a,
					Section: SectionHManga,
				})
			}
		}
	}

	// Console-view visibility: log each newly-discovered archive (book).
	for _, na := range result {
		log.Printf("[DISCOVERY] New archive (book) for artist %s: %s", na.Artist, na.Archive)
	}
	return result
}

// notifyNewArchives groups new archives by artist and dispatches a
// NotifyArchiveBatch call to each enabled notifier. Shared between the
// verify (rescan and no-rescan paths) and rescan/API scan sites to keep
// the grouping+dispatch logic in one place.
func notifyNewArchives(newArchives []NewArchive, section string, gn *GotifyNotifier, dn *DiscordNotifier) {
	if len(newArchives) == 0 {
		return
	}
	grouped := make(map[string][]string)
	for _, na := range newArchives {
		grouped[na.Artist] = append(grouped[na.Artist], na.Archive)
	}
	for artist, archives := range grouped {
		if gn != nil {
			gn.NotifyArchiveBatch(artist, section, archives)
		}
		if dn != nil {
			dn.NotifyArchiveBatch(artist, section, archives)
		}
	}
}

// IsChapterNotified checks if a specific chapter has already had a notification pushed.
// Deprecated: use tracker.IsNotified directly; kept as a thin wrapper for existing callers.
func (n *GotifyNotifier) IsChapterNotified(section, series, chapter string) bool {
	return n.tracker.IsNotified(section, series, chapter)
}

// GetPendingNotifications returns all chapters in the DB that have NOT yet had notifications pushed.
// Deprecated: use tracker.GetPending directly; kept as a thin wrapper for existing callers.
func (n *GotifyNotifier) GetPendingNotifications(db *InMemoryDB) []NewChapter {
	return n.tracker.GetPending(db)
}

// PushPendingNotifications sends notifications for all unnotified chapters
// (manga section) and unnotified archives (h-manga section). Returns the
// total number of batch notifications successfully sent across both kinds.
//
// The h-manga root directory is looked up from cfg.Directories[SectionHManga];
// if it's not configured, no archives are processed.
func (n *GotifyNotifier) PushPendingNotifications(db *InMemoryDB, cfg *Config) int {
	sent := 0

	pending := n.tracker.GetPending(db)
	if len(pending) > 0 {
		// Group by series+section for batch notifications
		grouped := make(map[string][]string) // key: "section|series" -> chapters
		sectionForSeries := make(map[string]string)
		for _, nc := range pending {
			key := fmt.Sprintf("%s|%s", nc.Section, nc.Series)
			grouped[key] = append(grouped[key], nc.Chapter)
			sectionForSeries[key] = nc.Section
		}

		failed := 0
		for key, chapters := range grouped {
			section := sectionForSeries[key]
			parts := strings.SplitN(key, "|", 2)
			series := ""
			if len(parts) == 2 {
				series = parts[1]
			}

			if n.NotifyBatch(series, section, chapters) {
				sent++
			} else {
				failed++
				log.Printf("[GOTIFY] PushPendingNotifications: failed to send for series=%s section=%s (%d chapters) â€” will remain in pending list", series, section, len(chapters))
			}
		}
		if failed > 0 {
			log.Printf("[GOTIFY] PushPendingNotifications: chapters: %d succeeded, %d failed", len(grouped)-failed, failed)
		}
	}

	if cfg != nil {
		hMangaDir := cfg.Directories[SectionHManga]
		if hMangaDir != "" {
			archivePending := n.tracker.GetPendingArchives(hMangaDir)
			if len(archivePending) > 0 {
				grouped := make(map[string][]string)
				sectionForArtist := make(map[string]string)
				for _, na := range archivePending {
					key := na.Artist
					grouped[key] = append(grouped[key], na.Archive)
					sectionForArtist[key] = na.Section
				}
				failed := 0
				for artist, archives := range grouped {
					section := sectionForArtist[artist]
					if n.NotifyArchiveBatch(artist, section, archives) {
						sent++
					} else {
						failed++
						log.Printf("[GOTIFY] PushPendingNotifications: failed to send for artist=%s section=%s (%d archives) â€” will remain in pending list", artist, section, len(archives))
					}
				}
				if failed > 0 {
					log.Printf("[GOTIFY] PushPendingNotifications: archives: %d succeeded, %d failed", len(grouped)-failed, failed)
				}
			}
		}
	}

	return sent
}
