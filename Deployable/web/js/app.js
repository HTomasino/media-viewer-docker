/**
 * App.js - Main application entry point
 */

import { configState, galleryState, readerState, historyRouter, SECTIONS, KEYBOARD_SHORTCUTS } from './core/core-registry.js';
import { _routeToSection, _sectionToRoute } from './core/state/gallery.js';
import { apiService, unifiedScanner } from './services/service-registry.js';
import { cache } from './db/db-registry.js';
import { favoritesStore } from './db/favorites.js';
import { Gallery, MangaReader, MediaModal, SeriesSplash } from './components/components-registry.js';
import { showNotification, withErrorBoundary, escapeHtml, debug } from './utils/utils-registry.js';

// Resolve the section for a series stub from galleryState.folders.
// Manga sections use chapter objects as folders; h-manga uses artist objects
// with a chapters array. Returns null if no match.
function _resolveSeriesFolder(seriesStub) {
    if (!seriesStub) return null;
    // Path match is only meaningful when both sides have a defined path.
    // The backend's Series struct doesn't include a `path` field, so for
    // manga folders seriesStub.path is `undefined` and a path comparison
    // would short-circuit to the first folder unconditionally. Match by
    // name as the primary identifier, and use path as a tie-breaker when
    // both sides have it.
    return galleryState.folders.find(f => {
        if (seriesStub.path && f.path === seriesStub.path) return true;
        return f.name === seriesStub.name;
    }) || null;
}

/**
 * Best-effort section inference from a series stub path. The server's path
 * layout is `<base>/<section>/<artist-or-series>/...`. We split on `/` and
 * pick the second segment, mapping it to a SECTIONS constant. Falls back
 * to the current gallery section if the path doesn't match the expected
 * layout (e.g. client mode where paths are relative).
 */
function _inferSeriesSection(seriesStub) {
    if (!seriesStub?.path) return galleryState.section;
    const parts = String(seriesStub.path).split('/').filter(Boolean);
    if (parts.length >= 2) {
        const sec = _routeToSection(parts[1]);
        if (sec) return sec;
    }
    return galleryState.section;
}

class MediaViewerApp {
    constructor() {
        this.gallery = null;
        this.reader = null;
        this.modal = null;
        this.splash = null;
        this.currentFavoriteIdentifier = null;
        this.currentFavoriteSection = null;
        this.currentFavoriteName = null;
        // Monotonic token for loadServerData — discards stale async responses
        // when the user switches sections faster than the fetches resolve.
        this._loadServerSeq = 0;
    }

    async init() {
        console.log('[APP] Initializing...');
        
        await configState.init();
        
        this.gallery = new Gallery(document.getElementById('gallery-container'));
        this.reader = new MangaReader(document.getElementById('manga-view-container'));
        this.modal = new MediaModal();
        this.splash = new SeriesSplash(document.getElementById('series-splash-container'));

        this._registerRoutes();
        
        this.setupEventListeners();
        await this.loadInitialData();

        this.updateFavoritesVisibility();

        console.log('[APP] Initialization complete');
    }

    _registerRoutes() {
        historyRouter.register('gallery:images', {
            enter: (payload) => this._navShowSection('images', payload),
        });
        historyRouter.register('gallery:manga', {
            enter: (payload) => this._navShowSection('manga', payload),
        });
        historyRouter.register('gallery:hmanga', {
            enter: (payload) => this._navShowSection('h-manga', payload),
        });
        historyRouter.register('gallery:hmanga:artist', {
            enter: (payload) => this._navShowArtist(payload?.artist),
        });
        historyRouter.register('splash:open', {
            enter: async (payload) => {
                if (!payload?.series) return;
                if (this.splash.isOpen() && this.splash.currentSeries?.name === payload.series.name) return;
                let series = _resolveSeriesFolder(payload.series);
                // Deep-link fallback: fetch series info + folders from server.
                if (!series?.chapters && configState.isServerMode()) {
                    try {
                        const sec = _inferSeriesSection(payload.series);
                        await apiService.getSeriesInfo(sec, payload.series.name);
                        const folders = await apiService.getFolders(sec);
                        galleryState.setFolders(folders);
                        series = _resolveSeriesFolder(payload.series);
                    } catch (e) {
                        console.warn('[APP] Failed to resolve deep-linked series for splash:', e);
                    }
                }
                if (!series?.chapters) return;
                this.splash.open(series);
            },
            exit: () => this.splash.close(),
        });
        historyRouter.register('modal:open', {
            enter: (payload) => {
                if (!payload?.file) return;
                if (this.modal.isOpen && this.modal.currentFile?.path === payload.file.path) return;
                const inGallery = galleryState.displayedItems.find(f => f.path === payload.file.path);
                if (!inGallery) return;
                this.modal.open(inGallery, galleryState.displayedItems);
            },
            exit: () => this.modal.close(),
        });
        historyRouter.register('reader:open', {
            enter: async (payload) => {
                if (!payload?.series || payload.chapterIndex == null) return;
                if (this.reader.isOpen && readerState.series?.name === payload.series.name && readerState.chapterIndex === payload.chapterIndex) return;
                // Edge case #13: close any previously-open reader first so
                // sidebar/top-header/breadcrumbs/favorite tracking fully
                // reset before the new reader takes over.
                if (this.reader.isOpen) this.reader.close();
                let series = _resolveSeriesFolder(payload.series);
                // Deep-link fallback: the stub doesn't carry chapters. Try
                // fetching series info from the server (which returns the
                // chapter list as part of the sidecar or by scanning the
                // folder). In client mode this is a no-op and we bail.
                if (!series?.chapters && configState.isServerMode()) {
                    try {
                        await apiService.getSeriesInfo(_inferSeriesSection(payload.series), payload.series.name);
                        const folders = await apiService.getFolders(_inferSeriesSection(payload.series));
                        galleryState.setFolders(folders);
                        series = _resolveSeriesFolder(payload.series);
                    } catch (e) {
                        console.warn('[APP] Failed to resolve deep-linked series:', e);
                    }
                }
                if (!series?.chapters) return;
                // restoreProgress is opt-OUT via payload: the splash flow
                // stamps `explicit: true` because the user just picked an
                // exact chapter (an old progress record must not hijack it).
                // Every other entry — replay after mobile tab reload,
                // deep-link shares — defaults to restoring saved progress.
                // Hardcoding restoreProgress:false here is what broke
                // "latest chapter read" on mobile: browsers discard and
                // reload tabs aggressively, replay re-entered reader:open
                // at the payload's original chapterIndex and ignored the
                // progress record entirely.
                this.reader.open(series, payload.chapterIndex, {
                    restoreProgress: !payload.explicit,
                });
            },
            exit: () => this.reader.close(),
        });
    }

