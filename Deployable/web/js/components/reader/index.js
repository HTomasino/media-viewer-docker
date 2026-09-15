import { readerState, galleryState } from '../../core/state/gallery.js';
import { configState } from '../../core/state/config.js';
import { apiService } from '../../services/service-registry.js';
import { historyRouter } from '../../core/history-router.js';
import { resourceManager, attachImageRetry } from '../../utils/urls.js';
import { READER_CONFIG, SECTIONS } from '../../core/config.js';
import { parseChapterName } from '../../utils/utils-registry.js';
import { debug } from '../../utils/debug.js';

/**
 * MangaReader - Manga reading component with mobile support
 * Uses lazy loading: only resolves URLs for the first INITIAL_LOAD_COUNT images
 * upfront, then loads remaining images one-at-a-time in sequential order after a
 * DELAYED_LOAD_MS delay. Each image is fully resolved before the next starts,
 * preventing backend flooding.
 *
 * Placeholder images have tall min-heights so the browser layout keeps them
 * off-screen, preventing all IntersectionObserver-style "in viewport" triggers.
 */
export class MangaReader {
    constructor(container) {
        this.container = container;
        this.wrapper = document.getElementById('manga-images-wrapper');
        this.isOpen = false;
        this.currentImages = [];
        this.uiVisible = true;
        this.isMobile = window.matchMedia('(max-width: 768px)').matches;

        // Lazy loading state
        this.urlCache = new Map();        // index -> resolved URL
        this.loadingPromises = new Map();  // index -> Promise (prevents duplicate loads)
        this.INITIAL_LOAD_COUNT = 3;       // Number of images to load upfront
        this.PREFETCH_AHEAD = 3;          // Number of images ahead to prefetch
        this.DELAYED_LOAD_MS = 5000;      // Delay before sequential loading begins
        this.PLACEHOLDER_HEIGHT = '120vh'; // Height for placeholder images
        this.currentChapter = null;        // Current chapter data
        this._initialBatchComplete = false; // True after first batch resolves
        this._abortLoading = false;         // Set to true by cleanup() to abort sequential load
        this._delayedLoadTimer = null;       // Timer for delayed sequential loading

        // AbortController to cleanly remove event listeners on close/reopen
        this._controlsAbort = null;
        this._touchAbort = null;
        this._clickAbort = null;
    }

    /**
     * Open a series in the reader.
     *
     * @param {Object} series                 Series descriptor with a `name`
     *                                        and `chapters` array.
     * @param {number} [chapterIndex=0]       Index into `series.chapters`.
     * @param {Object} [options={}]
     * @param {boolean} [options.restoreProgress=true]
     *   When true (the default), restore the last-read page/chapter from
     *   `progressStore`, overriding the explicit `chapterIndex` for the
     *   non-H-Manga sections. Pass `false` when the caller (e.g. the
     *   series splash) has already resolved the exact chapter the user
     *   asked for and doesn't want the reader to jump to the last-read
     *   chapter on top of that.
     */
    async open(series, chapterIndex = 0, options = {}) {
        // `restoreProgress: false` is used by callers (e.g. the series splash)
        // that have already resolved the exact chapter the user asked for and
        // don't want readerState.restoreProgress() to jump to the last-read
        // chapter. Default true preserves the prior behavior for direct opens.
        const { restoreProgress = true } = options;
        readerState.loadSeries(series, chapterIndex);
        if (restoreProgress) await readerState.restoreProgress();
        readerState.loadScaleMode();

        // Ensure fillPercent is never undefined (stale cache guard)
        if (readerState.fillPercent === undefined || readerState.fillPercent === null) {
            readerState.fillPercent = READER_CONFIG.DEFAULT_FILL_PERCENT ?? 100;
        }

        // Force fit-width on mobile
        if (this.isMobile) {
            readerState.scaleMode = READER_CONFIG.SCALE_MODES.FIT_WIDTH;
            readerState.fillPercent = 100; // Full width on mobile
        }

        this.show();
        this.setupControls();
        this.setupMobileUI();
        await this.loadChapter(readerState.chapterIndex);

        window.dispatchEvent(new CustomEvent('reader:opened', {
            detail: { series, chapterIndex }
        }));
    }

    show() {
        this.container.style.display = 'flex';
        document.getElementById('gallery-container').style.display = 'none';
        document.getElementById('gallery-breadcrumbs').style.display = 'none';
        document.getElementById('manga-settings-section').style.display = 'block';
        document.getElementById('global-tags-section').style.display = 'none';

        // Defensive cleanup: sessions upgraded from the old inline-style
        // toggleUI could carry stale !important styles on .top-header that
        // no CSS rule can override. The reader no longer writes them; scrub
        // any leftovers on open so the header is always recoverable.
        const topHeader = document.querySelector('.top-header');
        if (topHeader) {
            ['transform', 'opacity', 'height', 'min-height', 'overflow', 'pointer-events']
                .forEach(p => topHeader.style.removeProperty(p));
        }

        // Hide the sidebar while reading via a dedicated body class (same
        // single-mechanism pattern as immersive-mode). The reader must NOT
        // touch the sidebar's .collapsed class: .collapsed is the USER's
        // persisted gallery preference (configState.sidebarCollapsed), and
        // the previous add/remove here fought that preference — the sidebar
        // visibly popped up behind the gallery during route transitions and
        // was then hidden again, flickering on every reader open/close.
        // body.reader-open hides the sidebar in CSS on all breakpoints and
        // is removed on close, leaving the user's preference untouched.
        document.body.classList.add('reader-open');

        this.isOpen = true;
    }

    setupMobileUI() {
        debug('[READER] Setting up mobile UI, isMobile:', this.isMobile);

        // Hit zones must always have pointer-events: none so scroll/wheel events
        // pass through to the scrollable .reader-content underneath. Both mobile
        // (touch scroll) and desktop (mouse wheel in strip mode) need this.
        // Click handling is done via the wrapper click listener in setupDesktopClick.
        const hitZones = this.container.querySelectorAll('.reader-hit-zone');
        hitZones.forEach(zone => {
            zone.style.pointerEvents = 'none';
        });

        if (this.isMobile) {
            // Mobile: unified touch handling for taps vs swipes/scrolls
            this.setupTouchHandling();
        }

        // Desktop & mobile: click listener on the wrapper for left/center/right
        // zone interaction. The wrapper is the scrollable content, so clicks on
        // it naturally work. Hit zones are pointer-events: none, so clicks pass
        // through to the wrapper.
        this.setupWrapperClick();

        // Start with UI visible
        this.uiVisible = true;
        this.updateUIVisibility();
    }

