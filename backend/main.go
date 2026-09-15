// Media Viewer Server - Complete Go Implementation with Security & Performance
// Build (console app):      go build -ldflags "-s -w" -o media-server.exe .
// Build (Windows tray mode): go build -ldflags "-s -w -H windowsgui" -o media-server.exe .
//   The -H windowsgui build has no console window â€” the app runs in the system
//   tray, logs to server.log, and offers an on-demand console via tray > Show Window.
// This creates a single executable with all dependencies included

package main

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"image"
	"image/jpeg"
	"io"
	"log"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/disintegration/imaging"
	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
	_ "golang.org/x/image/webp"
)

// ============================================================
// Configuration
// ============================================================

type Config struct {
	Port             int               `json:"port"`
	Directories      map[string]string `json:"directories"`
	ThumbnailDir     string            `json:"thumbnail_dir"`
	WatchDirectories bool              `json:"watch_directories"`
	Mode             string            `json:"mode"` // "debug" or "release"

	// Security & Performance settings
	RateLimitRPS               int      `json:"rate_limit_rps"`                 // Requests per second (default: 100)
	RateLimitBurst             int      `json:"rate_limit_burst"`               // Burst size (default: 20)
	MaxConcurrent              int      `json:"max_concurrent"`                 // Max concurrent connections (default: 50)
	ConnLimitAcquireTimeoutSec int      `json:"conn_limit_acquire_timeout_sec"` // Seconds to wait for a connection slot before 503 (default: 5)
	ThumbnailWorkers           int      `json:"thumbnail_workers"`              // Thumbnail worker count (default: 5)
	ThumbScaleFactor           float64  `json:"thumb_scale_factor"`             // Thumbnail scale factor (default: 0.5 = 50%)
	ThumbMaxWidth              int      `json:"thumb_max_width"`                // Thumbnail max width in pixels (default: 400)
	ThumbMaxHeight             int      `json:"thumb_max_height"`               // Thumbnail max height in pixels (default: 300)
	RequestTimeoutSec          int      `json:"request_timeout_sec"`            // Request timeout in seconds (default: 30)
	MaxRequestMB               int      `json:"max_request_mb"`                 // Max request body size in MB (default: 10)
	TrustedProxies             []string `json:"trusted_proxies"`                // IPs of trusted reverse proxies (default: ["127.0.0.1", "::1"])
	MaxThumbnailMB             int      `json:"max_thumbnail_mb"`               // Max file size for thumbnail generation in MB (default: 100)

	// Write rate timeout settings
	// If a client is downloading slower than MinWriteRateBytes/sec, the connection
	// is terminated. This prevents stalled connections from consuming resources
	// indefinitely while allowing fast media streams to run as long as needed.
	WriteTimeoutSec   int `json:"write_timeout_sec"`    // Initial write deadline in seconds (default: 30); extended while data flows fast enough
	MinWriteRateBytes int `json:"min_write_rate_bytes"` // Minimum bytes/sec to keep connection alive (default: 512000 = 500 KB/s)

	// Responsive Image settings
	ResponsiveBuckets []int `json:"responsive_buckets"` // Size buckets for responsive images (default: [320, 640, 768, 1024, 1280, 1536, 1920, 2048])
	EnablePreloading  bool  `json:"enable_preloading"`  // Enable prev/next image preloading for mobile (default: true)

	// Preload settings
	PreloadDesktopDefault int `json:"preload_desktop_default"` // Default preload count for desktop (default: 3)
	PreloadMobileDefault  int `json:"preload_mobile_default"`  // Default preload count for mobile (default: 1)
	MaxPreloadCount       int `json:"max_preload_count"`       // Maximum allowed preload count (default: 5)

	// Rescan settings
	RescanIntervalSec int `json:"rescan_interval_sec"` // Seconds between automatic directory rescans (default: 600 = 10 min)

	// Index settings
	IndexPath      string `json:"index_path"`       // Directory for index files (default: "./index")
	IndexOnStartup string `json:"index_on_startup"` // "always" | "fallback" | "never" (default: "always")
	// Gotify push notifications
	Gotify GotifyConfig `json:"gotify"`

	// Discord bot notifications
	Discord DiscordConfig `json:"discord"`
}

// rawDiscordConfig mirrors DiscordConfig with a *bool for Enabled.
type rawDiscordConfig struct {
	Enabled     *bool  `json:"enabled"`
	BotToken    string `json:"bot_token"`
	RecipientID string `json:"recipient_id"`
	CooldownSec int    `json:"cooldown_sec"`
}

// mediaCacheControl is the cache policy for /api/media and /api/thumbnail
// responses. Files are addressed by mutable relative paths, so responses must
// never be marked `immutable` â€” an in-place edit (same path, new bytes) would
// otherwise stay invisible to clients for the full max-age window. The client
// versions URLs with ?v=<mtime> for files whose mtime it knows (see
// getThumbnailUrl/getMediaUrl); without `immutable`, a browser may also
// revalidate after max-age expires.
const mediaCacheControl = "public, max-age=604800"

// setFileVersionHeaders stamps X-File-Mtime (unix seconds) on a response so
// clients can version future requests for the same path. Never fails â€”
// versioning is best-effort and stat errors are ignored on purpose.
func setFileVersionHeaders(c *gin.Context, path string) {
	if info, err := os.Stat(path); err == nil {
		c.Header("X-File-Mtime", strconv.FormatInt(info.ModTime().Unix(), 10))
	}
}

// Section constants
const (
	SectionImages = "images"
	SectionManga  = "manga"
	SectionHManga = "h-manga"
)

var currentConfig *Config
var gotifyNotifier *GotifyNotifier
var discordNotifier *DiscordNotifier
var configFilePath string
var configMu sync.Mutex

// debugMode is a lock-free snapshot of currentConfig.Mode == "debug", flipped
// once at startup after config load. debugLog reads it on hot serving paths
// (thumbnail cache-hits, static-file NoRoute) where acquiring configMu would
// serialize traffic that was previously lock-free. Mode is never mutated
// after startup, so an atomic bool is sufficient and avoids the race the
// raw `currentConfig.Mode` read would otherwise have with config writers.
var debugMode atomic.Bool

// getCurrentConfig returns a snapshot of currentConfig under configMu. Use
// this for any read of currentConfig that may race with a write (e.g. an API
// toggle mutating the config). The returned pointer is the live *Config; do
// not mutate it without holding configMu â€” treat it as read-only.
func getCurrentConfig() *Config {
	configMu.Lock()
	defer configMu.Unlock()
	return currentConfig
}

// getGotifyNotifier returns a snapshot of the current gotifyNotifier pointer
// under configMu. Callers must nil-check the returned value before using it.
// A concurrent toggle/reset may nil the package var after this returns, but
// the caller's snapshot stays valid until the notifier's Stop() completes.
func getGotifyNotifier() *GotifyNotifier {
	configMu.Lock()
	defer configMu.Unlock()
	return gotifyNotifier
}

// getDiscordNotifier returns a snapshot of the current discordNotifier pointer
// under configMu. Callers must nil-check the returned value before using it.
func getDiscordNotifier() *DiscordNotifier {
	configMu.Lock()
	defer configMu.Unlock()
	return discordNotifier
}

// trayLogFile is the file that receives logs in tray mode (GUI-subsystem build).
// Set in main() when useTrayEarly is true. showConsoleWindow (windows) uses it
// to build an io.MultiWriter so opening the on-demand console does not stop
// server.log from receiving new entries.
var trayLogFile *os.File

// trayLogWriter is the buffered, thread-safe log writer used in tray mode.
// It wraps a bufio.Writer whose underlying writer is swappable at runtime, so
// showConsoleWindow can attach the on-demand console (via SetUnderlying with an
// io.MultiWriter) WITHOUT losing the buffer or racing with concurrent writers.
// All Write/Flush/SetUnderlying access is serialized by mu, making it safe for
// the periodic flush goroutine, log.Printf callers, and gin's logger to use
// concurrently. Set in main() when useTrayEarly is true.
var trayLogWriter *syncBufferedWriter

// syncBufferedWriter is a concurrency-safe buffered writer. bufio.Writer is not
// safe for concurrent use; this wrapper serializes Write, Flush, and
// SetUnderlying under a mutex. The underlying writer can be swapped at runtime
// (used to attach an on-demand console while preserving the buffer).
type syncBufferedWriter struct {
	mu  sync.Mutex
	buf *bufio.Writer
	w   io.Writer // current underlying writer (buf wraps this)
}

func newSyncBufferedWriter(w io.Writer, size int) *syncBufferedWriter {
	return &syncBufferedWriter{buf: bufio.NewWriterSize(w, size), w: w}
}

// Write implements io.Writer. Serialized with Flush/SetUnderlying by mu.
func (s *syncBufferedWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

// Flush flushes the buffer to the underlying writer. Thread-safe.
func (s *syncBufferedWriter) Flush() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Flush()
}

// SetUnderlying swaps the underlying writer and flushes any buffered data to
// the OLD writer first (so nothing is lost). The new writer receives all
// subsequent writes. Thread-safe; used by showConsoleWindow to attach the
// on-demand console via an io.MultiWriter while keeping the buffer in front.
func (s *syncBufferedWriter) SetUnderlying(w io.Writer) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.buf.Flush(); err != nil {
		return err
	}
	s.w = w
	s.buf.Reset(w)
	return nil
}

// exeRelativeBaseDir caches the executable directory for path resolution.
// Computed once at startup to avoid repeated os.Executable calls.
var exeRelativeBaseDir string

func init() {
	// Compute exe directory once at init time
	if execPath, err := os.Executable(); err == nil {
		exeRelativeBaseDir = filepath.Dir(execPath)
	} else {
		// Fallback: use current working directory
		if cwd, err := os.Getwd(); err == nil {
			exeRelativeBaseDir = cwd
		} else {
			exeRelativeBaseDir = "."
		}
	}
}

func saveConfig() error {
	if configFilePath == "" {
		return fmt.Errorf("config file path not set")
	}
	// Hold the lock through the entire copy + path conversion + marshal so a
	// concurrent writer to currentConfig (e.g. an API toggle mutating
	// Directories) cannot race on the shallow-copied Directories map during
	// serialization. The file I/O is performed outside the lock using the
	// local jsonData slice, which is safe since no other goroutine can reach it.
	configMu.Lock()
	// Create a copy of the config to save, converting exe-relative absolute
	// paths back to relative paths for human-readable config files.
	saveCfg := *currentConfig
	// Deep-copy the Directories map so the copy can't be mutated mid-marshal by a
	// concurrent writer holding currentConfig while we unlock for the file write.
	saveCfg.Directories = make(map[string]string, len(currentConfig.Directories))
	for k, v := range currentConfig.Directories {
		saveCfg.Directories[k] = v
	}
	saveCfg.ThumbnailDir = pathToExeRelative(saveCfg.ThumbnailDir)
	saveCfg.IndexPath = pathToExeRelative(saveCfg.IndexPath)
	saveCfg.Gotify.DataDir = pathToExeRelative(saveCfg.Gotify.DataDir)
	saveCfg.Gotify.BinaryPath = pathToExeRelative(saveCfg.Gotify.BinaryPath)

	jsonData, err := json.MarshalIndent(saveCfg, "", "  ")
	// Discord has no file paths; nothing extra to convert.
	configMu.Unlock()
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	tmpPath := configFilePath + ".tmp"
	if err := os.WriteFile(tmpPath, jsonData, 0644); err != nil {
		return fmt.Errorf("write temp config: %w", err)
	}
	if err := os.Rename(tmpPath, configFilePath); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("rename temp config: %w", err)
	}
	log.Printf("[CONFIG] Saved config to %s", configFilePath)
	return nil
}

func debugLog(format string, v ...interface{}) {
	if debugMode.Load() {
		log.Printf("[DEBUG] "+format, v...)
	}
}

// scanTimeoutDuration returns the per-scan context timeout. Scanning a large
// media library over CIFS/NFS can take far longer than the 5-minute default,
// so deployments can raise it via MV_SCAN_TIMEOUT_SEC (seconds, 0 = default).
// All image-scan call sites (startup, fallback, rescan, incremental, API)
// share this value.
func scanTimeoutDuration() time.Duration {
	if v := os.Getenv("MV_SCAN_TIMEOUT_SEC"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return time.Duration(n) * time.Second
		}
	}
	return 5 * time.Minute
}

func DefaultConfig() *Config {
	return &Config{
		Port:                       3000,
		Directories:                make(map[string]string),
		ThumbnailDir:               "./.thumbnails",
		WatchDirectories:           true,
		Mode:                       "release",
		RateLimitRPS:               100,
		RateLimitBurst:             20,
		MaxConcurrent:              50,
		ConnLimitAcquireTimeoutSec: 5,
		ThumbnailWorkers:           5,
		ThumbScaleFactor:           0.5,
		ThumbMaxWidth:              400,
		ThumbMaxHeight:             300,
		RequestTimeoutSec:          30,
		MaxRequestMB:               10,
		TrustedProxies:             []string{"127.0.0.1", "::1"},
		MaxThumbnailMB:             100,
		WriteTimeoutSec:            30,
		MinWriteRateBytes:          512000, // 500 KB/s â€” connections slower than this are terminated
		ResponsiveBuckets:          []int{320, 640, 768, 1024, 1280, 1536, 1920, 2048},
		EnablePreloading:           true,
		PreloadDesktopDefault:      3,
		PreloadMobileDefault:       1,
		MaxPreloadCount:            5,
		RescanIntervalSec:          600, // 10 minutes
		IndexPath:                  "./index",
		IndexOnStartup:             "always",
		Gotify: GotifyConfig{
			Enabled:         false,
			Port:            8180,
			AdminUser:       "admin",
			AdminPass:       "admin",
			DataDir:         "./gotify-data",
			CooldownSec:     3600,
			DefaultPriority: 5,
		},
		Discord: DiscordConfig{
			Enabled:     false,
			CooldownSec: 3600,
		},
	}
}

// calculateBucket finds the appropriate size bucket for responsive images
// Uses the minimum of width/height to handle orientation changes.
// When height is 0 (width-only mode, used for vertical strip readers),
// bucket is based solely on width â€” no height constraint.
func calculateBucket(width, height int, buckets []int) int {
	if len(buckets) == 0 {
		buckets = []int{320, 640, 768, 1024, 1280, 1536, 1920, 2048}
	}

	// Width-only mode: constrain by width alone (manga/h-manga vertical strip readers)
	if height <= 0 {
		for _, bucket := range buckets {
			if bucket >= width {
				return bucket
			}
		}
		return buckets[len(buckets)-1]
	}

	// Use minimum dimension to handle orientation changes gracefully
	minDim := width
	if height < minDim {
		minDim = height
	}

	// Find the smallest bucket that fits
	for _, bucket := range buckets {
		if bucket >= minDim {
			return bucket
		}
	}

	// If larger than all buckets, return largest
	return buckets[len(buckets)-1]
}

// isMobileUserAgent checks if the request is from a mobile device
func isMobileUserAgent(userAgent string) bool {
	ua := strings.ToLower(userAgent)
	mobilePatterns := []string{
		"mobile", "android", "iphone", "ipad", "ipod", "windows phone",
		"blackberry", "webos", "silk", "opera mini", "opera mobi",
	}
	for _, pattern := range mobilePatterns {
		if strings.Contains(ua, pattern) {
			return true
		}
	}
	return false
}

// addPreloadHeaders adds X-Preload-Previous, X-Preload-Next, and Link headers for mobile preloading
// Uses actual next/prev paths passed from frontend based on current sorting
// endpoint should be "/api/thumbnail" or "/api/media" depending on which endpoint was called
// Deprecated: Use addPreloadHeadersWithCount instead
func addPreloadHeaders(c *gin.Context, endpoint, currentPath, section string, bucket, currentIndex, total int, nextPath, prevPath string, width, height int, dpr float64) {
	// Convert single paths to arrays for backward compatibility
	var nextPaths, prevPaths []string
	if nextPath != "" {
		nextPaths = []string{nextPath}
	}
	if prevPath != "" {
		prevPaths = []string{prevPath}
	}
	// Default to 1 preload for backward compatibility
	addPreloadHeadersWithCount(c, endpoint, currentPath, section, bucket, currentIndex, total, 1, nextPaths, prevPaths, width, height, dpr)
}

// addPreloadHeadersWithCount adds X-Preload-Previous, X-Preload-Next, and Link headers with configurable preload count
// preloadCount determines how many images ahead/behind to preload (1 = just immediate neighbors)
// nextPaths and prevPaths are arrays of paths for each preload index ahead/behind
func addPreloadHeadersWithCount(c *gin.Context, endpoint, currentPath, section string, bucket, currentIndex, total, preloadCount int, nextPaths, prevPaths []string, width, height int, dpr float64) {
	debugLog("[PRELOAD] Called: endpoint=%s, currentPath=%s, index=%d, total=%d, preloadCount=%d, nextPaths=%d, prevPaths=%d",
		endpoint, currentPath, currentIndex, total, preloadCount, len(nextPaths), len(prevPaths))

	if total <= 0 || currentIndex < 0 || currentIndex >= total {
		debugLog("[PRELOAD] Skipped: total=%d, currentIndex=%d", total, currentIndex)
		return
	}

	// Ensure preloadCount is at least 1
	if preloadCount < 1 {
		preloadCount = 1
	}

	// Use the same endpoint as the request to ensure preloaded URLs match
	sectionParam := ""
	if section != "" && section != SectionImages {
		sectionParam = "&section=" + section
	}
	// Note: We don't include bucket parameter in preload URLs because
	// the client doesn't know the bucket when making the actual request
	// The server will calculate it from width/height parameters
	_ = bucket // bucket is calculated server-side, don't include in URL

	// Build arrays of preload URLs for multiple preloads
	var prevURLs []string
	var nextURLs []string

	// Calculate previous indices (up to preloadCount)
	for i := 1; i <= preloadCount; i++ {
		if currentIndex-i >= 0 {
			// Get the appropriate path from prevPaths array (index i-1 for the 1st previous, etc.)
			prevPath := ""
			if i-1 < len(prevPaths) {
				prevPath = prevPaths[i-1]
			}
			if prevPath != "" {
				prevIndex := currentIndex - i
				// Build URL matching what the client will actually request
				// Width-only mode (height == 0): omit height for vertical strip readers
				dimsStr := fmt.Sprintf("width=%d&dpr=%.1f", width, dpr)
				if height > 0 {
					dimsStr = fmt.Sprintf("width=%d&height=%d&dpr=%.1f", width, height, dpr)
				}
				prevURL := fmt.Sprintf("%s/%s?%s&nocache=1&index=%d&total=%d%s",
					endpoint, encodePath(prevPath), dimsStr, prevIndex, total, sectionParam)
				prevURLs = append(prevURLs, prevURL)
			}
		}
	}

	// Calculate next indices (up to preloadCount)
	for i := 1; i <= preloadCount; i++ {
		if currentIndex+i < total {
			// Get the appropriate path from nextPaths array (index i-1 for the 1st next, etc.)
			nextPath := ""
			if i-1 < len(nextPaths) {
				nextPath = nextPaths[i-1]
			}
			if nextPath != "" {
				nextIndex := currentIndex + i
				// Build URL matching what the client will actually request
				// Width-only mode (height == 0): omit height for vertical strip readers
				dimsStr := fmt.Sprintf("width=%d&dpr=%.1f", width, dpr)
				if height > 0 {
					dimsStr = fmt.Sprintf("width=%d&height=%d&dpr=%.1f", width, height, dpr)
				}
				nextURL := fmt.Sprintf("%s/%s?%s&nocache=1&index=%d&total=%d%s",
					endpoint, encodePath(nextPath), dimsStr, nextIndex, total, sectionParam)
				nextURLs = append(nextURLs, nextURL)
			}
		}
	}

	// Set headers - only the immediate previous/next for single URL headers
	// (maintaining backward compatibility with existing clients)
	if len(prevURLs) > 0 {
		c.Header("X-Preload-Previous", prevURLs[0])
		log.Printf("[PRELOAD DEBUG] Set X-Preload-Previous: %s", prevURLs[0])
	}

	if len(nextURLs) > 0 {
		c.Header("X-Preload-Next", nextURLs[0])
		// Don't send Link header - the client handles preloading via X-Preload headers
		log.Printf("[PRELOAD DEBUG] Set X-Preload-Next: %s", nextURLs[0])
	}

	// Set additional headers for multiple preloads if count > 1
	if preloadCount > 1 {
		// Join all URLs with commas for multi-value headers
		if len(prevURLs) > 1 {
			c.Header("X-Preload-Previous-All", strings.Join(prevURLs, ","))
		}
		if len(nextURLs) > 1 {
			c.Header("X-Preload-Next-All", strings.Join(nextURLs, ","))
		}
	}
}

// calculateSequentialPath generates the path for a sequential index
// This is a simple implementation that assumes numeric filenames
// In a real scenario, this would need access to the file list
// Returns empty string if the filename doesn't follow a clear sequential pattern
func calculateSequentialPath(currentPath string, targetIndex int) string {
	// Extract directory and filename
	dir := filepath.Dir(currentPath)
	base := filepath.Base(currentPath)
	ext := filepath.Ext(base)
	nameWithoutExt := strings.TrimSuffix(base, ext)

	// Try to find a numeric pattern in the filename
	// Common patterns: "001.jpg", "page_01.jpg", "img-1.jpg", etc.
	var targetName string

	// Check if the name ends with a number
	numRegex := regexp.MustCompile(`(\d+)$`)
	if matches := numRegex.FindStringSubmatch(nameWithoutExt); len(matches) > 1 {
		// Found a number at the end
		numStr := matches[1]
		numLen := len(numStr)

		// Only use this pattern if the number is reasonable (2-4 digits)
		// This filters out random IDs like "HFAZWmjX0AAiPcs" where "cs" is not a page number
		if numLen >= 2 && numLen <= 4 {
			prefix := nameWithoutExt[:len(nameWithoutExt)-numLen]
			targetName = fmt.Sprintf("%s%0*d%s", prefix, numLen, targetIndex+1, ext)
		} else {
			// Number is too long or too short - probably not a page number
			return ""
		}
	} else {
		// No number found - can't calculate sequential path
		return ""
	}

	if dir == "." {
		return targetName
	}
	return filepath.Join(dir, targetName)
}

// encodePath properly encodes a path for use in URLs
func encodePath(path string) string {
	return strings.ReplaceAll(url.PathEscape(path), "%2F", "/")
}

// ============================================================
// Data Models
// ============================================================

type MediaFile struct {
	Path     string   `json:"path"`
	Name     string   `json:"name"`
	Type     string   `json:"type"`
	Mtime    int64    `json:"mtime"`
	Number   *int     `json:"number,omitempty"`
	Tags     []string `json:"tags"`
	Section  string   `json:"section,omitempty"`
	FileSize int64    `json:"file_size,omitempty"`
	FileHash string   `json:"file_hash,omitempty"`
}

type Series struct {
	Name      string    `json:"name"`
	UpdatedAt int64     `json:"updated_at,omitempty"`
	Chapters  []Chapter `json:"chapters"`
	Section   string    `json:"_section,omitempty"`
}

type Chapter struct {
	Name   string     `json:"name"`
	Images []ImageRef `json:"images"`
}

type ImageRef struct {
	Name  string `json:"name"`
	Path  string `json:"path"`
	Mtime int64  `json:"mtime,omitempty"`
}

type SeriesInfo struct {
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Author      string   `json:"author"`
	Status      string   `json:"status"`
	Genres      []string `json:"genres"`
	Source      string   `json:"source"`
	CoverURL    string   `json:"coverUrl"`
	CoverPath   string   `json:"coverPath"`
}

// rawSeriesInfo mirrors the first-version schema of series-info.json.
type rawSeriesInfo struct {
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Author      string   `json:"author"`
	Status      string   `json:"status"`
	Genres      []string `json:"genres"`
	CoverURL    string   `json:"coverUrl"`
	Source      string   `json:"source"`
}

// resolveSeriesCover returns a relative cover path or external URL for the
// series splash page. It prefers local files over external URLs to avoid CORS.
func resolveSeriesCover(seriesDir string, info rawSeriesInfo) string {
	coverExts := []string{".webp", ".jpg", ".jpeg", ".png"}
	for _, ext := range coverExts {
		candidate := filepath.Join(seriesDir, "cover"+ext)
		if _, err := os.Stat(candidate); err == nil {
			return "cover" + ext
		}
	}

	// Flat series fallback: first image file directly in the series directory.
	entries, _ := os.ReadDir(seriesDir)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if IsMediaFile(entry.Name()) {
			return entry.Name()
		}
	}

	if info.CoverURL != "" {
		return info.CoverURL
	}
	return ""
}

// readSeriesInfo loads series-info.json from the series directory and
// normalizes it into a SeriesInfo response. Missing fields become empty
// strings; the title falls back to the folder name.
func readSeriesInfo(seriesDir, seriesName string) SeriesInfo {
	info := SeriesInfo{Title: seriesName}

	data, err := os.ReadFile(filepath.Join(seriesDir, "series-info.json"))
	if err != nil {
		info.CoverPath = resolveSeriesCover(seriesDir, rawSeriesInfo{})
		return info
	}

	var raw rawSeriesInfo
	if err := json.Unmarshal(data, &raw); err != nil {
		info.CoverPath = resolveSeriesCover(seriesDir, raw)
		return info
	}

	if raw.Title != "" {
		info.Title = raw.Title
	}
	info.Description = raw.Description
	info.Author = raw.Author
	info.Status = raw.Status
	info.Source = raw.Source
	info.CoverURL = raw.CoverURL
	info.Genres = raw.Genres
	info.CoverPath = resolveSeriesCover(seriesDir, raw)
	return info
}

const IndexVersion = 3

type IndexFile struct {
	Version   int    `json:"version"`
	Section   string `json:"section"`
	RootDir   string `json:"root_dir"`
	ScannedAt string `json:"scanned_at"`
}

type IndexImagesFile struct {
	IndexFile
	Files []MediaFile `json:"files"`
}

type IndexMangaFile struct {
	IndexFile
	Series []Series `json:"series"`
}

// migrateTask tracks a section that needs v1â†’v2 hash migration.
// Migration is deferred to a background goroutine so the server can
// start accepting connections immediately instead of blocking for
// minutes while hashing thousands of files.
type migrateTask struct {
	section string
	dir     string
}

// pendingMigrations collects sections that need v1â†’v2 hash migration.
// Populated during index loading; consumed by runPendingMigrations
// after the HTTP server starts listening.
var pendingMigrations []migrateTask

func indexFilePath(cfg *Config, section string) string {
	dir := cfg.IndexPath
	if dir == "" {
		dir = "./index"
	}
	return filepath.Join(dir, "index-"+section+".json")
}

// archiveIndex moves the current index file for a section to an archive subdirectory
// with a timestamp suffix. This preserves the previous index in case the new scan
// produces unexpected results.
func archiveIndex(cfg *Config, section string) error {
	currentPath := indexFilePath(cfg, section)

	// Check if current index exists
	if _, err := os.Stat(currentPath); err != nil {
		if os.IsNotExist(err) {
			debugLog("[INDEX] No existing index to archive for %s", section)
			return nil
		}
		return fmt.Errorf("stat index %s: %w", section, err)
	}

	// Create archive directory
	dir := cfg.IndexPath
	if dir == "" {
		dir = "./index"
	}
	archiveDir := filepath.Join(dir, "archive")
	if err := os.MkdirAll(archiveDir, 0755); err != nil {
		return fmt.Errorf("create archive directory: %w", err)
	}

	// Move with timestamp suffix
	timestamp := time.Now().Format("20060102-150405")
	archivePath := filepath.Join(archiveDir, "index-"+section+"-"+timestamp+".json")

	if err := os.Rename(currentPath, archivePath); err != nil {
		// On some systems (e.g., cross-device), Rename may fail; fall back to copy+delete
		debugLog("[INDEX] Rename failed for archive (%v), falling back to copy", err)
		data, readErr := os.ReadFile(currentPath)
		if readErr != nil {
			return fmt.Errorf("read index for archive %s: %w", section, readErr)
		}
		if writeErr := os.WriteFile(archivePath, data, 0644); writeErr != nil {
			return fmt.Errorf("write archive index %s: %w", section, writeErr)
		}
		if delErr := os.Remove(currentPath); delErr != nil {
			log.Printf("[INDEX] Warning: could not remove original after archive copy: %v", delErr)
		}
	}

	log.Printf("[INDEX] Archived %s index to %s", section, archivePath)

	// Clean up old archives: keep only the 5 most recent per section
	cleanOldArchives(archiveDir, section, 5)

	return nil
}

// cleanOldArchives removes old archive files for a section, keeping only the
// most recent maxKeep files. Archives are sorted by modification time.
func cleanOldArchives(archiveDir, section string, maxKeep int) {
	prefix := "index-" + section + "-"
	entries, err := os.ReadDir(archiveDir)
	if err != nil {
		return
	}

	// Collect archive files for this section
	var archives []os.DirEntry
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasPrefix(entry.Name(), prefix) {
			archives = append(archives, entry)
		}
	}

	// If we have more than maxKeep, remove the oldest
	if len(archives) <= maxKeep {
		return
	}

	// Sort by name (timestamp-based names sort chronologically)
	sort.Slice(archives, func(i, j int) bool {
		return archives[i].Name() < archives[j].Name()
	})

	// Remove oldest files (all except the last maxKeep)
	for i := 0; i < len(archives)-maxKeep; i++ {
		oldPath := filepath.Join(archiveDir, archives[i].Name())
		if err := os.Remove(oldPath); err != nil {
			log.Printf("[INDEX] Warning: could not remove old archive %s: %v", oldPath, err)
		} else {
			log.Printf("[INDEX] Removed old archive: %s", oldPath)
		}
	}
}

func saveIndex(cfg *Config, section string, data interface{}) error {
	path := indexFilePath(cfg, section)
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("create index directory: %w", err)
	}

	jsonData, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("marshal index: %w", err)
	}

	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, jsonData, 0644); err != nil {
		return fmt.Errorf("write temp index: %w", err)
	}

	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("rename temp index: %w", err)
	}

	debugLog("[INDEX] Saved index for %s to %s (%d bytes)", section, path, len(jsonData))
	return nil
}

func loadIndex(cfg *Config, section string, data interface{}) error {
	path := indexFilePath(cfg, section)
	jsonData, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read index %s: %w", section, err)
	}

	if err := json.Unmarshal(jsonData, data); err != nil {
		return fmt.Errorf("unmarshal index %s: %w", section, err)
	}

	debugLog("[INDEX] Loaded index for %s from %s (%d bytes)", section, path, len(jsonData))
	return nil
}

func saveImagesIndex(cfg *Config, section, rootDir string, files []MediaFile) error {
	idx := IndexImagesFile{
		IndexFile: IndexFile{
			Version:   IndexVersion,
			Section:   section,
			RootDir:   rootDir,
			ScannedAt: time.Now().Format(time.RFC3339),
		},
		Files: files,
	}
	return saveIndex(cfg, section, idx)
}

func saveMangaIndex(cfg *Config, section, rootDir string, series []Series) error {
	idx := IndexMangaFile{
		IndexFile: IndexFile{
			Version:   IndexVersion,
			Section:   section,
			RootDir:   rootDir,
			ScannedAt: time.Now().Format(time.RFC3339),
		},
		Series: series,
	}
	return saveIndex(cfg, section, idx)
}

func loadImagesIndex(cfg *Config, section string) (*IndexImagesFile, error) {
	var idx IndexImagesFile
	if err := loadIndex(cfg, section, &idx); err != nil {
		return nil, err
	}
	if idx.Version != IndexVersion && idx.Version != 1 && idx.Version != 2 {
		return nil, fmt.Errorf("index version mismatch: got %d, want %d", idx.Version, IndexVersion)
	}
	if idx.Version == 2 {
		log.Printf("[INDEX] Loading v2 index for %s â€” will upgrade to v%d on next save", section, IndexVersion)
	}
	if idx.Section != section {
		return nil, fmt.Errorf("index section mismatch: got %s, want %s", idx.Section, section)
	}
	return &idx, nil
}

func loadMangaIndex(cfg *Config, section string) (*IndexMangaFile, error) {
	var idx IndexMangaFile
	if err := loadIndex(cfg, section, &idx); err != nil {
		return nil, err
	}
	if idx.Version != IndexVersion && idx.Version != 1 && idx.Version != 2 {
		return nil, fmt.Errorf("index version mismatch: got %d, want %d", idx.Version, IndexVersion)
	}
	if idx.Version == 2 {
		log.Printf("[INDEX] Loading v2 index for %s â€” will upgrade to v%d on next save", section, IndexVersion)
	}
	if idx.Section != section {
		return nil, fmt.Errorf("index section mismatch: got %s, want %s", idx.Section, section)
	}
	return &idx, nil
}

func loadIndexIntoDB(cfg *Config, db *InMemoryDB, section string) (bool, error) {
	dir := cfg.Directories[section]
	if dir == "" {
		return false, fmt.Errorf("no directory configured for section %s", section)
	}

	switch section {
	case SectionImages:
		idx, err := loadImagesIndex(cfg, section)
		if err != nil {
			return false, err
		}
		if idx.RootDir != dir {
			return false, fmt.Errorf("index root dir mismatch: index has %s, config has %s", idx.RootDir, dir)
		}
		for i := range idx.Files {
			idx.Files[i].Section = section
			db.SaveFile(idx.Files[i])
		}
		// Migrate v1 indices in the background so the server can start
		// listening immediately. Computing SHA-256 hashes for thousands of
		// files on a network drive can take minutes â€” blocking startup
		// prevents any connections during that entire time.
		if idx.Version == 1 {
			pendingMigrations = append(pendingMigrations, migrateTask{section: section, dir: dir})
			log.Printf("[MIGRATE] Section %q has v1 index â€” migration will run in background", section)
		}
		return true, nil

	case SectionManga, SectionHManga:
		idx, err := loadMangaIndex(cfg, section)
		if err != nil {
			return false, err
		}
		if idx.RootDir != dir {
			return false, fmt.Errorf("index root dir mismatch: index has %s, config has %s", idx.RootDir, dir)
		}
		db.SaveMangaFolders(section, idx.Series)
		return true, nil
	}

	return false, fmt.Errorf("unknown section: %s", section)
}

func verifySection(cfg *Config, db *InMemoryDB, section string) {
	dir := cfg.Directories[section]
	if dir == "" {
		return
	}

	if !db.TrySetScanning(section) {
		log.Printf("[VERIFY] Skipping %s â€” scan already in progress", section)
		return
	}
	defer db.SetScanning(section, false)

	log.Printf("[VERIFY] Starting verification for %s", section)
	start := time.Now()

	var changed bool
	switch section {
	case SectionImages:
		changed = verifyImagesSection(cfg, db, section, dir)
	case SectionManga:
		changed = verifyMangaSection(cfg, db, section, dir)
	case SectionHManga:
		changed = verifyMangaSection(cfg, db, section, dir)
	}

	if changed {
		if err := saveSectionIndex(cfg, db, section); err != nil {
			log.Printf("[VERIFY] Failed to save index for %s: %v", section, err)
		}
	} else {
		log.Printf("[VERIFY] No changes for %s â€” skipping index save", section)
	}
	log.Printf("[VERIFY] Completed %s in %v", section, time.Since(start))
}

