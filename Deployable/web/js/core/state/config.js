/**
 * ConfigState - User preferences and app configuration
 */

import { apiService } from '../../services/service-registry.js';
import { handles } from '../../db/db-registry.js';

export const configState = {
    serverMode: false,
    serverConfig: null,
    handles: new Map(),
    dirFormats: new Map(),
    excludedTags: new Set(),
    darkMode: true,
    sidebarCollapsed: false,
    // Cached init promise so a second init() call (e.g. from a late event
    // listener) returns the in-flight result instead of re-running
    // detectServerMode, which would race with the first call and could flip
    // serverMode back to false mid-load. Reset to undefined only if init ever
    // needs to be re-run intentionally.
    _initPromise: null,

    async detectServerMode() {
        this.serverMode = await apiService.detectMode();
        this.serverConfig = apiService.serverConfig;
        return this.serverMode;
    },

    isServerMode() {
        return this.serverMode;
    },

    getMediaUrl(path, options = {}) {
        if (this.serverMode) {
            // Pass through options (including section) for responsive images
            return apiService.getMediaUrl(path, options);
        }
        return null;
    },

    getThumbnailUrl(path, section = 'images') {
        if (this.serverMode) {
            return apiService.getThumbnailUrl(path, section);
        }
        return null;
    },

    async setHandle(section, handle) {
        this.handles.set(section, handle);
        await handles.save(section, handle);
    },

    async getHandle(section) {
        if (!this.handles.has(section)) {
            const handle = await handles.get(section);
            if (handle) this.handles.set(section, handle);
        }
        return this.handles.get(section);
    },

    async clearHandle(section) {
        this.handles.delete(section);
        await handles.save(section, null);
    },

    setDirFormat(path, format) {
        this.dirFormats.set(path, format);
    },

    getDirFormat(path) {
        return this.dirFormats.get(path) || 'standard';
    },

    async loadDirFormats() {
        const { db } = await import('../../db/index.js');
        const result = await db.get(db.stores.DIR_FORMATS, 'all');
        if (result) {
            Object.entries(result).forEach(([path, format]) => {
                this.dirFormats.set(path, format);
            });
        }
    },

    addExcludedTag(tag) {
        const sanitized = tag.toLowerCase().trim();
        this.excludedTags.add(sanitized);
        this.saveExcludedTags();
    },

    removeExcludedTag(tag) {
        this.excludedTags.delete(tag.toLowerCase());
        this.saveExcludedTags();
    },

    hasExcludedTag(tag) {
        return this.excludedTags.has(tag.toLowerCase());
    },

    saveExcludedTags() {
        localStorage.setItem('userExcludedTags', Array.from(this.excludedTags).join(','));
    },

    loadExcludedTags() {
        const stored = localStorage.getItem('userExcludedTags');
        if (stored) {
            this.excludedTags = new Set(stored.split(',').filter(t => t));
        }
    },

    toggleDarkMode() {
        this.darkMode = !this.darkMode;
        document.body.classList.toggle('dark-mode', this.darkMode);
        localStorage.setItem('theme', this.darkMode ? 'dark' : 'light');
    },

    loadTheme() {
        const theme = localStorage.getItem('theme');
        this.darkMode = theme !== 'light';
        document.body.classList.toggle('dark-mode', this.darkMode);
    },

    toggleSidebar() {
        this.setSidebarCollapsed(!this.sidebarCollapsed);
    },

    setSidebarCollapsed(collapsed) {
        this.sidebarCollapsed = collapsed;
        this.applySidebarState();
        localStorage.setItem('sidebarCollapsed', String(collapsed));
    },

    loadSidebarState() {
        // Mobile: the sidebar is a slide-in drawer, closed by default —
        // the floating hamburger opens it. Desktop: the strip is always
        // visible (open) unless the user collapsed it.
        const isMobile = window.matchMedia('(max-width: 768px)').matches;
        this.sidebarCollapsed = isMobile
            ? true
            : localStorage.getItem('sidebarCollapsed') === 'true';
        this.applySidebarState();
    },

    /**
     * Single source of truth for sidebar visibility. On mobile the sidebar
     * is a fixed slide-in drawer: collapsed = off-canvas (translateX),
     * open = .sidebar-drawer-open with the backdrop visible. On desktop it's
     * the classic strip and .collapsed narrows it. _applySectionUI calls
     * this on every section entry so the state always settles to the
     * persisted choice; body.reader-open (reader) hides it entirely on all
     * breakpoints.
     */
    applySidebarState() {
        const sidebar = document.getElementById('sidebar');
        const backdrop = document.getElementById('sidebar-backdrop');
        const open = !this.sidebarCollapsed;
        sidebar?.classList.toggle('sidebar-drawer-open', open);
        backdrop?.classList.toggle('visible', open);
    },

    async init() {
        // Guard against double-init: return the cached promise so concurrent
        // or repeated callers await the same detection pass instead of racing
        // two detectServerMode() calls (which could interleave and leave
        // serverMode in the wrong state for the duration of the session).
        if (this._initPromise) {
            return this._initPromise;
        }
        this._initPromise = (async () => {
            this.loadTheme();
            this.loadExcludedTags();
            this.loadSidebarState();
            this.loadPreloadSettings();
            this.loadImageCacheLimit();
            await this.loadDirFormats();
            await this.detectServerMode();
        })();
        return this._initPromise;
    },

    // Preload settings
    preloadDesktopCount: 3,

    getPreloadCount() {
        const isMobile = window.matchMedia('(max-width: 768px)').matches;
        if (isMobile) return 1; // Fixed for mobile
        return this.preloadDesktopCount;
    },

    setPreloadCount(count) {
        const num = parseInt(count, 10);
        if (num >= 1 && num <= 5) {
            this.preloadDesktopCount = num;
            localStorage.setItem('preload-desktop-count', num);
        }
    },

    loadPreloadSettings() {
        const stored = localStorage.getItem('preload-desktop-count');
        if (stored) {
            const count = parseInt(stored, 10);
            if (count >= 1 && count <= 5) {
                this.preloadDesktopCount = count;
            }
        }
    },

    // Image-cache limit: max number of cached images the client keeps.
    // Drives the modal preload cache hard cap and (via a SW postMessage)
    // the service worker's media-cache LRU bound. 0 = unlimited.
    imageCacheLimit: 200,

    getImageCacheLimit() {
        return this.imageCacheLimit;
    },

    setImageCacheLimit(limit) {
        const num = parseInt(limit, 10);
        if (!Number.isNaN(num) && num >= 0) {
            this.imageCacheLimit = num;
            localStorage.setItem('image-cache-limit', num);
        }
    },

    loadImageCacheLimit() {
        const stored = localStorage.getItem('image-cache-limit');
        if (stored === null) return;
        const num = parseInt(stored, 10);
        if (!Number.isNaN(num) && num >= 0) {
            this.imageCacheLimit = num;
        }
    },

    // Push the current limit to the service worker so its media-cache LRU
    // honors the user's choice. Safe to call at any time: if no SW controls
    // the page the message is silently dropped.
    syncImageCacheLimit() {
        try {
            navigator.serviceWorker?.controller?.postMessage({
                type: 'SET_MEDIA_CACHE_LIMIT',
                limit: this.imageCacheLimit,
            });
        } catch {
            // SW unavailable (e.g. insecure context) — the SW default holds.
        }
    }
};
