/**
 * ApiService - Centralized server API client
 * Eliminates scattered fetch calls throughout script.js
 */

import { debug } from '../../utils/debug.js';

// Thrown when a fetch exceeds its timeout. Callers can check `err.isTimeout`
// (or use `instanceof ApiTimeoutError`) to show a user-facing "request timed
// out" message instead of a generic network error.
export class ApiTimeoutError extends Error {
    constructor(message = 'Request timed out') {
        super(message);
        this.name = 'ApiTimeoutError';
        this.isTimeout = true;
    }
}

export class ApiService {
    constructor() {
        this.isServerMode = false;
        this.serverConfig = null;
        this.baseUrl = '';
        this.devicePixelRatio = window.devicePixelRatio || 1;
        this.isMobile = window.matchMedia('(max-width: 768px)').matches;
        this.screenWidth = window.screen.width;
        this.screenHeight = window.screen.height;

        // Default timeout for API requests. Long operations (scan/reindex on
        // large libraries) can pass a larger timeoutMs to fetchWithTimeout.
        this.defaultTimeoutMs = 15000;

        // path -> mtime (unix seconds), learned from the backend's
        // X-File-Mtime response header. Used to version media/thumbnail URLs
        // with ?v=<mtime> so an in-place edit busts the browser cache even
        // though cache keys are mutable relative paths.
        this._fileMtimes = new Map();

        // Update on resize/orientation change
        window.addEventListener('resize', () => {
            this.isMobile = window.matchMedia('(max-width: 768px)').matches;
            this.screenWidth = window.screen.width;
            this.screenHeight = window.screen.height;
            this.devicePixelRatio = window.devicePixelRatio || 1;
        });
    }

    // recordFileMtime stores a path's mtime from an X-File-Mtime response
    // header. Best-effort: unknown paths simply have no version param.
    recordFileMtime(path, mtimeSeconds) {
        if (path && mtimeSeconds) this._fileMtimes.set(path, String(mtimeSeconds));
    }

    // fileVersionParam returns "v=<mtime>" for a known path, or '' otherwise.
    fileVersionParam(path) {
        const mtime = this._fileMtimes.get(path);
        return mtime ? `v=${mtime}` : '';
    }

    // fetchWithTimeout wraps fetch with an AbortController-based timeout so a
    // hung connection doesn't leave the UI spinning indefinitely. On timeout
    // it throws ApiTimeoutError. `options` is the normal fetch options plus an
    // optional `timeoutMs` (defaults to this.defaultTimeoutMs).
    async fetchWithTimeout(url, options = {}) {
        const { timeoutMs, ...fetchOpts } = options;
        const timeout = timeoutMs || this.defaultTimeoutMs;
        const controller = new AbortController();
        const timer = setTimeout(() => controller.abort(), timeout);
        try {
            const resp = await fetch(url, { ...fetchOpts, signal: controller.signal });
            this._maybeRecordFileMtime(url, resp);
            return resp;
        } catch (e) {
            // An AbortError means our timeout fired; surface it as a typed error
            // so callers can distinguish it from a genuine network failure.
            if (e.name === 'AbortError') {
                throw new ApiTimeoutError(`Request to ${url} timed out after ${timeout}ms`);
            }
            throw e;
        } finally {
            clearTimeout(timer);
        }
    }

    // _maybeRecordFileMtime feeds _fileMtimes from X-File-Mtime responses so
    // getMediaUrl/getThumbnailUrl can append ?v=<mtime> version params. The
    // mtime header is stamped by the backend on every /api/media and
    // /api/thumbnail response (including the streamed nocache variants).
    // Without this hook, fileVersionParam() always returned '' and the
    // intended per-file-version cache busting never materialized — which is
    // why previously-served images stopped being cache hits.
    _maybeRecordFileMtime(url, resp) {
        try {
            if (!resp?.ok) return;
            const mtime = resp.headers.get('X-File-Mtime');
            if (!mtime) return;
            const pathMatch = url.match(/\/api\/(?:media|thumbnail)\/([^?]+)/);
            if (!pathMatch) return;
            this.recordFileMtime(decodeURIComponent(pathMatch[1]), mtime);
        } catch {
            // Header parsing is best-effort; never break the caller's fetch.
        }
    }