func verifyImagesSection(cfg *Config, db *InMemoryDB, section, dir string) bool {
	// Snapshot of current file paths in the DB. This is intentionally taken once
	// under RLock. Files deleted during the loop below are still present in this
	// snapshot, but that's harmless â€” they won't exist on disk so they won't be
	// re-added by the filepath.Walk pass. Modified/added files written via SaveFile
	// are safe because SaveFile acquires its own lock.
	db.mu.RLock()
	existingPaths := make(map[string]bool)
	if paths, ok := db.filesBySection[section]; ok {
		for p := range paths {
			existingPaths[p] = true
		}
	}
	db.mu.RUnlock()

	var modified int

	// For rename detection: when a file is missing from disk, we need its
	// stored FileHash + parent directory to match it against a newly-detected
	// file at a different path. Without this, a rename is treated as a
	// delete + add pair, which loses tag associations, notification state,
	// and forces a thumbnail cache regeneration (the cache is keyed by
	// sha256 of the relative path).
	type removedCandidate struct {
		relPath   string
		file      MediaFile
		parentDir string // relative parent directory ("foo/bar")
		fullSize  int64
	}
	var removed []removedCandidate

	for relPath := range existingPaths {
		fullPath := filepath.Join(dir, relPath)
		info, err := os.Stat(fullPath)
		if err != nil {
			if os.IsNotExist(err) {
				db.mu.RLock()
				f, exists := db.files[relPath]
				db.mu.RUnlock()
				if exists {
					removed = append(removed, removedCandidate{
						relPath:   relPath,
						file:      f,
						parentDir: filepath.ToSlash(filepath.Dir(relPath)),
						fullSize:  f.FileSize,
					})
				} else {
					// DB entry vanished between the RLock snapshots above and
					// here (concurrent API edit). Still need to remove the
					// path from filesBySection so it doesn't linger.
					removed = append(removed, removedCandidate{relPath: relPath})
				}
			}
			continue
		}

		db.mu.RLock()
		f, exists := db.files[relPath]
		db.mu.RUnlock()
		if exists && f.Mtime != info.ModTime().Unix() {
			pathTags := pathTagsFromDir(relPath)
			folder := filepath.Base(filepath.Dir(fullPath))
			num, tags := ParseFilename(info.Name(), folder)
			tags = append(pathTags, tags...)
			if f.Type == "video" {
				tags = FilterTags([]string{folder})
			}
			f.Mtime = info.ModTime().Unix()
			f.FileSize = info.Size()
			f.Number = num
			f.Tags = dedupeTags(tags)
			f.FileHash = computeFileHash(fullPath)
			db.SaveFile(f)
			modified++
		}
	}

	// Walk disk to find files not in the DB. We defer the actual insert to
	// after rename matching so a renamed file's existing DB entry can be
	// updated in place rather than deleted+reinserted.
	ctx, cancel := context.WithTimeout(context.Background(), scanTimeoutDuration())
	defer cancel()
	type addedCandidate struct {
		rel       string
		fullPath  string
		info      os.FileInfo
		fileHash  string
		parentDir string
	}
	var added []addedCandidate
	walkErr := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if err != nil || info.IsDir() || !IsMediaFile(info.Name()) {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			log.Printf("[VERIFY] Cannot compute relative path for %s: %v", path, err)
			return nil
		}
		rel = filepath.ToSlash(rel)
		if !existingPaths[rel] {
			added = append(added, addedCandidate{
				rel:       rel,
				fullPath:  path,
				info:      info,
				parentDir: filepath.ToSlash(filepath.Dir(rel)),
			})
		}
		return nil
	})
	if walkErr != nil {
		log.Printf("[VERIFY] Error walking directory %s: %v", dir, walkErr)
	}

	// Compute hashes for added files so we can match renames. Hashes are
	// already computed by SaveFile for brand-new files; doing it here too is
	// a duplicate cost on the non-rename path but keeps the match logic
	// straightforward and avoids a second walk.
	for i := range added {
		added[i].fileHash = computeFileHash(added[i].fullPath)
	}

	// Match renames: for each added file, look for a removed file in the
	// same parent directory with the same content hash AND size. Match 1:1
	// (a removed entry is consumed by the first added file that matches it)
	// so two files with identical content in the same directory still each
	// get a clean delete+add â€” the second add won't false-match the first's
	// consumed removed slot.
	renamed := 0
	consumedRemoved := make(map[int]bool)
	consumedAdded := make(map[int]bool)
	// renamePairs maps removed index -> added index so the apply phase can
	// look up the new path/name without re-matching.
	renamePairs := make(map[int]int)
	for ai, a := range added {
		for ri, r := range removed {
			if consumedRemoved[ri] {
				continue
			}
			if r.file.FileHash == "" || r.file.FileHash != a.fileHash {
				continue
			}
			if r.fullSize != a.info.Size() {
				continue
			}
			if r.parentDir != a.parentDir {
				continue
			}
			consumedRemoved[ri] = true
			consumedAdded[ai] = true
			renamePairs[ri] = ai
			renamed++
			break
		}
	}

	// Apply all changes atomically under one lock so the tag/hash/file maps
	// are consistent at every point a concurrent reader can observe them.
	trulyRemoved := 0
	trulyAdded := 0
	db.mu.Lock()
	for ri, r := range removed {
		if consumedRemoved[ri] {
			// Rename: update the existing entry's Path/Name in place. The
			// DB key changes from old relPath to new relPath; secondary
			// indexes are remapped. FileHash is preserved (the match
			// precondition proved the content is unchanged).
			ai, ok := renamePairs[ri]
			if !ok {
				// Shouldn't happen â€” consumedRemoved[ri] was only set when
				// we matched an added entry. Fall back to treating as removed.
				consumedRemoved[ri] = false
				continue
			}
			a := added[ai]

			oldEntry := r.file
			newEntry := oldEntry
			newEntry.Path = a.rel
			newEntry.Name = a.info.Name()
			newEntry.Mtime = a.info.ModTime().Unix()
			newEntry.FileSize = a.info.Size()
			// Recompute path-derived fields for the new location. Tags from
			// the filename/folder may differ from the old location; the
			// simplest correct behavior is to rebuild them from the new
			// path the same way a fresh insert would.
			newPathTags := pathTagsFromDir(a.rel)
			newFolder := filepath.Base(filepath.Dir(a.rel))
			newNum, newTags := ParseFilename(a.info.Name(), newFolder)
			newTags = append(newPathTags, newTags...)
			if newEntry.Type == "video" {
				newTags = FilterTags([]string{newFolder})
			}
			newEntry.Number = newNum
			newEntry.Tags = dedupeTags(newTags)

			// Remap the secondary indexes: remove old keys, insert new.
			//
			// We intentionally bypass db.saveFileLocked here. That helper
			// inserts at f.Path unconditionally and treats it as either a
			// new insert or an in-place update of the SAME path â€” neither
			// works for a rename where the key moves from oldEntry.Path to
			// a.rel. The match precondition (same FileHash + size + parent
			// dir) guarantees oldEntry still owns the hash mapping if one
			// exists, so the hash-index remap below is safe.
			db.removeFileTagAssociations(oldEntry.Path, oldEntry.Tags)
			db.files[a.rel] = newEntry
			delete(db.files, oldEntry.Path)
			if db.filesBySection[section] != nil {
				delete(db.filesBySection[section], oldEntry.Path)
				db.filesBySection[section][a.rel] = true
			}
			// Hash indexes: the hash is unchanged (rename-detection precondition),
			// but hashToPath's value points at the old path â€” update it.
			if newEntry.FileHash != "" {
				if htp, ok := db.hashToPath[section]; ok {
					if htp[newEntry.FileHash] == oldEntry.Path {
						htp[newEntry.FileHash] = a.rel
					}
				}
			}
			// Re-associate tags at the new path so tag stats follow the file.
			db.addFileTagAssociations(a.rel, newEntry.Tags)
			continue
		}
		// Truly removed â€” delete as before.
		if oldEntry, ok := db.files[r.relPath]; ok {
			db.removeFileTagAssociations(r.relPath, oldEntry.Tags)
			if oldEntry.FileHash != "" {
				// Only delete the shared hash from hashesBySection / hashToPath
				// when THIS file actually owns the hash mapping. After the
				// dedup fix, a colliding duplicate file (same content hash as
				// another file in the section) is indexed in db.files but is
				// NOT registered in the hash index (hashCollision skips it).
				// Unconditionally deleting hashesBySection[hash] here would wipe
				// the original owner's hash entry, corrupting the index. The
				// htp[f.FileHash] == relPath check confirms ownership.
				if htp, ok := db.hashToPath[section]; ok && htp[oldEntry.FileHash] == r.relPath {
					if hashes, ok := db.hashesBySection[section]; ok {
						delete(hashes, oldEntry.FileHash)
					}
					delete(htp, oldEntry.FileHash)
				}
			}
			delete(db.files, r.relPath)
			delete(db.filesBySection[section], r.relPath)
			trulyRemoved++
		}
	}

	// Insert truly-new files (those that weren't matched as renames).
	for ai, a := range added {
		if consumedAdded[ai] {
			continue
		}
		pathTags := pathTagsFromDir(a.rel)
		folder := filepath.Base(filepath.Dir(a.rel))
		num, tags := ParseFilename(a.info.Name(), folder)
		tags = append(pathTags, tags...)
		fileType := "image"
		if isVideoFile(a.info.Name()) {
			fileType = "video"
			tags = FilterTags([]string{folder})
		}
		file := MediaFile{
			Path:     a.rel,
			Name:     a.info.Name(),
			Type:     fileType,
			Mtime:    a.info.ModTime().Unix(),
			Number:   num,
			Tags:     dedupeTags(tags),
			Section:  section,
			FileSize: a.info.Size(),
			FileHash: a.fileHash,
		}
		db.saveFileLocked(file)
		trulyAdded++
	}
	db.mu.Unlock()

	totalChanges := trulyRemoved + renamed + modified + trulyAdded
	if totalChanges > 0 {
		log.Printf("[VERIFY] %s changes: %d renamed, %d removed, %d modified, %d added", section, renamed, trulyRemoved, modified, trulyAdded)
	} else {
		log.Printf("[VERIFY] %s: no changes detected", section)
	}
	return totalChanges > 0
}

func verifyMangaSection(cfg *Config, db *InMemoryDB, section, dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		log.Printf("[VERIFY] Cannot read directory %s: %v", dir, err)
		return false
	}

	var prevMangaState map[string][]string
	var prevArchiveState map[string][]string
	// Gate notifications on either notifier being configured. The original
	// code only checked Gotify, which silently broke Discord-only setups
	// (the chapter path also has this pre-existing bug â€” fixed here too).
	gn := getGotifyNotifier()
	dn := getDiscordNotifier()
	notifiersActive := gn != nil || dn != nil
	if notifiersActive {
		if section == SectionHManga {
			// h-manga notifies on archive additions, not new chapter
			// subdirectories. Unlike chapters (DB-derived), archives are
			// disk-derived â€” a new .cbz in an existing artist dir does NOT
			// trigger needsRescan (the artist dir already exists and no
			// subdirs changed). So we must take this snapshot here even
			// when no rescan is needed; gating on needsRescan would silently
			// skip archive detection on the periodic timer.
			prevArchiveState = getHMangaArchiveSnapshot(dir)
		} else {
			prevMangaState = getMangaChapterSnapshot(db, section)
		}
	}

	db.mu.RLock()
	existingSeries := make(map[string]bool)
	currentFolders, hasFolders := db.mangaFolders[section]
	if hasFolders {
		for _, s := range currentFolders {
			existingSeries[s.Name] = true
		}
	}
	db.mu.RUnlock()

	needsRescan := false

	// Check 1: New directories on disk that aren't in the DB
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if !existingSeries[entry.Name()] {
			needsRescan = true
			break
		}
	}

	// Check 2: Directories in the DB that no longer exist on disk
	if !needsRescan {
		for _, s := range currentFolders {
			if _, err := os.Stat(filepath.Join(dir, s.Name)); os.IsNotExist(err) {
				needsRescan = true
				break
			}
		}
	}

	// Check 3: Chapter-level changes within existing series.
	// When a chapter directory is renamed, the series directory still exists
	// but its chapter list no longer matches. We build a quick lookup of
	// what chapters the DB expects for each series and verify they exist on disk.
	if !needsRescan {
		// Build a map from series name -> set of chapter names for quick lookup.
		// Skip "Root" chapter names since they are virtual â€” they represent
		// root-level images in the series directory, not actual subdirectories.
		seriesChapters := make(map[string]map[string]bool)
		for _, s := range currentFolders {
			chSet := make(map[string]bool, len(s.Chapters))
			for _, ch := range s.Chapters {
				if ch.Name != "Root" {
					chSet[ch.Name] = true
				}
			}
			seriesChapters[s.Name] = chSet
		}

		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			chSet, ok := seriesChapters[entry.Name()]
			if !ok {
				// Already detected as new directory above
				continue
			}

			seriesPath := filepath.Join(dir, entry.Name())
			subEntries, err := os.ReadDir(seriesPath)
			if err != nil {
				// Can't read directory â€” might be a permission issue, trigger rescan
				needsRescan = true
				break
			}

			// Check if this series has sub-directories (chapters)
			hasSubDirs := false
			for _, sub := range subEntries {
				if sub.IsDir() {
					hasSubDirs = true
					break
				}
			}

			if hasSubDirs {
				// Build set of on-disk chapter directory names
				onDiskChapters := make(map[string]bool)
				for _, sub := range subEntries {
					if sub.IsDir() {
						onDiskChapters[sub.Name()] = true
					}
				}

				// Check: every chapter in the DB must exist on disk.
				// (Virtual "Root" chapters were already excluded from chSet.)
				for chName := range chSet {
					if !onDiskChapters[chName] {
						needsRescan = true
						break
					}
				}
				if needsRescan {
					break
				}

				// Check: every chapter directory on disk must be in the DB
				for diskChName := range onDiskChapters {
					if !chSet[diskChName] {
						needsRescan = true
						break
					}
				}
				if needsRescan {
					break
				}
			} else {
				// Series with no sub-directories (flat structure).
				// Check that the file list in the DB still matches.
				// For flat series, there's typically a single "Root" or self-named chapter.
				// Verify that media files on disk match what's in the DB.
				// Find the chapter entry for this series to compare files.
				var seriesObj *Series
				for i := range currentFolders {
					if currentFolders[i].Name == entry.Name() {
						seriesObj = &currentFolders[i]
						break
					}
				}
				if seriesObj != nil && len(seriesObj.Chapters) > 0 {
					// Build set of on-disk media file names
					onDiskFiles := make(map[string]bool)
					for _, sub := range subEntries {
						if !sub.IsDir() && IsMediaFile(sub.Name()) {
							onDiskFiles[sub.Name()] = true
						}
					}
					// Build set of DB file names
					dbFiles := make(map[string]bool)
					for _, ch := range seriesObj.Chapters {
						for _, img := range ch.Images {
							dbFiles[img.Name] = true
						}
					}
					// Check for mismatches
					for dbName := range dbFiles {
						if !onDiskFiles[dbName] {
							needsRescan = true
							break
						}
					}
					if !needsRescan {
						for diskName := range onDiskFiles {
							if !dbFiles[diskName] {
								needsRescan = true
								break
							}
						}
					}
					if needsRescan {
						break
					}
				}
			}
		}
	}

	if needsRescan || !hasFolders {
		var folders []Series
		if section == SectionManga {
			folders = ScanMangaStructure(dir)
		} else {
			folders = ScanHMangaStructure(dir)
		}
		db.SaveMangaFolders(section, folders)
		log.Printf("[VERIFY] %s: rescanned (%d series)", section, len(folders))

		if notifiersActive {
			if section == SectionHManga && prevArchiveState != nil {
				currentArchiveState := getHMangaArchiveSnapshot(dir)
				newArchives := diffHMangaArchives(prevArchiveState, currentArchiveState)
				notifyNewArchives(newArchives, section, gn, dn)
			} else if prevMangaState != nil {
				newChapters := diffMangaChapters(prevMangaState, folders, section)
				if len(newChapters) > 0 {
					grouped := make(map[string][]string)
					for _, nc := range newChapters {
						grouped[nc.Series] = append(grouped[nc.Series], nc.Chapter)
					}
					for series, chapters := range grouped {
						if gn != nil {
							gn.NotifyBatch(series, section, chapters)
						}
						if dn != nil {
							dn.NotifyBatch(series, section, chapters)
						}
					}
				}
			}
		}
		return true
	} else {
		log.Printf("[VERIFY] %s: no changes detected", section)
		// For h-manga, archives are disk-derived and a new .cbz in an
		// existing artist dir does NOT trigger needsRescan (the artist dir
		// already exists and no subdirs changed). So we still diff the
		// archive snapshot here even when no DB rescan was needed â€” this is
		// the periodic-timer path for archive detection.
		if notifiersActive && section == SectionHManga && prevArchiveState != nil {
			currentArchiveState := getHMangaArchiveSnapshot(dir)
			newArchives := diffHMangaArchives(prevArchiveState, currentArchiveState)
			notifyNewArchives(newArchives, section, gn, dn)
		}
		return false
	}
}

func saveSectionIndex(cfg *Config, db *InMemoryDB, section string) error {
	dir := cfg.Directories[section]
	if dir == "" {
		return nil
	}

	switch section {
	case SectionImages:
		files := db.GetFilesBySection(section)
		// If scanning produced no files, retain the old index rather than
		// writing an empty one. This protects against transient issues like
		// a network drive being temporarily unavailable.
		if len(files) == 0 {
			oldPath := indexFilePath(cfg, section)
			if _, err := os.Stat(oldPath); err == nil {
				log.Printf("[INDEX] No files found for %s â€” retaining existing index", section)
				return nil
			}
			log.Printf("[INDEX] No files found for %s and no existing index â€” writing empty index", section)
		}
		// Archive the old index before writing the new one
		if err := archiveIndex(cfg, section); err != nil {
			log.Printf("[INDEX] Warning: failed to archive old index for %s: %v", section, err)
		}
		return saveImagesIndex(cfg, section, dir, files)
	case SectionManga, SectionHManga:
		folders := db.GetMangaFolders(section)
		// If scanning produced no series, retain the old index rather than
		// writing an empty one. This protects against transient issues.
		if len(folders) == 0 {
			oldPath := indexFilePath(cfg, section)
			if _, err := os.Stat(oldPath); err == nil {
				log.Printf("[INDEX] No series found for %s â€” retaining existing index", section)
				return nil
			}
			log.Printf("[INDEX] No series found for %s and no existing index â€” writing empty index", section)
		}
		// Archive the old index before writing the new one
		if err := archiveIndex(cfg, section); err != nil {
			log.Printf("[INDEX] Warning: failed to archive old index for %s: %v", section, err)
		}
		return saveMangaIndex(cfg, section, dir, folders)
	}
	return nil
}

type indexSaveState struct {
	mu     sync.Mutex
	timers map[string]*time.Timer
}

var indexSaver indexSaveState

func initDebouncedSaveIndex() {
	indexSaver = indexSaveState{
		timers: make(map[string]*time.Timer),
	}
}

func debouncedSaveIndex(cfg *Config, db *InMemoryDB, section string) {
	indexSaver.mu.Lock()
	defer indexSaver.mu.Unlock()

	if t, ok := indexSaver.timers[section]; ok {
		t.Stop()
	}

	indexSaver.timers[section] = time.AfterFunc(5*time.Second, func() {
		indexSaver.mu.Lock()
		delete(indexSaver.timers, section)
		indexSaver.mu.Unlock()

		if err := saveSectionIndex(cfg, db, section); err != nil {
			log.Printf("[INDEX] Debounced save failed for %s: %v", section, err)
		}
	})
}

// flushPendingIndexSaves cancels all pending debounced index-save timers and
// synchronously writes the current index for every section that had a pending
// save. This is used on forced-shutdown paths (e.g. Windows CTRL_CLOSE_EVENT,
// where the OS hard-kills the process ~5s after the handler returns) to ensure
// in-memory index state is persisted before the process can be terminated,
// rather than relying on a background triggerShutdown that may not beat the
// OS deadline. Safe to call concurrently with debouncedSaveIndex.
func flushPendingIndexSaves(cfg *Config, db *InMemoryDB) {
	indexSaver.mu.Lock()
	pending := make([]string, 0, len(indexSaver.timers))
	for section, t := range indexSaver.timers {
		t.Stop()
		pending = append(pending, section)
	}
	indexSaver.timers = make(map[string]*time.Timer)
	indexSaver.mu.Unlock()

	for _, section := range pending {
		if err := saveSectionIndex(cfg, db, section); err != nil {
			log.Printf("[INDEX] Flush save failed for %s: %v", section, err)
		} else {
			log.Printf("[INDEX] Flushed pending save for %s", section)
		}
	}
}

func dedupeTags(tags []string) []string {
	if len(tags) <= 1 {
		return tags
	}
	seen := make(map[string]bool, len(tags))
	result := make([]string, 0, len(tags))
	for _, tag := range tags {
		if !seen[tag] {
			seen[tag] = true
			result = append(result, tag)
		}
	}
	return result
}

func computeFileHash(fullPath string) string {
	f, err := os.Open(fullPath)
	if err != nil {
		return ""
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

func pathTagsFromDir(relPath string) []string {
	relDir := filepath.Dir(relPath)
	var pathTags []string
	if relDir != "." {
		parts := strings.Split(filepath.ToSlash(relDir), "/")
		for _, part := range parts {
			if part == "" {
				continue
			}
			normalized := strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(part, " ", "_"), "-", "_"))
			if normalized != "" {
				pathTags = append(pathTags, normalized)
			}
		}
	}
	return pathTags
}

func migrateFileHashes(db *InMemoryDB, section, dir string) {
	files := db.GetFilesBySection(section)
	migrated := 0
	for _, f := range files {
		if f.FileHash != "" {
			continue
		}
		fullPath := filepath.Join(dir, f.Path)
		hash := computeFileHash(fullPath)
		if hash == "" {
			log.Printf("[MIGRATE] Warning: could not compute hash for %s", f.Path)
			continue
		}
		f.FileHash = hash
		db.SaveFile(f)
		migrated++
	}
	if migrated > 0 {
		log.Printf("[MIGRATE] Computed file hashes for %d files in section %q (v1 index migration)", migrated, section)
	}
}

// ============================================================
// In-Memory Database
// ============================================================

type InMemoryDB struct {
	files           map[string]MediaFile
	filesBySection  map[string]map[string]bool   // section -> set of file paths
	hashesBySection map[string]map[string]bool   // section -> set of file content hashes for dedup
	hashToPath      map[string]map[string]string // section -> hash -> file path (reverse index)
	tags            map[string][]string
	mangaFolders    map[string][]Series // section -> series list
	mu              sync.RWMutex
	scanning        map[string]bool
	scanningMu      sync.RWMutex
	// lastReindex[section] records the most recently completed reindex result
	// for that section. The entry from a prior run remains visible while a
	// new run is in flight â€” a polling client uses IsScanning to detect
	// "still running" and overwrites its view when the goroutine writes
	// the new result on completion.
	lastReindex   map[string]reindexResult
	lastReindexMu sync.RWMutex
}

// reindexResult is the cached outcome of the most recent reindex pass for a
// section. Persisted on the DB so that even after the originating HTTP
// handler returns (and the client disconnects), a later polling request can
// still observe the result. See /api/reindex/:section and /api/reindex/status.
type reindexResult struct {
	StartedAt   time.Time `json:"started_at"`
	CompletedAt time.Time `json:"completed_at"`
	Changed     bool      `json:"changed"`
	Count       int       `json:"count"`
	Error       string    `json:"error,omitempty"`
}

func NewInMemoryDB() *InMemoryDB {
	return &InMemoryDB{
		files:           make(map[string]MediaFile),
		filesBySection:  make(map[string]map[string]bool),
		hashesBySection: make(map[string]map[string]bool),
		hashToPath:      make(map[string]map[string]string),
		tags:            make(map[string][]string),
		mangaFolders:    make(map[string][]Series),
		scanning:        make(map[string]bool),
		lastReindex:     make(map[string]reindexResult),
	}
}

func (db *InMemoryDB) SaveFile(f MediaFile) {
	db.mu.Lock()
	defer db.mu.Unlock()
	db.saveFileLocked(f)
}

// saveFileLocked is the single source of truth for inserting/updating a
// MediaFile and maintaining every derived index (filesBySection, the tag
// reverse index db.tags, and the content-hash indexes hashesBySection /
// hashToPath). Caller MUST hold db.mu (the write lock).
//
// Factored out of SaveFile so bulk callers (SaveFiles, scan paths) can apply
// many files under a single write-lock acquisition instead of taking and
// releasing the lock per file. Go's sync.RWMutex is write-preferring, so a
// scan that calls SaveFile in a tight loop starves concurrent readers (the
// /api/files, /api/folders, /api/tags/stats endpoints all read under
// RLock); batching the writes collapses the reader-starvation window from
// "the entire scan" to a single brief splice at the end.
func (db *InMemoryDB) saveFileLocked(f MediaFile) {
	f.Tags = dedupeTags(f.Tags)

	if db.hashesBySection[f.Section] == nil {
		db.hashesBySection[f.Section] = make(map[string]bool)
	}

	hash := f.FileHash

	if _, pathExists := db.files[f.Path]; !pathExists {
		// New file. We intentionally do NOT skip files whose content hash
		// matches an existing entry: a media gallery legitimately contains
		// the same image in multiple folders (cross-posted art, imagesets,
		// copied files, renamed/moved files). Suppressing the new path here
		// leaves real files on disk unindexed and unservable. The hash index
		// (hashesBySection / hashToPath) is kept only for deletion
		// bookkeeping â€” see the collision handling below for updates.
		if hash != "" && db.hashesBySection[f.Section][hash] {
			debugLog("[DEDUP] Indexing duplicate-content file %q in section %q (hash %s already owned by %q)", f.Path, f.Section, hash, db.hashToPath[f.Section][hash])
		}
	}
	// Existing file update: clean up old hash associations before inserting new
	// ones. This prevents stale hash entries from blocking other files.

	if old, exists := db.files[f.Path]; exists {
		db.removeFileTagAssociations(f.Path, old.Tags)
		if old.FileHash != "" {
			delete(db.hashesBySection[f.Section], old.FileHash)
			if db.hashToPath[f.Section] != nil && db.hashToPath[f.Section][old.FileHash] == f.Path {
				delete(db.hashToPath[f.Section], old.FileHash)
			}
		}
	}

	// For existing file updates, check if the new hash is already owned by a different file.
	// If so, skip updating the hash indexes to avoid orphaning the other file's mapping.
	hashCollision := hash != "" && db.hashesBySection[f.Section][hash] && db.hashToPath[f.Section][hash] != f.Path
	if hashCollision {
		log.Printf("[DEDUP] Hash collision on update: %s in section %q already owned by %s, current: %s â€” keeping existing hash mapping", hash, f.Section, db.hashToPath[f.Section][hash], f.Path)
	}

	db.files[f.Path] = f
	if hash != "" && !hashCollision {
		db.hashesBySection[f.Section][hash] = true
		if db.hashToPath[f.Section] == nil {
			db.hashToPath[f.Section] = make(map[string]string)
		}
		db.hashToPath[f.Section][hash] = f.Path
	}

	if db.filesBySection[f.Section] == nil {
		db.filesBySection[f.Section] = make(map[string]bool)
	}
	db.filesBySection[f.Section][f.Path] = true

	db.addFileTagAssociations(f.Path, f.Tags)
}

// SaveFiles applies a batch of MediaFiles under a single write-lock
// acquisition. It is the bulk counterpart to SaveFile and preserves the
// exact same invariants (tag reverse index, filesBySection, hash indexes)
// per file. Use this from scan/index paths that discover many files at once
// so the write lock is held once for the batch rather than once per file â€”
// Go's sync.RWMutex is write-preferring, so per-file SaveFile calls during
// a scan starve all reader endpoints (/api/files, /api/folders, /api/tags,
// /api/tags/stats) for the scan's entire duration.
//
// The input slice is processed in order; later entries for the same path
// win over earlier ones (same semantics as calling SaveFile repeatedly).
func (db *InMemoryDB) SaveFiles(files []MediaFile) {
	db.mu.Lock()
	defer db.mu.Unlock()
	for _, f := range files {
		db.saveFileLocked(f)
	}
}

// ClearAndSaveFiles atomically replaces ALL data for a section with the given
// file list, under a single write-lock acquisition. It tears down the
// section's previous files (and their tag associations / hash indexes) and
// rebuilds it from `files`, so concurrent readers never observe an empty
// section mid-scan â€” they see either the old set or the new set, never a
// partial/empty window in between.
//
// This is the atomic clear+splice counterpart to ScanImages's batched write.
// Previously the scan flow called db.ClearSection(section) (which deletes
// filesBySection[section], the hash indexes, and all tag associations) and
// THEN walked the directory calling db.SaveFile per file, leaving the
// section completely empty for the entire walk duration. Reader endpoints
// (/api/files, /api/folders, /api/tags/stats) read the maps raw and don't
// consult the scanning flag, so they returned [] for the whole scan. With
// ClearAndSaveFiles the empty window collapses to a single locked splice.
//
// `files` should already be the complete set for the section (typically
// produced by ScanImages's walk). Callers MUST NOT call ClearSection before
// this â€” that would reintroduce the empty window this method exists to
// prevent.
func (db *InMemoryDB) ClearAndSaveFiles(section string, files []MediaFile) {
	db.mu.Lock()
	defer db.mu.Unlock()

	// Tear down the section's previous file entries and their tag
	// associations + hash indexes. This mirrors ClearSection's body but
	// runs under the same lock as the insert below so there's no observable
	// empty gap. (ClearSection itself is left as-is for manga callers.)
	if paths, ok := db.filesBySection[section]; ok {
		for path := range paths {
			if f, exists := db.files[path]; exists {
				db.removeFileTagAssociations(path, f.Tags)
				if f.FileHash != "" {
					if hashes, ok := db.hashesBySection[section]; ok {
						delete(hashes, f.FileHash)
					}
					if htp, ok := db.hashToPath[section]; ok && htp[f.FileHash] == path {
						delete(htp, f.FileHash)
					}
				}
				delete(db.files, path)
			}
		}
		delete(db.filesBySection, section)
	}
	// Reset the hash indexes for the section (ClearSection deletes the keys;
	// here we reset them so saveFileLocked's lazy-init check still works and
	// stale hashes from the old set can't leak into the new set).
	db.hashesBySection[section] = make(map[string]bool)
	db.hashToPath[section] = make(map[string]string)

	for _, f := range files {
		// Preserve the caller's section on each file (defensive â€” scan
		// paths already set it, but a caller passing cross-section files
		// would otherwise corrupt filesBySection for multiple sections).
		f.Section = section
		db.saveFileLocked(f)
	}
}

func (db *InMemoryDB) GetFilesBySection(section string) []MediaFile {
	db.mu.RLock()
	defer db.mu.RUnlock()

	// Use section index for O(1) lookup instead of O(n) scan
	var files []MediaFile
	if paths, ok := db.filesBySection[section]; ok {
		files = make([]MediaFile, 0, len(paths))
		for path := range paths {
			if f, ok := db.files[path]; ok {
				files = append(files, f)
			}
		}
	}

	// Default sort: newest first (mtime descending)
	sort.Slice(files, func(i, j int) bool {
		return files[i].Mtime > files[j].Mtime
	})

	return files
}

func (db *InMemoryDB) GetAllTags() map[string][]string {
	db.mu.RLock()
	defer db.mu.RUnlock()
	result := make(map[string][]string)
	for k, v := range db.tags {
		result[k] = v
	}
	return result
}

func (db *InMemoryDB) SaveMangaFolders(section string, folders []Series) {
	db.mu.Lock()
	defer db.mu.Unlock()

	// Note: Manga MediaFile entries built from ImageRef only have Path, Name, Type,
	// and Section â€” Mtime, FileSize, Number, and Tags are zero-valued. These fields
	// are not needed for manga serving (the API serves folder structure, not files),
	// but consumers relying on those fields for manga images should be aware of this.
	// Remove old file entries and their tag associations for this section.
	if oldPaths, ok := db.filesBySection[section]; ok {
		for path := range oldPaths {
			if f, exists := db.files[path]; exists {
				for _, tag := range f.Tags {
					t := strings.ToLower(tag)
					db.tags[t] = removeFromSlice(db.tags[t], path)
					if len(db.tags[t]) == 0 {
						delete(db.tags, t)
					}
				}
				delete(db.files, path)
			}
		}
	}
	db.filesBySection[section] = make(map[string]bool)
	db.hashesBySection[section] = make(map[string]bool)
	db.hashToPath[section] = make(map[string]string)
	for _, s := range folders {
		for _, ch := range s.Chapters {
			for _, img := range ch.Images {
				f := MediaFile{
					Path:    img.Path,
					Name:    img.Name,
					Type:    "image",
					Section: section,
				}
				db.files[f.Path] = f
				db.filesBySection[section][f.Path] = true
			}
		}
	}

	db.mangaFolders[section] = folders
}

// ClearSection removes all data for a section before re-scanning.
// This prevents stale entries from files that have been deleted or renamed.
func (db *InMemoryDB) ClearSection(section string) {
	db.mu.Lock()
	defer db.mu.Unlock()

	// Remove all files in this section and their tag associations
	if paths, ok := db.filesBySection[section]; ok {
		for path := range paths {
			if f, exists := db.files[path]; exists {
				// Remove tag associations for this file
				for _, tag := range f.Tags {
					tag = strings.ToLower(tag)
					db.tags[tag] = removeFromSlice(db.tags[tag], path)
					if len(db.tags[tag]) == 0 {
						delete(db.tags, tag)
					}
				}
				delete(db.files, path)
			}
		}
		delete(db.filesBySection, section)
	}

	delete(db.hashesBySection, section)
	delete(db.hashToPath, section)

	// Clear manga folders for this section (manga/h-manga)
	delete(db.mangaFolders, section)
}

func (db *InMemoryDB) GetMangaFolders(section string) []Series {
	db.mu.RLock()
	defer db.mu.RUnlock()
	if folders, ok := db.mangaFolders[section]; ok {
		return folders
	}
	return []Series{}
}

func (db *InMemoryDB) SetScanning(section string, scanning bool) {
	db.scanningMu.Lock()
	defer db.scanningMu.Unlock()
	db.scanning[section] = scanning
}

func (db *InMemoryDB) IsScanning(section string) bool {
	db.scanningMu.RLock()
	defer db.scanningMu.RUnlock()
	return db.scanning[section]
}

// TrySetScanning atomically checks if a section is NOT scanning, and if so,
// sets it to scanning and returns true. This eliminates TOCTOU races between
// the check and the set.
func (db *InMemoryDB) TrySetScanning(section string) bool {
	db.scanningMu.Lock()
	defer db.scanningMu.Unlock()
	if db.scanning[section] {
		return false
	}
	db.scanning[section] = true
	return true
}

// SetLastReindex records the most recent reindex outcome for a section. Safe
// to call from the reindex goroutine; a later polling client reads via
// GetLastReindex.
func (db *InMemoryDB) SetLastReindex(section string, r reindexResult) {
	db.lastReindexMu.Lock()
	defer db.lastReindexMu.Unlock()
	db.lastReindex[section] = r
}

// GetLastReindex returns the most recent reindex result for the section.
// The boolean return is false when no reindex has ever finished for that
// section. While a new reindex is in flight, the PRIOR result is still
// returned (if any) â€” callers should pair this with IsScanning to tell
// "in flight" from "finished".
func (db *InMemoryDB) GetLastReindex(section string) (reindexResult, bool) {
	db.lastReindexMu.RLock()
	defer db.lastReindexMu.RUnlock()
	r, ok := db.lastReindex[section]
	return r, ok
}

func (db *InMemoryDB) GetFileSection(path string) string {
	db.mu.RLock()
	defer db.mu.RUnlock()
	if f, ok := db.files[path]; ok {
		return f.Section
	}
	return ""
}

// GetTagStats returns tag counts sorted by frequency.
// If section is non-empty, only counts files belonging to that section.
func (db *InMemoryDB) GetTagStats(section string) []TagStat {
	db.mu.RLock()
	defer db.mu.RUnlock()

	// If filtering by section, build a set of paths in that section for O(1) lookup
	var sectionPaths map[string]bool
	if section != "" {
		sectionPaths = db.filesBySection[section]
	}

	stats := make([]TagStat, 0, len(db.tags))
	for tag, paths := range db.tags {
		count := 0
		for _, p := range paths {
			// Skip duplicate paths (defensive)
			if sectionPaths != nil {
				// Only count files in the specified section
				if !sectionPaths[p] {
					continue
				}
			}
			count++
		}
		if count > 0 {
			stats = append(stats, TagStat{
				Tag:   tag,
				Count: count,
			})
		}
	}

	// Sort by count descending, then by name
	sort.Slice(stats, func(i, j int) bool {
		if stats[i].Count != stats[j].Count {
			return stats[i].Count > stats[j].Count
		}
		return stats[i].Tag < stats[j].Tag
	})

	return stats
}

// GetFilesByTag returns all files with a specific tag.
// If section is non-empty, only returns files in that section.
// Results are deduplicated to prevent double-counting.
func (db *InMemoryDB) GetFilesByTag(tag, section string) []MediaFile {
	db.mu.RLock()
	defer db.mu.RUnlock()

	tag = strings.ToLower(tag)
	paths, ok := db.tags[tag]
	if !ok {
		return []MediaFile{}
	}

	seen := make(map[string]bool, len(paths))
	files := make([]MediaFile, 0, len(paths))
	for _, path := range paths {
		// Skip duplicates (defensive - should not happen after SaveFile fix)
		if seen[path] {
			continue
		}
		seen[path] = true

		f, exists := db.files[path]
		if !exists {
			continue
		}

		// Filter by section if specified
		if section != "" && f.Section != section {
			continue
		}

		files = append(files, f)
	}
	return files
}