    /**
     * Apply section-level UI resets (nav button, favorites toggle, favorites
     * off, page reset). Used by both _navShowSection and the section-change
     * handler to keep behavior in one place.
     */
    _applySectionUI(section) {
        document.querySelectorAll('.nav-btn').forEach(b => b.classList.remove('active'));
        const navBtn = document.querySelector(`.nav-btn[data-section="${section}"]`);
        if (navBtn) navBtn.classList.add('active');

        galleryState.section = section;
        galleryState.currentArtist = null;
        galleryState.page = 0;
        galleryState.activeTag = null;
        galleryState._chapterArtistMap.clear();
        galleryState.favoritesOnly = false;
        // Refresh displayedItems so it reflects the new section. Without
        // this, displayedItems still references the previous section's
        // items (e.g. manga folders, or h-manga chapters inside an artist)
        // and the render() call in _navShowSection's `hasData` short-circuit
        // path keeps painting the stale cards.
        galleryState.updateDisplay();

        const toggleBtn = document.getElementById('favorites-toggle-btn');
        if (toggleBtn) {
            toggleBtn.textContent = '☆ Show Favorites Only';
            toggleBtn.classList.remove('active');
        }
        // Re-assert the user's persisted sidebar preference on every
        // section entry — the reader temporarily hides the sidebar via
        // body.reader-open, and any stale .collapsed state from a
        // transition settles here to exactly what the user chose.
        configState.applySidebarState();
        this.updateFavoritesVisibility();
    }

    async _navShowSection(section, filterPayload = null) {
        this._applySectionUI(section);
        // Apply filter state from the URL payload (if any) BEFORE the
        // hasData check so the rendered gallery reflects the filter. We
        // only apply filters that are explicitly present in the payload
        // to avoid clobbering the user's current filter on back navigation.
        if (filterPayload) {
            if ('tag' in filterPayload) galleryState.activeTag = filterPayload.tag || null;
            if ('favorites' in filterPayload) galleryState.favoritesOnly = !!filterPayload.favorites;
            galleryState.page = 0;
            galleryState.updateDisplay();
        }
        // Paint the cached snapshot first so the section switch feels
        // instant. The network fetch then runs in the background and
        // overwrites via setFiles/setFolders → updateDisplay →
        // gallery.render() inside loadServerData; the user sees fresh
        // data the moment it arrives without waiting on the network for
        // the first paint. The *Section tracker ensures we don't render
        // the wrong section's stale data (folders from a previous
        // h_manga visit don't satisfy a manga back-to-gallery).
        const hasData = section === SECTIONS.IMAGES
            ? (galleryState.files?.length > 0 && galleryState.filesSection === section)
            : (galleryState.folders?.length > 0 && galleryState.foldersSection === section);
        if (hasData) {
            this.gallery.render();
        }
        this._syncFilterUI();
        // In server mode, skip the cached tag render — the background
        // loadServerData() will trigger renderPopularTags() with fresh
        // data within milliseconds, so doing it twice is just wasted
        // DOM work. In client mode there IS no background refresh
        // (loadClientData is the only source of truth), so we still
        // render the cached tags synchronously.
        if (!configState.isServerMode()) {
            await this.renderPopularTags();
        }

        // Always fetch fresh data — the in-memory copy above is only a
        // render hint. In server mode the fetch runs in the background
        // so the navigation isn't blocked; the resulting setFiles /
        // setFolders will trigger a repaint via loadServerData. In
        // client mode we still need the synchronous load (it may pull
        // from the IndexedDB cache or surface the "select a directory"
        // prompt), so we await it as before.
        if (configState.isServerMode()) {
            this.loadServerData({ silent: true }).catch((e) => {
                console.warn('[APP] Background refresh failed:', e);
            });
        } else {
            await this.loadClientData();
        }
    }

    async _navShowArtist(artistStub) {
        if (!artistStub) return;
        const alreadyShowing = galleryState.currentArtist?.name === artistStub.name &&
            galleryState.currentArtist?.path === artistStub.path;
        // Ensure the h-manga section is loaded. Unlike _navShowSection
        // (where the section overview can paint an empty state and let
        // the background fill in), the artist drill-down CANNOT proceed
        // without the folder list — find() below needs it. So we MUST
        // await the load. The cached snapshot is still painted first
        // (when available) so the click feels instant.
        let needsSectionLoad = false;
        if (galleryState.section !== SECTIONS.H_MANGA) {
            this._applySectionUI(SECTIONS.H_MANGA);
            needsSectionLoad = true;
        } else if (galleryState.foldersSection !== SECTIONS.H_MANGA || !galleryState.folders?.length) {
            needsSectionLoad = true;
        }
        if (needsSectionLoad) {
            if (galleryState.foldersSection === SECTIONS.H_MANGA && galleryState.folders?.length) {
                this.gallery.render();
            }
            await this._loadCurrentSectionData();
        } else if (!alreadyShowing && configState.isServerMode()) {
            // Cached folders exist but may be stale (server-side reindex,
            // external change). Paint the cached snapshot immediately via
            // the normal path below, then refresh the folder list in the
            // background — loadServerData re-binds currentArtist against
            // the fresh list and re-renders. Skipping this (the old
            // behavior served cached chapters indefinitely) is what made
            // the artist view stale until manual renavigation.
            this.loadServerData({ silent: true }).catch((e) => {
                console.warn('[APP] Background artist refresh failed:', e);
            });
        }
        // alreadyShowing && !needsSectionLoad: the click handler's render
        // raced the router's enter — nothing new to paint.

        // Path match is only meaningful when both sides have a defined path.
        // The backend's Series struct doesn't include a `path` field, so for
        // manga folders artistStub.path is `undefined` and a path comparison
        // would short-circuit to the first folder unconditionally. Match by
        // name as the primary identifier, and use path as a tie-breaker when
        // both sides have it.
        const artist = galleryState.folders.find(f => {
            if (artistStub.path && f.path === artistStub.path) return true;
            return f.name === artistStub.name;
        });
        if (!artist) return;

        galleryState.currentArtist = artist;
        galleryState.page = 0;
        galleryState.updateDisplay();
        this.gallery.render();

        const breadcrumbs = document.getElementById('gallery-breadcrumbs');
        const breadcrumbName = document.getElementById('breadcrumb-artist-name');
        if (breadcrumbs) breadcrumbs.style.display = 'block';
        if (breadcrumbName) breadcrumbName.textContent = artist.name;

        await this.renderPopularTags();
    }

    