    setupTouchHandling() {
        // Remove previous touch listeners if any
        if (this._touchAbort) {
            this._touchAbort.abort();
        }
        this._touchAbort = new AbortController();
        const signal = this._touchAbort.signal;

        let touchStartX = 0;
        let touchStartY = 0;
        let touchStartTime = 0;
        const minSwipeDistance = 50;       // Minimum distance for a horizontal swipe
        const maxTapDistance = 15;          // Max total movement for a tap (much stricter)
        const maxTapDuration = 300;         // Max time (ms) for a tap — longer presses aren't taps

        this.wrapper.addEventListener('touchstart', (e) => {
            touchStartX = e.changedTouches[0].screenX;
            touchStartY = e.changedTouches[0].screenY;
            touchStartTime = Date.now();
        }, { passive: true, signal });

        this.wrapper.addEventListener('touchend', (e) => {
            const touchEndX = e.changedTouches[0].screenX;
            const touchEndY = e.changedTouches[0].screenY;
            const deltaX = touchEndX - touchStartX;
            const deltaY = touchEndY - touchStartY;
            const totalMovement = Math.sqrt(deltaX * deltaX + deltaY * deltaY);
            const elapsed = Date.now() - touchStartTime;

            // A tap requires: small movement, short duration
            // This prevents accidental toggles when swiping to scroll
            if (totalMovement < maxTapDistance && elapsed < maxTapDuration) {
                this.handleTap(e.changedTouches[0]);
            } else if (Math.abs(deltaX) > Math.abs(deltaY) && Math.abs(deltaX) > minSwipeDistance) {
                // Horizontal swipe — only if clearly horizontal and long enough
                // Swipes should NOT cycle chapters — only navigate within the current chapter
                if (deltaX < 0) {
                    // Swipe left - next page (no chapter cycling)
                    this.nextPage(false);
                } else {
                    // Swipe right - previous page (no chapter cycling)
                    this.previousPage(false);
                }
            }
            // Vertical swipes (scrolling) and ambiguous gestures are ignored
        }, { passive: true, signal });
    }

    /**
     * Set up click handling on the wrapper for desktop and mobile browser-click
     * events. Hit zones have pointer-events: none, so clicks on them pass through
     * to the wrapper. This method uses the same left/center/right zone logic as
     * the hit zones but receives events on the scrollable content itself, so
     * scroll/wheel events work naturally in strip mode.
     */
    setupWrapperClick() {
        if (this._clickAbort) {
            this._clickAbort.abort();
        }
        this._clickAbort = new AbortController();
        const signal = this._clickAbort.signal;

        this.wrapper.addEventListener('click', (e) => {
            // Don't interfere with clicks on actual interactive elements inside the wrapper
            // (e.g. the next chapter button, links, etc.)
            if (e.target.closest('button, a, select, input')) return;

            // Don't toggle if clicking on controls (top/bottom bars)
            if (e.target.closest('.reader-controls')) return;

            const screenWidth = window.innerWidth;
            const leftBound = screenWidth / 3;
            const rightBound = screenWidth * 2 / 3;
            const clientX = e.clientX;

            if (clientX > leftBound && clientX < rightBound) {
                // Center third — toggle UI
                this.toggleUI();
            } else if (readerState.mode !== READER_CONFIG.MODES.STRIP) {
                // Left/right thirds in page modes — navigate pages
                // In strip mode, left/right clicks do nothing (scroll handles navigation)
                if (clientX <= leftBound) {
                    this.previousPage();
                } else {
                    this.nextPage();
                }
            }
        }, { signal });
    }

    handleTap(touch) {
        // Don't toggle if tapping on controls
        if (touch.target && touch.target.closest('.reader-controls')) return;

        const isMobile = window.matchMedia('(max-width: 768px)').matches;

        if (isMobile) {
            // On mobile, any tap on the content area toggles immersive mode.
            // Page navigation is handled via swipe gestures instead.
            this.toggleUI();
            return;
        }

        // Desktop: center third toggles UI, left/right thirds navigate pages
        const screenWidth = window.innerWidth;
        const leftBound = screenWidth / 3;
        const rightBound = screenWidth * 2 / 3;
        const clientX = touch.clientX;

        if (clientX > leftBound && clientX < rightBound) {
            // Center third — toggle UI
            this.toggleUI();
        } else if (readerState.mode !== READER_CONFIG.MODES.STRIP) {
            // Left/right thirds in page modes — navigate pages
            if (clientX <= leftBound) {
                this.previousPage();
            } else {
                this.nextPage();
            }
        }
    }

    toggleUI() {
        this.uiVisible = !this.uiVisible;
        this.updateUIVisibility();

        // Sidebar/top-header hiding on mobile is owned ENTIRELY by
        // body.immersive-mode CSS (mirrors the modal's single-mechanism
        // pattern). The previous inline top-header writes duplicated the
        // CSS rules with !important — two mechanisms for one behavior,
        // where any path that removed one but not the other desynced the
        // state and left the top bar unreachable (no CSS rule can beat an
        // inline !important). The body class is now the single source:
        // class add/remove can't desync.
        // The sidebar's .collapsed class is NOT touched here — it's the
        // user's persisted gallery preference; body.reader-open (set in
        // show()) already hides the sidebar for the whole reader session.
        // NOTE: this.isMobile is a constructor-time boolean PROPERTY (the
        // class also had an isMobile() method briefly — it was shadowed by
        // the property and threw TypeError at runtime; live-tested). Re-read
        // the media query here so rotation across the 768px breakpoint is
        // respected.
        const isMobile = window.matchMedia('(max-width: 768px)').matches;
        if (isMobile) {
            document.body.classList.toggle('immersive-mode', !this.uiVisible);
            if (!this.uiVisible) this._maybeShowImmersiveHint();
        }
    }