// UpdateFileTags updates tags for a specific file
func (db *InMemoryDB) UpdateFileTags(path string, newTags []string) error {
	db.mu.Lock()
	defer db.mu.Unlock()

	file, ok := db.files[path]
	if !ok {
		return fmt.Errorf("file not found: %s", path)
	}

	// Remove file from old tag associations (shared invariant helper).
	db.removeFileTagAssociations(path, file.Tags)

	// Update file tags (deduplicate to prevent duplicate tag entries)
	file.Tags = dedupeTags(newTags)
	db.files[path] = file

	// Add file to new tag associations (shared invariant helper).
	db.addFileTagAssociations(path, file.Tags)

	return nil
}

// removeFileTagAssociations drops the file at path from every tag it
// currently belongs to in db.tags, deleting tag keys that become empty.
// Caller MUST hold db.mu (the write lock).
//
// This is the single source of truth for the "tear down a file's old tag
// associations" invariant, shared by BulkUpdateTags and SaveFile so the
// reverse-index maintenance cannot drift between them.
func (db *InMemoryDB) removeFileTagAssociations(path string, oldTags []string) {
	for _, tag := range oldTags {
		lc := strings.ToLower(tag)
		db.tags[lc] = removeFromSlice(db.tags[lc], path)
		if len(db.tags[lc]) == 0 {
			delete(db.tags, lc)
		}
	}
}

// addFileTagAssociations appends the file at path to every tag in newTags
// in the db.tags reverse index. Caller MUST hold db.mu (the write lock).
//
// This is the companion to removeFileTagAssociations and the single source
// of truth for the "add a file's new tag associations" invariant, shared by
// SaveFile, UpdateFileTags, and BulkUpdateTags so the reverse-index rebuild
// cannot drift between them.
func (db *InMemoryDB) addFileTagAssociations(path string, newTags []string) {
	for _, tag := range newTags {
		lc := strings.ToLower(tag)
		db.tags[lc] = append(db.tags[lc], path)
	}
}

// BulkUpdateTags performs bulk tag operations
func (db *InMemoryDB) BulkUpdateTags(paths []string, add, remove, set []string) map[string]interface{} {
	db.mu.Lock()
	defer db.mu.Unlock()

	updated := 0
	errors := []string{}

	for _, path := range paths {
		file, ok := db.files[path]
		if !ok {
			errors = append(errors, fmt.Sprintf("file not found: %s", path))
			continue
		}

		// Remove old tag associations (shared invariant helper).
		db.removeFileTagAssociations(path, file.Tags)

		// Apply operations in order: remove, set, add
		tagSet := make(map[string]bool)
		for _, tag := range file.Tags {
			tagSet[strings.ToLower(tag)] = true
		}

		// Remove
		for _, tag := range remove {
			delete(tagSet, strings.ToLower(tag))
		}

		// Set (replaces all)
		if len(set) > 0 {
			tagSet = make(map[string]bool)
			for _, tag := range set {
				tagSet[strings.ToLower(tag)] = true
			}
		}

		// Add
		for _, tag := range add {
			tagSet[strings.ToLower(tag)] = true
		}

		// Convert back to slice and deduplicate
		newTags := make([]string, 0, len(tagSet))
		for tag := range tagSet {
			newTags = append(newTags, tag)
		}
		file.Tags = dedupeTags(newTags)
		db.files[path] = file

		// Add new tag associations (shared invariant helper).
		db.addFileTagAssociations(path, file.Tags)

		updated++
	}

	return map[string]interface{}{
		"updated": updated,
		"errors":  errors,
	}
}

// FlushTags removes the given tags from every file in the database.
// If flushAll is true, ALL tags are removed from every file (a full flush);
// otherwise only the tags in the tags list (lowercased) are removed.
// Only files whose tag set actually changes are counted as updated.
// The optional section filter restricts the operation to files in that
// section (empty string = all sections).
//
// This is the backend for the Tag Management "Flush All Tags" / "Flush
// Selected Tags" buttons. To avoid the TOCTOU bug a snapshot/compute/apply
// split would introduce (a concurrent UpdateFileTags between snapshot and
// apply would be clobbered by the stale snapshot, desyncing db.tags from
// db.files), the whole operation runs under db.mu.Lock(). The O(k^2)
// concern from per-file removeFromSlice is avoided by only touching the
// reverse index for tags actually removed from each file, and by not
// rebuilding db.tags entries that don't change.
//
// Phase 1 (under RLock) scopes the candidate path set to avoid iterating
// every file for a targeted flush: for a targeted flush, only paths that
// currently hold one of the target tags (via db.tags) are candidates; for
// a full flush, all tagged files are candidates.
func (db *InMemoryDB) FlushTags(tags []string, section string, flushAll bool) map[string]interface{} {
	// Build the set of tags to remove (lowercased).
	removeSet := make(map[string]bool, len(tags))
	for _, t := range tags {
		removeSet[strings.ToLower(t)] = true
	}

	// Phase 1: collect the candidate path set under a short read lock.
	// For a targeted flush we use db.tags to scope to only files that
	// actually hold one of the target tags â€” O(files_with_target_tags)
	// instead of O(all_files). For a full flush we must visit every tagged
	// file. We only collect paths here (not tag slices) so the lock hold
	// time is minimal and Phase 3 re-reads the CURRENT tags under the
	// write lock, eliminating the stale-snapshot TOCTOU window.
	db.mu.RLock()
	var candidatePaths []string
	if flushAll {
		// Full flush: every tagged file is a candidate.
		if section != "" {
			if set, ok := db.filesBySection[section]; ok {
				candidatePaths = make([]string, 0, len(set))
				for p := range set {
					if f, ok := db.files[p]; ok && len(f.Tags) > 0 {
						candidatePaths = append(candidatePaths, p)
					}
				}
			}
		} else {
			candidatePaths = make([]string, 0, len(db.files))
			for p, f := range db.files {
				if len(f.Tags) > 0 {
					candidatePaths = append(candidatePaths, p)
				}
			}
		}
	} else {
		// Targeted flush: only files that hold one of the target tags.
		// Use db.tags[lc] to find them in O(files_with_target_tags) rather
		// than scanning every file. Deduplicate paths that hold multiple
		// target tags.
		seen := make(map[string]bool)
		for lc := range removeSet {
			for _, p := range db.tags[lc] {
				if section != "" {
					if f, ok := db.files[p]; !ok || f.Section != section {
						continue
					}
				}
				if !seen[p] {
					seen[p] = true
					candidatePaths = append(candidatePaths, p)
				}
			}
		}
	}
	db.mu.RUnlock()

	// Phase 2: apply under the write lock. Re-read each file's CURRENT
	// tags (not a stale snapshot) and recompute the removal against the
	// live set. This eliminates the TOCTOU window: any concurrent
	// UpdateFileTags/SaveFile that ran between Phase 1 and now has already
	// updated file.Tags and db.tags, and we operate on that updated state.
	// The reverse index is maintained per-file via removeFileTagAssociations
	// (tear down old) + re-append survivors, so db.tags stays consistent
	// with db.files at every point.
	db.mu.Lock()
	updated := 0
	removedTagInstances := 0

	for _, path := range candidatePaths {
		file, ok := db.files[path]
		if !ok || len(file.Tags) == 0 {
			continue
		}

		// Recompute the removal against the CURRENT tag list.
		newTags := make([]string, 0, len(file.Tags))
		changed := false
		for _, tag := range file.Tags {
			lc := strings.ToLower(tag)
			if flushAll || removeSet[lc] {
				changed = true
				removedTagInstances++
				// Remove ONLY this (being-flushed) tag's association from
				// the reverse index. Survivor tags are left untouched in
				// db.tags, avoiding the O(N^2) teardown+re-append that
				// removeFileTagAssociations + re-add would incur for
				// popular survivor tags during a full flush.
				db.tags[lc] = removeFromSlice(db.tags[lc], path)
				if len(db.tags[lc]) == 0 {
					delete(db.tags, lc)
				}
			} else {
				newTags = append(newTags, tag)
			}
		}
		if !changed {
			continue
		}

		// Write the new tag set. Survivor associations in db.tags were never
		// removed, so no re-append is needed â€” the reverse index stays
		// consistent with db.files under the held write lock.
		file.Tags = newTags
		db.files[path] = file
		updated++
	}
	db.mu.Unlock()

	return map[string]interface{}{
		"updated":               updated,
		"removed_tag_instances": removedTagInstances,
		"flushed_all":           flushAll,
	}
}

// ScanDirectory rescans a specific directory/folder within a section.
// For images, it does a full rescan of the section.
// For manga/h-manga, it rescans the entire section structure (folder-level granularity).
// When quick=true, it runs a lightweight verification instead of a full clear-and-rescan.
func (db *InMemoryDB) ScanDirectory(section, folder string, quick bool) error {
	cfg := getCurrentConfig()
	if cfg == nil {
		return fmt.Errorf("config not initialized")
	}
	dir := cfg.Directories[section]
	if dir == "" {
		return fmt.Errorf("directory not configured for section %s", section)
	}

	log.Printf("[SCAN] Rescanning %s/%s (quick=%v)", section, folder, quick)

	if quick {
		// Quick mode: verify and only update if changes detected.
		// verifySection handles its own scanning lock via TrySetScanning.
		verifySection(cfg, db, section)
		return nil
	}

	// Full rescan: clear and rebuild.
	// rescanSection handles its own scanning lock via TrySetScanning.
	rescanSection(cfg, section, dir, db)
	return nil
}

// rescanSection performs a full rescan of a single section, clearing stale data first.
// This handles both newly added files and files that have been deleted.
func rescanSection(cfg *Config, section, dir string, db *InMemoryDB) {
	if !db.TrySetScanning(section) {
		log.Printf("[RESCAN] Skipping %s â€” scan already in progress", section)
		return
	}
	defer db.SetScanning(section, false)

	var prevMangaState map[string][]string
	var prevArchiveState map[string][]string
	isMangaSection := section == SectionManga || section == SectionHManga
	// Gate on either notifier being configured so Discord-only setups
	// also produce notifications (the original gn-only gate silently
	// disabled Discord-only chapter notifications too).
	gn := getGotifyNotifier()
	dn := getDiscordNotifier()
	notifiersActive := gn != nil || dn != nil
	if notifiersActive && isMangaSection {
		if section == SectionHManga {
			// h-manga notifies on archive additions, not new chapter
			// subdirectories. Snapshot the disk-derived archive set so a
			// later rescan can diff against it.
			prevArchiveState = getHMangaArchiveSnapshot(dir)
		} else {
			prevMangaState = getMangaChapterSnapshot(db, section)
		}
	}

	// For manga sections, clear stale data before re-scanning so removed
	// files disappear (SaveMangaFolders tears down old file/tag entries but
	// not the folder list itself, so ClearSection is still needed). For the
	// images section we DON'T clear here: ScanImages walks the directory and
	// calls db.ClearAndSaveFiles at the end, which atomically swaps the old
	// set for the new one under a single write lock. Clearing first would
	// leave the section empty for the entire walk and readers would observe
	// [] mid-scan.
	start := time.Now()
	switch section {
	case SectionImages:
		ctx, cancel := context.WithTimeout(context.Background(), scanTimeoutDuration())
		defer cancel()
		if err := ScanImages(ctx, dir, db, section); err != nil {
			log.Printf("[RESCAN ERROR] Failed to rescan %s: %v", section, err)
			return
		}
		files := db.GetFilesBySection(section)
		metrics.FilesScanned.Add(uint64(len(files)))
		log.Printf("[RESCAN] Completed %s: %d files (%v)", section, len(files), time.Since(start))
	case SectionManga:
		db.ClearSection(section)
		folders := ScanMangaStructure(dir)
		db.SaveMangaFolders(section, folders)
		log.Printf("[RESCAN] Completed %s: %d folders (%v)", section, len(folders), time.Since(start))
	case SectionHManga:
		db.ClearSection(section)
		folders := ScanHMangaStructure(dir)
		db.SaveMangaFolders(section, folders)
		log.Printf("[RESCAN] Completed %s: %d folders (%v)", section, len(folders), time.Since(start))
	}

	if notifiersActive && isMangaSection {
		if section == SectionHManga && prevArchiveState != nil {
			currentArchiveState := getHMangaArchiveSnapshot(dir)
			newArchives := diffHMangaArchives(prevArchiveState, currentArchiveState)
			notifyNewArchives(newArchives, section, gn, dn)
		} else {
			currentFolders := db.GetMangaFolders(section)
			newChapters := diffMangaChapters(prevMangaState, currentFolders, section)
			if len(newChapters) > 0 {
				grouped := make(map[string][]string)
				for _, nc := range newChapters {
					grouped[nc.Series] = append(grouped[nc.Series], nc.Chapter)
				}
				for series, chapters := range grouped {
					if gn != nil {
						gn.NotifyBatch(series, section, chapters)
					}
					if dn != nil {
						dn.NotifyBatch(series, section, chapters)
					}
				}
			}
		}
	}

	if err := saveSectionIndex(cfg, db, section); err != nil {
		log.Printf("[RESCAN] Failed to save index for %s: %v", section, err)
	}
}

// startPeriodicRescan launches a background goroutine that periodically rescans
// all configured directories. It waits for the initial scan to complete (initialWait)
// before starting the periodic timer.
func startPeriodicRescan(cfg *Config, db *InMemoryDB, initialWait time.Duration) {
	if !cfg.WatchDirectories {
		log.Println("[RESCAN] Periodic rescan disabled (watch_directories=false)")
		return
	}

	interval := time.Duration(cfg.RescanIntervalSec) * time.Second
	if interval < 30*time.Second {
		interval = 30 * time.Second
		log.Printf("[RESCAN] Rescan interval too low, clamped to 30s")
	}

	go func() {
		// Wait for initial startup scan to finish before starting periodic scans
		time.Sleep(initialWait)

		log.Printf("[RESCAN] Starting periodic rescan every %v", interval)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for range ticker.C {
			for section, dir := range cfg.Directories {
				if dir == "" {
					continue
				}
				// Skip if a scan is already in progress for this section
				if db.IsScanning(section) {
					log.Printf("[RESCAN] Skipping %s â€” scan already in progress", section)
					continue
				}
				log.Printf("[RESCAN] Starting periodic verify for %s", section)
				// Use verifySection instead of rescanSection for periodic checks.
				// This only writes indexes when changes are detected, avoiding
				// unnecessary archive entries every rescan interval.
				verifySection(cfg, db, section)
			}
			if thumbnailPool != nil {
				if deleted := thumbnailPool.CullDeletedSources(db); deleted > 0 {
					log.Printf("[CACHE CULL] Removed %d orphaned thumbnail(s) for deleted sources", deleted)
				}
				if deleted := thumbnailPool.EnforceSizeCap(cacheCapBytes); deleted > 0 {
					log.Printf("[CACHE CULL] Size cap: removed %d old file(s)", deleted)
				}
			}
		}
	}()
}

// TagStat represents tag statistics
type TagStat struct {
	Tag   string `json:"tag"`
	Count int    `json:"count"`
}

// Helper function to remove string from slice
func removeFromSlice(slice []string, s string) []string {
	result := make([]string, 0, len(slice))
	for _, item := range slice {
		if item != s {
			result = append(result, item)
		}
	}
	return result
}

// ============================================================
// Rate Limiter
// ============================================================

type RateLimiter struct {
	tokens   chan struct{}
	interval time.Duration
	stopCh   chan struct{}
}

func NewRateLimiter(rps int, burst int) *RateLimiter {
	if rps <= 0 {
		rps = 100
	}
	if burst <= 0 {
		burst = 20
	}

	rl := &RateLimiter{
		tokens:   make(chan struct{}, burst),
		interval: time.Second / time.Duration(rps),
		stopCh:   make(chan struct{}),
	}
	// Fill initial tokens
	for i := 0; i < burst; i++ {
		rl.tokens <- struct{}{}
	}
	// Replenish tokens
	go rl.replenish()
	return rl
}

func (rl *RateLimiter) replenish() {
	ticker := time.NewTicker(rl.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			select {
			case rl.tokens <- struct{}{}:
			default:
			}
		case <-rl.stopCh:
			return
		}
	}
}

func (rl *RateLimiter) Stop() {
	close(rl.stopCh)
}

func (rl *RateLimiter) Allow() bool {
	select {
	case <-rl.tokens:
		return true
	default:
		return false
	}
}

func RateLimitMiddleware(limiter *RateLimiter) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !limiter.Allow() {
			c.JSON(429, gin.H{"error": "Rate limit exceeded. Slow down."})
			c.Abort()
			return
		}
		c.Next()
	}
}

// ============================================================
// Connection Limiter
// ============================================================

type ConnectionLimiter struct {
	sem chan struct{}
}

func NewConnectionLimiter(max int) *ConnectionLimiter {
	if max <= 0 {
		max = 50
	}
	return &ConnectionLimiter{sem: make(chan struct{}, max)}
}

func (cl *ConnectionLimiter) Acquire(ctx context.Context) error {
	select {
	case cl.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (cl *ConnectionLimiter) Release() {
	select {
	case <-cl.sem:
	default:
	}
}

func ConnectionLimitMiddleware(limiter *ConnectionLimiter, acquireTimeout time.Duration) gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx, cancel := context.WithTimeout(c.Request.Context(), acquireTimeout)
		defer cancel()

		if err := limiter.Acquire(ctx); err != nil {
			c.JSON(503, gin.H{"error": "Server busy, try again later"})
			c.Abort()
			return
		}
		defer limiter.Release()
		c.Next()
	}
}

// ============================================================
// Minimum Write Rate Middleware
// ============================================================

// minWriteRateWriter wraps gin.ResponseWriter to track bytes written
// and enforce a minimum write rate. If the client is receiving data
// slower than the configured minimum rate, the connection is terminated.
// As long as data is flowing at or above the minimum rate, the connection
// stays alive indefinitely â€” allowing large media streams on fast connections.
//
// All fields accessed by the monitor goroutine are protected by mu.
// Write/WriteString are called from the handler goroutine; checkRate
// and field reads are called from the monitor goroutine.
type minWriteRateWriter struct {
	gin.ResponseWriter
	mu           sync.Mutex
	minRate      int           // minimum bytes per second
	initialDelay time.Duration // initial grace period before rate enforcement
	bytesWritten int64         // total bytes written since last rate check
	lastWrite    time.Time     // time of last successful Write call
	lastCheck    time.Time     // time of last rate check/reset
	startTime    time.Time     // time the handler started
	handlerDone  atomic.Bool   // set when the handler has returned; prevents late abort actions
}

func (w *minWriteRateWriter) Write(data []byte) (int, error) {
	n, err := w.ResponseWriter.Write(data)
	if err != nil {
		return n, err
	}

	w.mu.Lock()
	w.bytesWritten += int64(n)
	w.lastWrite = time.Now()
	w.mu.Unlock()

	return n, nil
}

func (w *minWriteRateWriter) WriteString(s string) (int, error) {
	n, err := w.ResponseWriter.WriteString(s)
	if err != nil {
		return n, err
	}

	w.mu.Lock()
	w.bytesWritten += int64(n)
	w.lastWrite = time.Now()
	w.mu.Unlock()

	return n, err
}

// checkRate verifies the write rate is above the minimum threshold.
// Returns true if the rate is acceptable (or not enough time has passed
// for a meaningful measurement), false if it's too slow.
// Must be called with w.mu held.
func (w *minWriteRateWriter) checkRateLocked() bool {
	if w.minRate <= 0 {
		return true // rate checking disabled
	}

	now := time.Now()
	elapsed := now.Sub(w.lastCheck)
	if elapsed < 2*time.Second {
		// Not enough time has passed for a meaningful rate check
		return true
	}

	bytesPerSec := float64(w.bytesWritten) / elapsed.Seconds()
	w.bytesWritten = 0
	w.lastCheck = now

	// If data is flowing at or above the minimum rate, the connection is healthy
	return bytesPerSec >= float64(w.minRate)
}

// MinWriteRateMiddleware returns a Gin middleware that enforces a minimum
// write rate for all responses. If the client is downloading below the
// configured minimum rate (default 500 KB/s), the connection is terminated.
// This allows fast media streams to run indefinitely while preventing
// stalled or extremely slow connections from consuming server resources.
//
// The middleware wraps the ResponseWriter with rate tracking and uses a
// background goroutine to periodically check if data is flowing above
// the minimum rate. Connections below the threshold are terminated.
//
// During an initial grace period (WriteTimeoutSec, default 30s), handlers
// that haven't written any data yet are given a pass â€” they may be doing
// computation before producing output (e.g., directory scanning).
func MinWriteRateMiddleware(minRateBytesPerSec int, initialTimeout time.Duration) gin.HandlerFunc {
	return func(c *gin.Context) {
		if minRateBytesPerSec <= 0 {
			c.Next()
			return
		}

		now := time.Now()
		wrapper := &minWriteRateWriter{
			ResponseWriter: c.Writer,
			minRate:        minRateBytesPerSec,
			initialDelay:   initialTimeout,
			lastCheck:      now,
			startTime:      now,
		}
		c.Writer = wrapper

		// Start a goroutine that monitors write rate and terminates
		// connections that are too slow. The goroutine checks every 3 seconds
		// whether the average write rate since the last check is above the
		// minimum threshold.
		done := make(chan struct{})

		// Mark the wrapper as done as soon as the handler returns. The monitor
		// goroutine consults this flag before performing any post-handler
		// actions (like Hijack) so a late ticker fire after ServeHTTP has
		// finished becomes a no-op instead of panicking inside the stdlib.
		defer func() {
			wrapper.handlerDone.Store(true)
			close(done) // Ensure done is closed even if c.Next() panics
		}()

		go func() {
			ticker := time.NewTicker(3 * time.Second)
			defer ticker.Stop()

			for {
				select {
				case <-ticker.C:
					wrapper.mu.Lock()
					withinGrace := time.Since(wrapper.startTime) < initialTimeout
					noBytesYet := wrapper.bytesWritten == 0 && wrapper.lastWrite.IsZero()
					rateOk := wrapper.checkRateLocked()
					lastWrite := wrapper.lastWrite
					wrapper.mu.Unlock()
					// Read handlerDone atomically (outside mu) â€” the handler
					// goroutine sets it with no lock held, so it must be atomic
					// to avoid a data race with the monitor goroutine here.
					doneFlag := wrapper.handlerDone.Load()

					// If the handler has already returned, ServeHTTP is finished.
					// Any Hijack attempt now would panic in the stdlib, and the
					// server is already tearing down this connection, so just exit.
					if doneFlag {
						return
					}

					if withinGrace && noBytesYet {
						// Handler is still computing within the initial grace period.
						// Don't enforce the rate yet â€” give it time to produce output.
						continue
					}

					if !rateOk {
						// Client is downloading too slowly â€” abort the connection.
						wrapper.mu.Lock()
						elapsed := time.Since(wrapper.startTime)
						lastWriteAgo := time.Since(lastWrite)
						wrapper.mu.Unlock()
						// Re-check handlerDone atomically: the handler may have
						// completed between the first check and now. Hijacking a
						// finished ServeHTTP panics, so bail out.
						if wrapper.handlerDone.Load() {
							return
						}

						log.Printf("[RATE LIMIT] Terminating slow connection from %s: rate below %d bytes/sec (elapsed: %v, last write: %v ago)",
							c.ClientIP(), minRateBytesPerSec, elapsed, lastWriteAgo)

						// Close the underlying connection to immediately stop I/O.
						// Only the Hijack() call itself is recover-wrapped: the
						// stdlib panics with "Hijack called after ServeHTTP
						// finished" if ServeHTTP has completed between our last
						// doneFlag check and this call. conn.Close() is called
						// outside the recover so that any close-time panic
						// propagates instead of being masked (which could leak
						// the hijacked fd and hide a real defect).
						var hijackedConn net.Conn
						func() {
							defer func() {
								if r := recover(); r != nil {
									log.Printf("[RATE LIMIT] Suppressed post-handler Hijack panic for %s: %v",
										c.ClientIP(), r)
								}
							}()
							if hijacker, ok := wrapper.ResponseWriter.(http.Hijacker); ok {
								if conn, _, err := hijacker.Hijack(); err == nil {
									hijackedConn = conn
								}
							}
						}()
						if hijackedConn != nil {
							hijackedConn.Close()
						}
						// Fallback: just return from goroutine, handler will see broken pipe
						return
					}
				case <-c.Request.Context().Done():
					// Request was cancelled (client disconnected, handler finished, etc.)
					return
				case <-done:
					// Handler finished normally
					return
				}
			}
		}()

		c.Next()
	}
}

// ffmpegAvailable tracks whether ffmpeg and ffprobe were found on the system.
// When unavailable, video thumbnails fall back to a raw copy and image thumbnails
// use the Go imaging library with JPEG encoding instead of WebP.
var ffmpegAvailable bool