    setupEventListeners() {
        // Navigation
        document.querySelectorAll('.nav-btn[data-section]').forEach(btn => {
            btn.addEventListener('click', (e) => this.handleSectionChange(e));
        });

        // Reindex buttons (server-mode only)
        document.querySelectorAll('.reindex-btn[data-section]').forEach(btn => {
            btn.addEventListener('click', () => this.handleReindex(btn.dataset.section));
        });

        // Directory selection
        const selectDirBtn = document.getElementById('select-dir-btn');
        if (selectDirBtn) {
            selectDirBtn.addEventListener('click', () => {
                this.requestDirectoryAccess(galleryState.section);
            });
        }

        const fallbackInput = document.getElementById('fallback-dir-input');
        if (fallbackInput) {
            fallbackInput.addEventListener('change', (e) => this.handleFallbackFiles(e.target.files));
        }

        // Search
        const searchBar = document.getElementById('search-bar');
        if (searchBar) {
            searchBar.addEventListener('input', (e) => this.handleSearch(e.target.value));
        }

        // Theme toggle
        const themeToggle = document.getElementById('theme-toggle');
        if (themeToggle) {
            themeToggle.addEventListener('click', () => configState.toggleDarkMode());
        }

        // Settings dropdown
        const settingsToggle = document.getElementById('settings-toggle');
        const settingsDropdown = document.getElementById('settings-dropdown');

        if (settingsToggle && settingsDropdown) {
            // Toggle dropdown visibility
            settingsToggle.addEventListener('click', (e) => {
                e.stopPropagation();
                const isVisible = settingsDropdown.style.display === 'block';
                settingsDropdown.style.display = isVisible ? 'none' : 'block';
            });

            // Close when clicking outside
            document.addEventListener('click', (e) => {
                if (!settingsDropdown.contains(e.target) && e.target !== settingsToggle) {
                    settingsDropdown.style.display = 'none';
                }
            });
        }

        // Preload count setting
        const preloadCountSelect = document.getElementById('preload-count-select');
        if (preloadCountSelect) {
            // Load saved value
            const currentCount = configState.getPreloadCount();
            preloadCountSelect.value = currentCount.toString();

            // Handle changes
            preloadCountSelect.addEventListener('change', (e) => {
                const count = parseInt(e.target.value, 10);
                configState.setPreloadCount(count);
                console.log('[SETTINGS] Preload count set to:', count);
            });
        }

        // Manga page fill setting
        const mangaPageFillSelect = document.getElementById('manga-page-fill-select');
        if (mangaPageFillSelect) {
            // Load saved value from galleryState
            const savedMode = localStorage.getItem('mangaScaleMode') || 'original';
            mangaPageFillSelect.value = savedMode;

            // Show/hide percentage dropdown based on mode
            const fillPercentSelect = document.getElementById('manga-fill-percent-select');
            if (fillPercentSelect) {
                fillPercentSelect.style.display = savedMode === 'original' ? 'none' : 'block';
            }

            // Handle changes
            mangaPageFillSelect.addEventListener('change', (e) => {
                const mode = e.target.value;
                localStorage.setItem('mangaScaleMode', mode);
                // Show/hide percentage dropdown
                const fillPercentEl = document.getElementById('manga-fill-percent-select');
                if (fillPercentEl) {
                    fillPercentEl.style.display = mode === 'original' ? 'none' : 'block';
                }
                // Apply to current reader if open
                if (this.reader && this.reader.isOpen) {
                    if (typeof readerState.setScaleMode === 'function') {
                        readerState.setScaleMode(mode);
                    } else {
                        readerState.scaleMode = mode;
                        localStorage.setItem('mangaScaleMode', mode);
                    }
                    this.reader.setScaleMode(mode);
                }
                console.log('[SETTINGS] Manga page fill set to:', mode);
            });
        }

        // Fill percentage setting (header dropdown)
        const mangaFillPercentSelect = document.getElementById('manga-fill-percent-select');
        if (mangaFillPercentSelect) {
            const savedPercent = localStorage.getItem('mangaFillPercent') || '100';
            mangaFillPercentSelect.value = savedPercent;

            mangaFillPercentSelect.addEventListener('change', (e) => {
                const percent = parseInt(e.target.value, 10);
                if (typeof readerState.setFillPercent === 'function') {
                    readerState.setFillPercent(percent);
                } else {
                    readerState.fillPercent = percent;
                    localStorage.setItem('mangaFillPercent', percent);
                }
                if (this.reader && this.reader.isOpen) {
                    this.reader.setFillPercent(percent);
                }
                // Sync sidebar dropdown
                const sidebarFillPercent = document.getElementById('manga-fill-percent-side-select');
                if (sidebarFillPercent) sidebarFillPercent.value = percent;
                console.log('[SETTINGS] Fill percentage set to:', percent + '%');
            });
        }

        // Load mode setting (lazy vs sequential). The dropdown drives
        // readerState.loadMode (persisted) and is applied live to an open
        // reader via the MangaReader component's setLoadMode method.
        const loadModeSelect = document.getElementById('manga-load-mode-select');
        if (loadModeSelect) {
            // Load saved value (fall back to the config default)
            readerState.loadLoadMode();
            loadModeSelect.value = readerState.loadMode;

            // Keep the sidebar dropdown (if any) in sync with the saved value
            const sidebarLoadMode = document.getElementById('manga-load-mode-side-select');
            if (sidebarLoadMode) sidebarLoadMode.value = readerState.loadMode;

            loadModeSelect.addEventListener('change', (e) => {
                const mode = e.target.value;
                readerState.setLoadMode(mode);
                // Apply to current reader if open
                if (this.reader && this.reader.isOpen) {
                    this.reader.setLoadMode(mode);
                }
                if (sidebarLoadMode) sidebarLoadMode.value = mode;
                debug('[SETTINGS] Load mode set to:', mode);
            });
        }

        // Image cache limit setting. Controls both the modal's in-memory
        // preload cache and the service worker's persistent media cache
        // (persisted in localStorage, applied live on change).
        const imageCacheLimitSelect = document.getElementById('image-cache-limit-select');
        if (imageCacheLimitSelect) {
            imageCacheLimitSelect.value = configState.getImageCacheLimit().toString();

            imageCacheLimitSelect.addEventListener('change', (e) => {
                const limit = parseInt(e.target.value, 10);
                configState.setImageCacheLimit(limit);
                if (this.modal) this.modal.setCacheLimit(limit);
                configState.syncImageCacheLimit();
                console.log('[SETTINGS] Image cache limit set to:', limit === 0 ? 'unlimited' : limit);
            });

            // Push the persisted limit to the service worker on boot (the SW
            // may have restarted since the page last ran).
            configState.syncImageCacheLimit();
        }

        // Clear cache button
        const clearCacheBtn = document.getElementById('clear-cache-btn');
        if (clearCacheBtn) {
            clearCacheBtn.addEventListener('click', () => {
                // Clear the modal's preload cache
                if (this.modal) {
                    const clearedCount = this.modal.clearCache();
                    console.log('[SETTINGS] Cleared', clearedCount, 'cached images');
                    alert(`Cleared ${clearedCount} cached images`);
                }
            });
        }

        // Sidebar toggle (in-sidebar hamburger collapses; floating one opens;
        // backdrop tap closes)
        const sidebarToggle = document.getElementById('sidebar-toggle');
        const sidebarOpen = document.getElementById('sidebar-open');
        const sidebarBackdrop = document.getElementById('sidebar-backdrop');
        if (sidebarToggle) sidebarToggle.addEventListener('click', () => configState.toggleSidebar());
        if (sidebarOpen) sidebarOpen.addEventListener('click', () => configState.toggleSidebar());
        if (sidebarBackdrop) sidebarBackdrop.addEventListener('click', () => configState.setSidebarCollapsed(true));

        // Modal close
        const closeModal = document.getElementById('close-modal');
        const mediaModal = document.getElementById('media-modal');
        if (closeModal) closeModal.addEventListener('click', () => historyRouter.back());
        if (mediaModal) mediaModal.addEventListener('click', (e) => {
            if (e.target === e.currentTarget) historyRouter.back();
        });

        // Reader controls
        const mangaBackBtn = document.getElementById('manga-back-btn');
        if (mangaBackBtn) mangaBackBtn.addEventListener('click', () => historyRouter.back());

        // Keyboard shortcuts
        document.addEventListener('keydown', (e) => this.handleKeyboard(e));

        // Gallery events — the route's enter opens the overlay, so these
        // handlers just push the route. Avoid calling modal.open/reader.open/
        // splash.open here directly: it double-opens (the second call is a
        // no-op via the early-return guard, but it wastes a tick and races).
        window.addEventListener('gallery:openMedia', (e) => {
            const file = e.detail;
            historyRouter.push('modal:open', { file: { path: file.path, type: file.type } });
        });

        window.addEventListener('gallery:openReader', (e) => {
            const { series, chapterIndex } = e.detail;
            historyRouter.push('reader:open', { series: { name: series.name, path: series.path }, chapterIndex });
            // Option B safety net: write legacy lastView so a router bug
            // doesn't strand the user without a restore path.
            galleryState.saveLastView();
        });

        // Open series splash (manga only)
        window.addEventListener('gallery:openSeriesSplash', (e) => {
            const { series } = e.detail || {};
            if (!series) return;
            // Intentionally NOT calling galleryState.saveLastView() here.
            // The user is still on the section overview (gallery grid); we
            // only persist state when they navigate *into* a reader, since
            // restoreLastView's job is to put the reader back on the screen.
            // Persisting on splash-open would just overwrite the saved state
            // with the same section we already have.
            historyRouter.push('splash:open', { series: { name: series.name, path: series.path } });
        });

        window.addEventListener('splash:openReader', async (e) => {
            const { series, chapterIndex } = e.detail || {};
            if (!series) return;
            // Pop the splash entry; popOverlays runs splash.close (the route's
            // exit) which nulls splash.currentSeries. Push only the stub —
            // the reader's enter handler will resolve the full series from
            // galleryState.folders or by fetching from the server. This
            // keeps the URL/localStorage payload small.
            const seriesStub = { name: series.name, path: series.path };
            await historyRouter.popOverlays();
            // explicit:true tells the reader:open enter handler the user
            // picked this exact chapter in the splash — do not restore an
            // older progress position over their choice.
            historyRouter.push('reader:open', { series: seriesStub, chapterIndex, explicit: true });
            galleryState.saveLastView();
        });

        // Filter by tag from modal. setActiveTag calls _syncFilterToUrl which
        // triggers replace → _navShowSection → render() and _syncFilterUI(),
        // so no additional render/sync is needed here.
        window.addEventListener('gallery:filterByTag', (e) => {
            const tag = e.detail;
            if (tag) {
                galleryState.setActiveTag(tag);
                console.log('[APP] Filtered gallery by tag:', tag);
            }
        });

        // Clear tag filter button — see gallery:filterByTag above.
        const clearTagBtn = document.getElementById('clear-tag-btn');
        if (clearTagBtn) {
            clearTagBtn.addEventListener('click', () => {
                galleryState.setActiveTag(null);
                console.log('[APP] Cleared tag filter');
            });
        }

        // Back to artists button (H-Manga)
        const backToArtistsBtn = document.getElementById('back-to-artists-btn');
        if (backToArtistsBtn) {
            backToArtistsBtn.addEventListener('click', () => this.backToArtists());
        }

        // Favorites toggle
        const favoritesToggleBtn = document.getElementById('favorites-toggle-btn');
        if (favoritesToggleBtn) {
            favoritesToggleBtn.addEventListener('click', () => this.toggleFavoritesOnly());
        }

        // Random button
        const randomBtn = document.getElementById('random-btn');
        if (randomBtn) {
            randomBtn.addEventListener('click', () => this.openRandom());
        }

        // Favorite current item
        const favoriteCurrentBtn = document.getElementById('favorite-current-btn');
        if (favoriteCurrentBtn) {
            favoriteCurrentBtn.addEventListener('click', () => this.toggleCurrentFavorite());
        }

        // Modal/Reader events for favorite context tracking
        window.addEventListener('modal:opened', (e) => this.onModalOpened(e.detail));
        window.addEventListener('modal:closed', () => this.onModalClosed());
        window.addEventListener('reader:opened', (e) => this.onReaderOpened(e.detail));
        window.addEventListener('reader:closed', () => this.onReaderClosed());
    }

