package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const discordAPIBase = "https://discord.com/api/v10"

// DiscordConfig holds the user-visible settings for the Discord notifier.
type DiscordConfig struct {
	Enabled     bool   `json:"enabled"`
	BotToken    string `json:"bot_token"`
	RecipientID string `json:"recipient_id"`
	CooldownSec int    `json:"cooldown_sec"`
}

// rateLimitBucket tracks Discord's per-bucket rate-limit reset time.
type rateLimitBucket struct {
	resetAt time.Time
}

// DiscordNotifier pushes new manga chapter notifications to a Discord user DM
// using a bot token. It is lazy-starting: Start() runs on first send or toggle.
type DiscordNotifier struct {
	config      DiscordConfig
	client      *http.Client
	tracker     *NotificationTracker
	cooldown    sync.Map
	rateBuckets map[string]*rateLimitBucket
	rateMu      sync.Mutex
	channelID   string
	ready       uint32
	// startMu guards lazy initialization so concurrent NotifyBatch calls don't
	// both attempt to validate the token and create a DM channel.
	startMu sync.Mutex
	// sendMu serializes all Discord HTTP requests per notifier so the bucket
	// tracking logic doesn't race with itself.
	sendMu sync.Mutex
}

// NewDiscordNotifier creates a notifier. It does not perform network calls.
func NewDiscordNotifier(cfg DiscordConfig, tracker *NotificationTracker) *DiscordNotifier {
	return &DiscordNotifier{
		config:      cfg,
		client:      &http.Client{Timeout: 15 * time.Second},
		tracker:     tracker,
		rateBuckets: make(map[string]*rateLimitBucket),
	}
}

// IsEnabled reports whether Discord notifications are enabled in config.
func (n *DiscordNotifier) IsEnabled() bool {
	return n.config.Enabled
}

// isReady reports whether Start() has validated the token and cached a DM channel.
func (n *DiscordNotifier) isReady() bool {
	return atomic.LoadUint32(&n.ready) == 1
}

// Start validates the bot token and resolves the DM channel for RecipientID.
// It is safe to call multiple times; once ready, subsequent calls are no-ops.
func (n *DiscordNotifier) Start() error {
	n.startMu.Lock()
	defer n.startMu.Unlock()
	if n.isReady() {
		return nil
	}
	if !n.config.Enabled || n.config.BotToken == "" {
		return fmt.Errorf("discord not configured")
	}

	// Validate token by fetching the bot's own user info.
	var me struct {
		ID string `json:"id"`
	}
	if err := n.discordRequest("GET", "/users/@me", nil, &me); err != nil {
		return fmt.Errorf("token validation failed: %w", err)
	}
	log.Printf("[DISCORD] Token validated (bot user %s)", me.ID)

	// Resolve or create the DM channel for the recipient.
	if n.config.RecipientID == "" {
		return fmt.Errorf("recipient_id not configured")
	}
	channel, err := n.createDMChannel(n.config.RecipientID)
	if err != nil {
		return fmt.Errorf("failed to resolve DM channel: %w", err)
	}
	n.channelID = channel
	atomic.StoreUint32(&n.ready, 1)
	log.Printf("[DISCORD] DM channel cached: %s", channel)
	return nil
}

// Stop clears the ready state so the notifier short-circuits until restarted.
func (n *DiscordNotifier) Stop() {
	atomic.StoreUint32(&n.ready, 0)
}

// UpdateConfig updates the config and clears ready state if the recipient changed.
func (n *DiscordNotifier) UpdateConfig(cfg DiscordConfig) {
	recipientChanged := cfg.RecipientID != "" && cfg.RecipientID != n.config.RecipientID
	n.config = cfg
	if recipientChanged {
		atomic.StoreUint32(&n.ready, 0)
		n.channelID = ""
	}
}