// detectFFmpeg checks whether ffmpeg and ffprobe are available on the system.
// It searches PATH first, then common install directories on Windows/macOS/Linux.
func detectFFmpeg() {
	// Helper: try finding a binary by name on PATH, then in common locations.
	findBinary := func(name string) string {
		// Try PATH first
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
		// Common extra install locations
		var candidates []string
		if runtime.GOOS == "windows" {
			candidates = []string{
				`C:\ffmpeg\bin\` + name + `.exe`,
				`C:\ProgramData\ffmpeg\bin\` + name + `.exe`,
				`C:\Program Files\ffmpeg\bin\` + name + `.exe`,
				`C:\Program Files (x86)\ffmpeg\bin\` + name + `.exe`,
				filepath.Join(os.Getenv("USERPROFILE"), "ffmpeg", "bin", name+".exe"),
				filepath.Join(os.Getenv("LOCALAPPDATA"), "Programs", "ffmpeg", "bin", name+".exe"),
			}
		} else {
			candidates = []string{
				"/usr/local/bin/" + name,
				"/usr/bin/" + name,
				"/opt/homebrew/bin/" + name,
			}
		}
		for _, c := range candidates {
			if _, err := os.Stat(c); err == nil {
				return c
			}
		}
		return ""
	}

	ffmpegPath := findBinary("ffmpeg")
	ffprobePath := findBinary("ffprobe")

	if ffmpegPath != "" && ffprobePath != "" {
		ffmpegAvailable = true
		log.Printf("[FFMPEG] Detected ffmpeg=%s ffprobe=%s â€” video thumbnails and WebP output enabled", ffmpegPath, ffprobePath)
	} else {
		ffmpegAvailable = false
		log.Printf("[FFMPEG] ffmpeg/ffprobe not found â€” video thumbnails will use raw copy fallback, image thumbnails will use JPEG encoding")
		if ffmpegPath != "" {
			log.Printf("[FFMPEG]   ffmpeg found at: %s (but ffprobe missing)", ffmpegPath)
		}
		if ffprobePath != "" {
			log.Printf("[FFMPEG]   ffprobe found at: %s (but ffmpeg missing)", ffprobePath)
		}
	}
}

// ============================================================
// Dependency Check & Auto-Install
// ============================================================

type Dependency struct {
	Name        string
	Binaries    []string
	SearchPaths []string
	DownloadURL string
	InstallDir  string
	Required    bool
	Description string
	WindowsOnly bool
	IsArchive   bool
	ExtractGlob string
	RenameTo    string
}

func getDependencies(cfg *Config) []Dependency {
	deps := []Dependency{
		{
			Name:     "ffmpeg",
			Binaries: []string{"ffmpeg", "ffmpeg.exe"},
			SearchPaths: func() []string {
				if runtime.GOOS == "windows" {
					return []string{
						`C:\ffmpeg\bin\ffmpeg.exe`,
						`C:\ProgramData\ffmpeg\bin\ffmpeg.exe`,
						`C:\Program Files\ffmpeg\bin\ffmpeg.exe`,
						`C:\Program Files (x86)\ffmpeg\bin\ffmpeg.exe`,
						filepath.Join(os.Getenv("USERPROFILE"), "ffmpeg", "bin", "ffmpeg.exe"),
						filepath.Join(os.Getenv("LOCALAPPDATA"), "Programs", "ffmpeg", "bin", "ffmpeg.exe"),
					}
				}
				return []string{"/usr/local/bin/ffmpeg", "/usr/bin/ffmpeg", "/opt/homebrew/bin/ffmpeg"}
			}(),
			DownloadURL: "https://www.gyan.dev/ffmpeg/builds/ffmpeg-release-essentials.zip",
			InstallDir:  "tools",
			Required:    false,
			Description: "Video thumbnail extraction and WebP output",
			WindowsOnly: true,
			IsArchive:   true,
			ExtractGlob: "bin/ffmpeg.exe",
		},
		{
			Name:     "ffprobe",
			Binaries: []string{"ffprobe", "ffprobe.exe"},
			SearchPaths: func() []string {
				if runtime.GOOS == "windows" {
					return []string{
						`C:\ffmpeg\bin\ffprobe.exe`,
						`C:\ProgramData\ffmpeg\bin\ffprobe.exe`,
						`C:\Program Files\ffmpeg\bin\ffprobe.exe`,
						`C:\Program Files (x86)\ffmpeg\bin\ffprobe.exe`,
						filepath.Join(os.Getenv("USERPROFILE"), "ffmpeg", "bin", "ffprobe.exe"),
						filepath.Join(os.Getenv("LOCALAPPDATA"), "Programs", "ffmpeg", "bin", "ffprobe.exe"),
					}
				}
				return []string{"/usr/local/bin/ffprobe", "/usr/bin/ffprobe", "/opt/homebrew/bin/ffprobe"}
			}(),
			DownloadURL: "https://www.gyan.dev/ffmpeg/builds/ffmpeg-release-essentials.zip",
			InstallDir:  "tools",
			Required:    false,
			Description: "Video duration detection (bundled with ffmpeg)",
			WindowsOnly: true,
			IsArchive:   true,
			ExtractGlob: "bin/ffprobe.exe",
		},
	}

	if cfg.Gotify.Enabled {
		binaryPath := cfg.Gotify.BinaryPath
		if binaryPath == "" {
			binaryPath = "./tools/gotify-server.exe"
		}
		if !filepath.IsAbs(binaryPath) {
			binaryPath = resolveRelativeToExe(binaryPath)
		}
		gotifyBinaryName := filepath.Base(binaryPath)
		gotifiySearchPaths := []string{binaryPath}
		if runtime.GOOS == "windows" {
			gotifiySearchPaths = append(gotifiySearchPaths,
				`C:\gotify\gotify-server.exe`,
				filepath.Join(os.Getenv("USERPROFILE"), "gotify", "gotify-server.exe"),
			)
		}
		deps = append(deps, Dependency{
			Name:        "gotify-server",
			Binaries:    []string{gotifyBinaryName},
			SearchPaths: gotifiySearchPaths,
			DownloadURL: "https://github.com/gotify/server/releases/latest/download/gotify-windows-amd64.exe",
			InstallDir:  "tools",
			Required:    false,
			Description: "Push notifications for new manga chapters",
			WindowsOnly: true,
			IsArchive:   false,
			RenameTo:    "gotify-server.exe",
		})
	}

	return deps
}

func findDependencyBinary(dep Dependency) string {
	for _, name := range dep.Binaries {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}
	for _, p := range dep.SearchPaths {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	toolsDir := filepath.Join(exeRelativeBaseDir, dep.InstallDir)
	for _, name := range dep.Binaries {
		candidate := filepath.Join(toolsDir, name)
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	return ""
}

func checkDependencies(cfg *Config) []Dependency {
	var missing []Dependency
	for _, dep := range getDependencies(cfg) {
		if dep.WindowsOnly && runtime.GOOS != "windows" {
			continue
		}
		path := findDependencyBinary(dep)
		if path != "" {
			log.Printf("[DEP] %s found at %s", dep.Name, path)
		} else {
			log.Printf("[DEP] %s not found â€” %s unavailable", dep.Name, dep.Description)
			missing = append(missing, dep)
		}
	}
	return missing
}

func promptInstall(dep Dependency, isDaemon bool) bool {
	if isDaemon {
		return false
	}
	if !isTerminal() {
		return false
	}
	fmt.Printf("[DEPENDENCY] %s not found. %s will be unavailable.\n", dep.Name, dep.Description)
	fmt.Printf("  Download and install to ./%s/ ? [Y/n]: ", dep.InstallDir)
	var response string
	fmt.Scanln(&response)
	response = strings.TrimSpace(strings.ToLower(response))
	return response == "" || response == "y" || response == "yes"
}

func isTerminal() bool {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

func downloadFile(url, destPath string) error {
	resp, err := http.Get(url)
	if err != nil {
		return fmt.Errorf("download failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download failed: HTTP %d", resp.StatusCode)
	}

	total := resp.ContentLength
	out, err := os.Create(destPath)
	if err != nil {
		return fmt.Errorf("create file failed: %w", err)
	}
	defer out.Close()

	buf := make([]byte, 32*1024)
	var downloaded int64
	lastPct := -1
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			written, writeErr := out.Write(buf[:n])
			if writeErr != nil {
				return fmt.Errorf("write failed: %w", writeErr)
			}
			if written != n {
				return fmt.Errorf("short write: wrote %d of %d bytes", written, n)
			}
			downloaded += int64(n)
			if total > 0 {
				pct := int(float64(downloaded) / float64(total) * 100)
				if pct != lastPct && pct%10 == 0 {
					fmt.Printf("  %s: %d%%\n", filepath.Base(destPath), pct)
					lastPct = pct
				}
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return fmt.Errorf("read failed: %w", readErr)
		}
	}

	if total > 0 {
		fmt.Printf("  %s: 100%%\n", filepath.Base(destPath))
	}
	return nil
}

func extractFromZip(zipPath, globPattern, destDir string) (string, error) {
	r, err := zip.OpenReader(zipPath)
	if err != nil {
		return "", fmt.Errorf("open zip failed: %w", err)
	}
	defer r.Close()

	targetBase := filepath.Base(globPattern)
	// Resolve and clean the dest dir once so every candidate outPath can be
	// checked against it. Reject any archive entry that would escape destDir
	// (path traversal) before touching the filesystem.
	cleanDest := filepath.Clean(destDir)
	destFile := ""
	for _, f := range r.File {
		if f.FileInfo().IsDir() {
			continue
		}
		// Reject entries that contain ".." segments â€” a malicious archive
		// could place "../../bin/ffmpeg.exe" and escape the install dir even
		// if the basename matches the target.
		if strings.Contains(f.Name, "..") {
			log.Printf("[DEP] Rejecting zip entry with path traversal segment: %q", f.Name)
			continue
		}
		zipBase := filepath.Base(f.Name)
		if zipBase != targetBase {
			continue
		}
		matched, matchErr := filepath.Match(globPattern, f.Name)
		if matchErr != nil || !matched {
			if strings.HasSuffix(f.Name, "/"+targetBase) || strings.HasSuffix(f.Name, "\\"+targetBase) {
				matched = true
			}
		}
		if !matched {
			if strings.Contains(f.Name, "/bin/"+targetBase) || strings.Contains(f.Name, "\\bin\\"+targetBase) {
				matched = true
			}
		}
		if matched {
			outPath := filepath.Join(destDir, targetBase)
			// Path-traversal guard: ensure the resolved output path stays
			// inside destDir. filepath.Join + filepath.Clean normalizes any
			// embedded ".."/"." so this catches attempts to escape via a
			// crafted targetBase or destDir. Compare with a separator suffix
			// so a destDir like "/a/b" doesn't falsely match "/a/bc".
			cleanOut := filepath.Clean(outPath)
			if cleanOut != cleanDest && !strings.HasPrefix(cleanOut+string(filepath.Separator), cleanDest+string(filepath.Separator)) {
				log.Printf("[DEP] Rejecting zip entry %q: resolved path %q escapes dest dir %q", f.Name, cleanOut, cleanDest)
				continue
			}
			if err := extractZipFile(f, outPath); err != nil {
				return "", fmt.Errorf("extract %s failed: %w", f.Name, err)
			}
			destFile = outPath
		}
	}
	if destFile == "" {
		return "", fmt.Errorf("no file matching %q found in archive", globPattern)
	}
	return destFile, nil
}

func extractZipFile(f *zip.File, destPath string) error {
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()

	out, err := os.OpenFile(destPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
	if err != nil {
		return err
	}
	defer out.Close()

	_, err = io.Copy(out, rc)
	return err
}

func checkAndInstallDependencies(cfg *Config, isDaemon bool) {
	missing := checkDependencies(cfg)
	if len(missing) == 0 {
		return
	}

	approved := make([]Dependency, 0, len(missing))
	for _, dep := range missing {
		if promptInstall(dep, isDaemon) {
			approved = append(approved, dep)
		} else {
			log.Printf("[DEP] Skipping %s installation", dep.Name)
		}
	}

	archiveCache := make(map[string]string)
	defer func() {
		for _, p := range archiveCache {
			os.Remove(p)
			os.RemoveAll(filepath.Dir(p))
		}
	}()

	for _, dep := range approved {
		if err := installDependencyWithCache(dep, archiveCache); err != nil {
			log.Printf("[DEP] Failed to install %s: %v", dep.Name, err)
		}
	}
}

func installDependencyWithCache(dep Dependency, archiveCache map[string]string) error {
	installDir := filepath.Join(exeRelativeBaseDir, dep.InstallDir)
	if err := os.MkdirAll(installDir, 0755); err != nil {
		return fmt.Errorf("create directory %s: %w", installDir, err)
	}

	if runtime.GOOS != "windows" {
		fmt.Printf("[DEP] %s: Auto-install is only available on Windows.\n", dep.Name)
		fmt.Printf("  Install manually: apt install ffmpeg / brew install ffmpeg\n")
		return fmt.Errorf("auto-install not supported on %s", runtime.GOOS)
	}

	if dep.IsArchive {
		archivePath, ok := archiveCache[dep.DownloadURL]
		if !ok {
			fmt.Printf("[DEP] Downloading %s...\n", dep.Name)
			tmpDir, err := os.MkdirTemp("", "mv-dep-*")
			if err != nil {
				return fmt.Errorf("create temp dir: %w", err)
			}
			archivePath = filepath.Join(tmpDir, "download.zip")
			if err := downloadFile(dep.DownloadURL, archivePath); err != nil {
				os.RemoveAll(tmpDir)
				return fmt.Errorf("download %s: %w", dep.Name, err)
			}
			archiveCache[dep.DownloadURL] = archivePath
		} else {
			fmt.Printf("[DEP] Using cached archive for %s...\n", dep.Name)
		}

		fmt.Printf("[DEP] Extracting %s...\n", dep.Name)
		_, err := extractFromZip(archivePath, dep.ExtractGlob, installDir)
		if err != nil {
			return fmt.Errorf("extract %s: %w", dep.Name, err)
		}

		path := findDependencyBinary(dep)
		if path == "" {
			return fmt.Errorf("%s installation completed but binary not found", dep.Name)
		}
		log.Printf("[DEP] %s installed successfully at %s", dep.Name, path)
		return nil
	}

	return installBinaryDep(dep, installDir)
}

func installBinaryDep(dep Dependency, installDir string) error {
	fmt.Printf("[DEP] Downloading %s...\n", dep.Name)

	downloadName := dep.RenameTo
	if downloadName == "" {
		downloadName = dep.Binaries[0]
	}
	destPath := filepath.Join(installDir, downloadName)
	if err := downloadFile(dep.DownloadURL, destPath); err != nil {
		return fmt.Errorf("download %s: %w", dep.Name, err)
	}

	path := findDependencyBinary(dep)
	if path == "" {
		return fmt.Errorf("%s installation completed but binary not found", dep.Name)
	}
	log.Printf("[DEP] %s installed successfully at %s", dep.Name, path)
	return nil
}

// isVideoFile returns true if the file extension indicates a video format.
func isVideoFile(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	return ext == ".mp4" || ext == ".webm" || ext == ".avi" || ext == ".mov" || ext == ".mkv"
}

// getVideoDuration uses ffprobe to get the duration of a video file in seconds.
// Returns 0 and an error if ffprobe is not available or fails.
func getVideoDuration(videoPath string) (float64, error) {
	if !ffmpegAvailable {
		return 0, fmt.Errorf("ffprobe not available")
	}

	cmd := exec.Command("ffprobe",
		"-v", "error",
		"-show_entries", "format=duration",
		"-of", "default=noprint_wrappers=1:nokey=1",
		"-i", videoPath,
	)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return 0, fmt.Errorf("ffprobe failed: %w, stderr: %s", err, stderr.String())
	}

	durationStr := strings.TrimSpace(stdout.String())
	if durationStr == "" || durationStr == "N/A" {
		return 0, fmt.Errorf("ffprobe returned no duration for %s", videoPath)
	}

	duration, err := strconv.ParseFloat(durationStr, 64)
	if err != nil {
		return 0, fmt.Errorf("parse duration %q: %w", durationStr, err)
	}

	return duration, nil
}

// extractVideoFrame extracts a single frame from a video file at the specified
// timestamp (in seconds) and writes it to outputPath as a JPEG image.
func extractVideoFrame(videoPath string, timestamp float64, outputPath string) error {
	if !ffmpegAvailable {
		return fmt.Errorf("ffmpeg not available")
	}

	// Format timestamp as HH:MM:SS.mmm for ffmpeg
	hours := int(timestamp / 3600)
	minutes := int((timestamp - float64(hours)*3600) / 60)
	seconds := timestamp - float64(hours)*3600 - float64(minutes)*60
	seekStr := fmt.Sprintf("%02d:%02d:%06.3f", hours, minutes, seconds)

	cmd := exec.Command("ffmpeg",
		"-ss", seekStr, // Seek to timestamp
		"-i", videoPath, // Input file
		"-vframes", "1", // Extract 1 frame
		"-q:v", "2", // High quality JPEG
		"-y",       // Overwrite output
		"-nostdin", // No stdin interaction
		outputPath,
	)

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("ffmpeg frame extraction failed: %w, stderr: %s", err, stderr.String())
	}

	// Verify output file exists and has content
	info, err := os.Stat(outputPath)
	if err != nil {
		return fmt.Errorf("ffmpeg output file missing: %w", err)
	}
	if info.Size() == 0 {
		os.Remove(outputPath)
		return fmt.Errorf("ffmpeg produced empty output for %s at %s", videoPath, seekStr)
	}

	return nil
}

// convertToWebP uses ffmpeg to convert an input image file to WebP format at
// the specified output path. If ffmpeg is not available or the conversion fails,
// it falls back to using the Go imaging library to produce a JPEG thumbnail.
func convertToWebP(inputPath, outputPath string) error {
	if !ffmpegAvailable {
		return fmt.Errorf("ffmpeg not available â€” use JPEG fallback")
	}

	cmd := exec.Command("ffmpeg",
		"-i", inputPath,
		"-y",              // Overwrite output
		"-nostdin",        // No stdin interaction
		"-c:v", "libwebp", // Use libwebp encoder explicitly
		"-lossless", "0", // Lossy mode for smaller files
		"-compression_level", "4", // Good quality/speed tradeoff (0-6)
		"-q:v", "80", // Quality factor (0-100, higher = better)
		outputPath)

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("ffmpeg WebP conversion failed: %w, stderr: %s", err, stderr.String())
	}

	// Verify output
	info, err := os.Stat(outputPath)
	if err != nil {
		return fmt.Errorf("WebP output file missing: %w", err)
	}
	if info.Size() == 0 {
		os.Remove(outputPath)
		return fmt.Errorf("ffmpeg produced empty WebP output")
	}

	return nil
}

// ============================================================
// Thumbnail Pool (Async Generation)
// ============================================================

// streamDecodeSem caps how many streamResponsiveImage decodes+resizes run
// concurrently. Each decode fully materializes the source image (tens of MB
// for tall manga strips; the WebP clamp path holds a second copy), so this
// bounds the path's transient memory regardless of how many connection slots
// are active. Streaming itself is not gated â€” only the CPU/RAM-heavy segment.
var streamDecodeSem = make(chan struct{}, 3)

type ThumbnailJob struct {
	OriginalPath string
	Section      string
	Width        int // Target width for responsive images
	Height       int // Target height for responsive images
	Bucket       int // Size bucket (calculated from min(width, height))
	Result       chan ThumbnailResult
}

type ThumbnailResult struct {
	Path    string
	Size    int64
	Error   error
	Latency time.Duration
	Bucket  int // The bucket size used
}

type ResponsiveCacheKey struct {
	Path   string
	Bucket int
}

type OrphanedThumbnail struct {
	CachePath string    `json:"cache_path"`
	Size      int64     `json:"size"`
	ModTime   time.Time `json:"mod_time"`
}

type ThumbnailAuditResult struct {
	Section      string              `json:"section"`
	Total        int                 `json:"total"`
	Generated    int                 `json:"generated"`
	Missing      int                 `json:"missing"`
	Failed       int                 `json:"failed"`
	Orphaned     int                 `json:"orphaned"`
	Orphans      []OrphanedThumbnail `json:"orphans,omitempty"`
	MissingFiles []MediaFile         `json:"missing_files,omitempty"`
}

type ThumbnailPool struct {
	queue        chan ThumbnailJob // responsive-bucket jobs (workers-1 consumers)
	thumbQueue   chan ThumbnailJob // bucket-0 thumbnail jobs (1 dedicated consumer)
	workers      int
	cacheDir     string
	cacheHits    atomic.Uint64
	cacheMiss    atomic.Uint64
	wg           sync.WaitGroup
	shuttingDown atomic.Bool

	// queuedMu guards queuedJobs, which enables cheap dedup: TrySubmit skips
	// enqueueing a job whose section|path|bucket key is already queued. This
	// prevents a fast gallery scroll from stacking hundreds of duplicate jobs
	// (memory-cheap per job, but each one later holds a fully decoded original
	// in a worker). Keys are removed by the worker as the job starts running.
	queuedMu   sync.Mutex
	queuedJobs map[string]bool
}

var thumbnailPool *ThumbnailPool

func NewThumbnailPool(workers int, cacheDir string) *ThumbnailPool {
	if workers <= 0 {
		workers = 5
	}
	// Two queues partition the workers: exactly one consumes bucket-0
	// thumbnail jobs, the rest consume responsive-bucket jobs. This keeps a
	// flood of responsive jobs from starving thumbnails and caps how many
	// huge originals the thumbnail tier decodes at once (1).
	tp := &ThumbnailPool{
		queue:      make(chan ThumbnailJob, 1000),
		thumbQueue: make(chan ThumbnailJob, 1000),
		workers:    workers,
		cacheDir:   cacheDir,
		queuedJobs: make(map[string]bool),
	}
	tp.Start()
	return tp
}

func (tp *ThumbnailPool) Start() {
	tp.wg.Add(1)
	go tp.thumbWorker()
	for i := 0; i < tp.workers-1; i++ {
		tp.wg.Add(1)
		go tp.worker(i)
	}
}

func (tp *ThumbnailPool) worker(id int) {
	tp.runWorker(tp.queue)
}

func (tp *ThumbnailPool) thumbWorker() {
	tp.runWorker(tp.thumbQueue)
}

func (tp *ThumbnailPool) runWorker(queue chan ThumbnailJob) {
	defer tp.wg.Done()
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[WORKER PANIC] Worker recovered from panic: %v", r)
		}
	}()
	for job := range queue {
		start := time.Now()
		path, size, err := tp.generateThumbnail(job)
		// Release the dedup key only after generation finishes (success or
		// failure) so requests arriving while the job runs are also deduped â€”
		// otherwise a slow generation window would keep re-queueing copies.
		tp.queuedMu.Lock()
		delete(tp.queuedJobs, job.poolKey())
		tp.queuedMu.Unlock()

		job.Result <- ThumbnailResult{
			Path:    path,
			Size:    size,
			Error:   err,
			Latency: time.Since(start),
			Bucket:  job.Bucket,
		}
	}
}

func (job *ThumbnailJob) poolKey() string {
	return job.Section + "|" + job.OriginalPath + "|" + strconv.Itoa(job.Bucket)
}

func (tp *ThumbnailPool) enqueue(job ThumbnailJob, queue chan ThumbnailJob) error {
	select {
	case queue <- job:
		debugLog("Thumbnail job queued: %s (bucket: %d)", job.OriginalPath, job.Bucket)
		return nil
	default:
		return fmt.Errorf("thumbnail queue full")
	}
}

func (tp *ThumbnailPool) Submit(job ThumbnailJob) {
	if tp.shuttingDown.Load() {
		job.Result <- ThumbnailResult{Error: fmt.Errorf("thumbnail pool shutting down")}
		return
	}
	if err := tp.enqueue(job, tp.queueFor(job)); err != nil {
		job.Result <- ThumbnailResult{Error: err}
	}
}

// TrySubmit enqueues a job without ever blocking or erroring the caller's
// channel; used by deferred (stale-while-revalidate) handlers where a full
// queue or shutdown just means background generation is skipped. Jobs whose
// section|path|bucket key is already queued are dropped silently â€” the
// in-flight copy will produce the cached file either way.
func (tp *ThumbnailPool) TrySubmit(job ThumbnailJob) error {
	if tp.shuttingDown.Load() {
		return fmt.Errorf("thumbnail pool shutting down")
	}
	key := job.poolKey()
	tp.queuedMu.Lock()
	if tp.queuedJobs[key] {
		tp.queuedMu.Unlock()
		return nil
	}
	tp.queuedJobs[key] = true
	tp.queuedMu.Unlock()

	if err := tp.enqueue(job, tp.queueFor(job)); err != nil {
		tp.queuedMu.Lock()
		delete(tp.queuedJobs, key)
		tp.queuedMu.Unlock()
		return err
	}
	return nil
}

func (tp *ThumbnailPool) queueFor(job ThumbnailJob) chan ThumbnailJob {
	if job.Bucket == 0 {
		return tp.thumbQueue
	}
	return tp.queue
}

func (tp *ThumbnailPool) Stop() {
	tp.shuttingDown.Store(true)
	close(tp.queue)
	close(tp.thumbQueue)
	tp.wg.Wait()
}

// GetCachePath returns the cache path for a thumbnail or responsive image.
// For thumbnails (bucket 0), all output is in WebP format (.webp extension).
// For responsive images (bucket > 0), the original extension is preserved
// (WebP source files use .jpg since the Go imaging library doesn't encode WebP).
func (tp *ThumbnailPool) GetCachePath(relPath string, bucket int) string {
	// Normalize path separators to forward slashes before hashing so cache keys
	// are consistent regardless of whether paths came from the DB (forward
	// slashes) or from SanitizePath/filepath.Clean (backslashes on Windows).
	normalizedPath := filepath.ToSlash(relPath)

	hash := sha256.Sum256([]byte(normalizedPath))
	hashStr := hex.EncodeToString(hash[:])[:16]

	if bucket > 0 {
		// Responsive cache structure â€” preserve original extension
		ext := filepath.Ext(normalizedPath)
		if strings.ToLower(ext) == ".webp" {
			ext = ".jpg"
		}
		return filepath.Join(tp.cacheDir, "responsive", strconv.Itoa(bucket), hashStr+ext)
	}

	// Thumbnail (bucket 0) â€” always WebP format for smaller file sizes
	return filepath.Join(tp.cacheDir, hashStr+".webp")
}

// CheckCache checks if a cached responsive image exists for the exact requested bucket.
// Returns (cachePath, bucket, true) on exact hit, or ("", 0, false) on miss.
// Does NOT fall back to smaller buckets â€” a much smaller cached image looks worse
// than generating the correct size on-the-fly, and the size mismatch causes
// Content-Length discrepancies between HEAD and GET responses.
func (tp *ThumbnailPool) CheckCache(relPath string, bucket int) (string, int, bool) {
	if bucket == 0 {
		// Thumbnail tier â€” check WebP path first (current), then alternative paths
		cachePath := tp.GetCachePath(relPath, 0)
		if _, err := os.Stat(cachePath); err == nil {
			tp.cacheHits.Add(1)
			return cachePath, 0, true
		}
		// Fallback: check legacy JPEG thumbnail (before WebP migration)
		legacyPath := strings.TrimSuffix(cachePath, ".webp") + ".jpg"
		if _, err := os.Stat(legacyPath); err == nil {
			tp.cacheHits.Add(1)
			return legacyPath, 0, true
		}
	} else {
		// Responsive tier â€” only exact match, no fallbacks
		cachePath := tp.GetCachePath(relPath, bucket)
		if _, err := os.Stat(cachePath); err == nil {
			tp.cacheHits.Add(1)
			return cachePath, bucket, true
		}
	}

	tp.cacheMiss.Add(1)
	return "", 0, false
}

func (tp *ThumbnailPool) AuditSection(section string, db *InMemoryDB) (ThumbnailAuditResult, map[string]bool) {
	db.mu.RLock()

	result := ThumbnailAuditResult{
		Section: section,
	}

	paths, ok := db.filesBySection[section]
	if !ok {
		db.mu.RUnlock()
		return result, nil
	}

	type fileEntry struct {
		path string
		file MediaFile
	}

	var entries []fileEntry
	for path := range paths {
		if f, exists := db.files[path]; exists {
			entries = append(entries, fileEntry{path: path, file: f})
		}
	}
	db.mu.RUnlock()

	hashSet := make(map[string]bool, len(entries))
	result.Total = len(paths)

	for _, e := range entries {
		hash := sha256.Sum256([]byte(e.path))
		hashStr := hex.EncodeToString(hash[:])[:16]
		hashSet[hashStr] = true

		cachePath := tp.GetCachePath(e.path, 0)
		found := false
		if _, err := os.Stat(cachePath); err == nil {
			found = true
		}
		if !found {
			legacyPath := strings.TrimSuffix(cachePath, ".webp") + ".jpg"
			if _, err := os.Stat(legacyPath); err == nil {
				found = true
			}
		}

		if found {
			result.Generated++
		} else {
			ext := strings.ToLower(filepath.Ext(e.path))
			if (isVideoFile(e.path) || ext == ".gif") && !ffmpegAvailable {
				result.Failed++
			} else {
				result.Missing++
				result.MissingFiles = append(result.MissingFiles, e.file)
			}
		}
	}

	return result, hashSet
}

func (tp *ThumbnailPool) FindOrphans(section string, db *InMemoryDB, hashSet ...map[string]bool) []OrphanedThumbnail {
	var hs map[string]bool
	if len(hashSet) > 0 && hashSet[0] != nil {
		hs = hashSet[0]
	} else {
		db.mu.RLock()
		hs = make(map[string]bool)
		if paths, ok := db.filesBySection[section]; ok {
			for path := range paths {
				hash := sha256.Sum256([]byte(path))
				hashStr := hex.EncodeToString(hash[:])[:16]
				hs[hashStr] = true
			}
		}
		db.mu.RUnlock()
	}

	var orphans []OrphanedThumbnail

	entries, err := os.ReadDir(tp.cacheDir)
	if err != nil {
		return orphans
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		name := entry.Name()
		ext := strings.ToLower(filepath.Ext(name))
		if ext != ".webp" && ext != ".jpg" {
			continue
		}

		hashPrefix := strings.TrimSuffix(name, filepath.Ext(name))
		if len(hashPrefix) != 16 {
			continue
		}

		if hs[hashPrefix] {
			continue
		}

		info, err := entry.Info()
		if err != nil {
			continue
		}

		orphans = append(orphans, OrphanedThumbnail{
			CachePath: filepath.Join(tp.cacheDir, name),
			Size:      info.Size(),
			ModTime:   info.ModTime(),
		})
	}

	return orphans
}

func (tp *ThumbnailPool) CullOrphans(orphans []OrphanedThumbnail) (int, error) {
	deleted := 0
	var firstErr error
	for _, orphan := range orphans {
		if err := os.Remove(orphan.CachePath); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		deleted++
	}
	return deleted, firstErr
}

// cacheCapBytes bounds the thumbnail disk cache. When the cache exceeds this,
// the oldest files are deleted until it fits. 2GB is generous for a
// handful-of-clients server while keeping disk usage predictable.
const cacheCapBytes = 2 << 30 // 2 GiB

// CullDeletedSources removes bucket-0 thumbnails whose source file is no
// longer in the DB (deleted or moved). Called on every periodic rescan so the
// cache self-cleans instead of growing forever.
func (tp *ThumbnailPool) CullDeletedSources(db *InMemoryDB) int {
	orphans := tp.FindOrphans(SectionImages, db)
	deleted, _ := tp.CullOrphans(orphans)
	return deleted
}

// EnforceSizeCap deletes the oldest cache files (by mtime) until the cache
// fits within capBytes. Only top-level thumbnail files are counted; the
// responsive subdirectory is included in the walk. Returns the count of
// deleted files.
func (tp *ThumbnailPool) EnforceSizeCap(capBytes int64) int {
	if capBytes <= 0 {
		return 0
	}
	var total int64
	type entry struct {
		path string
		mod  time.Time
		size int64
	}
	var files []entry
	filepath.Walk(tp.cacheDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil //nolint:nilerr // walk errors just skip the file
		}
		ext := strings.ToLower(filepath.Ext(path))
		if ext != ".webp" && ext != ".jpg" && ext != ".jpeg" {
			return nil
		}
		total += info.Size()
		files = append(files, entry{path: path, mod: info.ModTime(), size: info.Size()})
		return nil
	})
	if total <= capBytes {
		return 0
	}
	sort.Slice(files, func(i, j int) bool { return files[i].mod.Before(files[j].mod) })
	deleted := 0
	for _, f := range files {
		if total <= capBytes {
			break
		}
		if err := os.Remove(f.path); err != nil {
			continue
		}
		total -= f.size
		deleted++
	}
	return deleted
}

func (tp *ThumbnailPool) generateThumbnail(job ThumbnailJob) (string, int64, error) {
	// Snapshot config once under the lock; thumbnail workers run in background
	// goroutines and must not race a concurrent config writer.
	cfg := getCurrentConfig()
	if cfg == nil {
		return "", 0, fmt.Errorf("config not initialized")
	}
	// Find original file
	dir := cfg.Directories[job.Section]
	if dir == "" {
		return "", 0, fmt.Errorf("section not configured: %s", job.Section)
	}

	originalPath := filepath.Join(dir, job.OriginalPath)
	if _, err := os.Stat(originalPath); err != nil {
		return "", 0, err
	}

	// Determine file type
	ext := strings.ToLower(filepath.Ext(originalPath))
	isVideo := isVideoFile(originalPath)
	isAnimated := ext == ".gif"
	isImage := ext == ".jpg" || ext == ".jpeg" || ext == ".png" || ext == ".webp" || ext == ".bmp"

	cachePath := tp.GetCachePath(job.OriginalPath, job.Bucket)

	// Check if already cached
	if info, err := os.Stat(cachePath); err == nil {
		return cachePath, info.Size(), nil
	}
	// Also check legacy JPEG thumbnails for images (before WebP migration)
	if !isVideo && job.Bucket == 0 && strings.HasSuffix(cachePath, ".webp") {
		legacyJPGPath := strings.TrimSuffix(cachePath, ".webp") + ".jpg"
		if info, err := os.Stat(legacyJPGPath); err == nil {
			return legacyJPGPath, info.Size(), nil
		}
	}

	// Create cache directory
	os.MkdirAll(filepath.Dir(cachePath), 0750)

	// ---------------------------------------------------------------
	// Video thumbnail: extract keyframe at 10% of duration, resize
	// per thumbnail rules, and save as WebP. Requires ffmpeg.
	// ---------------------------------------------------------------
	if isVideo {
		if ffmpegAvailable {
			// Get video duration to calculate 10% timestamp
			duration, durationErr := getVideoDuration(originalPath)
			seekTime := duration * 0.1 // 10% into the video

			// If duration is unknown, try seeking to 1 second as a reasonable default
			if durationErr != nil || seekTime < 1.0 {
				log.Printf("[THUMB] Could not determine duration for %s (%v), using 1s seek", job.OriginalPath, durationErr)
				seekTime = 1.0
			}

			// Extract frame to a temporary JPEG file
			tmpFrame, tmpErr := os.CreateTemp("", "video-frame-*.jpg")
			if tmpErr != nil {
				log.Printf("[THUMB] Failed to create temp file for video frame extraction: %v", tmpErr)
				// Fall through to error
			} else {
				tmpFramePath := tmpFrame.Name()
				tmpFrame.Close()
				defer os.Remove(tmpFramePath)

				if extractErr := extractVideoFrame(originalPath, seekTime, tmpFramePath); extractErr != nil {
					log.Printf("[THUMB] Video frame extraction failed for %s: %v", job.OriginalPath, extractErr)
					// Fall through to error
				} else {
					// Frame extracted â€” now resize and save as WebP thumbnail
					src, imgErr := imaging.Open(tmpFramePath)
					if imgErr != nil {
						log.Printf("[THUMB] Failed to open extracted video frame %s: %v", tmpFramePath, imgErr)
						// Fall through to error
					} else {
						bounds := src.Bounds()
						origWidth := bounds.Dx()
						origHeight := bounds.Dy()

						// Calculate target dimensions using config thumb settings
						maxWidth := cfg.ThumbMaxWidth
						maxHeight := cfg.ThumbMaxHeight
						scaleFactor := cfg.ThumbScaleFactor
						var newWidth, newHeight int

						if scaleFactor > 0 && scaleFactor < 1.0 {
							newWidth = int(float64(origWidth) * scaleFactor)
							newHeight = int(float64(origHeight) * scaleFactor)
						} else {
							newWidth = origWidth
							newHeight = origHeight
						}
						if maxWidth > 0 && newWidth > maxWidth {
							ratio := float64(maxWidth) / float64(newWidth)
							newWidth = maxWidth
							newHeight = int(float64(newHeight) * ratio)
						}
						if maxHeight > 0 && newHeight > maxHeight {
							ratio := float64(maxHeight) / float64(newHeight)
							newHeight = maxHeight
							newWidth = int(float64(newWidth) * ratio)
						}
						if newWidth > origWidth || newHeight > origHeight {
							newWidth = origWidth
							newHeight = origHeight
						}
						if newWidth < 1 {
							newWidth = 1
						}
						if newHeight < 1 {
							newHeight = 1
						}

						dst := imaging.Thumbnail(src, newWidth, newHeight, imaging.Box)

						var thumbPath string
						if job.Bucket == 0 {
							// Thumbnail â€” save as WebP via ffmpeg, fallback to JPEG
							actualPath, webpErr := resizeAndSaveAsWebP(dst, cachePath, 80)
							if webpErr != nil {
								return "", 0, fmt.Errorf("failed to save video thumbnail: %w", webpErr)
							}
							thumbPath = actualPath
						} else {
							// Responsive video frame â€” save as JPEG (bucket path uses original ext)
							if saveErr := imaging.Save(dst, cachePath, imaging.JPEGQuality(85)); saveErr != nil {
								return "", 0, fmt.Errorf("failed to save video frame thumbnail: %w", saveErr)
							}
							thumbPath = cachePath
						}

						debugLog("Generated video thumbnail: %s (frame at %.1fs, %dx%d -> %dx%d, bucket: %d)",
							job.OriginalPath, seekTime, origWidth, origHeight, newWidth, newHeight, job.Bucket)

						info, _ := os.Stat(thumbPath)
						return thumbPath, info.Size(), nil
					}
				}
			}
		}

		// No ffmpeg available â€” cannot generate a video thumbnail.
		// Videos must never be streamed as thumbnails; return an error.
		return "", 0, fmt.Errorf("ffmpeg required for video thumbnail: %s", job.OriginalPath)
	}

	// ---------------------------------------------------------------
	// Animated image thumbnail (GIF): extract first frame via ffmpeg,
	// resize, and save as static WebP. If ffmpeg unavailable, return
	// error â€” never stream a raw animated file as a thumbnail.
	// ---------------------------------------------------------------
	if isAnimated {
		if !ffmpegAvailable {
			return "", 0, fmt.Errorf("ffmpeg required for animated thumbnail (GIF): %s", job.OriginalPath)
		}

		// Extract first frame to a temporary JPEG file
		tmpFrame, tmpErr := os.CreateTemp("", "gif-frame-*.jpg")
		if tmpErr != nil {
			return "", 0, fmt.Errorf("failed to create temp file for GIF frame: %w", tmpErr)
		}
		tmpFramePath := tmpFrame.Name()
		tmpFrame.Close()
		defer os.Remove(tmpFramePath)

		// Seek to 0 seconds â€” first frame
		if extractErr := extractVideoFrame(originalPath, 0, tmpFramePath); extractErr != nil {
			return "", 0, fmt.Errorf("failed to extract GIF first frame: %w", extractErr)
		}

		// Open the extracted frame and resize it as a thumbnail
		src, imgErr := imaging.Open(tmpFramePath)
		if imgErr != nil {
			return "", 0, fmt.Errorf("failed to open extracted GIF frame: %w", imgErr)
		}

		bounds := src.Bounds()
		origWidth := bounds.Dx()
		origHeight := bounds.Dy()

		// Calculate target dimensions using same logic as video thumbnails
		maxWidth := cfg.ThumbMaxWidth
		maxHeight := cfg.ThumbMaxHeight
		scaleFactor := cfg.ThumbScaleFactor
		var newWidth, newHeight int

		if scaleFactor > 0 && scaleFactor < 1.0 {
			newWidth = int(float64(origWidth) * scaleFactor)
			newHeight = int(float64(origHeight) * scaleFactor)
		} else {
			newWidth = origWidth
			newHeight = origHeight
		}
		if maxWidth > 0 && newWidth > maxWidth {
			ratio := float64(maxWidth) / float64(newWidth)
			newWidth = maxWidth
			newHeight = int(float64(newHeight) * ratio)
		}
		if maxHeight > 0 && newHeight > maxHeight {
			ratio := float64(maxHeight) / float64(newHeight)
			newHeight = maxHeight
			newWidth = int(float64(newWidth) * ratio)
		}
		if newWidth > origWidth || newHeight > origHeight {
			newWidth = origWidth
			newHeight = origHeight
		}
		if newWidth < 1 {
			newWidth = 1
		}
		if newHeight < 1 {
			newHeight = 1
		}

		dst := imaging.Thumbnail(src, newWidth, newHeight, imaging.Box)

		if job.Bucket == 0 {
			actualPath, webpErr := resizeAndSaveAsWebP(dst, cachePath, 80)
			if webpErr != nil {
				return "", 0, webpErr
			}
			cachePath = actualPath
		} else {
			if saveErr := imaging.Save(dst, cachePath, imaging.JPEGQuality(85)); saveErr != nil {
				return "", 0, fmt.Errorf("failed to save GIF frame thumbnail: %w", saveErr)
			}
		}

		debugLog("Generated animated thumbnail: %s (%dx%d -> %dx%d, bucket: %d)",
			job.OriginalPath, origWidth, origHeight, newWidth, newHeight, job.Bucket)

		info, _ := os.Stat(cachePath)
		return cachePath, info.Size(), nil
	}

	// ---------------------------------------------------------------
	// Image thumbnail generation (static images only)
	// ---------------------------------------------------------------
	if isImage {
		src, err := imaging.Open(originalPath)
		if err != nil {
			return "", 0, fmt.Errorf("failed to open image: %w", err)
		}

		// Get original dimensions
		bounds := src.Bounds()
		origWidth := bounds.Dx()
		origHeight := bounds.Dy()

		// Calculate target dimensions
		var newWidth, newHeight int

		if job.Width > 0 || job.Height > 0 {
			// Responsive sizing: use actual requested dimensions (with DPR already applied)
			requestedWidth := job.Width
			requestedHeight := job.Height

			if origWidth > origHeight {
				if origWidth > requestedWidth {
					newWidth = requestedWidth
					newHeight = int(float64(origHeight) * float64(requestedWidth) / float64(origWidth))
				} else {
					newWidth = origWidth
					newHeight = origHeight
				}
			} else {
				if origHeight > requestedHeight {
					newHeight = requestedHeight
					newWidth = int(float64(origWidth) * float64(requestedHeight) / float64(origHeight))
				} else {
					newWidth = origWidth
					newHeight = origHeight
				}
			}
		} else if job.Bucket > 0 {
			maxDim := job.Bucket
			if origWidth > origHeight {
				if origWidth > maxDim {
					newWidth = maxDim
					newHeight = int(float64(origHeight) * float64(maxDim) / float64(origWidth))
				} else {
					newWidth = origWidth
					newHeight = origHeight
				}
			} else {
				if origHeight > maxDim {
					newHeight = maxDim
					newWidth = int(float64(origWidth) * float64(maxDim) / float64(origHeight))
				} else {
					newWidth = origWidth
					newHeight = origHeight
				}
			}
		} else {
			// Legacy sizing using config (thumbnail / bucket 0)
			maxWidth := cfg.ThumbMaxWidth
			maxHeight := cfg.ThumbMaxHeight
			scaleFactor := cfg.ThumbScaleFactor

			if scaleFactor > 0 && scaleFactor < 1.0 {
				newWidth = int(float64(origWidth) * scaleFactor)
				newHeight = int(float64(origHeight) * scaleFactor)
			} else {
				newWidth = origWidth
				newHeight = origHeight
			}

			if maxWidth > 0 && newWidth > maxWidth {
				ratio := float64(maxWidth) / float64(newWidth)
				newWidth = maxWidth
				newHeight = int(float64(newHeight) * ratio)
			}
			if maxHeight > 0 && newHeight > maxHeight {
				ratio := float64(maxHeight) / float64(newHeight)
				newHeight = maxHeight
				newWidth = int(float64(newWidth) * ratio)
			}
		}

		// Ensure we don't upscale
		if newWidth > origWidth || newHeight > origHeight {
			newWidth = origWidth
			newHeight = origHeight
		}

		// Ensure minimum dimensions (1x1)
		if newWidth < 1 {
			newWidth = 1
		}
		if newHeight < 1 {
			newHeight = 1
		}

		// Resize using Box resampling (fast, sufficient for cache-tier thumbs)
		dst := imaging.Thumbnail(src, newWidth, newHeight, imaging.Box)

		if job.Bucket == 0 {
			// Thumbnail tier â€” output as WebP for smaller file sizes
			// resizeAndSaveAsWebP uses ffmpeg if available, falls back to JPEG
			actualPath, webpErr := resizeAndSaveAsWebP(dst, cachePath, 80)
			if webpErr != nil {
				return "", 0, fmt.Errorf("failed to save thumbnail: %w", webpErr)
			}
			// Use the actual path (may differ from cachePath if JPEG fallback was used)
			cachePath = actualPath
		} else {
			// Responsive tier â€” save with original extension (cache path already has correct ext)
			srcExt := strings.ToLower(filepath.Ext(originalPath))
			if srcExt == ".webp" {
				err = imaging.Save(dst, cachePath, imaging.JPEGQuality(85))
			} else {
				err = imaging.Save(dst, cachePath)
			}
			if err != nil {
				return "", 0, fmt.Errorf("failed to save thumbnail: %w", err)
			}
		}

		debugLog("Generated thumbnail: %s (%dx%d -> %dx%d, bucket: %d)", job.OriginalPath, origWidth, origHeight, newWidth, newHeight, job.Bucket)
	}

	info, _ := os.Stat(cachePath)
	return cachePath, info.Size(), nil
}

// resizeAndSaveAsWebP saves a resized image as WebP. It first saves the image
// to a temporary JPEG file, then converts it to WebP using ffmpeg (if available).
// If ffmpeg is not available, it falls back to saving directly as JPEG.
// Returns the actual file path where the thumbnail was saved (may differ from
// cachePath if a JPEG fallback was used).
func resizeAndSaveAsWebP(dst *image.NRGBA, cachePath string, quality int) (string, error) {
	if ffmpegAvailable {
		// Save resized image to temp JPEG first
		tmpJPEG, err := os.CreateTemp("", "thumb-*.jpg")
		if err != nil {
			// Can't create temp file â€” fall back to JPEG
			log.Printf("[THUMB] Failed to create temp file for WebP conversion: %v, falling back to JPEG", err)
			return saveAsJPEGFallback(dst, cachePath)
		}
		tmpJPEGPath := tmpJPEG.Name()
		tmpJPEG.Close()
		defer os.Remove(tmpJPEGPath)

		// Save resized image as high-quality JPEG to temp file
		if err := imaging.Save(dst, tmpJPEGPath, imaging.JPEGQuality(95)); err != nil {
			return "", fmt.Errorf("failed to save temp JPEG for WebP conversion: %w", err)
		}

		// Convert JPEG â†’ WebP using ffmpeg
		if err := convertToWebP(tmpJPEGPath, cachePath); err != nil {
			log.Printf("[THUMB] ffmpeg WebP conversion failed: %v, falling back to JPEG for %s", err, cachePath)
			// ffmpeg conversion failed â€” save as JPEG under .jpg path
			_ = os.Remove(cachePath) // remove any partial WebP
			return saveAsJPEGFallback(dst, cachePath)
		}

		return cachePath, nil
	}

	// No ffmpeg â€” fall back to saving as JPEG using the Go imaging library
	return saveAsJPEGFallback(dst, cachePath)
}

// saveAsJPEGFallback saves the thumbnail as JPEG when ffmpeg/WebP is unavailable.
// It replaces the .webp extension in cachePath with .jpg and saves there.
// Returns the actual path where the file was saved.
func saveAsJPEGFallback(dst *image.NRGBA, cachePath string) (string, error) {
	// Since bucket-0 thumbs use .webp in cache path, but we can't produce WebP,
	// save as JPEG under a .jpg path instead.
	jpegPath := strings.TrimSuffix(cachePath, ".webp") + ".jpg"
	if err := imaging.Save(dst, jpegPath, imaging.JPEGQuality(80)); err != nil {
		return "", fmt.Errorf("failed to save JPEG fallback thumbnail: %w", err)
	}
	return jpegPath, nil
}

func (tp *ThumbnailPool) GetStats() (hits, misses uint64) {
	return tp.cacheHits.Load(), tp.cacheMiss.Load()
}

// GenerateThumbnail generates a thumbnail for a specific file synchronously
func GenerateThumbnail(relPath, section string) error {
	if thumbnailPool == nil {
		return fmt.Errorf("thumbnail pool not initialized")
	}

	// Use bucket 0 (config-based sizing)
	job := ThumbnailJob{
		OriginalPath: relPath,
		Section:      section,
		Bucket:       0,
		Result:       make(chan ThumbnailResult, 1),
	}

	thumbnailPool.Submit(job)

	// Wait for result with timeout
	select {
	case result := <-job.Result:
		return result.Error
	case <-time.After(60 * time.Second):
		return fmt.Errorf("thumbnail generation timeout")
	}
}

func (tp *ThumbnailPool) QueueLen() int {
	return len(tp.queue)
}

// webpMaxDimension is the maximum width or height that the libwebp encoder can
// handle. Images exceeding this in either dimension will fail to encode as WebP
// via ffmpeg. When an image exceeds this limit after responsive resizing, we
// either clamp it down proportionally (losing a small amount of resolution) or
// fall back to JPEG encoding which supports dimensions up to 65535.
const webpMaxDimension = 16383

// calcResizeDimensions calculates target dimensions for responsive image resizing,
// maintaining aspect ratio. This logic is shared between the streaming path
// (streamResponsiveImage) and the thumbnail path (generateThumbnail) to avoid
// drift. width and height are the viewport dimensions (before DPR scaling).
// dpr is the device pixel ratio. Returns the target pixel dimensions after DPR
// application and aspect-ratio-preserving resize.
func calcResizeDimensions(origWidth, origHeight, width, height int, dpr float64) (newWidth, newHeight int) {
	scaledWidth := int(float64(width) * dpr)
	scaledHeight := int(float64(height) * dpr)

	// Width-only mode (height == 0): constrain by width only.
	// Used for manga/h-manga vertical strip readers where height is unconstrained.
	if scaledHeight <= 0 {
		if origWidth > scaledWidth {
			newWidth = scaledWidth
			newHeight = int(float64(origHeight) * float64(scaledWidth) / float64(origWidth))
		} else {
			newWidth = origWidth
			newHeight = origHeight
		}
	} else if origWidth > origHeight {
		// Landscape
		if origWidth > scaledWidth {
			newWidth = scaledWidth
			newHeight = int(float64(origHeight) * float64(scaledWidth) / float64(origWidth))
		} else {
			newWidth = origWidth
			newHeight = origHeight
		}
	} else {
		// Portrait
		if origHeight > scaledHeight {
			newHeight = scaledHeight
			newWidth = int(float64(origWidth) * float64(scaledHeight) / float64(origHeight))
		} else {
			newWidth = origWidth
			newHeight = origHeight
		}
	}

	// Don't upscale
	if newWidth > origWidth || newHeight > origHeight {
		newWidth = origWidth
		newHeight = origHeight
	}
	if newWidth < 1 {
		newWidth = 1
	}
	if newHeight < 1 {
		newHeight = 1
	}

	return newWidth, newHeight
}

// streamResponsiveImage generates a viewport-sized image and streams it directly
// to the HTTP response using chunked transfer encoding, without buffering the
// entire output in memory. This allows the browser to begin decoding and rendering
// the image as data arrives, providing a significantly better perceived load time
// compared to buffering the entire response before sending.
//
// The function tries these formats in order:
//  1. WebP via ffmpeg piped directly to the response (best compression + streaming)
//  2. JPEG via Go imaging library streamed to the response
//  3. Original file served directly via http.ServeContent (already streams)
//
// WebP does not support progressive/interlaced display in the JPEG sense, but it
// does support incremental decoding â€” browsers use all available bytes to render
// rows as they arrive. By streaming the output via chunked transfer encoding,
// bytes reach the browser during encoding rather than after, enabling this
// incremental rendering. This is the primary benefit: eliminating the latency of
// waiting for the full encode + full send before the browser sees a single pixel.
//
// For the preload/blob path (used by preloadCache on the frontend), images are
// fetched via normal GET requests that consume the full streamed response into a
// blob, which works seamlessly with chunked transfer encoding.
func streamResponsiveImage(ctx context.Context, c *gin.Context, originalPath string, width, height int, dpr float64, cfg *Config, sanitizedPath, section string, bucket, index, total int, preloadCount int, nextPaths, prevPaths []string) {
	// Check for early cancellation
	select {
	case <-ctx.Done():
		return
	default:
	}

	// Check if file exists
	if _, err := os.Stat(originalPath); err != nil {
		c.JSON(404, gin.H{"error": "Not found"})
		return
	}

	// Check if it's an image
	ext := strings.ToLower(filepath.Ext(originalPath))
	isImage := ext == ".jpg" || ext == ".jpeg" || ext == ".png" || ext == ".webp" || ext == ".bmp"
	if !isImage {
		// Not an image format we can resize â€” fall back to streaming the original
		c.File(originalPath)
		return
	}

	// GIFs should be served as-is via streaming (animated format, skip resizing)
	if ext == ".gif" {
		c.File(originalPath)
		return
	}

	// Check for cancellation before opening image
	select {
	case <-ctx.Done():
		return
	default:
	}

	// Open the source image. The decode (and any WebP clamp re-decode) fully
	// materializes the image in RAM â€” tens of MB per tall manga strip â€” so
	// this section is capped by streamDecodeSem regardless of how many
	// connection slots are active. The slot is held through the WebP clamp
	// preparation AND the encode/stream phase, and released exactly once by
	// the deferred receive below (when the handler returns).
	select {
	case <-ctx.Done():
		return
	case streamDecodeSem <- struct{}{}:
	}
	defer func() { <-streamDecodeSem }()

	src, err := imaging.Open(originalPath)
	if err != nil {
		// Failed to open â€” fall back to streaming the original file.
		// c.File() uses http.ServeContent which supports range requests and streaming.
		log.Printf("[MEDIA STREAM] Failed to open image %s, serving original: %v", sanitizedPath, err)
		c.Header("X-Fallback", "open-error")
		c.File(originalPath)
		return
	}

	// Get original dimensions
	bounds := src.Bounds()
	origWidth := bounds.Dx()
	origHeight := bounds.Dy()

	// Calculate target dimensions using shared logic
	newWidth, newHeight := calcResizeDimensions(origWidth, origHeight, width, height, dpr)

	// Resize using Lanczos resampling
	dst := imaging.Thumbnail(src, newWidth, newHeight, imaging.Lanczos)

	// If the resized image still exceeds WebP's maximum dimension (16383px),
	// clamp it proportionally. This primarily affects very tall manga strip
	// images where the original height can exceed 16000px. The clamped dimensions
	// are used for WebP streaming; JPEG fallback handles the full resolution.
	//
	// For example, an 800Ã—16956 image becomes 800Ã—16383 â€” a mere 3.4% reduction
	// in height that preserves the reading experience while ensuring WebP encoding
	// succeeds. Without this clamp, ffmpeg would fail with "Picture size is too
	// large" and return zero bytes while already committing Content-Type: image/webp.
	clampedForWebP := false
	webpWidth, webpHeight := newWidth, newHeight
	if webpWidth > webpMaxDimension {
		ratio := float64(webpMaxDimension) / float64(webpWidth)
		webpWidth = webpMaxDimension
		webpHeight = int(float64(webpHeight) * ratio)
		if webpHeight < 1 {
			webpHeight = 1
		}
		clampedForWebP = true
	}
	if webpHeight > webpMaxDimension {
		ratio := float64(webpMaxDimension) / float64(webpHeight)
		webpHeight = webpMaxDimension
		webpWidth = int(float64(webpWidth) * ratio)
		if webpWidth < 1 {
			webpWidth = 1
		}
		clampedForWebP = true
	}

	// If we clamped for WebP, resize the image to the clamped dimensions.
	// The full-resolution dst is kept around for the JPEG fallback path.
	var webpDst *image.NRGBA
	if clampedForWebP {
		webpDst = imaging.Thumbnail(src, webpWidth, webpHeight, imaging.Lanczos)
	} else {
		webpDst = dst
	}
	// Decode + resize are done. The streamDecodeSem slot is NOT released
	// here: it is released exactly once by the deferred receive at the top
	// of this function (after acquire). An earlier version also released
	// manually at this point â€” a double-release that drained the 3-slot
	// semaphore until the deferred receive of a returning handler blocked
	// forever, wedging the request *after* its body bytes were written
	// (client saw the data but never the chunked terminator â†’ "transfer
	// cut off halfway"), and leaking the goroutine, its connection slot,
	// and its metrics entry. Keep the single release-on-return invariant.

	// Check for cancellation before encoding
	select {
	case <-ctx.Done():
		return
	default:
	}

	// Version the streamed response with the source file's mtime so the
	// client can append ?v=<mtime> to future URLs for the same file,
	// letting its HTTP/SW cache stay keyed per file version.
	setFileVersionHeaders(c, originalPath)

	// Try WebP encoding via ffmpeg if available â€” best compression + streaming.
	// ffmpeg pipes WebP output directly to the response, so bytes start flowing
	// immediately as ffmpeg encodes, without buffering the entire image first.
	if ffmpegAvailable {
		streamed := streamWebPViaFfmpeg(c, webpDst, cfg, sanitizedPath, section, bucket, index, total, preloadCount, nextPaths, prevPaths, width, height, dpr, origWidth, origHeight, webpWidth, webpHeight)
		if streamed {
			return
		}
		// ffmpeg failed â€” fall through to JPEG streaming
		log.Printf("[MEDIA STREAM] ffmpeg WebP stream failed for %s, falling back to JPEG streaming", sanitizedPath)
	}

	// Fallback: encode as JPEG and stream via chunked transfer encoding.
	// JPEG is universally supported and Go's image/jpeg supports progressive mode,
	// which produces multiple scans (coarse â†’ fine) that browsers render incrementally.
	// Even without progressive mode, streaming via c.Stream() eliminates the latency
	// of buffering the entire response before sending.

	if cfg.EnablePreloading && index >= 0 && total > 0 && (section == SectionImages || section == SectionHManga) {
		addPreloadHeadersWithCount(c, "/api/media", sanitizedPath, section, bucket, index, total, preloadCount, nextPaths, prevPaths, width, height, dpr)
	}

	// Encode as JPEG in memory, then stream to the client via chunked transfer encoding.
	// Although this still requires buffering the full encode in memory, using c.Stream()
	// with chunked transfer encoding avoids the need for a Content-Length header,
	// allowing the browser to start processing the response marginally sooner.
	//
	// This path is only used when ffmpeg is unavailable for WebP streaming â€” the
	// ffmpeg path (streamWebPViaFfmpeg) is preferred because it provides true
	// byte-by-byte streaming from the encoder output pipe directly to the client.
	var jpegBuf bytes.Buffer
	if encodeErr := imaging.Encode(&jpegBuf, dst, imaging.JPEG, imaging.JPEGQuality(85)); encodeErr != nil {
		log.Printf("[MEDIA STREAM] JPEG encode error for %s: %v", sanitizedPath, encodeErr)
		// Fall back to serving the original file â€” let c.File set the correct Content-Type
		c.Header("X-Fallback", "encode-error")
		c.File(originalPath)
		return
	}

	// Set response headers AFTER successful encode so they match the actual content.
	// If we set Content-Type before encoding and encoding fails, we'd serve the
	// original file (which may be PNG/WebP/etc.) with an incorrect Content-Type.
	c.Header("Content-Type", "image/jpeg")
	c.Header("X-Cache", "MEMORY-STREAM")
	c.Header("X-Image-Bucket", strconv.Itoa(bucket))
	c.Header("Cache-Control", mediaCacheControl)
	setFileVersionHeaders(c, originalPath)

	c.Stream(func(w io.Writer) bool {
		_, copyErr := io.Copy(w, bytes.NewReader(jpegBuf.Bytes()))
		if copyErr != nil {
			log.Printf("[MEDIA STREAM] JPEG stream error for %s: %v", sanitizedPath, copyErr)
		}
		return false // done streaming
	})

	debugLog("Streamed responsive image (JPEG fallback): %s (%dx%d -> %dx%d)", originalPath, origWidth, origHeight, newWidth, newHeight)
}

// streamWebPViaFfmpeg attempts to encode the image as WebP using ffmpeg and
// stream the output directly to the HTTP response. Returns true if the image
// was successfully streamed, false if ffmpeg failed (caller should fall back).
//
// This pipes ffmpeg's stdout directly to the HTTP response writer, so WebP
// encoded bytes reach the browser as they're produced â€” no buffering of the
// entire WebP file in memory. Combined with WebP's incremental decoding support,
// this means the browser can start rendering rows as soon as data arrives.
//
// Safety: Uses context cancellation with a configurable timeout to kill the
// ffmpeg process if it hangs or the client disconnects. The pipe goroutine
// is also cancelled on context expiry to prevent goroutine leaks.
func streamWebPViaFfmpeg(c *gin.Context, img *image.NRGBA, cfg *Config, sanitizedPath, section string, bucket, index, total int, preloadCount int, nextPaths, prevPaths []string, width, height int, dpr float64, origWidth, origHeight, newWidth, newHeight int) bool {
	// Safety check: WebP has a maximum dimension of 16383 pixels. If either
	// dimension exceeds this limit, ffmpeg/libwebp will fail with
	// "Picture size is too large" and produce zero output bytes. This results
	// in a 200 OK response with Content-Type: image/webp but an empty body.
	// Return false here so the caller falls back to JPEG streaming, which
	// supports dimensions up to 65535.
	if newWidth > webpMaxDimension || newHeight > webpMaxDimension {
		log.Printf("[MEDIA STREAM] Image dimensions %dx%d exceed WebP max of %d for %s, falling back to JPEG",
			newWidth, newHeight, webpMaxDimension, sanitizedPath)
		return false
	}

	// Create a context with timeout to prevent hung ffmpeg processes from
	// consuming resources indefinitely. The timeout mirrors the server's
	// request timeout configuration.
	timeout := time.Duration(cfg.RequestTimeoutSec) * time.Second
	if timeout < 10*time.Second {
		timeout = 10 * time.Second // minimum timeout
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), timeout)
	defer cancel()

	// Step 1: Encode the resized image as JPEG via a pipe for ffmpeg input.
	// We use io.Pipe instead of a temp file to avoid disk I/O latency.
	srcPipeReader, srcPipeWriter := io.Pipe()

	// Track pipe goroutine completion so we can wait for it before returning.
	pipeDone := make(chan struct{})

	// Write JPEG to pipe in a goroutine so we can simultaneously pipe it to ffmpeg.
	// The goroutine exits when: jpeg.Encode finishes, the write end is closed,
	// or the context is cancelled (via closing the read end).
	go func() {
		defer close(pipeDone)
		defer srcPipeWriter.Close()

		jpegErr := jpeg.Encode(srcPipeWriter, img, &jpeg.Options{Quality: 95})
		if jpegErr != nil {
			log.Printf("[MEDIA STREAM] JPEG pipe encode error for %s: %v", sanitizedPath, jpegErr)
		}
	}()

	// Step 2: Run ffmpeg with input from pipe and output WebP to a temp FILE.
	// Use CommandContext so ffmpeg is killed when the context expires (timeout
	// or client disconnect). This prevents orphaned ffmpeg processes.
	//
	// IMPORTANT: When reading from a pipe, ffmpeg cannot seek backwards to probe
	// the input format. Without an explicit -f flag, ffmpeg fails with
	// "Invalid data found when processing input" because it can't detect the
	// JPEG format from a non-seekable stream. The -f image2pipe flag tells ffmpeg
	// to expect a single JPEG image from the pipe.
	//
	// OUTPUT MUST BE A SEEKABLE FILE, NOT pipe:1. ffmpeg's WebP pipe muxer
	// writes placeholder size fields it cannot back-patch (RIFF size AND VP8
	// chunk header), producing a container that ffmpeg itself accepts but
	// Chromium's WebP demuxer rejects — modal/reader images "load and cache
	// but display blank". A temp file takes the seekable-muxer path, identical
	// to the thumbnail pipeline whose output decodes fine. Single images are
	// tens to hundreds of KB, so the extra disk IO is negligible.
	tmpOut, err := os.CreateTemp("", "mv-webp-*.webp")
	if err != nil {
		log.Printf("[MEDIA STREAM] Failed to create temp file for ffmpeg output: %v", err)
		srcPipeReader.Close()
		<-pipeDone
		return false
	}
	tmpPath := tmpOut.Name()
	tmpOut.Close()
	defer os.Remove(tmpPath)

	cmd := exec.CommandContext(ctx, "ffmpeg",
		"-nostdin",
		"-y",               // Overwrite the temp file (os.CreateTemp pre-creates it)
		"-f", "image2pipe", // Input format: JPEG image from pipe (required for pipe input)
		"-i", "pipe:0", // Read JPEG from stdin pipe
		"-c:v", "libwebp", // WebP encoder
		"-lossless", "0", // Lossy mode for smaller files
		"-q:v", "80", // Quality
		"-compression_level", "4", // Speed/quality balance
		"-f", "webp", // Output format
		tmpPath, // Seekable temp file: correct RIFF/VP8 sizes on the wire
	)
	cmd.Stdin = srcPipeReader

	// Capture stderr for diagnostics on failure
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if startErr := cmd.Start(); startErr != nil {
		log.Printf("[MEDIA STREAM] Failed to start ffmpeg: %v", startErr)
		srcPipeReader.Close()
		// Wait for pipe goroutine to finish
		<-pipeDone
		return false
	}

	// Add preload headers
	if cfg.EnablePreloading && index >= 0 && total > 0 && (section == SectionImages || section == SectionHManga) {
		addPreloadHeadersWithCount(c, "/api/media", sanitizedPath, section, bucket, index, total, preloadCount, nextPaths, prevPaths, width, height, dpr)
	}

	// Step 3: Wait for ffmpeg to finish and clean up.
	waitErr := cmd.Wait()
	if waitErr != nil {
		if ctx.Err() != nil {
			// Context cancellation (timeout or client disconnect) — expected
			debugLog("[MEDIA STREAM] ffmpeg cancelled for %s: ctx=%v cmd=%v", sanitizedPath, ctx.Err(), waitErr)
		} else {
			log.Printf("[MEDIA STREAM] ffmpeg exited with error for %s: %v (stderr: %s)", sanitizedPath, waitErr, stderr.String())
		}
	}

	// Close the pipe read end to unblock the pipe goroutine if it hasn't exited yet.
	// This is safe to call multiple times — io.PipeReader.Close returns an error
	// on second call but doesn't panic.
	srcPipeReader.Close()

	// Wait for the JPEG pipe goroutine to finish before returning.
	// This ensures we don't leak the goroutine.
	<-pipeDone

	// Step 4: Read the seekable-muxed WebP and send it complete with
	// Content-Length. The seekable muxer writes correct RIFF/VP8 sizes, so no
	// header patching is needed.
	stdoutBytes, readErr := os.ReadFile(tmpPath)
	if readErr != nil {
		log.Printf("[MEDIA STREAM] Failed to read ffmpeg temp output for %s: %v", sanitizedPath, readErr)
		return false
	}

	if len(stdoutBytes) == 0 {
		log.Printf("[MEDIA STREAM] ffmpeg produced no output for %s (stderr: %s) — falling back to JPEG", sanitizedPath, stderr.String())
		return false
	}

	// Set Content-Type and headers.
	c.Header("Content-Type", "image/webp")
	c.Header("X-Cache", "MEMORY-STREAM-WEBP")
	c.Header("X-Image-Bucket", strconv.Itoa(bucket))
	c.Header("Cache-Control", mediaCacheControl)
	c.Header("Content-Length", strconv.Itoa(len(stdoutBytes)))

	// Send the complete image. Content-Length matches the body exactly, so the
	// browser gets a deterministic, decodable response.
	if _, writeErr := c.Writer.Write(stdoutBytes); writeErr != nil {
		log.Printf("[MEDIA STREAM] Error writing WebP response for %s: %v", sanitizedPath, writeErr)
	}

	debugLog("Streamed responsive image (WebP via ffmpeg): original %s (%dx%d -> %dx%d, %d bytes)", sanitizedPath, origWidth, origHeight, newWidth, newHeight, len(stdoutBytes))
	return true
}

// ============================================================
// Server Metrics
// ============================================================

type ServerMetrics struct {
	RequestsTotal   atomic.Uint64
	RequestsActive  atomic.Int32
	ErrorsTotal     atomic.Uint64
	ThumbnailHits   atomic.Uint64
	ThumbnailMisses atomic.Uint64
	FilesScanned    atomic.Uint64
	startTime       time.Time
}

var metrics *ServerMetrics
var startTime time.Time

func NewServerMetrics() *ServerMetrics {
	return &ServerMetrics{
		startTime: time.Now(),
	}
}

func (m *ServerMetrics) RecordRequest() func() {
	m.RequestsTotal.Add(1)
	m.RequestsActive.Add(1)
	start := time.Now()

	return func() {
		m.RequestsActive.Add(-1)
		duration := time.Since(start)
		// Log slow requests. Read the lock-free debug flag instead of
		// snapshotting config under configMu on every request completion.
		if duration > 1*time.Second && debugMode.Load() {
			log.Printf("[SLOW] Request took %v", duration)
		}
	}
}

func (m *ServerMetrics) RecordError() {
	m.ErrorsTotal.Add(1)
}

func (m *ServerMetrics) GetStats() gin.H {
	// Store reference locally to avoid race condition
	tp := thumbnailPool
	thumbHits, thumbMisses := uint64(0), uint64(0)
	queueLen := 0
	if tp != nil {
		thumbHits, thumbMisses = tp.GetStats()
		queueLen = tp.QueueLen()
	}

	hitRate := float64(0)
	total := thumbHits + thumbMisses
	if total > 0 {
		hitRate = float64(thumbHits) / float64(total) * 100
	}

	return gin.H{
		"requests_total":      m.RequestsTotal.Load(),
		"requests_active":     m.RequestsActive.Load(),
		"errors_total":        m.ErrorsTotal.Load(),
		"thumbnail_hits":      thumbHits,
		"thumbnail_misses":    thumbMisses,
		"thumbnail_hit_rate":  fmt.Sprintf("%.2f%%", hitRate),
		"thumbnail_queue_len": queueLen,
		"files_scanned":       m.FilesScanned.Load(),
		"uptime":              time.Since(m.startTime).String(),
	}
}

func MetricsMiddleware(metrics *ServerMetrics) gin.HandlerFunc {
	return func(c *gin.Context) {
		done := metrics.RecordRequest()
		defer done()

		c.Next()

		// Track errors
		if c.Writer.Status() >= 400 {
			metrics.RecordError()
		}
	}
}

// ============================================================
// Security Functions
// ============================================================

// handleBulkTagUpdate implements POST /api/tags/bulk (dispatched from the
// /api/tags/*path catch-all).
func handleBulkTagUpdate(c *gin.Context, cfg *Config, db *InMemoryDB) {
	var req struct {
		Paths  []string `json:"paths"`
		Add    []string `json:"add,omitempty"`
		Remove []string `json:"remove,omitempty"`
		Set    []string `json:"set,omitempty"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(400, gin.H{"error": "Invalid request"})
		return
	}
	result := db.BulkUpdateTags(req.Paths, req.Add, req.Remove, req.Set)
	sectionsToSave := make(map[string]bool)
	for _, path := range req.Paths {
		if section := db.GetFileSection(path); section != "" {
			sectionsToSave[section] = true
		}
	}
	for section := range sectionsToSave {
		debouncedSaveIndex(cfg, db, section)
	}
	c.JSON(200, result)
}

// handleTagFlush implements POST /api/tags/flush (dispatched from the
// /api/tags/*path catch-all). Removes the given tags (or ALL tags if
// "all": true) from every file. Accepts an optional section query param to
// restrict the operation to one section. Body: { "tags": ["tag1","tag2"] }
// for a targeted flush, or { "all": true } for a full flush. An empty body or
// a malformed payload is rejected with 400 â€” we never infer flush-all from a
// missing/nil tags array, because that would turn any malformed request into
// a destructive global wipe.
func handleTagFlush(c *gin.Context, cfg *Config, db *InMemoryDB) {
	section := c.DefaultQuery("section", "")
	var req struct {
		Tags []string `json:"tags,omitempty"`
		All  bool     `json:"all,omitempty"`
	}
	// A body is required: we must not silently treat a missing or
	// malformed body as flush-all. ShouldBindJSON returns io.EOF for an
	// empty body, which we report as 400 so callers send an explicit
	// { "all": true } for a full flush.
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(400, gin.H{"error": "Invalid request body: send {\"tags\":[...]} for a targeted flush or {\"all\":true} to flush all tags"})
		return
	}

	// flushAll requires an explicit opt-in; a nil/empty tags array
	// without "all":true is a no-op error so a buggy client cannot
	// accidentally trigger a global wipe.
	flushAll := req.All
	if !flushAll && len(req.Tags) == 0 {
		c.JSON(400, gin.H{"error": "Either a non-empty tags array or {\"all\":true} is required"})
		return
	}

	log.Printf("[API] /api/tags/flush called from %s (section=%s, tags=%d, flushAll=%v)",
		c.ClientIP(), section, len(req.Tags), flushAll)

	result := db.FlushTags(req.Tags, section, flushAll)

	// Persist affected sections' indexes synchronously â€” this is a
	// destructive operation and the 200 response should imply durability.
	// debouncedSaveIndex could lose the change on an ungraceful shutdown
	// before its 5s timer fires.
	if section != "" {
		if err := saveSectionIndex(cfg, db, section); err != nil {
			log.Printf("[API] /api/tags/flush: failed to save index for %s: %v", section, err)
		}
	} else {
		for s := range cfg.Directories {
			if cfg.Directories[s] != "" {
				if err := saveSectionIndex(cfg, db, s); err != nil {
					log.Printf("[API] /api/tags/flush: failed to save index for %s: %v", s, err)
				}
			}
		}
	}
	c.JSON(200, result)
}

func SanitizePath(input string) (string, error) {
	// Remove null bytes
	input = strings.ReplaceAll(input, "\x00", "")

	// Normalize path separators to forward slashes first for consistent checking
	input = filepath.ToSlash(input)

	// Check for path-traversal components. We split into components and
	// reject any segment that is exactly ".." or "." â€” this blocks actual
	// traversal like "foo/../../etc" while allowing legitimate filenames
	// that contain ".." as a substring (e.g., "Super_Mario_Bros..png",
	// "foo..bar.png"). The previous strings.Contains(input, "..") check
	// was too broad and silently rejected those valid filenames with a
	// confusing "path traversal detected" error.
	for _, part := range strings.Split(input, "/") {
		if part == ".." || part == "." {
			return "", fmt.Errorf("path traversal detected")
		}
	}

	// Clean the path
	clean := filepath.Clean(input)

	// Check for absolute paths
	if filepath.IsAbs(clean) {
		return "", fmt.Errorf("absolute paths not allowed")
	}

	// Normalize back to forward slashes for consistent hashing and cache lookups.
	// filepath.Clean on Windows converts / to \, which would cause hash mismatches
	// between paths from the DB (forward slashes) and paths from URL requests
	// that pass through SanitizePath.
	clean = filepath.ToSlash(clean)

	// Reject suspicious patterns - only check for actual security threats
	suspicious := []string{
		"//", ".git", ".env", ".htaccess", ".htpasswd",
		".bashrc", ".profile", ".ssh", "etc/passwd", "etc/shadow",
	}
	lowerClean := strings.ToLower(clean)
	for _, pattern := range suspicious {
		if strings.Contains(lowerClean, pattern) {
			return "", fmt.Errorf("suspicious path pattern: %s", pattern)
		}
	}

	// Check for ~ only at start of path (home directory expansion attack)
	// Don't reject ~ in the middle of filenames (e.g., "~Ero Manga~")
	if strings.HasPrefix(clean, "~") {
		return "", fmt.Errorf("suspicious path pattern: leading tilde")
	}

	return clean, nil
}

// ValidateFileType checks if a file extension is allowed
func ValidateFileType(filename string) error {
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(filename), "."))
	allowed := map[string]bool{
		"jpg": true, "jpeg": true, "png": true, "gif": true,
		"webp": true, "bmp": true, "svg": true, "mp4": true,
		"webm": true, "avi": true, "mov": true, "mkv": true,
	}
	if !allowed[ext] {
		return fmt.Errorf("file type not allowed: %s", ext)
	}
	return nil
}