    /**
     * One-time hint so immersive mode isn't a dead end for first-time
     * users: the first tap hides ALL chrome, and the tap-again-to-restore
     * gesture is undiscoverable. Shown once per device (localStorage).
     */
    _maybeShowImmersiveHint() {
        try {
            if (localStorage.getItem('immersive-hint-shown')) return;
            localStorage.setItem('immersive-hint-shown', '1');
        } catch { /* storage unavailable — skip the hint */ }
        const hint = document.createElement('div');
        hint.textContent = 'Tap center to show controls';
        hint.style.cssText = 'position:fixed;bottom:20%;left:50%;transform:translateX(-50%);' +
            'background:rgba(15,17,26,0.9);color:#fff;padding:0.5rem 1rem;border-radius:8px;' +
            'font-size:0.85rem;z-index:200;pointer-events:none;transition:opacity 0.5s;';
        document.body.appendChild(hint);
        setTimeout(() => { hint.style.opacity = '0'; }, 2200);
        setTimeout(() => hint.remove(), 2800);
    }

    updateUIVisibility() {
        const controls = this.container.querySelectorAll('.reader-controls');
        controls.forEach(el => {
            el.classList.toggle('ui-hidden', !this.uiVisible);
        });

        // Also toggle hit zones visibility
        const hitZones = this.container.querySelectorAll('.reader-hit-zone');
        hitZones.forEach(zone => {
            zone.style.opacity = this.uiVisible ? '1' : '0';
        });
    }

    async loadChapter(index) {
        // Abort the PREVIOUS chapter's scroll listener BEFORE mutating
        // readerState. The old listener reads readerState.chapterIndex at
        // save time, so leaving it live across the state switch let a
        // momentum scroll write saveProgress()/markComplete() under the NEW
        // chapterIndex — a sticky isComplete=true on an unread chapter,
        // which then permanently drops it from "Continue" candidates.
        this._scrollAbort?.abort();
        this._scrollAbort = null;

        readerState.chapterIndex = index;
        const chapter = readerState.getChapter();

        if (!chapter) return;

        // Record the open position WITHOUT bumping lastViewedAt — simply
        // opening a chapter does not constitute reading it. The reader
        // bumps lastViewedAt via saveProgress() on actual reading events
        // (scroll in strip mode, page-turn in page mode, or markComplete).
        await readerState.updateProgress();
        readerState.saveLastView();

        this.cleanup();

        this.populateChapterDropdown();
        document.getElementById('chapter-dropdown').value = index;

        // Set mode and scale mode classes
        this.wrapper.className = `reader-content view-${readerState.mode}`;
        this.wrapper.dataset.scaleMode = readerState.scaleMode;
        this.wrapper.style.setProperty('--fill-percent', (readerState.fillPercent ?? READER_CONFIG.DEFAULT_FILL_PERCENT ?? 100) + '%');

        const scaleSelect = document.getElementById('manga-scale-select');
        if (scaleSelect) {
            scaleSelect.value = readerState.scaleMode;
        }

        // Update fill percentage dropdown visibility and values
        this.updateFillPercentVisibility(readerState.scaleMode);

        await this.loadImages(chapter);

        this.updateUI();
        this.wrapper.scrollTo(0, 0);
        this.updateNextChapterButton();
        this.setupNextChapterButton();
    }

    async loadImages(chapter) {
        this.currentImages = [];
        this.currentChapter = chapter;
        this._initialBatchComplete = false;
        this._abortLoading = false;
        // Cancel any pending delayed load from a previous chapter
        if (this._delayedLoadTimer) {
            clearTimeout(this._delayedLoadTimer);
            this._delayedLoadTimer = null;
        }
        if (readerState.mode === READER_CONFIG.MODES.STRIP) {
            await this.loadStripMode(chapter);
        } else {
            await this.loadPageMode(chapter);
        }
        this.applyScaleMode();
    }

    /**
     * Resolve the URL for an image, using cache if available.
     * Returns a Promise that resolves to the URL string.
     */
    async resolveMediaUrlCached(imgObj, index, total, nextObj, prevObj) {
        // Return cached URL if already resolved
        if (this.urlCache.has(index)) {
            return this.urlCache.get(index);
        }

        // Return existing loading promise if one is in progress
        if (this.loadingPromises.has(index)) {
            return this.loadingPromises.get(index);
        }

        // Create and cache the loading promise
        const loadPromise = this.resolveMediaUrl(imgObj, index, total, nextObj, prevObj)
            .then(url => {
                if (url) {
                    this.urlCache.set(index, url);
                }
                this.loadingPromises.delete(index);
                return url;
            })
            .catch(err => {
                this.loadingPromises.delete(index);
                console.error(`[READER] Failed to resolve URL for index ${index}:`, err);
                return null;
            });

        this.loadingPromises.set(index, loadPromise);
        return loadPromise;
    }

    /**
     * True when the user has selected strict sequential loading. In this mode
     * no image download may begin until the previous one has finished, so the
     * backend is never hit with parallel transcode requests.
     */
    _isSequentialMode() {
        return readerState.loadMode === READER_CONFIG.LOAD_MODES.SEQUENTIAL;
    }

    /**
     * Resolve once img has finished loading (or errored). Resolves immediately
     * if the image is already complete. Used by sequential mode to gate the
     * next download on the previous one finishing — img.src alone only starts
     * the fetch, it does not wait for it.
     */
    _waitForImageComplete(img) {
        return new Promise(resolve => {
            if (img.complete) return resolve();
            const done = () => {
                img.removeEventListener('load', done);
                img.removeEventListener('error', done);
                resolve();
            };
            img.addEventListener('load', done, { once: true });
            img.addEventListener('error', done, { once: true });
        });
    }

    /**
     * Ensure images around a given index are loaded.
     * Loads the current image and PREFETCH_AHEAD images ahead.
     * Uses the URL cache to skip already-loaded images.
     * In sequential mode URLs are resolved one-at-a-time (no parallel batch).
     */
    async ensureImagesLoaded(chapter, centerIndex) {
        const total = chapter.images.length;
        const start = Math.max(0, centerIndex - 1);
        const end = Math.min(total, centerIndex + this.PREFETCH_AHEAD + 1);

        if (this._isSequentialMode()) {
            for (let i = start; i < end; i++) {
                if (!this.urlCache.has(i) && !this.loadingPromises.has(i)) {
                    const imgObj = chapter.images[i];
                    const nextObj = i < total - 1 ? chapter.images[i + 1] : null;
                    const prevObj = i > 0 ? chapter.images[i - 1] : null;
                    await this.resolveMediaUrlCached(imgObj, i, total, nextObj, prevObj);
                }
            }
            return;
        }

        const loadPromises = [];
        for (let i = start; i < end; i++) {
            if (!this.urlCache.has(i) && !this.loadingPromises.has(i)) {
                const imgObj = chapter.images[i];
                const nextObj = i < total - 1 ? chapter.images[i + 1] : null;
                const prevObj = i > 0 ? chapter.images[i - 1] : null;
                loadPromises.push(this.resolveMediaUrlCached(imgObj, i, total, nextObj, prevObj));
            }
        }

        if (loadPromises.length > 0) {
            await Promise.all(loadPromises);
        }
    }