    backToArtists() {
        // Pop the artist drill-down entry. If the user was in the
        // favorites-only flat view (no artist drill-down in the stack),
        // replace the current entry with a plain gallery:hmanga so the
        // section overview is shown with favorites reset.
        if (historyRouter.hasRoute('gallery:hmanga:artist')) {
            historyRouter.back();
        } else {
            historyRouter.replace('gallery:hmanga', null, { resetScroll: true });
        }
    }

    showActiveTagFilter(tag) {
        const filterEl = document.getElementById('active-tag-filter');
        const nameEl = document.getElementById('active-tag-name');
        if (filterEl && nameEl) {
            nameEl.textContent = tag;
            filterEl.style.display = 'flex';
        }
    }

    hideActiveTagFilter() {
        const filterEl = document.getElementById('active-tag-filter');
        if (filterEl) {
            filterEl.style.display = 'none';
        }
    }

    /**
     * Sync the filter UI (active tag chip, favorites toggle button text/class)
     * to the current galleryState. Called by _navShowSection after the
     * section's data and filter state are applied. Also called by filter
     * setters to keep the UI in sync without a full re-render.
     */
    _syncFilterUI() {
        const toggleBtn = document.getElementById('favorites-toggle-btn');
        if (toggleBtn) {
            toggleBtn.textContent = galleryState.favoritesOnly
                ? '★ Show Favorites Only'
                : '☆ Show Favorites Only';
            toggleBtn.classList.toggle('active', galleryState.favoritesOnly);
        }
        if (galleryState.activeTag) {
            this.showActiveTagFilter(galleryState.activeTag);
        } else {
            this.hideActiveTagFilter();
        }
    }