// ============================================================
// Security Middleware
// ============================================================

func SecurityHeadersMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("X-Content-Type-Options", "nosniff")
		c.Header("X-Frame-Options", "DENY")
		c.Header("X-XSS-Protection", "1; mode=block")
		c.Header("Referrer-Policy", "strict-origin-when-cross-origin")
		c.Header("Content-Security-Policy", "default-src 'self'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'; img-src 'self' data: blob: https:; media-src 'self' blob:") // unsafe-inline required for SPA functionality
		c.Next()
	}
}

func MaxBodySize(maxBytes int64) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxBytes)
		c.Next()
	}
}

func ValidateSection(cfg *Config) gin.HandlerFunc {
	return func(c *gin.Context) {
		section := c.Param("section")
		if section == "" {
			section = c.Query("section")
		}

		if section == "" {
			c.Next()
			return
		}

		validSections := []string{SectionImages, SectionManga, SectionHManga}
		valid := false
		for _, s := range validSections {
			if s == section {
				valid = true
				break
			}
		}

		if !valid {
			c.JSON(400, gin.H{"error": "Invalid section"})
			c.Abort()
			return
		}

		if cfg.Directories[section] == "" {
			c.JSON(400, gin.H{"error": "Section not configured: " + section})
			c.Abort()
			return
		}

		c.Next()
	}
}

// SetupCORS returns explicit CORS configuration
func SetupCORS(cfg *Config) gin.HandlerFunc {
	port := cfg.Port
	if port == 0 {
		port = 3000
	}
	// Build localhost origins from the configured port so only the actual
	// listening port is allowed. Drop the hardcoded 8080 unless the server
	// happens to be configured to listen there too (which would be unusual â€”
	// 8080 is a common dev-server port and allowing it by default lets any
	// localhost:8080 site reach the API if the server is port-forwarded).
	origins := []string{
		fmt.Sprintf("http://localhost:%d", port),
		fmt.Sprintf("http://127.0.0.1:%d", port),
	}
	// If the config also explicitly listens on 8080 (e.g. a second instance),
	// allow it; otherwise don't.
	if port != 8080 {
		log.Printf("[CORS] Allowing localhost origins on port %d (8080 not auto-allowed)", port)
	}

	// Add configured origins from environment, but only if they are valid/safe
	if allowed := os.Getenv("ALLOWED_ORIGINS"); allowed != "" {
		for _, origin := range strings.Split(allowed, ",") {
			origin = strings.TrimSpace(origin)
			if isValidOrigin(origin) {
				origins = append(origins, origin)
			} else {
				log.Printf("[CORS WARNING] Rejecting unsafe origin: %s", origin)
			}
		}
	}

	config := cors.Config{
		AllowOrigins:     origins,
		AllowMethods:     []string{"GET", "POST", "HEAD", "OPTIONS"},
		AllowHeaders:     []string{"Origin", "Content-Type", "Accept", "Authorization"},
		ExposeHeaders:    []string{"Content-Length", "Content-Type", "X-Preload-Previous", "X-Preload-Next", "X-Preload-Previous-All", "X-Preload-Next-All", "Link", "X-Cache", "X-Image-Bucket", "X-File-Mtime"},
		AllowCredentials: true,
		MaxAge:           12 * time.Hour,
	}

	return cors.New(config)
}

// isValidOrigin validates that an origin is safe to add to CORS
// Only allows localhost/127.0.0.1, HTTPS URLs, or plain-http origins whose
// host is a private (RFC1918) IPv4 address — the LAN-trust deployment target.
// Wildcards and public http origins remain rejected.
func isValidOrigin(origin string) bool {
	// Must start with http:// or https://
	if !strings.HasPrefix(origin, "http://") && !strings.HasPrefix(origin, "https://") {
		return false
	}

	// Allow localhost and 127.0.0.1 on any port
	if strings.HasPrefix(origin, "http://localhost:") ||
		strings.HasPrefix(origin, "http://127.0.0.1:") ||
		strings.HasPrefix(origin, "https://localhost:") ||
		strings.HasPrefix(origin, "https://127.0.0.1:") {
		return true
	}

	// Private LAN IPs are acceptable over plain http: these origins are only
	// ever added when the admin EXPLICITLY lists them in ALLOWED_ORIGINS, and
	// home-server deployments (this project's target) serve LAN clients over
	// http. Anything not matching here must be https below.
	if strings.HasPrefix(origin, "http://") {
		host := strings.SplitN(strings.TrimPrefix(origin, "http://"), ":", 2)[0]
		host = strings.SplitN(host, "/", 2)[0] // strip trailing path, if any
		if ip := net.ParseIP(host); ip != nil && ip.IsPrivate() {
			return true
		}
		return false
	}

	// Only allow HTTPS for non-local origins
	if !strings.HasPrefix(origin, "https://") {
		return false
	}

	// Basic validation - must be a valid-looking HTTPS URL
	// Must have at least a domain (something after https://)
	if len(origin) <= len("https://") {
		return false
	}

	// Reject wildcards or patterns
	if strings.Contains(origin, "*") {
		return false
	}

	return true
}

// naturalCompare compares two strings using natural sort order.
// Numbers within strings are compared by their numeric value,
// so "chapter2" < "chapter10" (not lexicographic "chapter10" < "chapter2").
func naturalCompare(a, b string) bool {
	// Fast path: if strings are equal, a is not less than b
	if a == b {
		return false
	}

	i, j := 0, 0
	for i < len(a) && j < len(b) {
		ca, cb := a[i], b[j]

		// If both characters are digits, compare the full numeric runs
		if ca >= '0' && ca <= '9' && cb >= '0' && cb <= '9' {
			// Extract the full number from each string
			numA, lenA := extractNumber(a[i:])
			numB, lenB := extractNumber(b[j:])

			if numA != numB {
				return numA < numB
			}
			// Equal numbers: shorter representation comes first (e.g., "02" > "2")
			if lenA != lenB {
				return lenA < lenB
			}
			i += lenA
			j += lenB
			continue
		}

		// Compare characters case-insensitively
		la := strings.ToLower(string(ca))
		lb := strings.ToLower(string(cb))
		if la != lb {
			return la < lb
		}
		i++
		j++
	}

	// One string is a prefix of the other â€” shorter comes first
	return len(a) < len(b)
}

// extractNumber parses a leading numeric string and returns its value and length.
func extractNumber(s string) (int, int) {
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	if i == 0 {
		return 0, 0
	}
	n, _ := strconv.Atoi(s[:i])
	return n, i
}

// ============================================================
// File Scanner
// ============================================================

var stopWords = map[string]bool{
	"of": true, "the": true, "is": true, "a": true, "and": true,
	"in": true, "on": true, "at": true, "to": true, "for": true,
	"with": true, "i": true, "you": true, "me": true, "my": true,
	"like": true, "has": true, "set": true, "love": true, "from": true,
	"some": true, "sexy": true, "be": true, "have": true, "edit": true,
	"so": true, "do": true, "etc": true, "png": true, "jpg": true,
	"jpeg": true, "mp4": true, "gif": true, "webm": true,
	"webp": true, "bmp": true, "svg": true, "avi": true,
	"mov": true, "mkv": true,
}

var mediaExts = map[string]bool{
	"jpg": true, "jpeg": true, "png": true, "gif": true,
	"webp": true, "bmp": true, "svg": true, "mp4": true,
	"webm": true, "avi": true, "mov": true, "mkv": true,
}

// archiveExts lists the compressed-archive extensions that trigger an
// h-manga "new archive" notification when added directly inside an artist
// directory. Kept separate from mediaExts so h-manga scanning can still
// treat archive files as inert (they aren't page images) while the
// notification layer recognizes them.
var archiveExts = map[string]bool{
	"zip": true,
	"cbz": true,
	"rar": true,
	"7z":  true,
}