    /**
     * Apply a resolved URL to an image element.
     * Sets the src attribute for immediate display, or data-src for lazy loading.
     */
    applyUrlToImage(img, url, lazy = false) {
        if (lazy) {
            img.dataset.src = url;
            img.removeAttribute('data-loading');
        } else {
            img.src = url;
            img.removeAttribute('data-loading');
            // Auto-retry transient load failures (backend briefly busy during
            // a scan, a dropped connection, a stale service-worker response).
            // A broken manga page that would have loaded on the second try
            // otherwise stays blank forever with no error handler here.
            attachImageRetry(img, url, { maxRetries: 2, baseDelayMs: 500 });
        }

        // Remove placeholder state — the image now has real content
        img.removeAttribute('data-placeholder');
        img.style.minHeight = '';

        // Apply mobile styles if needed
        if (this.isMobile) {
            this.applyMobileStyles(img);
        }
    }

    async loadStripMode(chapter) {
        const totalImages = chapter.images.length;

        // Create placeholder images for ALL pages.
        // Each placeholder gets a tall inline min-height so it occupies real space
        // in the layout — this keeps them off-screen and prevents the browser from
        // thinking all images are "in viewport" at once.
        for (let i = 0; i < totalImages; i++) {
            const img = document.createElement('img');
            img.dataset.index = i;
            img.dataset.loading = 'false';
            img.dataset.placeholder = 'true';
            img.alt = `Page ${i + 1}`;
            img.className = 'manga-page';
            // Inline min-height ensures layout space even before CSS paints.
            // Removed in applyUrlToImage / loadPageImage once the real src is set.
            img.style.minHeight = this.PLACEHOLDER_HEIGHT;

            this.wrapper.appendChild(img);
            this.currentImages.push(img);
        }

        // Load the first INITIAL_LOAD_COUNT images immediately (no observer needed)
        // Resolve all URLs first, then apply them in order to guarantee sequential display
        const initialCount = Math.min(this.INITIAL_LOAD_COUNT, totalImages);
        const resolvedUrls = new Array(initialCount).fill(null);
        const initialPromises = [];
        for (let i = 0; i < initialCount; i++) {
            const imgObj = chapter.images[i];
            const nextObj = i < totalImages - 1 ? chapter.images[i + 1] : null;
            const prevObj = i > 0 ? chapter.images[i - 1] : null;

            initialPromises.push(
                this.resolveMediaUrlCached(imgObj, i, totalImages, nextObj, prevObj)
                    .then(url => {
                        resolvedUrls[i] = url;
                    })
            );
        }

        await Promise.all(initialPromises);

        // Apply URLs to images in sequential order. In sequential mode each
        // download must finish before the next src is set, so the initial
        // batch doesn't open with INITIAL_LOAD_COUNT parallel transcodes.
        for (let i = 0; i < initialCount; i++) {
            const url = resolvedUrls[i];
            if (!url) continue;
            const img = this.currentImages[i];
            if (img) {
                this.applyUrlToImage(img, url, false);
                if (this._isSequentialMode()) await this._waitForImageComplete(img);
            }
        }

        // Mark initial batch as complete
        this._initialBatchComplete = true;

        // After a delay, begin loading remaining images one-at-a-time in order.
        // The delay gives the initial images time to render before we start
        // hammering the backend with sequential requests.
        this._delayedLoadTimer = setTimeout(() => {
            this._delayedLoadTimer = null;
            this._loadRemainingSequentially(chapter, initialCount, totalImages);
        }, this.DELAYED_LOAD_MS);

        this.setupScrollTracking();
    }

    applyMobileStyles(img) {
        // Force horizontal fill on mobile using !important inline styles
        img.style.setProperty('width', '100vw', 'important');
        img.style.setProperty('max-width', '100vw', 'important');
        img.style.setProperty('height', 'auto', 'important');
    }

    /**
     * Load remaining images one-at-a-time in sequential order.
     * Each image is fully resolved and applied before the next begins.
     * In sequential mode each image must also finish DOWNLOADING (not just
     * start) before the next src is set, otherwise every image fetches in
     * parallel and floods the backend with transcode requests. Preload
     * headers are disabled in this mode for the same reason — their HEAD
     * requests and link prefetches would re-introduce parallel downloads.
     * Stops if _abortLoading is set (by cleanup() on chapter change/close).
     */
    async _loadRemainingSequentially(chapter, fromIndex, toIndex) {
        const sequential = this._isSequentialMode();
        for (let i = fromIndex; i < toIndex; i++) {
            // Abort if chapter changed or reader closed
            if (this._abortLoading) break;

            const img = this.currentImages[i];
            // Skip if already loaded (e.g. by ensureImagesLoaded from scroll)
            if (!img || img.src || img.dataset.src) continue;

            const imgObj = chapter.images[i];
            const nextObj = i < toIndex - 1 ? chapter.images[i + 1] : null;
            const prevObj = i > 0 ? chapter.images[i - 1] : null;

            const url = await this.resolveMediaUrlCached(imgObj, i, toIndex, nextObj, prevObj);
            if (this._abortLoading) break;

            if (url) {
                this.applyUrlToImage(img, url, false);
                if (sequential) {
                    // Wait for the download to finish before starting the next.
                    await this._waitForImageComplete(img);
                } else {
                    this._processImagePreload(url, i, toIndex);
                }
            }
        }
    }