// NotifyBatch sends a Discord message for new chapters. It mirrors Gotify's
// semantics: one notification per series per cooldown window, marking chapters
// as notified only on success.
func (n *DiscordNotifier) NotifyBatch(series, section string, chapters []string) bool {
	if !n.isReady() {
		if err := n.Start(); err != nil {
			log.Printf("[DISCORD] NotifyBatch start failed: %v", err)
			return false
		}
	}
	if !n.config.Enabled || n.config.BotToken == "" {
		log.Printf("[DISCORD] NotifyBatch skipped: enabled=%v, token=%v", n.config.Enabled, n.config.BotToken != "")
		return false
	}

	key := fmt.Sprintf("%s|%s", section, series)
	cooldownDur := time.Duration(n.config.CooldownSec) * time.Second
	if n.config.CooldownSec == 0 {
		cooldownDur = 3600 * time.Second
	}
	if last, ok := n.cooldown.Load(key); ok {
		entry := last.(cooldownEntry)
		if time.Since(entry.lastSent) < cooldownDur {
			debugLog("[DISCORD] Skipping batch notification for %s (cooldown)", series)
			return false
		}
	}

	if err := n.sendMessage(series, section, chapters); err != nil {
		log.Printf("[DISCORD] NotifyBatch failed for %s (%d chapters): %v — not marking as notified", series, len(chapters), err)
		return false
	}

	log.Printf("[DISCORD] Notification pushed for %s (%s): %d new chapter(s)", series, section, len(chapters))
	n.cooldown.Store(key, cooldownEntry{lastSent: time.Now()})
	n.tracker.Mark(section, series, chapters)
	return true
}

// NotifyArchiveBatch sends a Discord message for new archives added inside
// an h-manga artist directory. Mirrors NotifyBatch semantics but uses an
// archive-specific message containing the artist name and records via
// MarkArchive so it never collides with chapter tracking for the same
// artist. See GotifyNotifier.NotifyArchiveBatch for full context.
func (n *DiscordNotifier) NotifyArchiveBatch(artist, section string, archives []string) bool {
	if !n.isReady() {
		if err := n.Start(); err != nil {
			log.Printf("[DISCORD] NotifyArchiveBatch start failed: %v", err)
			return false
		}
	}
	if !n.config.Enabled || n.config.BotToken == "" {
		log.Printf("[DISCORD] NotifyArchiveBatch skipped: enabled=%v, token=%v", n.config.Enabled, n.config.BotToken != "")
		return false
	}
	if len(archives) == 0 {
		return false
	}

	key := fmt.Sprintf("%s|archive|%s", section, artist)
	cooldownDur := time.Duration(n.config.CooldownSec) * time.Second
	if n.config.CooldownSec == 0 {
		cooldownDur = 3600 * time.Second
	}
	if last, ok := n.cooldown.Load(key); ok {
		entry := last.(cooldownEntry)
		if time.Since(entry.lastSent) < cooldownDur {
			debugLog("[DISCORD] Skipping archive notification for %s (cooldown)", artist)
			return false
		}
	}

	if err := n.sendArchiveMessage(artist, section, archives); err != nil {
		log.Printf("[DISCORD] NotifyArchiveBatch failed for artist=%s (%d archives): %v — not marking as notified", artist, len(archives), err)
		return false
	}

	log.Printf("[DISCORD] Notification pushed for %s (%s): %d new archive(s)", artist, section, len(archives))
	n.cooldown.Store(key, cooldownEntry{lastSent: time.Now()})
	n.tracker.MarkArchive(section, artist, archives)
	return true
}

// SendTest sends a one-off test DM to verify the configuration.
func (n *DiscordNotifier) SendTest() error {
	if !n.config.Enabled {
		return fmt.Errorf("discord not enabled")
	}
	if err := n.Start(); err != nil {
		return err
	}
	return n.sendRawMessage("Test notification", "Your Discord bot notifier is configured correctly.")
}