// Pre-compiled regex patterns for ParseFilename and ParseFolderTags
var (
	regexPureNumber           = regexp.MustCompile(`^\d+$`)
	regexHashOrID             = regexp.MustCompile(`^[a-zA-Z0-9_]{6,}$`)
	regexContainsDigit        = regexp.MustCompile(`\d`)
	regexContainsAlpha        = regexp.MustCompile(`[a-zA-Z]`)
	regexLeadingNumber        = regexp.MustCompile(`^(\d+)`)
	regexLeadingDashes        = regexp.MustCompile(`^[-\s]+`)
	regexBracketFormat        = regexp.MustCompile(`^\[(.*?)\]\s*(.*)$`)
	regexSuffixFormat         = regexp.MustCompile(`^(.*?)\s*\[(.*?)\]$`)
	regexParenFormat          = regexp.MustCompile(`^(.*?)\s*\((.*?)\)$`)
	regexFolderTagNumberCheck = regexp.MustCompile(`^\d+([.,]\d+)?$`)
)

func IsMediaFile(name string) bool {
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(name), "."))
	return mediaExts[ext]
}

// IsArchiveFile reports whether name is a compressed archive (.zip / .cbz /
// .rar / .7z). Used by the h-manga notification system to fire a notification
// when an archive is added directly inside an artist directory. Archive files
// are NOT page images â€” IsMediaFile returns false for them â€” but the
// notification layer needs to recognize them so they're treated as events
// rather than silently skipped.
func IsArchiveFile(name string) bool {
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(name), "."))
	return archiveExts[ext]
}

func FilterTags(tags []string) []string {
	var filtered []string
	seen := make(map[string]bool)
	for _, t := range tags {
		t = strings.ToLower(strings.TrimSpace(t))
		if t == "" || stopWords[t] || seen[t] {
			continue
		}
		if regexFolderTagNumberCheck.MatchString(t) {
			continue
		}
		seen[t] = true
		filtered = append(filtered, t)
	}
	return filtered
}

func ParseFilename(filename string, folderName string) (number *int, tags []string) {
	name := strings.TrimRight(strings.TrimSuffix(filename, filepath.Ext(filename)), ".")

	// Check for pure numbers pattern (e.g., "123.jpg")
	if regexPureNumber.MatchString(name) {
		n, _ := strconv.Atoi(name)
		return &n, []string{}
	}

	// Check for hash/ID pattern: alphanumeric 6+ chars with both digits and letters
	// Examples: a1b2c3d4e5, 123456_p0
	isHashOrID := regexHashOrID.MatchString(name) &&
		regexContainsDigit.MatchString(name) &&
		regexContainsAlpha.MatchString(name)

	if isHashOrID {
		// Just extract leading number, no tags
		if m := regexLeadingNumber.FindStringSubmatch(name); len(m) > 1 {
			n, _ := strconv.Atoi(m[1])
			return &n, []string{}
		}
		return nil, []string{}
	}

	isSpaceOrDash := func(r rune) bool { return r == ' ' || r == '-' }

	// Original logic: look for number prefix followed by tags
	if m := regexLeadingNumber.FindStringSubmatch(name); len(m) > 1 {
		n, _ := strconv.Atoi(m[1])
		number = &n
		rest := strings.TrimSpace(name[len(m[0]):])
		rest = regexLeadingDashes.ReplaceAllString(rest, "")
		if rest != "" {
			tags = strings.FieldsFunc(rest, isSpaceOrDash)
		}
	} else {
		// No number at all - treat entire filename as tags
		tags = strings.FieldsFunc(name, isSpaceOrDash)
	}

	filtered := FilterTags(tags)

	// Fallback: If no tags extracted, use folder name tags
	if len(filtered) == 0 && folderName != "" {
		filtered = ParseFolderTags(folderName)
	}

	return number, filtered
}

// ParseFolderTags extracts tags from a folder name
func ParseFolderTags(folderName string) []string {
	var extracted []string

	// Try bracket format: [Artist] Series
	if m := regexBracketFormat.FindStringSubmatch(folderName); len(m) > 2 {
		extracted = append(extracted, m[1], m[2])
	} else if m := regexSuffixFormat.FindStringSubmatch(folderName); len(m) > 2 {
		// Suffix format: Series [Artist]
		extracted = append(extracted, m[1], m[2])
	} else if m := regexParenFormat.FindStringSubmatch(folderName); len(m) > 2 {
		// Parentheses format: Series (Artist)
		extracted = append(extracted, m[1], m[2])
	} else {
		// Standard format: just the folder name
		extracted = append(extracted, folderName)
	}

	// Replace spaces/hyphens with underscores and filter
	var result []string
	for _, e := range extracted {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		// Replace spaces and hyphens with underscores
		e = strings.ReplaceAll(e, " ", "_")
		e = strings.ReplaceAll(e, "-", "_")
		result = append(result, e)
	}

	return FilterTags(result)
}

func ScanImages(ctx context.Context, dir string, db *InMemoryDB, section string) error {
	if _, err := os.Stat(dir); err != nil {
		log.Printf("[SCAN ERROR] Cannot access directory %s: %v", dir, err)
		return fmt.Errorf("cannot access directory: %w", err)
	}

	// Collect discovered files first, then splice them into the DB in a
	// single batched write at the end (db.SaveFiles). Go's sync.RWMutex is
	// write-preferring, so calling db.SaveFile per file here would hold the
	// write lock repeatedly throughout the walk and starve every reader
	// endpoint (/api/files, /api/folders, /api/tags, /api/tags/stats) for
	// the scan's entire duration â€” which is what hung the Tag Management
	// page while a scan was running. Batching collapses the reader-starvation
	// window to one brief splice at the end.
	var collected []MediaFile

	walkErr := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		// Check for context cancellation
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		if err != nil {
			log.Printf("[SCAN WARNING] Error accessing %s: %v", path, err)
			return nil
		}
		if info.IsDir() || !IsMediaFile(info.Name()) {
			return nil
		}

		rel, err := filepath.Rel(dir, path)
		if err != nil {
			log.Printf("[SCAN WARNING] Cannot compute relative path for %s: %v", path, err)
			return nil
		}
		rel = filepath.ToSlash(rel)
		fileType := "image"
		if isVideoFile(info.Name()) {
			fileType = "video"
		}

		pathTags := pathTagsFromDir(rel)
		folder := filepath.Base(filepath.Dir(path))
		num, tags := ParseFilename(info.Name(), folder)
		tags = append(pathTags, tags...)

		if fileType == "video" {
			tags = FilterTags([]string{folder})
		}

		// Debug logging for tag generation
		if len(tags) == 0 && num == nil {
			log.Printf("[TAG DEBUG] No tags for file: %s (folder: %s)", info.Name(), folder)
		}

		file := MediaFile{
			Path:     rel,
			Name:     info.Name(),
			Type:     fileType,
			Mtime:    info.ModTime().Unix(),
			Number:   num,
			Tags:     dedupeTags(tags),
			Section:  section,
			FileSize: info.Size(),
			FileHash: computeFileHash(path),
		}
		collected = append(collected, file)
		return nil
	})

	// Atomically replace the section's file set with the discovered files
	// under a single write-lock acquisition. ClearAndSaveFiles tears down the
	// old entries (tag associations, hash indexes) and rebuilds from
	// `collected` in one locked splice, so concurrent readers see either
	// the old set or the new set â€” never an empty window mid-scan. (The
	// previous flow called ClearSection first, leaving the section empty for
	// the entire walk; readers returned [] until the scan finished.) Even on
	// very large libraries this is a bounded O(n) in-memory operation (no
	// disk I/O â€” hashes were already computed during the walk), so the
	// reader-starvation window stays short.
	if len(collected) > 0 {
		db.ClearAndSaveFiles(section, collected)
	} else {
		// No files discovered: still clear so the section reflects an empty
		// library rather than stale entries. ClearSection is fine here
		// because there's no follow-up insert to observe an empty gap
		// before (the section is genuinely empty on disk).
		db.ClearSection(section)
	}

	return walkErr
}

func ScanMangaStructure(dir string) []Series {
	var series []Series

	entries, _ := os.ReadDir(dir)
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		seriesPath := filepath.Join(dir, entry.Name())
		seriesObj := Series{Name: entry.Name(), Section: SectionManga}

		subEntries, _ := os.ReadDir(seriesPath)
		hasSubDirs := false
		for _, sub := range subEntries {
			if sub.IsDir() {
				hasSubDirs = true
				break
			}
		}

		var seriesMaxMtime int64
		if hasSubDirs {
			for _, sub := range subEntries {
				if !sub.IsDir() {
					continue
				}
				chapterPath := filepath.Join(seriesPath, sub.Name())
				result := collectImagesWithMtime(chapterPath, filepath.ToSlash(filepath.Join(entry.Name(), sub.Name())))
				if len(result.images) > 0 {
					seriesObj.Chapters = append(seriesObj.Chapters, Chapter{Name: sub.Name(), Images: result.images})
					if result.maxMtime > seriesMaxMtime {
						seriesMaxMtime = result.maxMtime
					}
				}
			}
		} else {
			result := collectImagesInDirWithMtime(seriesPath, filepath.ToSlash(entry.Name()))
			if len(result.images) > 0 {
				seriesObj.Chapters = append(seriesObj.Chapters, Chapter{Name: "Root", Images: result.images})
				seriesMaxMtime = result.maxMtime
			}
		}

		if len(seriesObj.Chapters) > 0 {
			sort.Slice(seriesObj.Chapters, func(i, j int) bool {
				return naturalCompare(seriesObj.Chapters[i].Name, seriesObj.Chapters[j].Name)
			})
			seriesObj.UpdatedAt = seriesMaxMtime
			series = append(series, seriesObj)
		}
	}

	sort.Slice(series, func(i, j int) bool {
		if series[i].UpdatedAt != series[j].UpdatedAt {
			return series[i].UpdatedAt > series[j].UpdatedAt
		}
		return naturalCompare(series[i].Name, series[j].Name)
	})

	return series
}

func ScanHMangaStructure(dir string) []Series {
	var books []Series

	entries, _ := os.ReadDir(dir)
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		bookPath := filepath.Join(dir, entry.Name())
		bookObj := Series{Name: entry.Name(), Section: SectionHManga}

		subEntries, _ := os.ReadDir(bookPath)
		hasSubDirs := false
		var rootImages []ImageRef
		var rootMaxMtime int64

		for _, sub := range subEntries {
			if sub.IsDir() {
				hasSubDirs = true
			} else if IsMediaFile(sub.Name()) {
				imgRef := ImageRef{
					Name: sub.Name(),
					Path: filepath.ToSlash(filepath.Join(entry.Name(), sub.Name())),
				}
				if info, err := sub.Info(); err == nil {
					t := info.ModTime().Unix()
					imgRef.Mtime = t
					if t > rootMaxMtime {
						rootMaxMtime = t
					}
				}
				rootImages = append(rootImages, imgRef)
			}
		}

		var bookMaxMtime int64
		if rootMaxMtime > bookMaxMtime {
			bookMaxMtime = rootMaxMtime
		}

		if hasSubDirs {
			for _, sub := range subEntries {
				if !sub.IsDir() {
					continue
				}
				chapterPath := filepath.Join(bookPath, sub.Name())
				result := collectImagesWithMtime(chapterPath, filepath.ToSlash(filepath.Join(entry.Name(), sub.Name())))
				if len(result.images) > 0 {
					bookObj.Chapters = append(bookObj.Chapters, Chapter{Name: sub.Name(), Images: result.images})
					if result.maxMtime > bookMaxMtime {
						bookMaxMtime = result.maxMtime
					}
				}
			}
			sort.Slice(rootImages, func(i, j int) bool {
				return naturalCompare(rootImages[i].Name, rootImages[j].Name)
			})
			if len(rootImages) > 0 {
				bookObj.Chapters = append([]Chapter{{Name: "Root", Images: rootImages}}, bookObj.Chapters...)
			}
		} else if len(rootImages) > 0 {
			bookObj.Chapters = append(bookObj.Chapters, Chapter{Name: entry.Name(), Images: rootImages})
		}

		if len(bookObj.Chapters) > 0 {
			if len(bookObj.Chapters) > 1 && bookObj.Chapters[0].Name == "Root" {
				sort.Slice(bookObj.Chapters[1:], func(i, j int) bool {
					return naturalCompare(bookObj.Chapters[1:][i].Name, bookObj.Chapters[1:][j].Name)
				})
			} else {
				sort.Slice(bookObj.Chapters, func(i, j int) bool {
					return naturalCompare(bookObj.Chapters[i].Name, bookObj.Chapters[j].Name)
				})
			}
			bookObj.UpdatedAt = bookMaxMtime
			books = append(books, bookObj)
		}
	}

	sort.Slice(books, func(i, j int) bool {
		if books[i].UpdatedAt != books[j].UpdatedAt {
			return books[i].UpdatedAt > books[j].UpdatedAt
		}
		return naturalCompare(books[i].Name, books[j].Name)
	})

	return books
}

const maxImageDepth = 10

type collectResult struct {
	images   []ImageRef
	maxMtime int64
}

func collectImagesWithMtime(dirPath, relPrefix string) collectResult {
	return collectImagesRecursiveWithMtime(dirPath, relPrefix, 0)
}

func collectImagesRecursiveWithMtime(dirPath, relPrefix string, depth int) collectResult {
	if depth > maxImageDepth {
		log.Printf("[WARN] Maximum recursion depth (%d) exceeded at %s, stopping", maxImageDepth, dirPath)
		return collectResult{}
	}
	var results []ImageRef
	var maxMtime int64
	entries, _ := os.ReadDir(dirPath)
	for _, entry := range entries {
		if entry.IsDir() {
			sub := collectImagesRecursiveWithMtime(filepath.Join(dirPath, entry.Name()), filepath.ToSlash(filepath.Join(relPrefix, entry.Name())), depth+1)
			results = append(results, sub.images...)
			if sub.maxMtime > maxMtime {
				maxMtime = sub.maxMtime
			}
		} else if IsMediaFile(entry.Name()) {
			imgRef := ImageRef{Name: entry.Name(), Path: filepath.ToSlash(filepath.Join(relPrefix, entry.Name()))}
			if info, err := entry.Info(); err == nil {
				t := info.ModTime().Unix()
				imgRef.Mtime = t
				if t > maxMtime {
					maxMtime = t
				}
			}
			results = append(results, imgRef)
		}
	}
	sort.Slice(results, func(i, j int) bool {
		return naturalCompare(results[i].Name, results[j].Name)
	})
	return collectResult{images: results, maxMtime: maxMtime}
}

func collectImagesInDirWithMtime(dirPath, relPrefix string) collectResult {
	var results []ImageRef
	var maxMtime int64
	entries, _ := os.ReadDir(dirPath)
	for _, entry := range entries {
		if !entry.IsDir() && IsMediaFile(entry.Name()) {
			imgRef := ImageRef{Name: entry.Name(), Path: filepath.ToSlash(filepath.Join(relPrefix, entry.Name()))}
			if info, err := entry.Info(); err == nil {
				t := info.ModTime().Unix()
				imgRef.Mtime = t
				if t > maxMtime {
					maxMtime = t
				}
			}
			results = append(results, imgRef)
		}
	}
	sort.Slice(results, func(i, j int) bool {
		return naturalCompare(results[i].Name, results[j].Name)
	})
	return collectResult{images: results, maxMtime: maxMtime}
}

// mergeConfig merges non-zero values from fileCfg into baseCfg
// rawFileConfig mirrors Config but uses *bool pointer fields for booleans
// that must distinguish "field omitted in JSON" (nil pointer) from "field
// explicitly set to false" (non-nil pointer with value false). This lets
// mergeConfig preserve the DefaultConfig value when a boolean is omitted,
// while still allowing an explicit false to override a true default.
//
// Only the boolean fields that have non-false defaults need pointer types;
// the rest mirror Config exactly (zero-value means "not set", matching the
// existing mergeConfig semantics). The json tags intentionally match Config
// so the on-disk format is unchanged.
type rawFileConfig struct {
	Port                       int               `json:"port"`
	Directories                map[string]string `json:"directories"`
	ThumbnailDir               string            `json:"thumbnail_dir"`
	WatchDirectories           *bool             `json:"watch_directories"`
	Mode                       string            `json:"mode"`
	RateLimitRPS               int               `json:"rate_limit_rps"`
	RateLimitBurst             int               `json:"rate_limit_burst"`
	MaxConcurrent              int               `json:"max_concurrent"`
	ConnLimitAcquireTimeoutSec int               `json:"conn_limit_acquire_timeout_sec"`
	ThumbnailWorkers           int               `json:"thumbnail_workers"`
	ThumbScaleFactor           float64           `json:"thumb_scale_factor"`
	ThumbMaxWidth              int               `json:"thumb_max_width"`
	ThumbMaxHeight             int               `json:"thumb_max_height"`
	RequestTimeoutSec          int               `json:"request_timeout_sec"`
	MaxRequestMB               int               `json:"max_request_mb"`
	TrustedProxies             []string          `json:"trusted_proxies"`
	MaxThumbnailMB             int               `json:"max_thumbnail_mb"`
	WriteTimeoutSec            int               `json:"write_timeout_sec"`
	MinWriteRateBytes          int               `json:"min_write_rate_bytes"`
	ResponsiveBuckets          []int             `json:"responsive_buckets"`
	EnablePreloading           *bool             `json:"enable_preloading"`
	PreloadDesktopDefault      int               `json:"preload_desktop_default"`
	PreloadMobileDefault       int               `json:"preload_mobile_default"`
	MaxPreloadCount            int               `json:"max_preload_count"`
	RescanIntervalSec          int               `json:"rescan_interval_sec"`
	IndexPath                  string            `json:"index_path"`
	IndexOnStartup             string            `json:"index_on_startup"`
	Gotify                     rawGotifyConfig   `json:"gotify"`
	Discord                    rawDiscordConfig  `json:"discord"`
}

// rawGotifyConfig mirrors GotifyConfig with a *bool for Enabled.
type rawGotifyConfig struct {
	Enabled         *bool  `json:"enabled"`
	ServerURL       string `json:"server_url"`
	BinaryPath      string `json:"binary_path"`
	Port            int    `json:"port"`
	AdminUser       string `json:"admin_user"`
	AdminPass       string `json:"admin_pass"`
	AppToken        string `json:"app_token"`
	DataDir         string `json:"data_dir"`
	CooldownSec     int    `json:"cooldown_sec"`
	DefaultPriority int    `json:"default_priority"`
}

func mergeConfig(baseCfg *Config, fileCfg *rawFileConfig) {
	if fileCfg.Port != 0 {
		baseCfg.Port = fileCfg.Port
		log.Printf("[CONFIG] Port from file: %d", fileCfg.Port)
	}
	for k, v := range fileCfg.Directories {
		if v != "" {
			baseCfg.Directories[k] = v
			log.Printf("[CONFIG] Directory %s from file: %s", k, v)
		}
	}
	if fileCfg.Mode != "" {
		baseCfg.Mode = fileCfg.Mode
		log.Printf("[CONFIG] Mode from file: %s", fileCfg.Mode)
	}
	if fileCfg.RateLimitRPS != 0 {
		baseCfg.RateLimitRPS = fileCfg.RateLimitRPS
		log.Printf("[CONFIG] RateLimitRPS from file: %d", fileCfg.RateLimitRPS)
	}
	if fileCfg.RateLimitBurst != 0 {
		baseCfg.RateLimitBurst = fileCfg.RateLimitBurst
		log.Printf("[CONFIG] RateLimitBurst from file: %d", fileCfg.RateLimitBurst)
	}
	if fileCfg.MaxConcurrent != 0 {
		baseCfg.MaxConcurrent = fileCfg.MaxConcurrent
		log.Printf("[CONFIG] MaxConcurrent from file: %d", fileCfg.MaxConcurrent)
	}
	if fileCfg.ConnLimitAcquireTimeoutSec != 0 {
		baseCfg.ConnLimitAcquireTimeoutSec = fileCfg.ConnLimitAcquireTimeoutSec
		log.Printf("[CONFIG] ConnLimitAcquireTimeoutSec from file: %d", fileCfg.ConnLimitAcquireTimeoutSec)
	}
	if fileCfg.ThumbnailWorkers != 0 {
		baseCfg.ThumbnailWorkers = fileCfg.ThumbnailWorkers
		log.Printf("[CONFIG] ThumbnailWorkers from file: %d", fileCfg.ThumbnailWorkers)
	}
	if fileCfg.ThumbScaleFactor != 0 {
		baseCfg.ThumbScaleFactor = fileCfg.ThumbScaleFactor
		log.Printf("[CONFIG] ThumbScaleFactor from file: %.2f", fileCfg.ThumbScaleFactor)
	}
	if fileCfg.ThumbMaxWidth != 0 {
		baseCfg.ThumbMaxWidth = fileCfg.ThumbMaxWidth
		log.Printf("[CONFIG] ThumbMaxWidth from file: %d", fileCfg.ThumbMaxWidth)
	}
	if fileCfg.ThumbMaxHeight != 0 {
		baseCfg.ThumbMaxHeight = fileCfg.ThumbMaxHeight
		log.Printf("[CONFIG] ThumbMaxHeight from file: %d", fileCfg.ThumbMaxHeight)
	}
	if fileCfg.RequestTimeoutSec != 0 {
		baseCfg.RequestTimeoutSec = fileCfg.RequestTimeoutSec
		log.Printf("[CONFIG] RequestTimeoutSec from file: %d", fileCfg.RequestTimeoutSec)
	}
	if fileCfg.MaxRequestMB != 0 {
		baseCfg.MaxRequestMB = fileCfg.MaxRequestMB
		log.Printf("[CONFIG] MaxRequestMB from file: %d", fileCfg.MaxRequestMB)
	}
	if fileCfg.MaxThumbnailMB != 0 {
		baseCfg.MaxThumbnailMB = fileCfg.MaxThumbnailMB
		log.Printf("[CONFIG] MaxThumbnailMB from file: %d", fileCfg.MaxThumbnailMB)
	}
	if fileCfg.WriteTimeoutSec != 0 {
		baseCfg.WriteTimeoutSec = fileCfg.WriteTimeoutSec
		log.Printf("[CONFIG] WriteTimeoutSec from file: %d", fileCfg.WriteTimeoutSec)
	}
	if fileCfg.MinWriteRateBytes != 0 {
		baseCfg.MinWriteRateBytes = fileCfg.MinWriteRateBytes
		log.Printf("[CONFIG] MinWriteRateBytes from file: %d", fileCfg.MinWriteRateBytes)
	}
	if len(fileCfg.TrustedProxies) > 0 {
		baseCfg.TrustedProxies = fileCfg.TrustedProxies
		log.Printf("[CONFIG] TrustedProxies from file: %v", fileCfg.TrustedProxies)
	}
	if len(fileCfg.ResponsiveBuckets) > 0 {
		baseCfg.ResponsiveBuckets = fileCfg.ResponsiveBuckets
		log.Printf("[CONFIG] ResponsiveBuckets from file: %v", fileCfg.ResponsiveBuckets)
	}
	if fileCfg.EnablePreloading != nil {
		baseCfg.EnablePreloading = *fileCfg.EnablePreloading
		log.Printf("[CONFIG] EnablePreloading from file: %v", *fileCfg.EnablePreloading)
	}
	if fileCfg.PreloadDesktopDefault != 0 {
		baseCfg.PreloadDesktopDefault = fileCfg.PreloadDesktopDefault
		log.Printf("[CONFIG] PreloadDesktopDefault from file: %d", fileCfg.PreloadDesktopDefault)
	}
	if fileCfg.PreloadMobileDefault != 0 {
		baseCfg.PreloadMobileDefault = fileCfg.PreloadMobileDefault
		log.Printf("[CONFIG] PreloadMobileDefault from file: %d", fileCfg.PreloadMobileDefault)
	}
	if fileCfg.MaxPreloadCount != 0 {
		baseCfg.MaxPreloadCount = fileCfg.MaxPreloadCount
		log.Printf("[CONFIG] MaxPreloadCount from file: %d", fileCfg.MaxPreloadCount)
	}
	// WatchDirectories is a bool with a true default â€” only override the
	// default when the file explicitly sets it (pointer non-nil), so an
	// omitted field preserves true while "watch_directories": false correctly
	// disables watching.
	if fileCfg.WatchDirectories != nil {
		baseCfg.WatchDirectories = *fileCfg.WatchDirectories
		log.Printf("[CONFIG] WatchDirectories from file: %v", *fileCfg.WatchDirectories)
	}
	if fileCfg.RescanIntervalSec != 0 {
		baseCfg.RescanIntervalSec = fileCfg.RescanIntervalSec
		log.Printf("[CONFIG] RescanIntervalSec from file: %d", fileCfg.RescanIntervalSec)
	}
	if fileCfg.ThumbnailDir != "" {
		baseCfg.ThumbnailDir = fileCfg.ThumbnailDir
		log.Printf("[CONFIG] ThumbnailDir from file: %s", fileCfg.ThumbnailDir)
	}
	if fileCfg.IndexPath != "" {
		baseCfg.IndexPath = fileCfg.IndexPath
		log.Printf("[CONFIG] IndexPath from file: %s", fileCfg.IndexPath)
	}
	if fileCfg.IndexOnStartup != "" {
		baseCfg.IndexOnStartup = fileCfg.IndexOnStartup
		log.Printf("[CONFIG] IndexOnStartup from file: %s", fileCfg.IndexOnStartup)
	}
	if fileCfg.Gotify.Enabled != nil {
		baseCfg.Gotify.Enabled = *fileCfg.Gotify.Enabled
		log.Printf("[CONFIG] Gotify.Enabled from file: %v", *fileCfg.Gotify.Enabled)
	}
	if fileCfg.Gotify.ServerURL != "" {
		baseCfg.Gotify.ServerURL = fileCfg.Gotify.ServerURL
		log.Printf("[CONFIG] Gotify.ServerURL from file: %s", fileCfg.Gotify.ServerURL)
	}
	if fileCfg.Gotify.BinaryPath != "" {
		baseCfg.Gotify.BinaryPath = fileCfg.Gotify.BinaryPath
		log.Printf("[CONFIG] Gotify.BinaryPath from file: %s", fileCfg.Gotify.BinaryPath)
	}
	if fileCfg.Gotify.Port != 0 {
		baseCfg.Gotify.Port = fileCfg.Gotify.Port
		log.Printf("[CONFIG] Gotify.Port from file: %d", fileCfg.Gotify.Port)
	}
	if fileCfg.Gotify.AdminUser != "" {
		baseCfg.Gotify.AdminUser = fileCfg.Gotify.AdminUser
	}
	if fileCfg.Gotify.AdminPass != "" {
		baseCfg.Gotify.AdminPass = fileCfg.Gotify.AdminPass
	}
	if fileCfg.Gotify.AppToken != "" {
		baseCfg.Gotify.AppToken = fileCfg.Gotify.AppToken
		log.Printf("[CONFIG] Gotify.AppToken from file: (configured)")
	}
	if fileCfg.Gotify.DataDir != "" {
		baseCfg.Gotify.DataDir = fileCfg.Gotify.DataDir
		log.Printf("[CONFIG] Gotify.DataDir from file: %s", fileCfg.Gotify.DataDir)
	}
	if fileCfg.Gotify.CooldownSec != 0 {
		baseCfg.Gotify.CooldownSec = fileCfg.Gotify.CooldownSec
		log.Printf("[CONFIG] Gotify.CooldownSec from file: %d", fileCfg.Gotify.CooldownSec)
	}
	if fileCfg.Gotify.DefaultPriority != 0 {
		baseCfg.Gotify.DefaultPriority = fileCfg.Gotify.DefaultPriority
		log.Printf("[CONFIG] Gotify.DefaultPriority from file: %d", fileCfg.Gotify.DefaultPriority)
	}
	if fileCfg.Discord.Enabled != nil {
		baseCfg.Discord.Enabled = *fileCfg.Discord.Enabled
		log.Printf("[CONFIG] Discord.Enabled from file: %v", *fileCfg.Discord.Enabled)
	}
	if fileCfg.Discord.BotToken != "" {
		baseCfg.Discord.BotToken = fileCfg.Discord.BotToken
		log.Printf("[CONFIG] Discord.BotToken from file: (configured)")
	}
	if fileCfg.Discord.RecipientID != "" {
		baseCfg.Discord.RecipientID = fileCfg.Discord.RecipientID
		log.Printf("[CONFIG] Discord.RecipientID from file: %s", fileCfg.Discord.RecipientID)
	}
	if fileCfg.Discord.CooldownSec != 0 {
		baseCfg.Discord.CooldownSec = fileCfg.Discord.CooldownSec
		log.Printf("[CONFIG] Discord.CooldownSec from file: %d", fileCfg.Discord.CooldownSec)
	}
}

// resolveRelativeToExe resolves a relative path to be relative to the
// executable's directory instead of the current working directory.
// Absolute paths are returned unchanged.
// This ensures config paths like "./index" always resolve next to the exe,
// regardless of what directory the user runs the exe from.
func resolveRelativeToExe(path string) string {
	if path == "" || filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(exeRelativeBaseDir, path)
}

// pathToExeRelative converts an absolute path back to a relative path
// relative to the executable directory. If the path is not under the exe
// directory, it is returned unchanged. Relative paths are returned unchanged.
func pathToExeRelative(path string) string {
	if path == "" || !filepath.IsAbs(path) {
		return path
	}
	rel, err := filepath.Rel(exeRelativeBaseDir, path)
	if err != nil {
		return path // can't make relative, return as-is
	}
	return rel
}

// ============================================================
// Main
// ============================================================