    async loadPageMode(chapter) {
        const totalImages = chapter.images.length;

        // Create placeholder images for ALL pages.
        // Each placeholder gets an inline min-height so it occupies space in the
        // layout immediately — this prevents all placeholders from collapsing to
        // zero height and being treated as "in viewport" simultaneously.
        for (let i = 0; i < totalImages; i++) {
            const img = document.createElement('img');
            img.dataset.index = i;
            img.dataset.loading = 'false';
            img.dataset.placeholder = 'true';
            img.alt = `Page ${i + 1}`;
            img.className = 'manga-page';
            // Inline min-height ensures layout space even before CSS paints.
            // Removed in applyUrlToImage / loadPageImage once the real src is set.
            img.style.minHeight = this.PLACEHOLDER_HEIGHT;

            this.wrapper.appendChild(img);
            this.currentImages.push(img);
        }

        // Load the first INITIAL_LOAD_COUNT images
        // Resolve all URLs first, then apply them in order to guarantee sequential display
        const initialCount = Math.min(this.INITIAL_LOAD_COUNT, totalImages);
        const resolvedUrls = new Array(initialCount).fill(null);
        const initialPromises = [];
        for (let i = 0; i < initialCount; i++) {
            const imgObj = chapter.images[i];
            const nextObj = i < totalImages - 1 ? chapter.images[i + 1] : null;
            const prevObj = i > 0 ? chapter.images[i - 1] : null;

            initialPromises.push(
                this.resolveMediaUrlCached(imgObj, i, totalImages, nextObj, prevObj)
                    .then(url => {
                        resolvedUrls[i] = url;
                    })
            );
        }

        await Promise.all(initialPromises);

        // Apply URLs to images in sequential order
        for (let i = 0; i < initialCount; i++) {
            const url = resolvedUrls[i];
            if (!url) continue;
            const img = this.currentImages[i];
            if (img) {
                img.src = url;
                img.removeAttribute('data-loading');
                img.removeAttribute('data-placeholder');
                img.style.minHeight = '';

                // Auto-retry transient load failures — page mode had no
                // onerror at all, so a single failed fetch left a blank page.
                attachImageRetry(img, url, { maxRetries: 2, baseDelayMs: 500 });

                // Mark for preload processing
                img.dataset.preloadIndex = i;
                img.dataset.preloadTotal = totalImages;

                // Process preload headers when image loads
                img.onload = () => {
                    this._processImagePreload(url, i, totalImages);
                };

                // Sequential mode: wait for the download to finish before
                // starting the next one (no parallel initial transcodes).
                if (this._isSequentialMode()) await this._waitForImageComplete(img);
            }
        }

        // Mark initial batch as complete so page navigation can load more
        this._initialBatchComplete = true;

        // Preload next few pages in background
        this.ensureImagesLoaded(chapter, initialCount);

        this.updatePageVisibility();
    }

    applyScaleMode() {
        this.currentImages.forEach(img => {
            img.dataset.scaleMode = readerState.scaleMode;
        });
    }

    async resolveMediaUrl(imgObj, index, total, nextObj, prevObj) {
        const section = galleryState.section || SECTIONS.IMAGES;
        if (configState.isServerMode()) {
            // Include viewport dimensions for responsive image sizing
            // This is required for the backend to calculate preload headers
            const viewportWidth = window.innerWidth;
            const viewportHeight = window.innerHeight;
            const dpr = window.devicePixelRatio || 1;

            const options = {
                width: viewportWidth,
                height: viewportHeight,
                dpr: dpr,
                responsive: true,
                // Include section so backend resolves file path from correct directory
                section: section
            };

            // For manga/h-manga sections, use nocache to generate viewport-sized
            // images in memory rather than caching every viewport variant to disk.
            // This avoids filling disk with per-viewport responsive copies of pages
            // that are typically viewed once in a reading session.
            // Only send width (not height) so the backend scales based on width alone —
            // manga pages are viewed in vertical strip mode where height is unconstrained.
            if (section === SECTIONS.MANGA || section === SECTIONS.H_MANGA) {
                options.nocache = true;
                delete options.height;
            }

            if (index !== undefined && total !== undefined) {
                options.index = index;
                options.total = total;
            }
            // Pass actual next/prev file paths for preloading
            if (nextObj && nextObj.path) {
                options.nextPath = nextObj.path;
            }
            if (prevObj && prevObj.path) {
                options.prevPath = prevObj.path;
            }
            const url = configState.getMediaUrl(imgObj.path, options);
            return url;
        }

        const file = imgObj.file || await imgObj.handle.getFile();
        return resourceManager.createUrl(file);
    }

    /**
     * Process preload headers for adjacent images in sequence.
     * Only active after the initial batch to prevent flooding the backend,
     * and never in sequential mode (HEAD requests + link prefetches would
     * re-introduce parallel downloads).
     */
    async _processImagePreload(url, index, total) {
        if (!this._initialBatchComplete || this._isSequentialMode()) {
            return;
        }

        if (index === undefined || !total) {
            return;
        }

        try {
            // Fetch HEAD to get preload headers without downloading full image
            const response = await fetch(url, { method: 'HEAD' });
            if (!response.ok) {
                return;
            }
            // HEAD responses also carry X-File-Mtime — record it so subsequent
            // getMediaUrl calls version their URLs with ?v=<mtime>.
            apiService._maybeRecordFileMtime(url, response);

            const preloadNext = response.headers.get('X-Preload-Next');
            const preloadPrev = response.headers.get('X-Preload-Previous');
            const linkHeader = response.headers.get('Link');

            // Preload next image (higher priority)
            if (preloadNext) {
                this._preloadImageUrl(preloadNext);
            }

            // Preload previous image (lower priority)
            if (preloadPrev) {
                this._preloadImageUrl(preloadPrev);
            }

            // Process Link header for HTTP/2 push hints
            if (linkHeader) {
                this._processLinkHeader(linkHeader);
            }
        } catch (e) {
            // Silently ignore preload errors
        }
    }

    /**
     * Preload an image URL by creating a prefetch link element.
     * Limits concurrent prefetches to avoid flooding.
     */
    _preloadImageUrl(url) {
        // Check if already preloaded
        if (document.querySelector(`link[rel="prefetch"][href="${url}"]`)) {
            return;
        }

        const link = document.createElement('link');
        link.rel = 'prefetch';
        link.as = 'image';
        link.href = url;

        // Clean up after load/error
        link.onload = () => {
            if (link.parentNode) link.parentNode.removeChild(link);
        };
        link.onerror = () => {
            if (link.parentNode) link.parentNode.removeChild(link);
        };

        document.head.appendChild(link);
    }

    /**
     * Process Link header for HTTP/2 push hints
     */
    _processLinkHeader(linkHeader) {
        // Parse Link header format: <url>; rel=preload; as=image
        const match = linkHeader.match(/<([^>]+)>[^;]*;\s*rel=preload/i);
        if (match && match[1]) {
            const url = match[1];
            this._preloadImageUrl(url);
        }
    }