    updateFavoritesVisibility() {
        const section = galleryState.section;
        const favSection = document.getElementById('favorites-section');
        if (favSection) {
            favSection.style.display = (section === SECTIONS.IMAGES || section === SECTIONS.H_MANGA) ? 'block' : 'none';
        }
    }

    shuffleDisplayedItems() {
        const arr = galleryState.displayedItems;
        for (let i = arr.length - 1; i > 0; i--) {
            const j = Math.floor(Math.random() * (i + 1));
            [arr[i], arr[j]] = [arr[j], arr[i]];
        }
        this.gallery.render();
    }

    openRandom() {
        const section = galleryState.section;
        const items = galleryState.displayedItems;
        if (!items || items.length === 0) {
            showNotification('No items to randomize', 'warning');
            return;
        }
        const idx = Math.floor(Math.random() * items.length);

        if (section === SECTIONS.IMAGES) {
            this.shuffleDisplayedItems();
            window.dispatchEvent(new CustomEvent('gallery:openMedia', {
                detail: items[idx]
            }));
            return;
        }

        if (section === SECTIONS.H_MANGA) {
            let series, chapterIndex;
            if (galleryState.currentArtist) {
                series = galleryState.currentArtist;
                const chapter = items[idx];
                chapterIndex = series.chapters.indexOf(chapter);
                if (chapterIndex < 0) {
                    showNotification('Could not resolve chapter for random item', 'warning');
                    return;
                }
            } else if (galleryState.favoritesOnly) {
                // Flattened favorite chapters: resolve parent artist via the map
                const chapter = items[idx];
                series = galleryState._chapterArtistMap.get(chapter);
                if (!series) {
                    showNotification('Could not resolve series for random item', 'warning');
                    return;
                }
                chapterIndex = series.chapters.indexOf(chapter);
                if (chapterIndex < 0) chapterIndex = 0;
            } else {
                series = items[idx];
                const chapters = series?.chapters || [];
                if (chapters.length === 0) {
                    showNotification('Selected artist has no books', 'warning');
                    return;
                }
                chapterIndex = Math.floor(Math.random() * chapters.length);
            }
            window.dispatchEvent(new CustomEvent('gallery:openReader', {
                detail: { series, chapterIndex }
            }));
            galleryState.saveLastView();
        }
    }

    async toggleFavoritesOnly() {
        const turningOn = !galleryState.favoritesOnly;
        if (turningOn) {
            await galleryState.loadFavorites(galleryState.section);
        }
        // currentArtist is reset to null by _applySectionUI (called via
        // _navShowSection from setFavoritesOnly's _syncFilterToUrl), so
        // no need to clear it here.
        galleryState.setFavoritesOnly(turningOn);
    }

    async toggleCurrentFavorite() {
        if (!this.currentFavoriteIdentifier || !this.currentFavoriteSection) return;
        const isFavorite = galleryState.favoriteIds.has(this.currentFavoriteIdentifier);
        if (isFavorite) {
            await favoritesStore.remove(this.currentFavoriteSection, this.currentFavoriteIdentifier);
            galleryState.favoriteIds.delete(this.currentFavoriteIdentifier);
        } else {
            await favoritesStore.add(this.currentFavoriteSection, this.currentFavoriteIdentifier, this.currentFavoriteName || this.currentFavoriteIdentifier);
            galleryState.favoriteIds.add(this.currentFavoriteIdentifier);
        }
        this.updateFavoriteCurrentBtn();
        this.gallery.render();
    }