    async detectMode() {
        // Retry /api/config for a short window. The server may still be binding
        // its listening socket when the page loads (e.g. "Open in Browser"
        // clicked during startup, or a slow bind). Without retry, a single
        // transient failure left the app in non-server mode showing the UI
        // with no data even though the server was reachable a moment later.
        //
        // Distinguish two failure modes so we don't penalize the no-server case:
        //  - A non-network response with a 4xx/5xx status (e.g. 404) means the
        //    origin is reachable but is NOT this server → bail out immediately
        //    (non-server mode) instead of retrying for 5s.
        //  - A network error (fetch threw) means the server isn't reachable yet
        //    → retry, since it may still be binding.
        const maxAttempts = 10;
        const delayMs = 500;
        for (let attempt = 1; attempt <= maxAttempts; attempt++) {
            try {
                const resp = await this.fetchWithTimeout('/api/config', { timeoutMs: 2000 });
                if (resp.ok) {
                    const config = await resp.json();
                    if (config.serverMode) {
                        this.isServerMode = true;
                        this.serverConfig = config;
                        return true;
                    }
                }
                // Got an HTTP response, but not a serverMode config. The origin
                // is reachable but isn't this server — no point retrying.
                this.isServerMode = false;
                return false;
            } catch (e) {
                // Network error (server not reachable yet) — retry below.
            }
            if (attempt < maxAttempts) {
                await new Promise(r => setTimeout(r, delayMs));
            }
        }
        this.isServerMode = false;
        return false;
    }

    async getFiles(section, timeoutMs) {
        this._assertServerMode();
        const resp = await this.fetchWithTimeout(`/api/files?section=${section}`, { timeoutMs });
        if (!resp.ok) throw new Error(`Failed to load files: ${resp.status}`);
        const files = await resp.json();
        // Learn path -> mtime so media/thumbnail URLs can be versioned with
        // ?v=<mtime> (see fileVersionParam). The backend also stamps
        // X-File-Mtime on responses for paths that change between fetches.
        for (const f of files) {
            if (f?.path && f.mtime) this._fileMtimes.set(f.path, String(Math.floor(f.mtime)));
        }
        return files;
    }

    async getFolders(section, timeoutMs) {
        this._assertServerMode();
        const resp = await this.fetchWithTimeout(`/api/folders?section=${section}`, { timeoutMs });
        if (!resp.ok) throw new Error(`Failed to load folders: ${resp.status}`);
        const folders = await resp.json();
        // Chapters/images are nested; record leaf image mtimes for URL versioning.
        for (const folder of folders) {
            for (const chapter of folder?.chapters || []) {
                for (const img of chapter?.images || []) {
                    if (img?.path && img.mtime) this._fileMtimes.set(img.path, String(Math.floor(img.mtime)));
                }
            }
        }
        return folders;
    }

    // Series info endpoint (used by the SeriesSplash). Reads the
    // series-info.json sidecar next to the series folder and may also
    // resolve a local cover image (coverPath). Throws on any non-OK
    // response so the splash can fall back to its placeholder copy.
    async getSeriesInfo(section, seriesName) {
        this._assertServerMode();
        const encoded = encodeURIComponent(seriesName);
        const resp = await this.fetchWithTimeout(`/api/series-info?section=${section}&series=${encoded}`);
        if (!resp.ok) throw new Error(`Failed to load series info: ${resp.status}`);
        return resp.json();
    }

    async scan(section) {
        this._assertServerMode();
        // Scans over large libraries can take minutes; allow callers to raise
        // the timeout via options.timeoutMs. Default 5min mirrors the server's
        // own scan context timeout.
        const resp = await this.fetchWithTimeout(`/api/scan/${section}`, {
            method: 'POST',
            timeoutMs: 5 * 60 * 1000,
        });
        if (!resp.ok) throw new Error(`Scan failed: ${resp.status}`);
        return resp.json();
    }