    setupScrollTracking() {
        let lastSave = 0;

        // AbortController-scoped: loadChapter → cleanup() aborts the previous
        // chapter's listener before the new one is installed. Without this,
        // one listener accumulated per chapter switch (N× updateUI,
        // saveProgress and markComplete per scroll event).
        this._scrollAbort?.abort();
        this._scrollAbort = new AbortController();
        const signal = this._scrollAbort.signal;

        this.wrapper.addEventListener('scroll', () => {
            this.updateUI();
            this.updateNextChapterButton();

            // Strip mode never advances readerState.pageIndex during scroll,
            // so saveProgress() recorded a stale page (usually 0). Sync the
            // visible page index first so the saved position matches what
            // the user actually sees.
            if (readerState.mode === READER_CONFIG.MODES.STRIP) {
                readerState.pageIndex = this.getVisibleImageIndex();
            }

            const now = Date.now();
            if (now - lastSave > READER_CONFIG.SCROLL_SAVE_INTERVAL) {
                lastSave = now;
                readerState.saveProgress();
            }

            const scrollPercent = (this.wrapper.scrollTop + this.wrapper.clientHeight) /
                this.wrapper.scrollHeight;
            if (scrollPercent > READER_CONFIG.COMPLETION_THRESHOLD) {
                readerState.markComplete();
            }
        }, { signal });
    }

    setupControls() {
        // Remove previous listeners if any (e.g. reader was closed and reopened)
        if (this._controlsAbort) {
            this._controlsAbort.abort();
        }
        this._controlsAbort = new AbortController();
        const signal = this._controlsAbort.signal;

        document.querySelectorAll('[id^="reader-mode-"]').forEach((btn) => {
            btn.addEventListener('click', (e) => {
                const mode = e.currentTarget.id.replace('reader-mode-', '');
                this.setMode(mode);
            }, { signal });
        });

        document.getElementById('manga-scale-select')?.addEventListener('change', (e) => {
            this.setScaleMode(e.target.value);
            // Show/hide fill percentage dropdown in sidebar
            const sideFillPercent = document.getElementById('manga-fill-percent-side-select');
            if (sideFillPercent) {
                sideFillPercent.style.display = e.target.value === 'original' ? 'none' : 'block';
            }
        }, { signal });

        document.getElementById('manga-fill-percent-side-select')?.addEventListener('change', (e) => {
            const percent = parseInt(e.target.value, 10);
            if (typeof readerState.setFillPercent === 'function') {
                readerState.setFillPercent(percent);
            } else {
                readerState.fillPercent = percent;
                localStorage.setItem('mangaFillPercent', percent);
            }
            this.setFillPercent(percent);
            // Sync header dropdown
            const headerFillPercent = document.getElementById('manga-fill-percent-select');
            if (headerFillPercent) headerFillPercent.value = percent;
        }, { signal });

        document.getElementById('chapter-dropdown')?.addEventListener('change', (e) => {
            this.loadChapter(parseInt(e.target.value));
        }, { signal });

        document.getElementById('reader-prev-page')?.addEventListener('click', () => this.previousPage(), { signal });
        document.getElementById('reader-next-page')?.addEventListener('click', () => this.nextPage(), { signal });

        // Immersive-exit handle: gesture-independent way back to the full
        // UI while immersive (the CSS only shows it when immersive-mode is
        // active on mobile).
        document.getElementById('immersive-exit-handle')?.addEventListener('click', (e) => {
            e.stopPropagation();
            this.toggleUI();
        }, { signal });

        // Note: the back button (#manga-back-btn) is intentionally NOT bound
        // here. app.js routes it through historyRouter.back(), whose exit
        // handler calls this.close(). Binding it here as well would run the
        // close path twice per click (double markComplete/saveProgress and
        // duplicate reader:closed events).
    }

    populateChapterDropdown() {
        const select = document.getElementById('chapter-dropdown');
        select.innerHTML = '';

        readerState.series.chapters.forEach((chap, idx) => {
            const opt = document.createElement('option');
            opt.value = idx;
            const { chapter, part } = parseChapterName(chap.name);
            const isNumeric = typeof chapter === 'number';
            opt.textContent = isNumeric
                ? (part > 0 ? `Ch.${chapter} (Part ${part})` : `Ch.${chapter}`)
                : chapter;
            select.appendChild(opt);
        });
    }

    setMode(mode) {
        readerState.setMode(mode);

        document.querySelectorAll('#reader-view-controls .icon-btn').forEach((btn) => {
            btn.classList.remove('active');
        });
        document.getElementById(`reader-mode-${mode}`)?.classList.add('active');

        this.loadChapter(readerState.chapterIndex);
    }

    setScaleMode(scaleMode) {
        if (typeof readerState.setScaleMode === 'function') {
            readerState.setScaleMode(scaleMode);
        } else {
            readerState.scaleMode = scaleMode;
            localStorage.setItem('mangaScaleMode', scaleMode);
        }
        this.wrapper.dataset.scaleMode = scaleMode;
        this.applyScaleMode();

        // Force one reflow for the whole batch instead of one per image —
        // img.offsetHeight per image caused a noticeable stall on huge
        // chapters. Hiding and re-showing via a class toggle lets the
        // browser recompute styles in a single pass.
        this.wrapper.style.display = 'none';
        void this.wrapper.offsetHeight;
        this.wrapper.style.display = '';

        // Update fill percentage dropdown visibility
        this.updateFillPercentVisibility(scaleMode);
    }

    setFillPercent(percent) {
        if (typeof readerState.setFillPercent === 'function') {
            readerState.setFillPercent(percent);
        } else {
            readerState.fillPercent = percent;
            localStorage.setItem('mangaFillPercent', percent);
        }
        this.wrapper.style.setProperty('--fill-percent', percent + '%');
        this.applyScaleMode();
    }

    // setLoadMode applies the lazy/sequential image-loading preference to the
    // open reader. The choice is persisted via readerState.setLoadMode so the
    // next reader open honors it. Live application on an already-open reader
    // is intentionally limited: the current chapter was laid out under the
    // previous mode, so we record the new mode and let it take effect on the
    // next chapter/series load rather than tearing down and re-laying-out
    // images mid-read (which would cause visible reflow and lost scroll).
    setLoadMode(mode) {
        if (typeof readerState.setLoadMode === 'function') {
            readerState.setLoadMode(mode);
        } else {
            readerState.loadMode = mode;
            localStorage.setItem('mangaLoadMode', mode);
        }
        debug('[READER] Load mode set to:', mode, '(applied on next chapter load)');
    }