    updateFavoriteCurrentBtn() {
        const btn = document.getElementById('favorite-current-btn');
        if (!btn) return;
        if (!this.currentFavoriteIdentifier) {
            btn.style.display = 'none';
            return;
        }
        btn.style.display = 'block';
        const isFavorite = galleryState.favoriteIds.has(this.currentFavoriteIdentifier);
        btn.textContent = isFavorite ? '★ Remove from Favorites' : '☆ Add to Favorites';
        btn.classList.toggle('is-favorite', isFavorite);
    }

    onModalOpened(detail) {
        if (galleryState.section !== SECTIONS.IMAGES) return;
        const file = detail?.file;
        if (!file) return;
        this.currentFavoriteIdentifier = file.path;
        this.currentFavoriteSection = SECTIONS.IMAGES;
        this.currentFavoriteName = file.name;
        galleryState.loadFavorites(SECTIONS.IMAGES).then(() => {
            this.updateFavoriteCurrentBtn();
        });
    }

    onModalClosed() {
        this.currentFavoriteIdentifier = null;
        this.currentFavoriteSection = null;
        this.currentFavoriteName = null;
        this.updateFavoriteCurrentBtn();
    }

    onReaderOpened(detail) {
        if (galleryState.section !== SECTIONS.H_MANGA) return;
        const series = detail?.series;
        const chapterIndex = detail?.chapterIndex;
        if (!series || chapterIndex == null) return;
        const chapter = series.chapters?.[chapterIndex];
        if (!chapter) return;
        const identifier = series.path + '/' + chapter.name;
        this.currentFavoriteIdentifier = identifier;
        this.currentFavoriteSection = SECTIONS.H_MANGA;
        this.currentFavoriteName = chapter.name;
        galleryState.loadFavorites(SECTIONS.H_MANGA).then(() => {
            this.updateFavoriteCurrentBtn();
        });
    }

    onReaderClosed() {
        this.currentFavoriteIdentifier = null;
        this.currentFavoriteSection = null;
        this.currentFavoriteName = null;
        this.updateFavoriteCurrentBtn();
        // Re-render the gallery so Continue badges re-query fresh progress.
        // reader.close() fired its saveProgress/markComplete before dispatching
        // reader:closed, but the badge queries ran when the cards were first
        // created — without this re-render a chapter finished in this session
        // kept its pre-reading badge (the "last read chapter not updating"
        // symptom within a section). Render is synchronous; the serialized
        // IndexedDB write from close() has already been queued.
        if (this.gallery) this.gallery.render();
    }

    async loadInitialData() {
        // If the URL is a deep link to a non-default section, set the
        // gallery state to that section BEFORE loading data so the first
        // fetch returns the right data. Without this, a deep link to
        // h_manga would load images first, then re-fetch h_manga.
        const deep = historyRouter.peekHash?.();
        if (deep?.route?.startsWith('gallery:')) {
            const section = _routeToSection(deep.route.slice('gallery:'.length));
            if (section && section !== galleryState.section) {
                galleryState.section = section;
            }
        }

        if (configState.isServerMode()) {
            await this.loadServerData();
        } else {
            await this.loadClientData();
        }

        // After initial data load, replay the nav stack. Replay routes the
        // top entry's enter() handler, which restores the saved section /
        // drill-down / overlay.
        historyRouter.replay();
    }

    async loadServerData({ silent = false } = {}) {
        // Monotonic token: only the latest invocation may write into
        // galleryState. Without this, two rapid section switches let the
        // slower (earlier) response land last and stamp the wrong section's
        // data as current (setFiles/setFolders label it with the *current*
        // section), which then passes the hasData guard and renders
        // cross-section garbage.
        const seq = ++this._loadServerSeq;
        try {
            const statusText = document.getElementById('status-text');
            if (!silent && statusText) statusText.textContent = 'Loading...';

            if (galleryState.section === SECTIONS.IMAGES) {
                const files = await apiService.getFiles(galleryState.section);
                if (seq !== this._loadServerSeq) return;
                galleryState.setFiles(files);
            } else {
                const folders = await apiService.getFolders(galleryState.section);
                if (seq !== this._loadServerSeq) return;
                galleryState.setFolders(folders);
                // Re-bind the displayed artist against the fresh folder list.
                // currentArtist may reference an object from the PREVIOUS
                // folders array (set during this fetch or earlier), so
                // displayedItems (= currentArtist.chapters) would keep
                // serving stale chapters while `folders` itself is fresh.
                // Match by name/path against the new array and re-point the
                // reference before updateDisplay. Checked at RESOLUTION time
                // (after the await), not at fetch start — the user can enter
                // an artist view while the background fetch is in flight.
                const showingArtist = galleryState.section === SECTIONS.H_MANGA && galleryState.currentArtist;
                if (showingArtist) {
                    const prev = galleryState.currentArtist;
                    const refreshed = galleryState.folders.find(f =>
                        (prev.path && f.path === prev.path) ||
                        f.name === prev.name
                    );
                    galleryState.currentArtist = refreshed || null;
                }
            }

            await galleryState.loadFavorites(galleryState.section);
            if (seq !== this._loadServerSeq) return;
            galleryState.updateDisplay();
            this.gallery.render();
            await this.renderPopularTags();

            if (statusText && !silent) {
                statusText.textContent = `Loaded ${galleryState.displayedItems.length} items`;
            }

            // Hide directory button in server mode
            const selectDirBtn = document.getElementById('select-dir-btn');
            if (selectDirBtn) selectDirBtn.style.display = 'none';

            // Show reindex buttons in server mode
            document.querySelectorAll('.reindex-btn').forEach(btn => {
                btn.style.display = '';
            });
        } catch (e) {
            console.error('Failed to load server data:', e);
            showNotification('Failed to load data from server', 'error');
        }
    }

    async _loadCurrentSectionData() {
        if (configState.isServerMode()) {
            await this.loadServerData();
        } else {
            await this.loadClientData();
        }
    }