    getMediaUrl(path, options = {}) {
        this._assertServerMode();
        const encodedPath = encodeURIComponent(path).replace(/%2F/g, '/').replace(/%5C/g, '/');

        const params = new URLSearchParams();

        // Add section parameter so backend knows which directory to search
        // This is critical for manga/h-manga media requests to resolve correctly
        if (options.section) {
            params.append('section', options.section);
        }

        // Add responsive image parameters for preloading
        // Width-only mode (no height) is used for vertical strip readers (manga/h-manga)
        // where height should be unconstrained
        if (options.width) {
            params.append('width', options.width);
            if (options.height) {
                params.append('height', options.height);
            }
            params.append('dpr', (options.dpr || 1).toFixed(1));
        }

        // Add nocache flag for memory-only images (used by modal)
        if (options.nocache) {
            params.append('nocache', '1');
        }

        // Add sequence context for preloading
        if (options.index !== undefined && options.total !== undefined) {
            params.append('index', options.index);
            params.append('total', options.total);
        }

        // Support preload count
        if (options.preloadCount && options.preloadCount > 0) {
            params.append('preloadCount', options.preloadCount);
        }

        // Support multiple next/prev paths for preloading
        if (options.nextPaths && Array.isArray(options.nextPaths)) {
            options.nextPaths.forEach(p => params.append('nextPaths', p));
        } else if (options.nextPath) {
            params.append('nextPaths', options.nextPath);
        }

        if (options.prevPaths && Array.isArray(options.prevPaths)) {
            options.prevPaths.forEach(p => params.append('prevPaths', p));
        } else if (options.prevPath) {
            params.append('prevPaths', options.prevPath);
        }

        const queryString = params.toString();
        // Version with the file's mtime when known (?v=<mtime>) — paths are
        // mutable, so without this an edited file keeps its 7-day-cached
        // bytes. X-File-Mtime responses feed _fileMtimes via recordFileMtime.
        const v = this.fileVersionParam(path);
        const fullUrl = queryString
            ? `/api/media/${encodedPath}?${queryString}${v ? '&' + v : ''}`
            : `/api/media/${encodedPath}${v ? '?' + v : ''}`;
        debug(`[API DEBUG] getMediaUrl: section=${options.section}, url=${fullUrl.substring(0, 200)}`);
        return fullUrl;
    }

    getThumbnailUrl(path, section = 'images') {
        this._assertServerMode();
        const encodedPath = encodeURIComponent(path).replace(/%2F/g, '/').replace(/%5C/g, '/');
        const v = this.fileVersionParam(path);
        return `/api/thumbnail/${encodedPath}?section=${section}${v ? '&' + v : ''}`;
    }

    // Tag management endpoints
    async getTagStats(section = 'images', timeoutMs) {
        this._assertServerMode();
        const resp = await this.fetchWithTimeout(`/api/tags/stats?section=${section}`, { timeoutMs });
        if (!resp.ok) throw new Error(`Failed to get tag stats: ${resp.status}`);
        return resp.json();
    }

    async getFilesByTag(tag, section = '') {
        this._assertServerMode();
        const encodedTag = encodeURIComponent(tag);
        const resp = await this.fetchWithTimeout(`/api/tags/${encodedTag}/files?section=${section}`);
        if (!resp.ok) throw new Error(`Failed to get files by tag: ${resp.status}`);
        return resp.json();
    }