    updateFillPercentVisibility(scaleMode) {
        // Update both header and sidebar fill percentage dropdowns
        const headerFillPercent = document.getElementById('manga-fill-percent-select');
        const sideFillPercent = document.getElementById('manga-fill-percent-side-select');
        const hidePercent = scaleMode === 'original';

        if (headerFillPercent) headerFillPercent.style.display = hidePercent ? 'none' : 'block';
        if (sideFillPercent) sideFillPercent.style.display = hidePercent ? 'none' : 'block';

        // Update selected values
        const currentValue = (readerState.fillPercent ?? READER_CONFIG.DEFAULT_FILL_PERCENT ?? 100) + '';
        if (headerFillPercent) headerFillPercent.value = currentValue;
        if (sideFillPercent) sideFillPercent.value = currentValue;
    }

    nextPage(allowChapterCycle = true) {
        if (readerState.mode === READER_CONFIG.MODES.STRIP) {
            const imgs = this.wrapper.querySelectorAll('img');
            const current = this.getVisibleImageIndex();
            if (current < imgs.length - 1) {
                imgs[current + 1].scrollIntoView({ behavior: 'smooth' });
                // Ensure the next few images are loaded
                this.ensureImagesLoaded(this.currentChapter, current + 1);
                // Swipe/page-turn in strip mode doesn't fire a scroll event,
                // so bump lastViewedAt here for actual reading activity.
                // Sync pageIndex to the visible image first — it is never
                // advanced by strip-mode scrolling, so saving without the
                // sync recorded page 0 as the resume position.
                readerState.pageIndex = current + 1;
                readerState.saveProgress();
            } else if (allowChapterCycle) {
                this.nextChapter();
            }
        } else {
            if (readerState.nextPage()) {
                // Load the image for the current page and prefetch ahead
                const pageIndex = readerState.pageIndex;
                this.loadPageImage(pageIndex);
                if (readerState.mode === READER_CONFIG.MODES.PAIR && pageIndex + 1 < this.currentImages.length) {
                    this.loadPageImage(pageIndex + 1);
                }
                // Prefetch ahead
                this.ensureImagesLoaded(this.currentChapter, pageIndex + this.PREFETCH_AHEAD);
                this.updatePageVisibility();
                // Record the actual page-turn as reading activity so the
                // resumed position is the page the user really reached
                // (single/pair mode never scrolls, so this is the only
                // place a page-mode page-turn is persisted).
                readerState.saveProgress();
            } else if (allowChapterCycle) {
                // User tried to advance past the last page — they've
                // finished this chapter. markComplete() is owned by
                // nextChapter() so the OLD chapter's record is correctly
                // stamped before the state advances to the new chapter.
                this.nextChapter();
            }
        }
        this.updateUI();
        this.updateNextChapterButton();
    }

    previousPage(allowChapterCycle = true) {
        if (readerState.mode === READER_CONFIG.MODES.STRIP) {
            const imgs = this.wrapper.querySelectorAll('img');
            const current = this.getVisibleImageIndex();
            if (current > 0) {
                imgs[current - 1].scrollIntoView({ behavior: 'smooth' });
                // Ensure the previous few images are loaded
                this.ensureImagesLoaded(this.currentChapter, current - 1);
            }
            // Strip mode: don't cycle to previous chapter on swipe — no-op at first page
        } else {
            if (readerState.previousPage()) {
                const pageIndex = readerState.pageIndex;
                this.loadPageImage(pageIndex);
                if (readerState.mode === READER_CONFIG.MODES.PAIR && pageIndex + 1 < this.currentImages.length) {
                    this.loadPageImage(pageIndex + 1);
                }
                this.updatePageVisibility();
            }
            // Page modes: don't cycle to previous chapter on swipe — no-op at first page
        }
        this.updateUI();
        this.updateNextChapterButton();
    }

    /**
     * Load a single page image in page mode if not already loaded.
     * Resolves the URL from cache or fetches it, then applies it to the img element.
     */
    async loadPageImage(index) {
        if (!this.currentChapter || index >= this.currentChapter.images.length || index < 0) return;

        const img = this.currentImages[index];
        if (!img || img.src || img.dataset.src) return; // Already loaded

        const chapter = this.currentChapter;
        const total = chapter.images.length;
        const imgObj = chapter.images[index];
        const nextObj = index < total - 1 ? chapter.images[index + 1] : null;
        const prevObj = index > 0 ? chapter.images[index - 1] : null;

        const url = await this.resolveMediaUrlCached(imgObj, index, total, nextObj, prevObj);
        if (url && img && !img.src) {
            img.src = url;
            img.removeAttribute('data-loading');
            img.removeAttribute('data-placeholder');
            img.style.minHeight = '';

            // Auto-retry transient load failures (page mode previously had
            // no error handler — a failed fetch left a blank page forever).
            attachImageRetry(img, url, { maxRetries: 2, baseDelayMs: 500 });

            // Mark for preload processing
            img.dataset.preloadIndex = index;
            img.dataset.preloadTotal = total;
            img.onload = () => {
                this._processImagePreload(url, index, total);
            };
        }
    }

    /**
     * Set up the "Next Chapter" button click handler.
     */
    setupNextChapterButton() {
        const btn = document.getElementById('reader-next-chapter-btn');
        if (!btn) return;
        btn.onclick = (e) => {
            e.stopPropagation();
            this.nextChapter();
        };
    }

    /**
     * Show or hide the "Next Chapter" button based on scroll position (strip) or page (page modes).
     * In strip mode: visible when scrolled past ~85% of the chapter.
     * In single/pair mode: visible when on the last page.
     */
    updateNextChapterButton() {
        const btn = document.getElementById('reader-next-chapter-btn');
        if (!btn) return;

        // Only show if there's a next chapter
        if (readerState.isLastChapter()) {
            btn.style.display = 'none';
            return;
        }

        if (readerState.mode === READER_CONFIG.MODES.STRIP) {
            // In strip mode, check scroll position
            const scrollTop = this.wrapper.scrollTop;
            const scrollHeight = this.wrapper.scrollHeight;
            const clientHeight = this.wrapper.clientHeight;
            const scrollPercent = (scrollTop + clientHeight) / scrollHeight;
            btn.style.display = scrollPercent > 0.85 ? 'flex' : 'none';
        } else {
            // In single/pair mode, show when on the last page
            const totalPages = readerState.getTotalPages();
            const step = readerState.mode === READER_CONFIG.MODES.PAIR ? 2 : 1;
            const isLastPage = readerState.pageIndex + step >= totalPages;
            btn.style.display = isLastPage ? 'flex' : 'none';
        }
    }