    async loadClientData() {
        const handle = await configState.getHandle(galleryState.section);
        
        if (handle) {
            const cached = await cache.load(galleryState.section);
            if (cached) {
                if (galleryState.section === SECTIONS.IMAGES) {
                    galleryState.setFiles(cached);
                } else {
                    galleryState.setFolders(cached);
                }
                await galleryState.loadFavorites(galleryState.section);
                this.gallery.render();
                await this.renderPopularTags();
                const statusText = document.getElementById('status-text');
                if (statusText) statusText.textContent = `Loaded ${cached.length} items from cache`;
            }
        }
    }

    async handleSectionChange(e) {
        const section = e.currentTarget.dataset.section;

        // Optimistic nav-button highlight so the click feels instant even
        // though the data reload happens via the route's enter handler.
        document.querySelectorAll('.nav-btn').forEach(b => b.classList.remove('active'));
        e.currentTarget.classList.add('active');

        // Pop any open overlays before replacing the gallery entry. The
        // overlay's exit() handlers will fire (closing modal/reader/splash).
        await historyRouter.popOverlays();

        // Replace the current gallery entry with the new section. The
        // router's replace calls _navShowSection (via the route's enter),
        // which resets UI state and loads data. Await so the section's data
        // is ready before this method returns (prevents double-click races).
        await galleryState.setSection(section);
    }

    async handleReindex(section) {
        const btn = document.querySelector(`.reindex-btn[data-section="${section}"]`);
        if (!btn || btn.classList.contains('scanning')) return;

        btn.classList.add('scanning');
        const statusText = document.getElementById('status-text');
        if (statusText) statusText.textContent = 'Re-indexing...';

        // Fire-and-forget POST. The server runs the index walk in a
        // goroutine and returns 202 immediately, so this only blocks on
        // the round-trip. The client is free to disconnect — the result
        // is cached on the DB and exposed via /api/reindex/status for
        // whoever navigates to this section next.
        try {
            await apiService.reindexSection(section);
            showNotification('Re-index started — refresh later to see changes', 'info');
            if (statusText) statusText.textContent = 'Re-index started';

            // Best-effort: while the user keeps this tab open, poll the
            // status endpoint. When the server finishes and reports
            // changed=true, swap in the fresh section data automatically.
            // If the user navigates away / closes the tab, the polling
            // just stops — the server's goroutine keeps indexing.
            this._pollReindexAndRefresh(section).catch((e) => {
                console.warn('[APP] Reindex poll failed:', e);
            });
        } catch (e) {
            console.error(`[APP] Reindex failed:`, e);
            showNotification(e.message || 'Re-index failed', 'error');
            if (statusText) statusText.textContent = 'Re-index failed';
        } finally {
            // Clear the button state right away — the index may still be
            // running on the server but the request has been accepted.
            // The button being "scanning" only needs to reflect the
            // client-side state of "did we send the request", not the
            // server-side state.
            btn.classList.remove('scanning');
        }
    }

    /**
     * Poll /api/reindex/status?section=X while the connection is alive.
     * When the server reports the index finished with changed=true,
     * refetch the section and navigate to it so the user sees the
     * new files without manually renavigating. If the connection
     * drops mid-poll, or the user navigates away from the section,
     * the loop exits cleanly and the server-side goroutine keeps
     * working — the user will see updates next time they open the
     * section.
     */
    async _pollReindexAndRefresh(section) {
        const POLL_INTERVAL_MS = 1500;
        const MAX_POLLS = 600; // 15 minutes; bail out after that
        const statusText = document.getElementById('status-text');
        for (let i = 0; i < MAX_POLLS; i++) {
            await new Promise(r => setTimeout(r, POLL_INTERVAL_MS));
            // If the user navigated to a different section, stop
            // polling — they no longer care about this reindex
            // result here (they'll see fresh data next visit).
            if (galleryState.section !== section) return;
            let status;
            try {
                status = await apiService.getReindexStatus(section);
            } catch (e) {
                // Lost the connection (or server went away). Stop polling
                // silently — the user can renavigate to see results.
                return;
            }
            if (status.running) continue;
            // Server finished. If anything changed, swap in the fresh
            // data. Otherwise just notify (saves a needless fetch).
            if (status.has_result && status.result.changed) {
                if (galleryState.section === section) {
                    showNotification(`Re-index complete — refreshing ${section}`, 'success');
                    // If the user is inside an artist drill-down of this
                    // section, pop it first: _applySectionUI nulls
                    // currentArtist, which would desync the UI (painting the
                    // folder grid) from the router (still on the artist
                    // route). Back to the section overview, then refresh.
                    if (historyRouter.hasRoute('gallery:hmanga:artist')) {
                        await historyRouter.popOverlays();
                    }
                    this._applySectionUI(section);
                    await this.loadServerData({ silent: true });
                    this._syncFilterUI();
                    await this.renderPopularTags();
                    if (statusText) statusText.textContent = `Loaded ${galleryState.displayedItems.length} items`;
                } else {
                    // They left the section — they'll pick up the
                    // fresh data on their next visit. Tell them so
                    // the notification isn't a lie.
                    showNotification(`Re-index complete — navigate to ${section} to see changes`, 'info');
                }
            } else if (status.has_result && !status.result.changed) {
                showNotification('Re-index complete — no changes detected', 'info');
                if (statusText) statusText.textContent = 'Re-index complete — no changes';
            }
            return;
        }
    }