func main() {
	configPath := flag.String("config", "config.json", "Config file path")
	port := flag.Int("port", 3000, "Server port")
	daemon := flag.Bool("daemon", false, "Run in background mode")
	logfile := flag.String("logfile", "", "Log file path (required with -daemon)")
	skipDeps := flag.Bool("skip-deps", false, "Skip dependency check and installation")
	noTray := flag.Bool("notray", false, "Disable system tray icon (run as console app)")
	flag.Parse()

	// Handle daemon mode
	if *daemon {
		if *logfile == "" {
			fmt.Fprintln(os.Stderr, "Error: -logfile is required when using -daemon")
			os.Exit(1)
		}

		// Open log file
		f, err := os.OpenFile(*logfile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error opening log file: %v\n", err)
			os.Exit(1)
		}
		defer f.Close()

		// Redirect stdout and stderr to log file
		log.SetOutput(f)
		os.Stdout = f
		os.Stderr = f

		log.Printf("[DAEMON] Server started in daemon mode, logging to %s", *logfile)
	}

	// In tray mode the executable is built as a GUI-subsystem binary
	// (-H windowsgui) so there is no console window â€” os.Stdout/os.Stderr
	// are invalid and log output would be lost. Redirect logs to a file
	// next to the executable so tray-mode operation is still observable.
	// Daemon mode already redirected above; -notray keeps the console.
	useTrayEarly := !*daemon && !*noTray
	if useTrayEarly {
		trayLogPath := resolveRelativeToExe("server.log")
		// Size-based rotation: if the existing log exceeds the cap, roll it
		// to server.log.old so it can't grow unbounded across deploys/restarts.
		const logCapBytes = 10 * 1024 * 1024 // 10 MB
		if info, err := os.Stat(trayLogPath); err == nil && info.Size() >= logCapBytes {
			_ = os.Rename(trayLogPath, trayLogPath+".old")
		}
		// 0600: the log contains client IPs (from the rate-limit middleware)
		// and server paths, so it must not be world-readable/writable.
		if f, err := os.OpenFile(trayLogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600); err == nil {
			trayLogFile = f
			// Buffer log writes behind a thread-safe wrapper so the periodic
			// flush goroutine, log.Printf callers, and gin's logger can all use
			// it concurrently without racing on bufio.Writer internals. The
			// underlying writer is swappable so showConsoleWindow can attach the
			// on-demand console (via MultiWriter) while keeping the buffer.
			trayLogWriter = newSyncBufferedWriter(f, 8*1024)
			log.SetOutput(trayLogWriter)
			// os.Stdout/Stderr remain the raw file for non-log writes (rare, e.g.
			// direct fmt.Fprintf(os.Stdout,...)). gin's per-request logger is
			// routed through the buffer explicitly in the router setup below via
			// gin.LoggerWithWriter(trayLogWriter), so request logs are coalesced.
			os.Stdout = f
			os.Stderr = f
			// Periodic flush + a final flush on shutdown keep the buffer from
			// sitting with unflushed data. The goroutine owns the final flush;
			// the defer signals it and WAITS for it to finish before closing,
			// so there is no concurrent double-Flush on shutdown.
			flushStop := make(chan struct{})
			flushDone := make(chan struct{})
			go func() {
				ticker := time.NewTicker(1 * time.Second)
				defer ticker.Stop()
				for {
					select {
					case <-ticker.C:
						_ = trayLogWriter.Flush()
					case <-flushStop:
						_ = trayLogWriter.Flush()
						close(flushDone)
						return
					}
				}
			}()
			defer func() {
				close(flushStop) // tell the goroutine to do the final flush
				<-flushDone      // wait until it has flushed and returned
				f.Close()        // no more writers; safe to close the file
			}()
			log.Printf("[TRAY] Tray mode â€” logs redirected to %s", trayLogPath)
		}
	}

	// Initialize start time for uptime tracking
	startTime = time.Now()

	// Start with default config
	cfg := DefaultConfig()
	currentConfig = cfg
	cfg.Port = *port

	// Resolve config path relative to executable if it's a relative path.
	// This ensures the config file is found next to the exe regardless of CWD.
	resolvedConfigPath := *configPath
	if resolvedConfigPath != "" && !filepath.IsAbs(resolvedConfigPath) {
		resolvedConfigPath = resolveRelativeToExe(resolvedConfigPath)
	}
	configFilePath = resolvedConfigPath

	// Load config from file
	if resolvedConfigPath != "" {
		data, err := os.ReadFile(resolvedConfigPath)
		if err == nil {
			// Unmarshal into a raw struct with *bool pointer fields so we can
			// distinguish "field omitted" (nil) from "field explicitly false".
			// mergeConfig only overrides the default when the pointer is non-nil.
			var raw rawFileConfig
			if json.Unmarshal(data, &raw) == nil {
				log.Printf("[CONFIG] Loaded config from %s", resolvedConfigPath)
				mergeConfig(cfg, &raw)
			}
		} else {
			log.Printf("[CONFIG] Could not read config file %s: %v", resolvedConfigPath, err)
		}
	}

	// Command line port override
	if *port != 3000 {
		cfg.Port = *port
		log.Printf("[CONFIG] Port overridden by command line: %d", cfg.Port)
	}

	// Environment variables override everything
	if imagesDir := os.Getenv("MV_IMAGES_DIR"); imagesDir != "" {
		cfg.Directories[SectionImages] = imagesDir
		log.Printf("[CONFIG] Directory images from env: %s", imagesDir)
	}
	if mangaDir := os.Getenv("MV_MANGA_DIR"); mangaDir != "" {
		cfg.Directories[SectionManga] = mangaDir
		log.Printf("[CONFIG] Directory manga from env: %s", mangaDir)
	}
	if hMangaDir := os.Getenv("MV_HMANGA_DIR"); hMangaDir != "" {
		cfg.Directories[SectionHManga] = hMangaDir
		log.Printf("[CONFIG] Directory h-manga from env: %s", hMangaDir)
	}

	// Resolve relative paths to be relative to the executable directory,
	// not the current working directory. This ensures consistent behavior
	// regardless of where the user runs the exe from.
	if cfg.ThumbnailDir != "" {
		cfg.ThumbnailDir = resolveRelativeToExe(cfg.ThumbnailDir)
		log.Printf("[CONFIG] ThumbnailDir resolved to: %s", cfg.ThumbnailDir)
	}
	if cfg.IndexPath != "" {
		cfg.IndexPath = resolveRelativeToExe(cfg.IndexPath)
		log.Printf("[CONFIG] IndexPath resolved to: %s", cfg.IndexPath)
	}
	if cfg.Gotify.DataDir != "" {
		cfg.Gotify.DataDir = resolveRelativeToExe(cfg.Gotify.DataDir)
		log.Printf("[CONFIG] Gotify.DataDir resolved to: %s", cfg.Gotify.DataDir)
	}
	if cfg.Gotify.BinaryPath != "" {
		cfg.Gotify.BinaryPath = resolveRelativeToExe(cfg.Gotify.BinaryPath)
		log.Printf("[CONFIG] Gotify.BinaryPath resolved to: %s", cfg.Gotify.BinaryPath)
	}

	log.Printf("Starting Media Viewer Server on port %d", cfg.Port)
	// Snapshot the debug flag once, lock-free, so debugLog on hot serving
	// paths doesn't acquire configMu. Mode is never mutated after startup.
	debugMode.Store(cfg.Mode == "debug")
	for section, dir := range cfg.Directories {
		if dir != "" {
			log.Printf("  %s: %s", section, dir)
		}
	}

	// Init database
	db := NewInMemoryDB()

	// Check and install missing dependencies before starting
	if !*skipDeps {
		checkAndInstallDependencies(cfg, *daemon)
	}

	// Detect ffmpeg/ffprobe for video thumbnail extraction and WebP output
	detectFFmpeg()

	// Initialize the shared notification tracker once if either notifier is
	// enabled. Both notifiers share the same on-disk set so a chapter is marked
	// notified once across services.
	var tracker *NotificationTracker
	if cfg.Gotify.Enabled || cfg.Discord.Enabled {
		dataDir := cfg.Gotify.DataDir
		if dataDir == "" {
			dataDir = "./gotify-data"
		}
		trackerPath := filepath.Join(dataDir, "notified-chapters.json")
		tracker = NewNotificationTracker(trackerPath)
	}

	// Initialize Gotify notifier if enabled. Start() spawns the gotify child
	// process and waits for its /health endpoint (up to ~30s); run it in a
	// goroutine so it does NOT block the tray icon or HTTP server from coming
	// up. Previously this was synchronous, so in tray mode (GUI-subsystem, no
	// console) the user saw nothing for up to 30s and assumed the app was hung.
	// NotifyBatch checks n.isReady() before doing anything, so notifications
	// attempted during startup are safely skipped until Start() completes. On
	// failure, n.ready stays 0 and NotifyBatch remains a no-op, so gotifyNotifier
	// is left non-nil (its IsEnabled()/status endpoints still work, and the
	// /api/gotify/toggle handler can retry Start()).
	if cfg.Gotify.Enabled {
		n := NewGotifyNotifier(cfg.Gotify, tracker)
		configMu.Lock()
		gotifyNotifier = n
		configMu.Unlock()
		// Track the in-flight Start() so Stop() (toggle-disable / shutdown) can
		// wait for it before touching n.cmd, avoiding a Start/Stop race.
		n.startDone.Add(1)
		go func() {
			defer n.startDone.Done()
			if err := n.Start(); err != nil {
				log.Printf("[GOTIFY] Failed to start: %v â€” notifications disabled", err)
				// Reset the package var to nil under the mutex so the toggle
				// retry branch (== nil) can re-Start, matching pre-async behavior.
				configMu.Lock()
				if gotifyNotifier == n {
					gotifyNotifier = nil
				}
				configMu.Unlock()
				return
			}
			// Auto-drain any notifications that were skipped during the startup
			// window (NotifyBatch short-circuits while !isReady). Without this,
			// chapters detected by a scan during startup would only be
			// recoverable via a manual "push pending" click.
			if sent := n.PushPendingNotifications(db, cfg); sent > 0 {
				log.Printf("[GOTIFY] Auto-sent %d pending notification(s) after startup", sent)
			}
		}()
	} else {
		log.Printf("[GOTIFY] Disabled in config")
	}

	if cfg.Discord.Enabled {
		dn := NewDiscordNotifier(cfg.Discord, tracker)
		configMu.Lock()
		discordNotifier = dn
		configMu.Unlock()
		log.Printf("[DISCORD] Enabled in config")
	} else {
		log.Printf("[DISCORD] Disabled in config")
	}

	// Register MIME types that Go's mime package may not know by default.
	// .webp is needed because cached thumbnails are now served as WebP files,
	// and mime.TypeByExtension(".webp") would otherwise return empty string.
	mime.AddExtensionType(".webp", "image/webp")
	// .mkv for Matroska video files
	mime.AddExtensionType(".mkv", "video/x-matroska")

	// Init thumbnail pool
	thumbnailPool = NewThumbnailPool(cfg.ThumbnailWorkers, cfg.ThumbnailDir)
	defer thumbnailPool.Stop()

	// Init metrics
	metrics = NewServerMetrics()

	// Initialize the debounced index saver
	initDebouncedSaveIndex()

	// Init rate limiter and connection limiter
	rateLimiter := NewRateLimiter(cfg.RateLimitRPS, cfg.RateLimitBurst)
	connLimiter := NewConnectionLimiter(cfg.MaxConcurrent)

	// Startup: index loading and scanning strategy based on index_on_startup config:
	//   "always"   â€” Load index first, verify in background (fast startup)
	//   "fallback" â€” Try full scan; only use index if scan fails (network drive unavailable)
	//   "never"    â€” Always do full scan, ignore index
	indexOnStartup := cfg.IndexOnStartup
	if indexOnStartup == "" {
		indexOnStartup = "always"
	}

	type scanResult struct {
		section string
		count   int
		err     error
	}

	var sectionsToFullScan []string
	indexLoadedSections := 0
	fullyScannedSections := make(map[string]bool)
	var fullyScannedMu sync.RWMutex

	switch indexOnStartup {
	case "always":
		// Load index first, then verify in background
		for section, dir := range cfg.Directories {
			if dir == "" {
				continue
			}
			loaded, err := loadIndexIntoDB(cfg, db, section)
			if loaded && err == nil {
				indexLoadedSections++
				var count int
				if section == SectionImages {
					count = len(db.GetFilesBySection(section))
				} else {
					count = len(db.GetMangaFolders(section))
				}
				log.Printf("[STARTUP] Loaded index for %s: %d items", section, count)
			} else {
				if err != nil {
					log.Printf("[STARTUP] Index load failed for %s: %v â€” will scan", section, err)
				}
				sectionsToFullScan = append(sectionsToFullScan, section)
			}
		}

	case "fallback":
		// Try full scan; fall back to index only if scan fails
		// First attempt full scan for all sections
		for section := range cfg.Directories {
			sectionsToFullScan = append(sectionsToFullScan, section)
		}

	case "never":
		// Always full scan, never use index
		for section, dir := range cfg.Directories {
			if dir != "" {
				sectionsToFullScan = append(sectionsToFullScan, section)
			}
		}
	}

	// Full-scan any sections that need it
	if len(sectionsToFullScan) > 0 {
		if indexLoadedSections == 0 || indexOnStartup == "fallback" {
			// No indices loaded (or fallback mode) â€” block until full scan completes so server has data
			log.Printf("[STARTUP] Full scanning %d sections: %v", len(sectionsToFullScan), sectionsToFullScan)
			var scanWg sync.WaitGroup
			scanResults := make(chan scanResult, len(sectionsToFullScan))

			for _, section := range sectionsToFullScan {
				dir := cfg.Directories[section]
				if dir == "" {
					continue
				}
				scanWg.Add(1)
				db.SetScanning(section, true)
				go func(sec, d string) {
					defer scanWg.Done()
					defer db.SetScanning(sec, false)
					// NOTE: do NOT ClearSection here for images â€” ScanImages
					// atomically swaps the file set via ClearAndSaveFiles at
					// the end, so clearing first would leave the section empty
					// for the entire walk. Manga branches clear below because
					// SaveMangaFolders needs the old folder list gone first.
					start := time.Now()
					switch sec {
					case SectionImages:
						ctx, cancel := context.WithTimeout(context.Background(), scanTimeoutDuration())
						defer cancel()
						if err := ScanImages(ctx, d, db, sec); err != nil {
							log.Printf("[STARTUP ERROR] Failed to scan %s: %v", sec, err)
							scanResults <- scanResult{sec, 0, err}
						} else {
							files := db.GetFilesBySection(sec)
							metrics.FilesScanned.Add(uint64(len(files)))
							log.Printf("[STARTUP] Scanned %s: %d files (%v)", sec, len(files), time.Since(start))
							scanResults <- scanResult{sec, len(files), nil}
						}
					case SectionManga:
						db.ClearSection(sec)
						folders := ScanMangaStructure(d)
						db.SaveMangaFolders(sec, folders)
						log.Printf("[STARTUP] Scanned %s: %d folders (%v)", sec, len(folders), time.Since(start))
						scanResults <- scanResult{sec, len(folders), nil}
					case SectionHManga:
						db.ClearSection(sec)
						folders := ScanHMangaStructure(d)
						db.SaveMangaFolders(sec, folders)
						log.Printf("[STARTUP] Scanned %s: %d folders (%v)", sec, len(folders), time.Since(start))
						scanResults <- scanResult{sec, len(folders), nil}
					}
				}(section, dir)
			}

			scanWg.Wait()
			close(scanResults)
			totalCount := 0
			for result := range scanResults {
				fullyScannedMu.Lock()
				fullyScannedSections[result.section] = true
				fullyScannedMu.Unlock()
				if result.err != nil {
					// Scan failed â€” try loading from index as fallback
					if indexOnStartup == "fallback" {
						log.Printf("[STARTUP] Scan failed for %s, trying index fallback: %v", result.section, result.err)
						dir := cfg.Directories[result.section]
						if dir != "" {
							loaded, loadErr := loadIndexIntoDB(cfg, db, result.section)
							if loaded && loadErr == nil {
								indexLoadedSections++
								log.Printf("[STARTUP] Fallback: loaded index for %s", result.section)
							} else if loadErr != nil {
								log.Printf("[STARTUP] Fallback: index load also failed for %s: %v", result.section, loadErr)
							}
						}
					}
				} else {
					totalCount += result.count
				}
				if err := saveSectionIndex(cfg, db, result.section); err != nil {
					log.Printf("[STARTUP] Failed to save index for %s: %v", result.section, err)
				}
			}
			log.Printf("[STARTUP] Full scan complete â€” %d total items", totalCount)
		} else {
			// Some indices loaded â€” scan remaining sections in background, start serving immediately
			log.Printf("[STARTUP] Background scanning %d sections that had no index: %v", len(sectionsToFullScan), sectionsToFullScan)
			for _, section := range sectionsToFullScan {
				dir := cfg.Directories[section]
				if dir == "" {
					continue
				}
				go func(sec, d string) {
					db.SetScanning(sec, true)
					defer db.SetScanning(sec, false)
					// NOTE: do NOT ClearSection here for images â€” ScanImages
					// atomically swaps the file set via ClearAndSaveFiles at
					// the end, so clearing first would leave the section empty
					// for the entire walk. Manga branches clear below.
					start := time.Now()
					switch sec {
					case SectionImages:
						ctx, cancel := context.WithTimeout(context.Background(), scanTimeoutDuration())
						defer cancel()
						if err := ScanImages(ctx, d, db, sec); err != nil {
							log.Printf("[STARTUP ERROR] Failed to scan %s: %v", sec, err)
							return
						}
						files := db.GetFilesBySection(sec)
						metrics.FilesScanned.Add(uint64(len(files)))
						log.Printf("[STARTUP] Background scan completed %s: %d files (%v)", sec, len(files), time.Since(start))
					case SectionManga:
						db.ClearSection(sec)
						folders := ScanMangaStructure(d)
						db.SaveMangaFolders(sec, folders)
						log.Printf("[STARTUP] Background scan completed %s: %d folders (%v)", sec, len(folders), time.Since(start))
					case SectionHManga:
						db.ClearSection(sec)
						folders := ScanHMangaStructure(d)
						db.SaveMangaFolders(sec, folders)
						log.Printf("[STARTUP] Background scan completed %s: %d folders (%v)", sec, len(folders), time.Since(start))
					}
					fullyScannedMu.Lock()
					fullyScannedSections[sec] = true
					fullyScannedMu.Unlock()
					if err := saveSectionIndex(cfg, db, sec); err != nil {
						log.Printf("[STARTUP] Failed to save index for %s: %v", sec, err)
					}
				}(section, dir)
			}
		}
	}

	// Start background verification for index-loaded sections (skip freshly full-scanned sections)
	if indexLoadedSections > 0 && indexOnStartup != "never" {
		go func() {
			time.Sleep(2 * time.Second)
			for section, dir := range cfg.Directories {
				if dir == "" {
					continue
				}
				if db.IsScanning(section) {
					continue
				}
				fullyScannedMu.RLock()
				skip := fullyScannedSections[section]
				fullyScannedMu.RUnlock()
				if skip {
					log.Printf("[VERIFY] Skipping %s â€” freshly full-scanned", section)
					continue
				}
				verifySection(cfg, db, section)
			}
		}()
	}

	// Start periodic directory rescan if enabled
	startPeriodicRescan(cfg, db, 10*time.Second)

	// Set Gin mode
	if cfg.Mode == "debug" {
		gin.SetMode(gin.DebugMode)
		log.Println("[STARTUP] Running in DEBUG mode")
	} else {
		gin.SetMode(gin.ReleaseMode)
		log.Println("[STARTUP] Running in RELEASE mode")
	}

	// Setup router with security middleware
	var r *gin.Engine
	if cfg.Mode == "debug" {
		// gin.Default() = gin.New() + gin.Logger() (writes to os.Stdout) +
		// gin.Recovery(). In tray mode os.Stdout is the raw log file, so
		// gin.Logger's per-request lines would bypass the buffered writer and
		// incur a syscall each. Route gin's logger through the thread-safe
		// buffered writer (trayLogWriter) when in tray mode so request logs
		// are coalesced with the rest of the log output.
		r = gin.New()
		if trayLogWriter != nil {
			r.Use(gin.LoggerWithWriter(trayLogWriter))
		} else {
			r.Use(gin.Logger())
		}
		r.Use(gin.Recovery())
	} else {
		r = gin.New()
		r.Use(gin.Recovery())
	}

	// Set trusted proxies to prevent IP spoofing via X-Forwarded-For headers
	r.SetTrustedProxies(cfg.TrustedProxies)

	// Apply global middleware â€” security headers, CORS, rate limiting, metrics.
	// Connection limiting is applied ONLY to the API group (not static files/media)
	// so that long-lived media streaming connections don't exhaust the semaphore
	// and block new clients from connecting.
	// MinWriteRateMiddleware enforces a minimum download speed on ALL routes.
	// Connections receiving data at â‰¥ 500 KB/s stay alive indefinitely, while
	// stalled/slow connections are terminated to free server resources.
	r.Use(SecurityHeadersMiddleware())
	r.Use(SetupCORS(cfg))
	r.Use(MaxBodySize(int64(cfg.MaxRequestMB) * 1024 * 1024))
	r.Use(RateLimitMiddleware(rateLimiter))
	r.Use(MetricsMiddleware(metrics))
	r.Use(MinWriteRateMiddleware(cfg.MinWriteRateBytes, time.Duration(cfg.WriteTimeoutSec)*time.Second))

	// Explicit route for /index.html
	r.GET("/index.html", func(c *gin.Context) {
		execPath, err := os.Executable()
		if err != nil {
			c.JSON(500, gin.H{"error": "Internal server error"})
			return
		}
		execDir := filepath.Dir(execPath)
		c.File(filepath.Join(execDir, "web", "index.html"))
	})

	// API Routes â€” connection-limited so that long-running media streams
	// don't prevent API requests from being served.
	api := r.Group("/api")
	{
		api.Use(ConnectionLimitMiddleware(connLimiter, time.Duration(cfg.ConnLimitAcquireTimeoutSec)*time.Second))
		api.Use(ValidateSection(cfg))

		// Series info endpoint
		api.GET("/series-info", func(c *gin.Context) {
			section := c.Query("section")
			seriesName := c.Query("series")

			if section == "" || seriesName == "" {
				c.JSON(400, gin.H{"error": "section and series parameters are required"})
				return
			}

			dir := cfg.Directories[section]
			if dir == "" {
				c.JSON(400, gin.H{"error": "Directory not configured for section: " + section})
				return
			}

			sanitizedSeries, err := SanitizePath(seriesName)
			if err != nil {
				c.JSON(403, gin.H{"error": "Invalid series name"})
				return
			}

			seriesDir := filepath.Join(dir, sanitizedSeries)
			stat, err := os.Stat(seriesDir)
			if err != nil || !stat.IsDir() {
				c.JSON(404, gin.H{"error": "Series not found"})
				return
			}

			info := readSeriesInfo(seriesDir, sanitizedSeries)
			if info.CoverPath != "" && !strings.HasPrefix(info.CoverPath, "http") {
				info.CoverPath = sanitizedSeries + "/" + info.CoverPath
			}

			c.JSON(200, info)
		})

		// Config
		api.GET("/config", func(c *gin.Context) {
			dirs := make(map[string]bool)
			for k, v := range cfg.Directories {
				dirs[k] = v != ""
			}
			c.JSON(200, gin.H{
				"serverMode":  true,
				"directories": dirs,
			})
		})

		// Health check endpoint
		api.GET("/health", func(c *gin.Context) {
			health := gin.H{
				"status":    "healthy",
				"timestamp": time.Now().Unix(),
				"uptime":    time.Since(startTime).String(),
				"metrics":   metrics.GetStats(),
			}

			// Check critical resources
			var checks []string
			if thumbnailPool.QueueLen() > 900 {
				checks = append(checks, "thumbnail_queue_near_capacity")
			}
			if metrics.RequestsActive.Load() > int32(cfg.MaxConcurrent-5) {
				checks = append(checks, "high_concurrent_load")
			}

			if len(checks) > 0 {
				health["status"] = "degraded"
				health["checks"] = checks
				c.JSON(503, health)
				return
			}

			c.JSON(200, health)
		})

		// Metrics endpoint
		api.GET("/metrics", func(c *gin.Context) {
			c.JSON(200, metrics.GetStats())
		})

		// Scan - synchronous, returns results
		api.POST("/scan/:section", func(c *gin.Context) {
			section := c.Param("section")
			log.Printf("[API] /api/scan/%s called from %s", section, c.ClientIP())
			dir := cfg.Directories[section]
			if dir == "" {
				c.JSON(400, gin.H{"error": "Directory not configured for section: " + section})
				return
			}

			if !db.TrySetScanning(section) {
				c.JSON(400, gin.H{"error": "Scan already in progress"})
				return
			}

			log.Printf("[SCAN] Starting scan for %s at %s", section, dir)
			start := time.Now()

			// Capture previous manga state for notifications.
			// Snapshot the notifier under configMu â€” a concurrent toggle can
			// nil the package var mid-scan, which would dereference nil below.
			isMangaSection := section == SectionManga || section == SectionHManga
			gn := getGotifyNotifier()
			dn := getDiscordNotifier()
			notifiersActive := gn != nil || dn != nil
			var prevMangaState map[string][]string
			var prevArchiveState map[string][]string
			if notifiersActive && isMangaSection {
				if section == SectionHManga {
					// h-manga notifies on archive additions, not new chapter
					// subdirectories. Snapshot the disk-derived archive set so
					// the post-scan diff can detect new archive files.
					prevArchiveState = getHMangaArchiveSnapshot(dir)
				} else {
					prevMangaState = getMangaChapterSnapshot(db, section)
				}
			}

			// For manga sections, clear stale data before re-scanning. For
			// the images section we DON'T clear here â€” ScanImages atomically
			// swaps the file set via ClearAndSaveFiles at the end of the
			// walk, so clearing first would leave the section empty for the
			// entire scan and readers would observe [] mid-scan.
			if isMangaSection {
				db.ClearSection(section)
			}

			var files []MediaFile
			var folders []Series
			var scanErr error

			switch section {
			case SectionImages:
				func() {
					// API scan uses longer timeout since it can take time for large directories
					ctx, cancel := context.WithTimeout(context.Background(), scanTimeoutDuration())
					defer cancel()
					scanErr = ScanImages(ctx, dir, db, section)
					if scanErr != nil {
						log.Printf("[SCAN ERROR] %v", scanErr)
						return
					}
					files = db.GetFilesBySection(section)
					metrics.FilesScanned.Add(uint64(len(files)))
					log.Printf("[SCAN] Found %d files in %s", len(files), section)
				}()
			case SectionManga:
				folders = ScanMangaStructure(dir)
				db.SaveMangaFolders(section, folders)
				log.Printf("[SCAN] Found %d folders in %s", len(folders), section)
			case SectionHManga:
				folders = ScanHMangaStructure(dir)
				db.SaveMangaFolders(section, folders)
				log.Printf("[SCAN] Found %d folders in %s", len(folders), section)
			}

			db.SetScanning(section, false)
			log.Printf("[SCAN] Completed %s in %v", section, time.Since(start))

			// Send notifications for new manga chapters. Use the snapshot taken
			// before the scan; if a toggle disabled a notifier mid-scan, its
			// NotifyBatch no-ops via isReady()/Enabled.
			if notifiersActive && isMangaSection && scanErr == nil {
				if section == SectionHManga && prevArchiveState != nil {
					currentArchiveState := getHMangaArchiveSnapshot(dir)
					newArchives := diffHMangaArchives(prevArchiveState, currentArchiveState)
					notifyNewArchives(newArchives, section, gn, dn)
				} else {
					currentFolders := db.GetMangaFolders(section)
					newChapters := diffMangaChapters(prevMangaState, currentFolders, section)
					if len(newChapters) > 0 {
						grouped := make(map[string][]string)
						for _, nc := range newChapters {
							grouped[nc.Series] = append(grouped[nc.Series], nc.Chapter)
						}
						for series, chapters := range grouped {
							if gn != nil {
								gn.NotifyBatch(series, section, chapters)
							}
							if dn != nil {
								dn.NotifyBatch(series, section, chapters)
							}
						}
					}
				}
			}

			// Only save the index if the scan succeeded
			if scanErr == nil {
				if err := saveSectionIndex(cfg, db, section); err != nil {
					log.Printf("[SCAN] Failed to save index for %s: %v", section, err)
				}
			}

			if scanErr != nil {
				c.JSON(500, gin.H{"error": "Scan failed: " + scanErr.Error()})
				return
			}

			if section == SectionImages {
				tags := db.GetAllTags()
				log.Printf("[SCAN] Returning %d files with %d tags", len(files), len(tags))
				c.JSON(200, gin.H{
					"files": files,
					"tags":  tags,
					"count": len(files),
				})
			} else {
				c.JSON(200, gin.H{
					"folders": folders,
					"count":   len(folders),
				})
			}
		})

		// Scan status
		api.GET("/scan/status", func(c *gin.Context) {
			status := make(map[string]bool)
			for _, section := range []string{SectionImages, SectionManga, SectionHManga} {
				status[section] = db.IsScanning(section)
			}
			c.JSON(200, gin.H{"scanning": status})
		})

		// Reindex - incremental, uses verifySection (checks mtime, adds new,
		// removes deleted). Runs ASYNCHRONOUSLY in a goroutine and returns
		// immediately with 202 Accepted so the client can disconnect while
		// the index walks the directory. The outcome is cached on the DB
		// and exposed via /api/reindex/status?section=X so the client can
		// pick up the result when it next navigates to the section.
		api.POST("/reindex/:section", func(c *gin.Context) {
			section := c.Param("section")
			log.Printf("[API] /api/reindex/%s called from %s", section, c.ClientIP())
			// cfg.Directories is keyed by the canonical section names
			// (images/manga/h-manga), so a missing entry rejects both
			// unknown sections and unconfigured ones in one check.
			dir := cfg.Directories[section]
			if dir == "" {
				c.JSON(400, gin.H{"error": "Unknown or unconfigured section: " + section})
				return
			}

			if !db.TrySetScanning(section) {
				c.JSON(409, gin.H{"error": "Scan already in progress"})
				return
			}

			log.Printf("[REINDEX] Queueing async incremental reindex for %s", section)

			// Detach the work from the HTTP handler. The goroutine owns the
			// scanning lock for its entire lifetime; the DB's lastReindex
			// entry survives the connection so a later client can observe
			// the outcome.
			go func(section, dir string) {
				start := time.Now()
				changed := false
				errMsg := ""

				// Panic-recover so a crash in the worker can't wedge the
				// scanning lock in true state forever (which would block
				// every future /scan and /reindex for this section).
				defer func() {
					if r := recover(); r != nil {
						errMsg = fmt.Sprintf("panic: %v", r)
						log.Printf("[REINDEX] PANIC in %s: %v", section, r)
					}
					count := 0
					if section == SectionImages {
						count = len(db.GetFilesBySection(section))
					} else {
						count = len(db.GetMangaFolders(section))
					}
					// Write the final result BEFORE releasing the scanning
					// lock. A polling client that observes running=false is
					// guaranteed to also see the completed result â€” no
					// window where it sees "not running, but still the old
					// (or partial) result" and mis-reports "no changes".
					db.SetLastReindex(section, reindexResult{
						StartedAt:   start,
						CompletedAt: time.Now(),
						Changed:     changed,
						Count:       count,
						Error:       errMsg,
					})
					db.SetScanning(section, false)
					log.Printf("[REINDEX] Completed %s in %v (changed=%v, err=%q)",
						section, time.Since(start), changed, errMsg)
				}()

				switch section {
				case SectionImages:
					changed = verifyImagesSection(cfg, db, section, dir)
				case SectionManga:
					changed = verifyMangaSection(cfg, db, section, dir)
				case SectionHManga:
					changed = verifyMangaSection(cfg, db, section, dir)
				}

				if changed {
					if err := saveSectionIndex(cfg, db, section); err != nil {
						log.Printf("[REINDEX] Failed to save index for %s: %v", section, err)
						errMsg = err.Error()
					}
				} else {
					log.Printf("[REINDEX] No changes for %s â€” skipping index save", section)
				}
			}(section, dir)

			// 202 Accepted â€” work is in flight, client may disconnect.
			// Polling endpoint: GET /api/reindex/status?section=X
			c.JSON(202, gin.H{
				"section":  section,
				"status":   "started",
				"started":  time.Now(),
				"poll_url": fmt.Sprintf("/api/reindex/status?section=%s", section),
			})
		})

		// Reindex status - reports the most recent reindex result for a
		// section so a client that disconnected during the original POST
		// (or a different client on a later visit) can still observe the
		// outcome. Returns the cached reindexResult plus a `running` flag
		// derived from the scanning lock. If no reindex has ever been
		// requested for the section, `running` and `has_result` are both
		// false and the client can safely fall back to a plain fetch.
		api.GET("/reindex/status", func(c *gin.Context) {
			section := c.DefaultQuery("section", "")
			if section == "" {
				c.JSON(400, gin.H{"error": "section query parameter is required"})
				return
			}
			result, ok := db.GetLastReindex(section)
			// Use a pointer so the field serializes as `null` when no
			// reindex has finished for this section â€” otherwise the
			// zero-value struct (with 0001-01-01 timestamps) leaks
			// into the response.
			var resultPtr *reindexResult
			if ok {
				resultPtr = &result
			}
			c.JSON(200, gin.H{
				"section":    section,
				"running":    db.IsScanning(section),
				"has_result": ok,
				"result":     resultPtr,
			})
		})

		// Files
		api.GET("/files", func(c *gin.Context) {
			section := c.DefaultQuery("section", SectionImages)
			log.Printf("[API] /api/files?section=%s called from %s", section, c.ClientIP())
			c.JSON(200, db.GetFilesBySection(section))
		})

		// Folders (for manga/h-manga)
		api.GET("/folders", func(c *gin.Context) {
			section := c.DefaultQuery("section", "manga")
			log.Printf("[API] /api/folders?section=%s called from %s", section, c.ClientIP())
			c.JSON(200, db.GetMangaFolders(section))
		})

		// Tags
		api.GET("/tags", func(c *gin.Context) {
			c.JSON(200, db.GetAllTags())
		})

		// Tags with counts (returns map of tag -> count)
		// Accepts optional section query param to filter by section
		api.GET("/tags/stats", func(c *gin.Context) {
			section := c.DefaultQuery("section", "")
			c.JSON(200, db.GetTagStats(section))
		})

		// Files by tag (optionally filtered by section)
		api.GET("/tags/:tag/files", func(c *gin.Context) {
			tag := c.Param("tag")
			section := c.DefaultQuery("section", "")
			c.JSON(200, db.GetFilesByTag(tag, section))
		})

		// Update tags for a specific file. Catch-all so nested paths
		// ("subdir/img.jpg") address correctly â€” the previous single-segment
		// ":path" route 404'd on any file not in the section root. Static
		// sibling routes (/tags/bulk, /tags/flush) cannot coexist with this
		// catch-all in gin's route tree, so they are dispatched manually.
		api.POST("/tags/*path", func(c *gin.Context) {
			sub := strings.TrimPrefix(c.Param("path"), "/")
			switch sub {
			case "bulk":
				handleBulkTagUpdate(c, cfg, db)
			case "flush":
				handleTagFlush(c, cfg, db)
			default:
				relPath := sub
				var req struct {
					Tags []string `json:"tags"`
				}
				if err := c.ShouldBindJSON(&req); err != nil {
					c.JSON(400, gin.H{"error": "Invalid request"})
					return
				}
				if err := db.UpdateFileTags(relPath, req.Tags); err != nil {
					c.JSON(500, gin.H{"error": err.Error()})
					return
				}
				section := db.GetFileSection(relPath)
				if section != "" {
					debouncedSaveIndex(cfg, db, section)
				}
				c.JSON(200, gin.H{"success": true})
			}
		})

		// Rescan specific directory
		api.POST("/scan/:section/:folder", func(c *gin.Context) {
			section := c.Param("section")
			folder := c.Param("folder")
			quick := c.DefaultQuery("mode", "full") == "quick"

			log.Printf("[API] Rescan request: section=%s, folder=%s, quick=%v", section, folder, quick)

			// Start async scan
			go func() {
				if err := db.ScanDirectory(section, folder, quick); err != nil {
					log.Printf("[SCAN ERROR] %v", err)
				}
			}()

			c.JSON(202, gin.H{
				"message":   "Scan started",
				"section":   section,
				"folder":    folder,
				"quickMode": quick,
			})
		})

		// Thumbnail status
		api.GET("/thumbnails/status", func(c *gin.Context) {
			section := c.DefaultQuery("section", SectionImages)
			result, hashSet := thumbnailPool.AuditSection(section, db)
			orphans := thumbnailPool.FindOrphans(section, db, hashSet)
			result.Orphans = orphans
			result.Orphaned = len(orphans)
			c.JSON(200, result)
		})

		// Generate missing thumbnails
		api.POST("/thumbnails/generate", func(c *gin.Context) {
			section := c.DefaultQuery("section", SectionImages)
			limit := 0
			if l := c.Query("limit"); l != "" {
				if parsed, err := strconv.Atoi(l); err == nil && parsed > 0 {
					limit = parsed
				}
			}

			audit, _ := thumbnailPool.AuditSection(section, db)
			missingFiles := audit.MissingFiles
			if limit > 0 && limit < len(missingFiles) {
				missingFiles = missingFiles[:limit]
			}

			go func() {
				for _, file := range missingFiles {
					if err := GenerateThumbnail(file.Path, section); err != nil {
						log.Printf("[THUMB GEN ERROR] %s: %v", file.Path, err)
					}
				}
			}()

			c.JSON(202, gin.H{
				"message":      "Thumbnail generation started",
				"count":        len(missingFiles),
				"section":      section,
				"limit":        limit,
				"totalMissing": audit.Missing,
			})
		})

		// Generate thumbnail for specific file
		api.POST("/thumbnails/generate/*path", func(c *gin.Context) {
			relPath := strings.TrimPrefix(c.Param("path"), "/")
			section := c.DefaultQuery("section", SectionImages)

			sanitizedPath, err := SanitizePath(relPath)
			if err != nil {
				c.JSON(400, gin.H{"error": "Invalid path"})
				return
			}

			if err := ValidateFileType(sanitizedPath); err != nil {
				c.JSON(400, gin.H{"error": "Invalid file type"})
				return
			}

			// Generate synchronously with timeout
			resultChan := make(chan error, 1)
			go func() {
				resultChan <- GenerateThumbnail(sanitizedPath, section)
			}()

			select {
			case err := <-resultChan:
				if err != nil {
					c.JSON(500, gin.H{"error": err.Error()})
					return
				}
				c.JSON(200, gin.H{"success": true})
			case <-time.After(30 * time.Second):
				c.JSON(504, gin.H{"error": "Thumbnail generation timeout"})
			}
		})

		api.POST("/thumbnails/cull-orphans", func(c *gin.Context) {
			section := c.DefaultQuery("section", SectionImages)
			orphans := thumbnailPool.FindOrphans(section, db)
			if len(orphans) == 0 {
				c.JSON(200, gin.H{"deleted": 0, "message": "No orphaned thumbnails"})
				return
			}
			deleted, err := thumbnailPool.CullOrphans(orphans)
			if err != nil {
				c.JSON(500, gin.H{"error": err.Error()})
				return
			}
			c.JSON(200, gin.H{"deleted": deleted, "total_orphans": len(orphans)})
		})

		api.GET("/thumbnails/orphans", func(c *gin.Context) {
			section := c.DefaultQuery("section", SectionImages)
			orphans := thumbnailPool.FindOrphans(section, db)
			c.JSON(200, gin.H{"section": section, "orphans": orphans, "count": len(orphans)})
		})

		api.GET("/gotify/status", func(c *gin.Context) {
			configMu.Lock()
			gcfg := currentConfig.Gotify
			// "running" reflects actual readiness (Start() completed), not just
			// enabled+present â€” during background startup the notifier exists
			// but isn't ready to send notifications yet.
			running := gotifyNotifier != nil && gotifyNotifier.isReady()
			gn := gotifyNotifier
			configMu.Unlock()

			pendingCount := 0
			if gn != nil {
				pendingCount = len(gn.GetPendingNotifications(db))
			}

			c.JSON(200, gin.H{
				"enabled":          gcfg.Enabled,
				"running":          running,
				"port":             gcfg.Port,
				"default_priority": gcfg.DefaultPriority,
				"cooldown_sec":     gcfg.CooldownSec,
				"pending_count":    pendingCount,
			})
		})

		api.POST("/gotify/toggle", func(c *gin.Context) {
			var req struct {
				Enabled bool `json:"enabled"`
			}
			if err := c.ShouldBindJSON(&req); err != nil {
				c.JSON(400, gin.H{"error": "Invalid request"})
				return
			}

			configMu.Lock()
			needSave := false
			if req.Enabled {
				// Start (or restart) gotify. Treat a present-but-not-ready
				// notifier (e.g. a failed background Start()) as needing a
				// fresh Start: stop the stale one first, then create+start.
				if gotifyNotifier != nil && !gotifyNotifier.isReady() {
					gotifyNotifier.Stop()
					gotifyNotifier = nil
				}
				if gotifyNotifier == nil {
					currentConfig.Gotify.Enabled = true
					dataDir := currentConfig.Gotify.DataDir
					if dataDir == "" {
						dataDir = "./gotify-data"
					}
					trackerPath := filepath.Join(dataDir, "notified-chapters.json")
					tracker := NewNotificationTracker(trackerPath)
					n := NewGotifyNotifier(currentConfig.Gotify, tracker)
					// Synchronous Start here (we hold configMu; the request blocks
					// until gotify is ready or fails). Track via startDone for
					// consistency so a subsequent Stop() waits if needed.
					n.startDone.Add(1)
					if err := n.Start(); err != nil {
						n.startDone.Done()
						log.Printf("[GOTIFY] Failed to start: %v", err)
						configMu.Unlock()
						c.JSON(500, gin.H{"error": "Failed to start Gotify: " + err.Error()})
						return
					}
					n.startDone.Done()
					gotifyNotifier = n
					needSave = true
					log.Printf("[GOTIFY] Started via API")
				}
			} else if !req.Enabled && gotifyNotifier != nil {
				gotifyNotifier.Stop()
				gotifyNotifier = nil
				currentConfig.Gotify.Enabled = false
				needSave = true
				log.Printf("[GOTIFY] Stopped via API")
			}

			enabled := currentConfig.Gotify.Enabled
			running := gotifyNotifier != nil && gotifyNotifier.isReady()
			configMu.Unlock()

			if needSave {
				if err := saveConfig(); err != nil {
					log.Printf("[CONFIG] Failed to save config after Gotify toggle: %v", err)
				}
			}

			c.JSON(200, gin.H{
				"enabled": enabled,
				"running": running,
			})
		})

		api.POST("/gotify/config", func(c *gin.Context) {
			var req struct {
				DefaultPriority int `json:"default_priority,omitempty"`
				CooldownSec     int `json:"cooldown_sec,omitempty"`
			}
			if err := c.ShouldBindJSON(&req); err != nil {
				c.JSON(400, gin.H{"error": "Invalid request"})
				return
			}

			configMu.Lock()
			if req.DefaultPriority > 0 {
				currentConfig.Gotify.DefaultPriority = req.DefaultPriority
			}
			if req.CooldownSec > 0 {
				currentConfig.Gotify.CooldownSec = req.CooldownSec
			}
			cfg := currentConfig.Gotify
			gn := gotifyNotifier
			configMu.Unlock()

			if gn != nil {
				gn.UpdateConfig(cfg)
			}

			if err := saveConfig(); err != nil {
				log.Printf("[CONFIG] Failed to save config after Gotify config update: %v", err)
			}

			c.JSON(200, gin.H{
				"default_priority": cfg.DefaultPriority,
				"cooldown_sec":     cfg.CooldownSec,
			})
		})

		api.POST("/gotify/reset", func(c *gin.Context) {
			configMu.Lock()
			if gotifyNotifier != nil {
				gotifyNotifier.Stop()
				gotifyNotifier = nil
			}
			currentConfig.Gotify.Enabled = true
			dataDir := currentConfig.Gotify.DataDir
			if dataDir == "" {
				dataDir = "./gotify-data"
			}
			trackerPath := filepath.Join(dataDir, "notified-chapters.json")
			tracker := NewNotificationTracker(trackerPath)
			n := NewGotifyNotifier(currentConfig.Gotify, tracker)
			if err := n.Start(); err != nil {
				log.Printf("[GOTIFY] Failed to restart: %v", err)
				configMu.Unlock()
				c.JSON(500, gin.H{"error": "Failed to restart Gotify: " + err.Error()})
				return
			}
			gotifyNotifier = n
			cfg := currentConfig.Gotify
			configMu.Unlock()

			if err := saveConfig(); err != nil {
				log.Printf("[CONFIG] Failed to save config after Gotify reset: %v", err)
			}

			log.Printf("[GOTIFY] Reset via API")

			c.JSON(200, gin.H{
				"enabled":          true,
				"running":          true,
				"port":             cfg.Port,
				"default_priority": cfg.DefaultPriority,
				"cooldown_sec":     cfg.CooldownSec,
			})
		})

		// Gotify pending notifications â€” list chapters not yet notified
		api.GET("/gotify/pending", func(c *gin.Context) {
			gn := getGotifyNotifier()
			if gn == nil {
				c.JSON(200, gin.H{"pending": []interface{}{}, "count": 0})
				return
			}

			pending := gn.GetPendingNotifications(db)

			// Convert to JSON-friendly format
			items := make([]map[string]string, 0, len(pending))
			for _, nc := range pending {
				items = append(items, map[string]string{
					"series":  nc.Series,
					"chapter": nc.Chapter,
					"section": nc.Section,
				})
			}

			c.JSON(200, gin.H{
				"pending": items,
				"count":   len(items),
			})
		})

		// Gotify push pending â€” send notifications for all unnotified chapters
		api.POST("/gotify/push-pending", func(c *gin.Context) {
			gn := getGotifyNotifier()
			if gn == nil {
				c.JSON(400, gin.H{"error": "Gotify not running"})
				return
			}

			sent := gn.PushPendingNotifications(db, getCurrentConfig())
			log.Printf("[GOTIFY] Push pending: %d batch notifications sent", sent)

			c.JSON(200, gin.H{
				"sent":    sent,
				"message": fmt.Sprintf("Pushed %d batch notification(s)", sent),
			})
		})

		// Discord status
		api.GET("/discord/status", func(c *gin.Context) {
			configMu.Lock()
			dcfg := currentConfig.Discord
			dn := discordNotifier
			configMu.Unlock()

			ready := dn != nil && dn.isReady()
			pendingCount := 0
			if dn != nil {
				pendingCount = len(dn.tracker.GetPending(db))
			}

			c.JSON(200, gin.H{
				"enabled":              dcfg.Enabled,
				"ready":                ready,
				"recipient_id":         dcfg.RecipientID,
				"cooldown_sec":         dcfg.CooldownSec,
				"pending_count":        pendingCount,
				"bot_token_configured": dcfg.BotToken != "",
			})
		})

		// Discord toggle
		api.POST("/discord/toggle", func(c *gin.Context) {
			var req struct {
				Enabled bool `json:"enabled"`
			}
			if err := c.ShouldBindJSON(&req); err != nil {
				c.JSON(400, gin.H{"error": "Invalid request"})
				return
			}

			configMu.Lock()
			needSave := false
			if req.Enabled {
				if discordNotifier == nil {
					dataDir := currentConfig.Gotify.DataDir
					if dataDir == "" {
						dataDir = "./gotify-data"
					}
					trackerPath := filepath.Join(dataDir, "notified-chapters.json")
					tracker := NewNotificationTracker(trackerPath)
					if gotifyNotifier != nil {
						gotifyNotifier.tracker = tracker
					}
					dn := NewDiscordNotifier(currentConfig.Discord, tracker)
					if err := dn.Start(); err != nil {
						log.Printf("[DISCORD] Failed to start: %v", err)
						configMu.Unlock()
						c.JSON(500, gin.H{"error": "Failed to start Discord: " + err.Error()})
						return
					}
					currentConfig.Discord.Enabled = true
					discordNotifier = dn
					needSave = true
					log.Printf("[DISCORD] Started via API")
				}
			} else if discordNotifier != nil {
				discordNotifier.Stop()
				discordNotifier = nil
				currentConfig.Discord.Enabled = false
				needSave = true
				log.Printf("[DISCORD] Stopped via API")
			}

			enabled := currentConfig.Discord.Enabled
			ready := discordNotifier != nil && discordNotifier.isReady()
			configMu.Unlock()

			if needSave {
				if err := saveConfig(); err != nil {
					log.Printf("[CONFIG] Failed to save config after Discord toggle: %v", err)
				}
			}

			c.JSON(200, gin.H{
				"enabled": enabled,
				"ready":   ready,
			})
		})

		// Discord config
		api.POST("/discord/config", func(c *gin.Context) {
			var req struct {
				BotToken    string `json:"bot_token,omitempty"`
				RecipientID string `json:"recipient_id,omitempty"`
				CooldownSec int    `json:"cooldown_sec,omitempty"`
			}
			if err := c.ShouldBindJSON(&req); err != nil {
				c.JSON(400, gin.H{"error": "Invalid request"})
				return
			}

			configMu.Lock()
			if req.BotToken != "" {
				currentConfig.Discord.BotToken = req.BotToken
			}
			if req.RecipientID != "" {
				currentConfig.Discord.RecipientID = req.RecipientID
			}
			if req.CooldownSec > 0 {
				currentConfig.Discord.CooldownSec = req.CooldownSec
			}
			cfg := currentConfig.Discord
			dn := discordNotifier
			configMu.Unlock()

			if dn != nil {
				dn.UpdateConfig(cfg)
			}

			if err := saveConfig(); err != nil {
				log.Printf("[CONFIG] Failed to save config after Discord config update: %v", err)
			}

			c.JSON(200, gin.H{
				"recipient_id":         cfg.RecipientID,
				"cooldown_sec":         cfg.CooldownSec,
				"bot_token_configured": cfg.BotToken != "",
			})
		})

		// Discord test
		api.POST("/discord/test", func(c *gin.Context) {
			dn := getDiscordNotifier()
			if dn == nil {
				configMu.Lock()
				if currentConfig.Discord.BotToken == "" || currentConfig.Discord.RecipientID == "" {
					configMu.Unlock()
					c.JSON(400, gin.H{"error": "Discord bot token and recipient ID must be saved before testing"})
					return
				}
				currentConfig.Discord.Enabled = true
				dataDir := currentConfig.Gotify.DataDir
				if dataDir == "" {
					dataDir = "./gotify-data"
				}
				trackerPath := filepath.Join(dataDir, "notified-chapters.json")
				tracker := NewNotificationTracker(trackerPath)
				if gotifyNotifier != nil {
					gotifyNotifier.tracker = tracker
				}
				dn = NewDiscordNotifier(currentConfig.Discord, tracker)
				discordNotifier = dn
				configMu.Unlock()
			}
			if err := dn.SendTest(); err != nil {
				log.Printf("[DISCORD] Test failed: %v", err)
				c.JSON(500, gin.H{"error": err.Error()})
				return
			}
			log.Printf("[DISCORD] Test message sent")
			c.JSON(200, gin.H{"success": true})
		})

		// Discord pending
		api.GET("/discord/pending", func(c *gin.Context) {
			dn := getDiscordNotifier()
			if dn == nil {
				c.JSON(200, gin.H{"pending": []interface{}{}, "count": 0})
				return
			}
			pending := dn.tracker.GetPending(db)
			items := make([]map[string]string, 0, len(pending))
			for _, nc := range pending {
				items = append(items, map[string]string{
					"series":  nc.Series,
					"chapter": nc.Chapter,
					"section": nc.Section,
				})
			}
			c.JSON(200, gin.H{
				"pending": items,
				"count":   len(items),
			})
		})

		// Discord push pending
		api.POST("/discord/push-pending", func(c *gin.Context) {
			dn := getDiscordNotifier()
			if dn == nil {
				c.JSON(400, gin.H{"error": "Discord not configured"})
				return
			}

			sent := dn.PushPendingNotifications(db, getCurrentConfig())
			log.Printf("[DISCORD] Push pending: %d batch notifications sent", sent)

			c.JSON(200, gin.H{
				"sent":    sent,
				"message": fmt.Sprintf("Pushed %d batch notification(s)", sent),
			})
		})

		// Thumbnail - async generation with disk caching (config-based sizing only)
		// No responsive parameters - those are handled by /api/media with nocache=1
		api.GET("/thumbnail/*path", func(c *gin.Context) {
			relPath := strings.TrimPrefix(c.Param("path"), "/")
			section := c.DefaultQuery("section", SectionImages)

			debugLog("[API] /api/thumbnail/%s called from %s", relPath, c.ClientIP())

			// Validate path security
			sanitizedPath, err := SanitizePath(relPath)
			if err != nil {
				log.Printf("[THUMB ERROR] Path validation failed: %v", err)
				c.JSON(403, gin.H{"error": "Invalid path"})
				return
			}

			// Validate file type
			if err := ValidateFileType(sanitizedPath); err != nil {
				log.Printf("[THUMB ERROR] File type validation failed: %v", err)
				c.JSON(403, gin.H{"error": "File type not allowed"})
				return
			}

			// Sequence context from the gallery: when present, attach preload
			// headers so the browser starts fetching neighboring thumbnails.
			index, _ := strconv.Atoi(c.DefaultQuery("index", "-1"))
			total, _ := strconv.Atoi(c.DefaultQuery("total", "0"))
			nextPaths := c.QueryArray("nextPaths")
			prevPaths := c.QueryArray("prevPaths")
			attachPreload := cfg.EnablePreloading && index >= 0 && total > 0 &&
				(section == SectionImages || section == SectionHManga)

			// Thumbnails always use bucket 0 (config-based sizing via generateThumbnail)
			bucket := 0

			// Check cache first (fast path)
			cachePath, _, cacheHit := thumbnailPool.CheckCache(sanitizedPath, bucket)
			if cacheHit {
				debugLog("Thumbnail cache hit: %s", sanitizedPath)
				c.Header("X-Cache", "HIT")
				c.Header("Cache-Control", mediaCacheControl)
				setFileVersionHeaders(c, filepath.Join(cfg.Directories[section], sanitizedPath))
				if attachPreload {
					addPreloadHeadersWithCount(c, "/api/thumbnail", sanitizedPath, section, bucket, index, total, 1, nextPaths, prevPaths, 0, 0, 1)
				}
				c.File(cachePath)
				return
			}

			// Deferred generation (stale-while-revalidate): submit the job and
			// serve the original immediately â€” a request goroutine must never
			// block on the thumbnail pool. The next request hits the real cache.
			resultChan := make(chan ThumbnailResult, 1)
			job := ThumbnailJob{
				OriginalPath: sanitizedPath,
				Section:      section,
				Bucket:       bucket, // 0 = use config-based sizing
				Result:       resultChan,
			}

			if err := thumbnailPool.TrySubmit(job); err != nil {
				log.Printf("[THUMB] Deferred generation skipped (queue unavailable): %v", err)
			}

			c.Header("X-Cache", "DEFERRED")
			c.Header("Cache-Control", "no-store")
			setFileVersionHeaders(c, filepath.Join(cfg.Directories[section], sanitizedPath))
			if attachPreload {
				addPreloadHeadersWithCount(c, "/api/thumbnail", sanitizedPath, section, bucket, index, total, 1, nextPaths, prevPaths, 0, 0, 1)
			}
			c.File(filepath.Join(cfg.Directories[section], sanitizedPath))
		})

		// HEAD handler for thumbnail preload headers
		api.HEAD("/thumbnail/*path", func(c *gin.Context) {
			// For HEAD requests, just return headers without body
			// Parse same parameters as GET
			relPath := strings.TrimPrefix(c.Param("path"), "/")
			_ = c.DefaultQuery("section", SectionImages)
			width, _ := strconv.Atoi(c.DefaultQuery("width", "0"))
			height, _ := strconv.Atoi(c.DefaultQuery("height", "0"))
			dpr, _ := strconv.ParseFloat(c.DefaultQuery("dpr", "1.0"), 64)
			if dpr <= 0 {
				dpr = 1.0
			}

			// Calculate bucket
			var bucket int
			if width > 0 || height > 0 {
				scaledWidth := int(float64(width) * dpr)
				scaledHeight := int(float64(height) * dpr)
				bucket = calculateBucket(scaledWidth, scaledHeight, cfg.ResponsiveBuckets)
			}

			// Validate and check cache
			sanitizedPath, err := SanitizePath(relPath)
			if err != nil {
				c.Status(404)
				return
			}

			if err := ValidateFileType(sanitizedPath); err != nil {
				c.Status(404)
				return
			}

			cachePath, actualBucket, cacheHit := thumbnailPool.CheckCache(sanitizedPath, bucket)
			if cacheHit {
				if info, err := os.Stat(cachePath); err == nil {
					c.Header("Content-Type", "image/jpeg")
					c.Header("Content-Length", strconv.FormatInt(info.Size(), 10))
					c.Header("X-Cache", "HIT")
					c.Header("X-Image-Bucket", strconv.Itoa(actualBucket))
					c.Status(200)
					return
				}
			}

			c.Status(404)
		})

		// Media streaming with security validation and mobile preloading
		// Supports nocache=1 for memory-only responsive images (used by modal)
		api.GET("/media/*path", func(c *gin.Context) {
			relPath := strings.TrimPrefix(c.Param("path"), "/")
			section := c.DefaultQuery("section", SectionImages)

			// Parse responsive/preloading parameters
			width, _ := strconv.Atoi(c.DefaultQuery("width", "0"))
			height, _ := strconv.Atoi(c.DefaultQuery("height", "0"))
			dpr, _ := strconv.ParseFloat(c.DefaultQuery("dpr", "1.0"), 64)
			if dpr <= 0 {
				dpr = 1.0
			}
			index, _ := strconv.Atoi(c.DefaultQuery("index", "-1"))
			total, _ := strconv.Atoi(c.DefaultQuery("total", "0"))
			fullSize := c.DefaultQuery("fullSize", "") == "1"
			nocache := c.DefaultQuery("nocache", "") == "1" // Memory-only mode

			// Validate section
			if cfg.Directories[section] == "" {
				c.JSON(400, gin.H{"error": "Invalid or unconfigured section: " + section})
				return
			}

			// Validate path security
			sanitizedPath, err := SanitizePath(relPath)
			if err != nil {
				c.JSON(403, gin.H{"error": "Invalid path"})
				return
			}

			// Validate file type
			if err := ValidateFileType(sanitizedPath); err != nil {
				c.JSON(403, gin.H{"error": "File type not allowed"})
				return
			}

			// Only search in the specified section's directory
			dir := cfg.Directories[section]
			candidate := filepath.Join(dir, sanitizedPath)
			info, err := os.Stat(candidate)
			if err != nil {
				c.JSON(404, gin.H{"error": "Not found"})
				return
			}

			ext := strings.ToLower(filepath.Ext(candidate))
			contentType := mime.TypeByExtension(ext)
			if contentType == "" {
				contentType = "application/octet-stream"
			}
			c.Header("Content-Type", contentType)

			// Skip responsive resizing for videos and animated formats (GIF).
			// These must be served as-is in the media endpoint â€” they are not
			// resized or re-encoded. Videos and animated images are only
			// suitable for streaming to a client, not for image processing.
			isAnimatedFormat := ext == ".gif"
			isVideoMedia := isVideoFile(candidate)
			if isVideoMedia || isAnimatedFormat {
				// Serve original file without any resizing or re-encoding
				c.Header("Content-Length", strconv.FormatInt(info.Size(), 10))
				c.Header("Cache-Control", mediaCacheControl)
				setFileVersionHeaders(c, candidate)
				if isVideoMedia {
					c.Header("X-Cache", "VIDEO-ORIGINAL")
				} else {
					c.Header("X-Cache", "ANIMATED-ORIGINAL")
				}
				c.File(candidate)
				return
			}

			// Set cache headers for images - cache for 7 days
			// Versioned via X-File-Mtime (the client appends ?v=<mtime> when it
			// knows the mtime) â€” paths are mutable, so never mark immutable.
			c.Header("Cache-Control", mediaCacheControl)
			c.Header("Vary", "Accept-Encoding")
			setFileVersionHeaders(c, candidate)

			// Calculate bucket for responsive images (optional for media)
			// Both manga and h-manga benefit from viewport-based responsive sizing
			// since their pages are typically high-resolution images
			var bucket int
			if !fullSize && !isVideoMedia && !isAnimatedFormat && (width > 0 || height > 0) {
				scaledWidth := int(float64(width) * dpr)
				scaledHeight := int(float64(height) * dpr)
				bucket = calculateBucket(scaledWidth, scaledHeight, cfg.ResponsiveBuckets)
			}

			// Determine preload count based on device type and client preference
			isMobile := isMobileUserAgent(c.Request.UserAgent())
			preloadCount := cfg.PreloadDesktopDefault
			if isMobile {
				preloadCount = cfg.PreloadMobileDefault
			}

			// Check if client requested a specific preload count
			clientPreloadCount, _ := strconv.Atoi(c.DefaultQuery("preloadCount", "0"))
			if clientPreloadCount > 0 {
				// Use client's requested count, but cap at max
				preloadCount = clientPreloadCount
				debugLog("[PRELOAD] Using client-requested preload count: %d", preloadCount)
			}

			// Cap at max preload count
			if cfg.MaxPreloadCount > 0 && preloadCount > cfg.MaxPreloadCount {
				debugLog("[PRELOAD] Capping at max: %d", cfg.MaxPreloadCount)
				preloadCount = cfg.MaxPreloadCount
			}

			debugLog("[PRELOAD] Final preload count: %d (isMobile: %v)", preloadCount, isMobile)

			// If nocache=1, stream responsive image directly to the client.
			// This is used by the modal for viewport-sized images and by the reader
			// for manga/h-manga pages to avoid caching per-viewport disk variants.
			//
			// Unlike the old approach (generateResponsiveImage + c.Data) which buffered
			// the entire image in memory before sending, streamResponsiveImage encodes
			// and streams in real-time via chunked transfer encoding. This eliminates
			// the latency of waiting for the full encode before the browser sees any
			// data, enabling incremental/progressive rendering as bytes arrive.
			if nocache && bucket > 0 && !fullSize && !isVideoMedia && !isAnimatedFormat {
				nextPaths := c.QueryArray("nextPaths")
				prevPaths := c.QueryArray("prevPaths")
				streamResponsiveImage(c.Request.Context(), c, candidate, width, height, dpr, cfg, sanitizedPath, section, bucket, index, total, preloadCount, nextPaths, prevPaths)
				return
			}

			// If responsive sizing requested and not fullSize, try to serve cached responsive version.
			// Manga and H-Manga use the nocache in-memory path instead of disk-caching
			// per-viewport variants, so they skip this disk-cache branch entirely.
			isMangaLikeSection := section == SectionManga || section == SectionHManga
			if bucket > 0 && !fullSize && !isMangaLikeSection && !isVideoMedia && !isAnimatedFormat {
				// Check for cached responsive version using thumbnail pool
				cachePath, actualBucket, cacheHit := thumbnailPool.CheckCache(sanitizedPath, bucket)
				if cacheHit {
					if cacheInfo, err := os.Stat(cachePath); err == nil {
						c.Header("Content-Length", strconv.FormatInt(cacheInfo.Size(), 10))
						c.Header("X-Cache", "HIT")
						c.Header("X-Image-Bucket", strconv.Itoa(actualBucket))
						c.Header("Cache-Control", mediaCacheControl)

						// Add preload headers for images and h-manga sections
						if cfg.EnablePreloading && index >= 0 && total > 0 && (section == SectionImages || section == SectionHManga) {
							nextPaths := c.QueryArray("nextPaths")
							prevPaths := c.QueryArray("prevPaths")
							addPreloadHeadersWithCount(c, "/api/media", sanitizedPath, section, actualBucket, index, total, preloadCount, nextPaths, prevPaths, width, height, dpr)
						}

						c.File(cachePath)
						return
					}
				}

				// Deferred generation (stale-while-revalidate): submit the job
				// and serve the original immediately â€” a request goroutine must
				// never block on the thumbnail pool.
				job := ThumbnailJob{
					OriginalPath: sanitizedPath,
					Section:      section,
					Width:        width,
					Height:       height,
					Bucket:       bucket,
					Result:       make(chan ThumbnailResult, 1),
				}
				if err := thumbnailPool.TrySubmit(job); err != nil {
					log.Printf("[MEDIA] Deferred responsive generation skipped (queue unavailable): %v", err)
				}

				c.Header("Content-Length", strconv.FormatInt(info.Size(), 10))
				c.Header("X-Cache", "DEFERRED")
				c.Header("Cache-Control", "no-store")

				// Add preload headers for images and h-manga sections
				if cfg.EnablePreloading && index >= 0 && total > 0 && (section == SectionImages || section == SectionHManga) {
					nextPaths := c.QueryArray("nextPaths")
					prevPaths := c.QueryArray("prevPaths")
					addPreloadHeadersWithCount(c, "/api/media", sanitizedPath, section, bucket, index, total, preloadCount, nextPaths, prevPaths, width, height, dpr)
				}

				c.File(candidate)
				return
			}

			// Serve original file (fullSize or no responsive params)
			c.Header("Content-Length", strconv.FormatInt(info.Size(), 10))

			// Add preload headers for mobile if enabled (even for original files)
			// Calculate responsive bucket for preload URLs even if current image is served at original size
			var preloadBucket int
			if width > 0 || height > 0 {
				scaledWidth := int(float64(width) * dpr)
				scaledHeight := int(float64(height) * dpr)
				preloadBucket = calculateBucket(scaledWidth, scaledHeight, cfg.ResponsiveBuckets)
			}
			if cfg.EnablePreloading && index >= 0 && total > 0 && (section == SectionImages || section == SectionHManga) {
				nextPaths := c.QueryArray("nextPaths")
				prevPaths := c.QueryArray("prevPaths")
				addPreloadHeadersWithCount(c, "/api/media", sanitizedPath, section, preloadBucket, index, total, preloadCount, nextPaths, prevPaths, width, height, dpr)
			}

			c.File(candidate)
		})

		// HEAD handler for /api/media - returns headers without body for preloading
		api.HEAD("/media/*path", func(c *gin.Context) {
			relPath := strings.TrimPrefix(c.Param("path"), "/")
			section := c.DefaultQuery("section", SectionImages)

			// Parse parameters
			width, _ := strconv.Atoi(c.DefaultQuery("width", "0"))
			height, _ := strconv.Atoi(c.DefaultQuery("height", "0"))
			dpr, _ := strconv.ParseFloat(c.DefaultQuery("dpr", "1.0"), 64)
			if dpr <= 0 {
				dpr = 1.0
			}
			nocache := c.DefaultQuery("nocache", "") == "1"

			// Parse sequence context for preloading
			index, _ := strconv.Atoi(c.DefaultQuery("index", "-1"))
			total, _ := strconv.Atoi(c.DefaultQuery("total", "0"))
			nextPaths := c.QueryArray("nextPaths")
			prevPaths := c.QueryArray("prevPaths")

			// Calculate bucket
			var bucket int
			if width > 0 || height > 0 {
				scaledWidth := int(float64(width) * dpr)
				scaledHeight := int(float64(height) * dpr)
				bucket = calculateBucket(scaledWidth, scaledHeight, cfg.ResponsiveBuckets)
			}

			// Validate path
			sanitizedPath, err := SanitizePath(relPath)
			if err != nil {
				c.Status(404)
				return
			}

			if err := ValidateFileType(sanitizedPath); err != nil {
				c.Status(404)
				return
			}

			dir := cfg.Directories[section]
			candidate := filepath.Join(dir, sanitizedPath)
			info, err := os.Stat(candidate)
			if err != nil {
				c.Status(404)
				return
			}

			ext := strings.ToLower(filepath.Ext(candidate))
			contentType := mime.TypeByExtension(ext)
			if contentType == "" {
				contentType = "application/octet-stream"
			}

			// Add preload headers if sequence context is provided
			if cfg.EnablePreloading && index >= 0 && total > 0 && (section == SectionImages || section == SectionHManga) {
				// Calculate responsive bucket for preload URLs
				var bucket int
				if width > 0 || height > 0 {
					scaledWidth := int(float64(width) * dpr)
					scaledHeight := int(float64(height) * dpr)
					bucket = calculateBucket(scaledWidth, scaledHeight, cfg.ResponsiveBuckets)
				}

				// Determine preload count based on device type and client preference
				isMobile := isMobileUserAgent(c.Request.UserAgent())
				preloadCount := cfg.PreloadDesktopDefault
				if isMobile {
					preloadCount = cfg.PreloadMobileDefault
				}

				// Check if client requested a specific preload count
				clientPreloadCount, _ := strconv.Atoi(c.DefaultQuery("preloadCount", "0"))
				if clientPreloadCount > 0 {
					preloadCount = clientPreloadCount
					debugLog("[PRELOAD HEAD] Using client-requested preload count: %d", preloadCount)
				}

				if cfg.MaxPreloadCount > 0 && preloadCount > cfg.MaxPreloadCount {
					debugLog("[PRELOAD HEAD] Capping at max: %d", cfg.MaxPreloadCount)
					preloadCount = cfg.MaxPreloadCount
				}

				debugLog("[PRELOAD HEAD] Final preload count: %d", preloadCount)

				addPreloadHeadersWithCount(c, "/api/media", sanitizedPath, section, bucket, index, total, preloadCount, nextPaths, prevPaths, width, height, dpr)
			}

			// For nocache=1, the GET handler generates responsive images in memory.
			// We cannot predict the exact size, so return minimal headers and let
			// the client discover the actual size from the GET response.
			if nocache {
				c.Header("Content-Type", contentType)
				c.Header("X-Cache", "MEMORY-ONLY")
				// Stamp X-File-Mtime so the client's HEAD-based mtime
				// discovery (reader/modal preload flows) can version URLs.
				setFileVersionHeaders(c, candidate)
				if bucket > 0 {
					c.Header("X-Image-Bucket", strconv.Itoa(bucket))
				}
				c.Status(200)
				return
			}

			// No responsive bucket or fullSize â€” return original file info
			if bucket == 0 {
				c.Header("Content-Type", contentType)
				c.Header("Content-Length", strconv.FormatInt(info.Size(), 10))
				setFileVersionHeaders(c, candidate)
				c.Status(200)
				return
			}

			// Check for cached responsive version
			cachePath, actualBucket, cacheHit := thumbnailPool.CheckCache(sanitizedPath, bucket)
			if cacheHit {
				if cacheInfo, err := os.Stat(cachePath); err == nil {
					c.Header("Content-Type", contentType)
					c.Header("Content-Length", strconv.FormatInt(cacheInfo.Size(), 10))
					c.Header("X-Cache", "HIT")
					c.Header("X-Image-Bucket", strconv.Itoa(actualBucket))
					setFileVersionHeaders(c, candidate)
					c.Status(200)
					return
				}
			}

			// No cache available
			c.Header("Content-Type", contentType)
			setFileVersionHeaders(c, candidate)
			c.Header("Content-Length", strconv.FormatInt(info.Size(), 10))
			c.Status(200)
		})
	}

	r.NoRoute(func(c *gin.Context) {
		path := c.Request.URL.Path
		debugLog("Static file request: %s", path)

		// Skip API routes
		if strings.HasPrefix(path, "/api/") {
			c.JSON(404, gin.H{"error": "Not found"})
			return
		}

		// Use SanitizePath for consistent path validation
		sanitizedPath, err := SanitizePath(strings.TrimPrefix(path, "/"))
		if err != nil {
			c.JSON(403, gin.H{"error": "Forbidden"})
			return
		}

		// Convert to OS path
		cleanPath := filepath.FromSlash(sanitizedPath)

		// Get executable directory
		execPath, err := os.Executable()
		if err != nil {
			c.JSON(500, gin.H{"error": "Internal server error"})
			return
		}
		execDir := filepath.Dir(execPath)

		// Build full path to web/ subdirectory
		baseDir := filepath.Join(execDir, "web")
		fullPath := filepath.Join(baseDir, cleanPath)
		debugLog("Looking for file at: %s", fullPath)

		// Check if file exists
		info, err := os.Stat(fullPath)
		if err == nil && !info.IsDir() {
			// Stream the file directly without loading it entirely into memory.
			// This prevents high memory usage when serving large static files
			// concurrently to multiple clients.
			ext := strings.ToLower(filepath.Ext(fullPath))
			contentType := "application/octet-stream"
			switch ext {
			case ".js":
				contentType = "application/javascript; charset=utf-8"
			case ".css":
				contentType = "text/css; charset=utf-8"
			case ".json":
				contentType = "application/json; charset=utf-8"
			case ".svg":
				contentType = "image/svg+xml"
			case ".html", ".htm":
				contentType = "text/html; charset=utf-8"
			default:
				contentType = mime.TypeByExtension(ext)
				if contentType == "" {
					contentType = "application/octet-stream"
				}
			}

			c.Header("Content-Type", contentType)
			// Prevent aggressive browser caching of JS/CSS/HTML files.
			// Chromium caches ES modules independently of the service worker,
			// so explicit no-cache headers ensure browsers revalidate on each load.
			// Combined with the SW's network-first strategy for JS and the
			// ?v=N query string on the entry script, this ensures stale modules
			// are refreshed on each page visit.
			switch ext {
			case ".js", ".mjs", ".css", ".html", ".htm":
				c.Header("Cache-Control", "no-cache, must-revalidate")
			default:
				// Other static assets (images, fonts, etc.) can be cached longer
				c.Header("Cache-Control", "public, max-age=3600")
			}
			c.File(fullPath)
			return
		}

		// For directories, try index.html
		if info != nil && info.IsDir() {
			indexPath := filepath.Join(fullPath, "index.html")
			if _, err := os.Stat(indexPath); err == nil {
				c.Header("Content-Type", "text/html; charset=utf-8")
				c.File(indexPath)
				return
			}
		}

		// Default to index.html for SPA routing
		c.Header("Content-Type", "text/html; charset=utf-8")
		c.Header("Cache-Control", "no-cache, must-revalidate")
		c.File(filepath.Join(baseDir, "index.html"))
	})

	// Start server with graceful shutdown
	// Server timeouts prevent slow clients from holding connections indefinitely.
	// WriteTimeout is 0 (disabled) because media streams can take arbitrarily long.
	// Instead, MinWriteRateMiddleware enforces a minimum download speed â€”
	// connections receiving data at â‰¥ MinWriteRateBytes/sec stay alive, while
	// stalled or slow connections are terminated.
	srv := &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.Port),
		Handler:           r,
		ReadTimeout:       30 * time.Second,  // Time allowed to read the full request (including body)
		WriteTimeout:      0,                 // Disabled â€” MinWriteRateMiddleware handles slow writes
		IdleTimeout:       120 * time.Second, // Time to keep keep-alive connections alive between requests
		ReadHeaderTimeout: 10 * time.Second,  // Time allowed to read request headers
	}

	// serverReady is closed once the listening socket is bound, signaling the
	// tray (and any auto-open behavior) that the HTTP server can accept
	// connections. This prevents "Open in Browser" from opening a URL before
	// the server is reachable, which let a service-worker-cached shell render
	// with no server data.
	serverReady := make(chan struct{})

	// Bind the listening socket explicitly so we know the port is bound the
	// moment net.Listen returns â€” this gives a reliable "server ready" signal
	// for the tray's "Open in Browser" action, so it never opens a browser
	// before the server can actually accept connections (which previously let
	// a service-worker-cached shell render with no server data).
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", cfg.Port))
	if err != nil {
		// In tray mode (GUI-subsystem build) there is no console, so a plain
		// log.Fatalf would be invisible (written only to server.log) and the
		// app would appear to do nothing. Surface the error in a dialog first
		// so the user sees e.g. "port 3000 already in use" and can act, then
		// exit. Flush the log so the failure is also recorded in server.log.
		msg := fmt.Sprintf("Failed to listen on port %d: %v\n\n"+
			"Another process may already be using this port. Try a different port with -port=<n>.", cfg.Port, err)
		log.Printf("[FATAL] %s", msg)
		if useTrayEarly {
			showFatalError(msg)
		}
		os.Exit(1)
	}
	log.Printf("Server ready at http://localhost:%d", cfg.Port)
	close(serverReady) // signal the tray that the HTTP server is accepting connections

	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Server error: %v", err)
		}
	}()

	// Run pending v1â†’v2 hash migrations in the background.
	// These are enqueued during index loading when a v1 index is found.
	// By deferring migration until after the server starts listening,
	// we avoid blocking startup for minutes while hashing thousands of
	// files on a network drive. The server serves data without hashes
	// (dedup is disabled) until migration completes, then saves the v2
	// index so the next startup is instant.
	if len(pendingMigrations) > 0 {
		go func() {
			for _, task := range pendingMigrations {
				log.Printf("[MIGRATE] Starting background hash migration for section %q", task.section)
				start := time.Now()
				migrateFileHashes(db, task.section, task.dir)
				// Persist the migrated index as v2 so we don't re-hash on next startup
				if err := saveSectionIndex(getCurrentConfig(), db, task.section); err != nil {
					log.Printf("[MIGRATE] Warning: failed to save migrated index for %q: %v", task.section, err)
				} else {
					log.Printf("[MIGRATE] Saved v2 index for section %q (took %v)", task.section, time.Since(start))
				}
			}
		}()
	}

	// Wait for quit signal (from tray, Ctrl+C, or termination)
	quitCh := make(chan struct{})
	var quitOnce sync.Once
	triggerQuit := func() {
		quitOnce.Do(func() { close(quitCh) })
	}

	useTray := !*daemon && !*noTray

	if useTray {
		preventConsoleClose(triggerQuit)
		// Wire the synchronous index-flush hook used by the CTRL_CLOSE_EVENT
		// handler so pending debounced saves are persisted before the OS can
		// hard-kill the process (~5s deadline). cfg and db are in scope here.
		triggerShutdownFlush = func() { flushPendingIndexSaves(cfg, db) }
		// This app is built as a GUI-subsystem binary (-H windowsgui) in tray
		// mode, so there is NO console window â€” the app lives entirely in the
		// system tray. That means: no taskbar item ever appears (no window),
		// the X-button-can-kill-the-app problem is eliminated (no console X),
		// and "Show Window" / "Hide Window" show/hide an on-demand console
		// for live log viewing. Logs go to server.log next to the exe.
		log.Println("[TRAY] Running in tray mode (no console window) â€” control via the tray icon: Show Window / Hide Window / Open in Browser / Quit")
		// One-time first-run notice so existing upgraders (accustomed to a
		// console window) learn the app is now tray-only. Gated by a marker
		// file; silent on subsequent runs.
		showFirstRunNotice()
		go func() {
			runTray(cfg.Port, serverReady, quitCh, triggerQuit)
		}()
		// hideConsoleWindow is a no-op when there is no console (GUI-subsystem
		// build), but kept for the -notray/console case where a console exists.
		hideConsoleWindow()
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)

	select {
	case <-quitCh:
		log.Println("[TRAY] Quit requested from system tray")
	case sig := <-sigCh:
		log.Printf("[SIGNAL] Received signal: %v", sig)
	}

	showConsoleWindow()
	log.Println("Shutting down...")

	// Graceful shutdown with 5 second timeout
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		log.Printf("Server forced to shutdown: %v", err)
	}

	// Stop notification notifiers. Snapshot pointers under configMu.
	configMu.Lock()
	gn := gotifyNotifier
	dn := discordNotifier
	configMu.Unlock()
	if gn != nil {
		gn.Stop()
	}
	if dn != nil {
		dn.Stop()
	}

	thumbnailPool.Stop()
	rateLimiter.Stop()

	log.Println("Server gracefully stopped")
	// NOTE: trayLogFile and its buffered writer are closed via the defer
	// registered in the tray-mode log setup block above.
}