// createDMChannel calls POST /users/@me/channels and returns the channel ID.
func (n *DiscordNotifier) createDMChannel(recipientID string) (string, error) {
	body := map[string]string{"recipient_id": recipientID}
	var result struct {
		ID string `json:"id"`
	}
	if err := n.discordRequest("POST", "/users/@me/channels", body, &result); err != nil {
		return "", err
	}
	if result.ID == "" {
		return "", fmt.Errorf("discord did not return a channel ID")
	}
	return result.ID, nil
}

// sendMessage builds and sends a Discord embed for the new chapters.
func (n *DiscordNotifier) sendMessage(series, section string, chapters []string) error {
	title := fmt.Sprintf("New Chapter: %s", series)
	var description string
	if len(chapters) == 1 {
		description = fmt.Sprintf("Chapter %q has been added to %s", chapters[0], section)
	} else {
		description = fmt.Sprintf("%d new chapters have been added to %s", len(chapters), section)
	}

	embed := map[string]interface{}{
		"title":       title,
		"description": description,
		"color":       0x5865F2,
		"fields": []map[string]interface{}{
			{
				"name":  "Chapters",
				"value": strings.Join(chapters, "\n"),
			},
		},
	}
	payload := map[string]interface{}{"embeds": []interface{}{embed}}
	return n.discordRequest("POST", "/channels/"+n.channelID+"/messages", payload, nil)
}

// sendArchiveMessage builds and sends a Discord embed for newly-detected
// archive files inside an h-manga artist directory. The message leads with
// the artist name and lists each archive filename under "Archives".
func (n *DiscordNotifier) sendArchiveMessage(artist, section string, archives []string) error {
	title := fmt.Sprintf("New Archive: %s", artist)
	var description string
	if len(archives) == 1 {
		description = fmt.Sprintf("Archive %q has been added to artist %q in %s", archives[0], artist, section)
	} else {
		description = fmt.Sprintf("%d new archives have been added to artist %q in %s", len(archives), artist, section)
	}

	embed := map[string]interface{}{
		"title":       title,
		"description": description,
		"color":       0x5865F2,
		"fields": []map[string]interface{}{
			{
				"name":  "Archives",
				"value": strings.Join(archives, "\n"),
			},
			{
				"name":  "Artist",
				"value": artist,
			},
		},
	}
	payload := map[string]interface{}{"embeds": []interface{}{embed}}
	return n.discordRequest("POST", "/channels/"+n.channelID+"/messages", payload, nil)
}

// sendRawMessage sends a plain text message to the cached DM channel.
func (n *DiscordNotifier) sendRawMessage(title, message string) error {
	payload := map[string]interface{}{
		"content": fmt.Sprintf("**%s**\n%s", title, message),
	}
	return n.discordRequest("POST", "/channels/"+n.channelID+"/messages", payload, nil)
}

// discordRequest executes a single Discord API request, honoring rate limits.
// It serializes all requests through sendMu to avoid racing the same bucket.
func (n *DiscordNotifier) discordRequest(method, path string, body interface{}, result interface{}) error {
	n.sendMu.Lock()
	defer n.sendMu.Unlock()

	url := discordAPIBase + path
	var bodyData []byte
	var err error
	if body != nil {
		bodyData, err = json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal request body: %w", err)
		}
	}

	makeReq := func() (*http.Request, error) {
		var bodyReader io.Reader
		if len(bodyData) > 0 {
			bodyReader = bytes.NewReader(bodyData)
		}
		req, err := http.NewRequest(method, url, bodyReader)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bot "+n.config.BotToken)
		if len(bodyData) > 0 {
			req.Header.Set("Content-Type", "application/json")
		}
		return req, nil
	}

	req, err := makeReq()
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}

	resp, err := n.client.Do(req)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	// On 429, honor Retry-After and retry once.
	if resp.StatusCode == http.StatusTooManyRequests {
		retryAfter := parseRetryAfter(resp.Header.Get("Retry-After"))
		var bodyRetry struct {
			RetryAfter float64 `json:"retry_after"`
		}
		respBody, _ := io.ReadAll(resp.Body)
		_ = json.Unmarshal(respBody, &bodyRetry)
		if bodyRetry.RetryAfter > 0 {
			retryAfter = time.Duration(bodyRetry.RetryAfter*1000) * time.Millisecond
		}
		if retryAfter <= 0 {
			retryAfter = time.Second
		}
		log.Printf("[DISCORD] Rate limited on %s, sleeping %v", path, retryAfter)
		time.Sleep(retryAfter)

		req, err = makeReq()
		if err != nil {
			return fmt.Errorf("create retry request: %w", err)
		}
		resp, err = n.client.Do(req)
		if err != nil {
			return fmt.Errorf("retry request failed: %w", err)
		}
		defer resp.Body.Close()
	}

	n.updateRateLimit(resp.Header)

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return fmt.Errorf("discord %s %s returned %d: %s", method, path, resp.StatusCode, string(respBody))
	}

	if result != nil && len(respBody) > 0 {
		if err := json.Unmarshal(respBody, result); err != nil {
			return fmt.Errorf("parse response: %w", err)
		}
	}
	return nil
}