    async renderPopularTags() {
        const container = document.getElementById('tag-filters-container');
        if (!container) {
            console.log('[APP] Tag container not found');
            return;
        }
        
        container.innerHTML = '';
        
        // Only show in Images section
        if (galleryState.section !== SECTIONS.IMAGES) {
            console.log('[APP] Not in IMAGES section, current:', galleryState.section);
            return;
        }

        let popularTags = [];

        if (configState.isServerMode()) {
            // In server mode, use the /api/tags/stats endpoint which returns
            // all tags with counts sorted by frequency — more reliable than
            // computing from the potentially incomplete client-side file list
            try {
                const stats = await apiService.getTagStats(galleryState.section);
                popularTags = stats.map(s => [s.tag, s.count]);
            } catch (e) {
                console.error('[APP] Failed to load tag stats from server:', e);
                // Fallback to client-side computation
                popularTags = galleryState.getPopularTags(100);
            }
        } else {
            // Client mode: compute from local file data
            popularTags = galleryState.getPopularTags(100);
        }
        
        console.log('[APP] Rendering', popularTags.length, 'popular tags');
        
        if (popularTags.length === 0) {
            container.innerHTML = '<div class="text-sm" style="color: var(--text-secondary); padding: 0.5rem;">No tags found</div>';
            return;
        }
        
        // Lazy loading setup
        const batchSize = 20;
        let renderedCount = 0;
        
        // Create sentinel for lazy loading
        const sentinel = document.createElement('div');
        sentinel.className = 'tag-sentinel';
        sentinel.style.height = '1px';
        
        const renderBatch = (startIndex) => {
            const endIndex = Math.min(startIndex + batchSize, popularTags.length);
            const fragment = document.createDocumentFragment();
            
            for (let i = startIndex; i < endIndex; i++) {
                const [tag, count] = popularTags[i];
                const btn = document.createElement('button');
                btn.className = 'tag-btn';
                btn.innerHTML = `<span class="tag-name">${escapeHtml(tag)}</span><span class="tag-count">${count}</span>`;
                btn.title = `${tag} (${count} images)`;
                btn.addEventListener('click', () => {
                    // setActiveTag triggers replace → _navShowSection which
                    // handles render() and _syncFilterUI() — no need to call
                    // them here too.
                    galleryState.setActiveTag(tag);
                });
                fragment.appendChild(btn);
            }
            
            container.insertBefore(fragment, sentinel);
            renderedCount = endIndex;
            
            if (renderedCount >= popularTags.length) {
                sentinel.style.display = 'none';
            }
        };
        
        container.appendChild(sentinel);
        renderBatch(0);
        
        // Setup intersection observer
        const observer = new IntersectionObserver((entries) => {
            entries.forEach(entry => {
                if (entry.isIntersecting && renderedCount < popularTags.length) {
                    renderBatch(renderedCount);
                }
            });
        }, {
            root: container.parentElement,
            rootMargin: '100px'
        });
        
        observer.observe(sentinel);
        container._tagObserver = observer;
    }

    async requestDirectoryAccess(section) {
        if (!section) return;
        
        try {
            const handle = await window.showDirectoryPicker({ mode: 'read' });
            await configState.setHandle(section, handle);
            await this.scanDirectory();
        } catch (e) {
            console.log('Directory selection cancelled');
        }
    }

    async scanDirectory() {
        const section = galleryState.section;
        const handle = await configState.getHandle(section);
        
        if (!handle) {
            showNotification('No directory selected', 'warning');
            return;
        }

        const statusText = document.getElementById('status-text');
        const progressBar = document.getElementById('scan-progress');
        
        if (statusText) statusText.textContent = 'Scanning...';
        if (progressBar) progressBar.style.width = '10%';

        try {
            const onProgress = (percent) => {
                if (progressBar) progressBar.style.width = `${percent}%`;
            };

            const data = await unifiedScanner.scan(section, handle, {
                onProgress,
                dirFormats: configState.dirFormats,
                excludedTags: configState.excludedTags
            });

            if (section === SECTIONS.IMAGES) {
                galleryState.setFiles(data);
                await cache.save('images', data);
            } else {
                galleryState.setFolders(data);
                await cache.save(section, data);
            }

            if (progressBar) progressBar.style.width = '100%';
            if (statusText) statusText.textContent = `Found ${data.length} items`;
            
            setTimeout(() => {
                if (progressBar) progressBar.style.width = '0%';
                if (statusText) statusText.textContent = 'Idle';
            }, 1500);

            this.gallery.render();
            await this.renderPopularTags();
            // Update the router stack so a reload rehydrates the freshly
            // scanned section. skipEnter avoids re-running _navShowSection
            // (data is already loaded and rendered).
            historyRouter.replace(_sectionToRoute(section), null, { resetScroll: true, skipEnter: true });
        } catch (e) {
            console.error('Scan error:', e);
            if (statusText) statusText.textContent = 'Error scanning directory';
            showNotification('Failed to scan directory', 'error');
        }
    }

    handleFallbackFiles(files) {
        console.log('Fallback files:', files);
    }

    handleSearch(query) {
        // Store the query on state and re-derive displayedItems from the
        // source arrays — filtering the derived array in place made each
        // keystroke shrink the result set cumulatively, so backspacing
        // could never restore items.
        galleryState.searchQuery = query.trim();
        galleryState.page = 0;
        galleryState.updateDisplay();
        this.gallery.render();
    }

    handleKeyboard(e) {
        if (e.key === KEYBOARD_SHORTCUTS.ESCAPE) {
            if (this.modal.isOpen) { historyRouter.back(); return; }
            if (this.splash.isOpen()) { historyRouter.back(); return; }
            if (this.reader.isOpen) { historyRouter.back(); return; }
        }

        // Guard: don't hijack keys typed into any form control (search input,
        // tag inputs, settings dropdowns) — e.g. space on a focused <select>
        // would otherwise toggle the theme.
        const isFormControl = ['INPUT', 'SELECT', 'TEXTAREA'].includes(e.target?.tagName);

        if (e.key === KEYBOARD_SHORTCUTS.TOGGLE_THEME && !isFormControl) {
            configState.toggleDarkMode();
        }

        if (e.key === KEYBOARD_SHORTCUTS.FOCUS_SEARCH && !isFormControl) {
            e.preventDefault();
            document.getElementById('search-bar')?.focus();
        }

        if (this.modal.isOpen) {
            if (e.key === 'ArrowLeft') this.modal.previous();
            if (e.key === 'ArrowRight') this.modal.next();
        }

        if (this.reader.isOpen) {
            if (e.key === 'ArrowLeft') this.reader.previousPage();
            if (e.key === 'ArrowRight') this.reader.nextPage();
        }
    }
}

// Initialize app
const app = new MediaViewerApp();
window.MediaViewerApp = app;

if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', () => app.init());
} else {
    app.init();
}