    async updateFileTags(path, tags) {
        this._assertServerMode();
        const encodedPath = encodeURIComponent(path).replace(/%2F/g, '/').replace(/%5C/g, '/');
        const resp = await this.fetchWithTimeout(`/api/tags/${encodedPath}`, {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ tags })
        });
        if (!resp.ok) throw new Error(`Failed to update tags: ${resp.status}`);
        return resp.json();
    }

    async bulkUpdateTags(paths, { add = [], remove = [], set = [] } = {}) {
        this._assertServerMode();
        const resp = await this.fetchWithTimeout('/api/tags/bulk', {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ paths, add, remove, set })
        });
        if (!resp.ok) throw new Error(`Failed to bulk update tags: ${resp.status}`);
        return resp.json();
    }

    // Flush tags from EVERY file. Two modes:
    //  - Targeted: pass a non-empty tags array → only those tags are removed.
    //  - Full flush: call flushAllTags(section) → sends { all: true }.
    // The backend rejects an empty/missing body or an empty tags array without
    // { all: true }, so a malformed request can never silently wipe all tags.
    // Optional section restricts the operation to one section.
    async flushTags(tags, section = '') {
        this._assertServerMode();
        const qs = section ? `?section=${encodeURIComponent(section)}` : '';
        const resp = await this.fetchWithTimeout(`/api/tags/flush${qs}`, {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ tags })
        });
        if (!resp.ok) throw new Error(`Failed to flush tags: ${resp.status}`);
        return resp.json();
    }

    // Flush ALL tags from every file. Sends an explicit { all: true } opt-in
    // so the backend never infers a global wipe from a missing/nil tags array.
    async flushAllTags(section = '') {
        this._assertServerMode();
        const qs = section ? `?section=${encodeURIComponent(section)}` : '';
        const resp = await this.fetchWithTimeout(`/api/tags/flush${qs}`, {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ all: true })
        });
        if (!resp.ok) throw new Error(`Failed to flush all tags: ${resp.status}`);
        return resp.json();
    }

    // Reindex section endpoint (incremental — checks mtime, adds new,
    // removes deleted). The server returns 202 immediately and runs the
    // actual walk in a goroutine, so this client call only blocks for the
    // network round-trip. The outcome (changed/count/error) is cached on
    // the DB and observed later via getReindexStatus(). The user can
    // disconnect — the server keeps indexing.
    async reindexSection(section) {
        this._assertServerMode();
        const resp = await this.fetchWithTimeout(`/api/reindex/${section}`, {
            method: 'POST',
            timeoutMs: 30000, // 30s is plenty for the server to accept and detach
        });
        if (!resp.ok) {
            if (resp.status === 409) throw new Error('Scan already in progress');
            throw new Error(`Reindex failed: ${resp.status}`);
        }
        return resp.json(); // { section, status: "started", started, poll_url }
    }

    // Poll the most recent reindex result for a section. The server keeps
    // the result on its DB after the reindex goroutine completes, so a
    // client that disconnected (or that revisits later) can still observe
    // whether the section changed since the last reindex request.
    // Returns { section, running, has_result, result: {started_at,
    // completed_at, changed, count, error?} }. `running` reflects the
    // server-side scanning lock; `has_result` is true once any reindex
    // has finished (or 0 has run yet).
    async getReindexStatus(section) {
        this._assertServerMode();
        const resp = await this.fetchWithTimeout(`/api/reindex/status?section=${encodeURIComponent(section)}`);
        if (!resp.ok) throw new Error(`Failed to get reindex status: ${resp.status}`);
        return resp.json();
    }

    // Directory rescan endpoint
    async rescanDirectory(section, folder, { quick = false } = {}) {
        this._assertServerMode();
        const mode = quick ? 'quick' : 'full';
        const encodedFolder = encodeURIComponent(folder);
        const resp = await this.fetchWithTimeout(`/api/scan/${section}/${encodedFolder}?mode=${mode}`, {
            method: 'POST',
            timeoutMs: 5 * 60 * 1000,
        });
        if (!resp.ok) throw new Error(`Failed to start rescan: ${resp.status}`);
        return resp.json();
    }

    // Thumbnail generation endpoints
    async getThumbnailStatus(section = 'images', timeoutMs) {
        this._assertServerMode();
        const resp = await this.fetchWithTimeout(`/api/thumbnails/status?section=${section}`, { timeoutMs });
        if (!resp.ok) throw new Error(`Failed to get thumbnail status: ${resp.status}`);
        return resp.json();
    }

    async generateMissingThumbnails(section = 'images', limit = 0) {
        this._assertServerMode();
        let url = `/api/thumbnails/generate?section=${section}`;
        if (limit > 0) url += `&limit=${limit}`;
        const resp = await this.fetchWithTimeout(url, { method: 'POST' });
        if (!resp.ok) throw new Error(`Failed to start thumbnail generation: ${resp.status}`);
        return resp.json();
    }

    async generateThumbnail(path, section = 'images') {
        this._assertServerMode();
        const encodedPath = encodeURIComponent(path).replace(/%2F/g, '/').replace(/%5C/g, '/');
        const resp = await this.fetchWithTimeout(`/api/thumbnails/generate/${encodedPath}?section=${section}`, {
            method: 'POST'
        });
        if (!resp.ok) throw new Error(`Failed to generate thumbnail: ${resp.status}`);
        return resp.json();
    }

    async cullOrphans(section = 'images') {
        this._assertServerMode();
        const resp = await this.fetchWithTimeout(`/api/thumbnails/cull-orphans?section=${section}`, { method: 'POST' });
        if (!resp.ok) throw new Error(`Failed to cull orphans: ${resp.status}`);
        return resp.json();
    }

    async getOrphans(section = 'images') {
        this._assertServerMode();
        const resp = await this.fetchWithTimeout(`/api/thumbnails/orphans?section=${section}`);
        if (!resp.ok) throw new Error(`Failed to get orphans: ${resp.status}`);
        return resp.json();
    }

    async getGotifyStatus(timeoutMs) {
        this._assertServerMode();
        const resp = await this.fetchWithTimeout('/api/gotify/status', { timeoutMs });
        if (!resp.ok) throw new Error(`Failed to get Gotify status: ${resp.status}`);
        return resp.json();
    }

    async toggleGotify(enabled) {
        this._assertServerMode();
        const resp = await this.fetchWithTimeout('/api/gotify/toggle', {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ enabled })
        });
        if (!resp.ok) throw new Error(`Failed to toggle Gotify: ${resp.status}`);
        return resp.json();
    }

    async updateGotifyConfig(settings) {
        this._assertServerMode();
        const resp = await this.fetchWithTimeout('/api/gotify/config', {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify(settings)
        });
        if (!resp.ok) throw new Error(`Failed to update Gotify config: ${resp.status}`);
        return resp.json();
    }

    async resetGotify() {
        this._assertServerMode();
        const resp = await this.fetchWithTimeout('/api/gotify/reset', {
            method: 'POST'
        });
        if (!resp.ok) throw new Error(`Failed to reset Gotify: ${resp.status}`);
        return resp.json();
    }

    async getGotifyPending() {
        this._assertServerMode();
        const resp = await this.fetchWithTimeout('/api/gotify/pending');
        if (!resp.ok) throw new Error(`Failed to get pending notifications: ${resp.status}`);
        return resp.json();
    }

    async pushPendingNotifications() {
        this._assertServerMode();
        const resp = await this.fetchWithTimeout('/api/gotify/push-pending', {
            method: 'POST'
        });
        if (!resp.ok) throw new Error(`Failed to push pending notifications: ${resp.status}`);
        return resp.json();
    }

    async getDiscordStatus(timeoutMs) {
        this._assertServerMode();
        const resp = await this.fetchWithTimeout('/api/discord/status', { timeoutMs });
        if (!resp.ok) throw new Error(`Failed to get Discord status: ${resp.status}`);
        return resp.json();
    }

    async toggleDiscord(enabled) {
        this._assertServerMode();
        const resp = await this.fetchWithTimeout('/api/discord/toggle', {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ enabled })
        });
        if (!resp.ok) throw new Error(`Failed to toggle Discord: ${resp.status}`);
        return resp.json();
    }

    async updateDiscordConfig(settings) {
        this._assertServerMode();
        const resp = await this.fetchWithTimeout('/api/discord/config', {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify(settings)
        });
        if (!resp.ok) throw new Error(`Failed to update Discord config: ${resp.status}`);
        return resp.json();
    }

    async testDiscord() {
        this._assertServerMode();
        const resp = await this.fetchWithTimeout('/api/discord/test', {
            method: 'POST'
        });
        if (!resp.ok) throw new Error(`Failed to test Discord: ${resp.status}`);
        return resp.json();
    }

    async getDiscordPending() {
        this._assertServerMode();
        const resp = await this.fetchWithTimeout('/api/discord/pending');
        if (!resp.ok) throw new Error(`Failed to get Discord pending: ${resp.status}`);
        return resp.json();
    }

    async pushDiscordPending() {
        this._assertServerMode();
        const resp = await this.fetchWithTimeout('/api/discord/push-pending', {
            method: 'POST'
        });
        if (!resp.ok) throw new Error(`Failed to push Discord pending: ${resp.status}`);
        return resp.json();
    }

    _assertServerMode() {
        if (!this.isServerMode) {
            throw new Error('Not in server mode');
        }
    }
}