    nextChapter() {
        // The "Next Chapter" button only shows once the user has reached
        // the end of the chapter, so by definition they've finished it.
        // Mark complete BEFORE advancing so the OLD chapter's record
        // reflects completion (after the state advances, the new
        // chapterIndex is what markComplete would target).
        if (readerState.mode !== READER_CONFIG.MODES.STRIP) {
            // Strip mode already marks complete via the scroll threshold.
            // Page mode relies on this explicit mark.
            if (readerState.isAtLastPage()) {
                readerState.markComplete();
            }
        }
        if (readerState.nextChapter()) {
            this.loadChapter(readerState.chapterIndex);
        } else {
            // No next chapter — close via the router, not a direct close().
            // A direct close() left the stale reader:open entry on the nav
            // stack and skipped the gallery re-render, so the Continue
            // badge kept showing the pre-session chapter.
            historyRouter.back();
        }
    }

    getVisibleImageIndex() {
        const scrollTop = this.wrapper.scrollTop;
        const imgs = this.wrapper.querySelectorAll('img');

        for (let i = 0; i < imgs.length; i++) {
            if (imgs[i].offsetTop - scrollTop > -100) {
                return i;
            }
        }
        return imgs.length - 1;
    }

    updatePageVisibility() {
        const imgs = this.wrapper.querySelectorAll('img');
        const current = readerState.pageIndex;
        const mode = readerState.mode;

        imgs.forEach((img, idx) => {
            if (mode === READER_CONFIG.MODES.SINGLE) {
                img.style.display = idx === current ? 'block' : 'none';
            } else if (mode === READER_CONFIG.MODES.PAIR) {
                img.style.display = (idx === current || idx === current + 1) ? 'block' : 'none';
            }
        });
    }

    updateUI() {
        const indicator = document.getElementById('reader-page-indicator');
        if (!indicator) return;

        if (readerState.mode === READER_CONFIG.MODES.STRIP) {
            const current = this.getVisibleImageIndex() + 1;
            const total = readerState.getTotalPages();
            indicator.textContent = `${current} / ${total}`;
        } else {
            const current = readerState.pageIndex + 1;
            const total = readerState.getTotalPages();
            indicator.textContent = `${current} / ${total}`;
        }
    }

    toggleControls() {
        // Deprecated - use toggleUI instead
        this.toggleUI();
    }

    cleanup() {
        // Abort any in-progress sequential loading
        this._abortLoading = true;

        // Cancel any pending delayed load timer
        if (this._delayedLoadTimer) {
            clearTimeout(this._delayedLoadTimer);
            this._delayedLoadTimer = null;
        }

        // Drop the scroll-tracking listener from the previous chapter load
        this._scrollAbort?.abort();
        this._scrollAbort = null;

        this.wrapper.querySelectorAll('img').forEach((img) => {
            // Dispose any retry attachment so its listener/timer are removed
            // before the element is detached/replaced.
            if (typeof img.__imageRetryDispose === 'function') img.__imageRetryDispose();
            if (img.src?.startsWith('blob:')) {
                resourceManager.revoke(img.src);
            }
            if (img.dataset.src?.startsWith('blob:')) {
                resourceManager.revoke(img.dataset.src);
            }
        });

        this.wrapper.innerHTML = '';
        this.currentImages = [];
        // Revoke blob URLs still held by the URL cache (client-mode File
        // handles): cleanup above only reaches blobs attached to <img>
        // elements; pending/preloaded entries never got a src assigned.
        this.urlCache.forEach((url) => {
            if (typeof url === 'string' && url.startsWith('blob:')) {
                resourceManager.revoke(url);
            }
        });
        this.urlCache.clear();
        this.loadingPromises.clear();
        this._initialBatchComplete = false;
        this.currentChapter = null;

        // Hide the next chapter button
        const nextBtn = document.getElementById('reader-next-chapter-btn');
        if (nextBtn) {
            nextBtn.style.display = 'none';
        }
    }

    close() {
        // If the user closes from the last page of the chapter in page
        // mode, they have finished reading it. Strip mode marks complete
        // via the scroll threshold — but that threshold (scrollTop +
        // clientHeight over scrollHeight > 0.9) can miss on long-format
        // strips whose lazy tail images are still loading (scrollHeight
        // keeps growing as images load, so 0.9 can be unreachably high for
        // a while). Closing at/near the true bottom completes the chapter
        // here too: bottom is "scrollTop + clientHeight >= scrollHeight".
        if (readerState.isAtLastPage()) {
            readerState.markComplete();
        } else if (readerState.mode === READER_CONFIG.MODES.STRIP) {
            const bottomGap = this.wrapper.scrollHeight -
                (this.wrapper.scrollTop + this.wrapper.clientHeight);
            // 2px slack absorbs rounding; anything more means the user
            // genuinely stopped short of the end.
            if (bottomGap <= 2) {
                readerState.markComplete();
            } else {
                readerState.saveProgress();
            }
        } else {
            readerState.saveProgress();
        }
        this.cleanup();

        // Abort event listeners
        if (this._controlsAbort) { this._controlsAbort.abort(); this._controlsAbort = null; }
        if (this._touchAbort) { this._touchAbort.abort(); this._touchAbort = null; }
        if (this._clickAbort) { this._clickAbort.abort(); this._clickAbort = null; }

        this.container.style.display = 'none';
        document.getElementById('gallery-container').style.display = 'grid';
        document.getElementById('manga-settings-section').style.display = 'none';
        document.getElementById('global-tags-section').style.display = 'block';

        // Remove the reader-open class — the sidebar returns to the USER's
        // persisted preference (configState.sidebarCollapsed → .collapsed),
        // not to a hardcoded open state. The previous unconditional
        // classList.remove('collapsed') here overrode a user who had the
        // sidebar collapsed, and re-showed it mid-transition (the "pops up
        // behind the gallery then immediately hides" flicker).
        document.body.classList.remove('reader-open');
        configState.applySidebarState();

        document.body.classList.remove('immersive-mode');

        const artist = readerState.series;
        if (galleryState.section === SECTIONS.H_MANGA && galleryState.currentArtist) {
            // Restore breadcrumbs for H-Manga book view
            const breadcrumbs = document.getElementById('gallery-breadcrumbs');
            if (breadcrumbs) breadcrumbs.style.display = 'block';
        }

        readerState.reset();
        this.isOpen = false;

        window.dispatchEvent(new CustomEvent('reader:closed'));
    }
}