// updateRateLimit parses X-RateLimit headers and sleeps if the bucket is exhausted.
func (n *DiscordNotifier) updateRateLimit(header http.Header) {
	bucketID := header.Get("X-RateLimit-Bucket")
	if bucketID == "" {
		return
	}

	resetAfter := parseRetryAfter(header.Get("X-RateLimit-Reset-After"))
	resetEpoch := int64(0)
	if v := header.Get("X-RateLimit-Reset"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			resetEpoch = int64(f)
		}
	}

	// If Remaining is 0, wait until the bucket resets.
	remaining := -1
	if v := header.Get("X-RateLimit-Remaining"); v != "" {
		remaining, _ = strconv.Atoi(v)
	}

	n.rateMu.Lock()
	defer n.rateMu.Unlock()

	bucket := n.rateBuckets[bucketID]
	if bucket == nil {
		bucket = &rateLimitBucket{}
		n.rateBuckets[bucketID] = bucket
	}

	var resetAt time.Time
	if resetAfter > 0 {
		resetAt = time.Now().Add(resetAfter)
	} else if resetEpoch > 0 {
		resetAt = time.Unix(resetEpoch, 0)
	}
	if !resetAt.IsZero() && resetAt.After(bucket.resetAt) {
		bucket.resetAt = resetAt
	}

	if remaining == 0 && !resetAt.IsZero() {
		delay := time.Until(resetAt)
		if delay > 0 {
			log.Printf("[DISCORD] Bucket %s exhausted, sleeping %v", bucketID, delay)
			time.Sleep(delay)
		}
	}
}

func parseRetryAfter(v string) time.Duration {
	if v == "" {
		return 0
	}
	if f, err := strconv.ParseFloat(v, 64); err == nil {
		return time.Duration(f*1000) * time.Millisecond
	}
	return 0
}

// PushPendingNotifications sends notifications for all unnotified chapters
// (manga section) and unnotified archives (h-manga section). Returns the
// total number of batch notifications successfully sent across both kinds.
// See GotifyNotifier.PushPendingNotifications for full context.
func (n *DiscordNotifier) PushPendingNotifications(db *InMemoryDB, cfg *Config) int {
	sent := 0

	pending := n.tracker.GetPending(db)
	if len(pending) > 0 {
		grouped := make(map[string][]string)
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
				log.Printf("[DISCORD] PushPendingNotifications: failed to send for series=%s section=%s (%d chapters)", series, section, len(chapters))
			}
		}
		if failed > 0 {
			log.Printf("[DISCORD] PushPendingNotifications: chapters: %d succeeded, %d failed", sent, failed)
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
						log.Printf("[DISCORD] PushPendingNotifications: failed to send for artist=%s section=%s (%d archives)", artist, section, len(archives))
					}
				}
				if failed > 0 {
					log.Printf("[DISCORD] PushPendingNotifications: archives: %d succeeded, %d failed", sent, failed)
				}
			}
		}
	}

	return sent
}
