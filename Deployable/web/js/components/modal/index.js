import { resourceManager, attachImageRetry } from '../../utils/urls.js';
import { configState } from '../../core/state/config.js';
import { apiService } from '../../services/service-registry.js';
import { galleryState } from '../../core/state/gallery.js';
import { historyRouter } from '../../core/history-router.js';
import { SECTIONS } from '../../core/config.js';
import { escapeHtml } from '../../utils/html.js';
import { db } from '../../db/index.js';

/**
 * MediaModal - Image/video viewer modal
 */
export class MediaModal {
    constructor() {
        this.modal = document.getElementById('media-modal');
        this.imgEl = document.getElementById('modal-img');
        this.vidEl = document.getElementById('modal-video');
        this.infoEl = document.getElementById('modal-info');
        this.currentFile = null;
        this.currentFiles = [];
        this.currentIndex = -1;
        this.isOpen = false;
        this.isImmersive = false;
        this.preloadCache = new Map();
        this.maxCacheDistance = 10;
        this.maxCacheEntries = 6;
        // User-configurable hard cap (settings dropdown → Cached Images
        // Limit). 0 = unlimited (count cap disabled; distance eviction and
        // revocation on close still apply). Defaults to the configState
        // value once app init assigns it via setCacheLimit.
        this.maxCacheEntries = configState.getImageCacheLimit();

        this.handleSwipe = this.handleSwipe.bind(this);
        this.handleTap = this.handleTap.bind(this);
        this.setupSwipeGestures();
    }
    
    setupSwipeGestures() {
        let touchStartX = 0;
        let touchStartY = 0;
        const minSwipeDistance = 50;
        
        this.modal.addEventListener('touchstart', (e) => {
            touchStartX = e.changedTouches[0].screenX;
            touchStartY = e.changedTouches[0].screenY;
        }, { passive: true });
        
        this.modal.addEventListener('touchend', (e) => {
            const touchEndX = e.changedTouches[0].screenX;
            const touchEndY = e.changedTouches[0].screenY;
            const deltaX = touchEndX - touchStartX;
            const deltaY = touchEndY - touchStartY;

            if (Math.abs(deltaX) < minSwipeDistance && Math.abs(deltaY) < minSwipeDistance) {
                this.handleTap(e.changedTouches[0]);
            } else if (Math.abs(deltaX) > Math.abs(deltaY)) {
                this.handleSwipe(touchStartX, touchEndX, minSwipeDistance);
            }
        }, { passive: true });
    }
    
    handleSwipe(startX, endX, minSwipeDistance) {
        const deltaX = endX - startX;
        
        if (Math.abs(deltaX) > minSwipeDistance) {
            if (deltaX < 0) {
                // Swipe left - next
                this.next();
            } else {
                // Swipe right - previous
                this.previous();
            }
        }
    }

    handleTap(touch) {
        // Center-tap intentionally does NOTHING in the modal. The previous
        // behavior toggled body.immersive-mode, which hides the sidebar and
        // top-header — but the modal is a fullscreen overlay, so those
        // elements are already invisible behind it. The toggle was a no-op
        // visually while the modal was open, and its only real effect hit
        // AFTER close: the user's sidebar/header would suddenly be gone
        // (immersive-mode has no chevron handle outside the reader).
        // Swipes handle prev/next; the close button handles exit.
        return;
    }

    async open(file, allFiles = []) {
        this.currentFile = file;
        this.currentFiles = allFiles;
        this.currentIndex = allFiles.indexOf(file);

        this.cleanup();

        // Trigger preload for adjacent images
        if (configState.isServerMode()) {
            this.fetchPreloadHeaders(file.path, this.currentIndex);
        }

        const url = await this.resolveUrl(file);

        if (file.type === 'video') {
            this.showVideo(url);
        } else {
            this.showImage(url);
        }

        this.showTagAssignment(file);

        this.modal.style.display = 'block';
        this.isOpen = true;

        window.dispatchEvent(new CustomEvent('modal:opened', { detail: { file } }));
    }

    getPreloadCount() {
        return configState.getPreloadCount();
    }

    buildMediaOptions(index, forActualUrl = false) {
        const total = this.currentFiles.length;
        const preloadCount = this.getPreloadCount();

        // Send actual viewport dimensions
        // Backend will constrain based on image orientation
        const viewportWidth = window.innerWidth;
        const viewportHeight = window.innerHeight;
        const dpr = window.devicePixelRatio || 1;

        const options = {
            index,
            total,
            width: viewportWidth,
            height: viewportHeight,
            dpr,
            nocache: true,
            responsive: true,
            preloadCount: preloadCount,
            // Include section so backend resolves file path from correct directory
            section: galleryState.section || SECTIONS.IMAGES
        };

        if (forActualUrl) {
            // Build arrays of next/prev paths for multiple preloads
            const nextPaths = [];
            const prevPaths = [];

            for (let i = 1; i <= preloadCount; i++) {
                if (index + i < total) {
                    nextPaths.push(this.currentFiles[index + i].path);
                }
                if (index - i >= 0) {
                    prevPaths.push(this.currentFiles[index - i].path);
                }
            }

            if (nextPaths.length > 0) {
                options.nextPaths = nextPaths;
            }
            if (prevPaths.length > 0) {
                options.prevPaths = prevPaths;
            }
        }

        return options;
    }

    async resolveUrl(file) {
        if (configState.isServerMode()) {
            // Check if we have a preloaded blob URL for this path
            if (this.preloadCache.has(file.path)) {
                const cacheEntry = this.preloadCache.get(file.path);
                console.log('[MODAL] Using preloaded image from cache:', file.path, 'index:', cacheEntry.index);
                return cacheEntry.blobUrl;
            }

            const options = this.buildMediaOptions(this.currentIndex, true);
            return configState.getMediaUrl(file.path, options);
        }

        const fileObj = file.file || await file.handle.getFile();
        return resourceManager.createUrl(fileObj);
    }

    async fetchPreloadHeaders(currentPath, currentIndex) {
        const options = this.buildMediaOptions(currentIndex, true);
        const url = configState.getMediaUrl(currentPath, options);

        try {
            const response = await fetch(url, { method: 'HEAD' });
            if (response.ok) {
                // HEAD responses carry X-File-Mtime — record it so subsequent
                // getMediaUrl calls version their URLs with ?v=<mtime>.
                apiService._maybeRecordFileMtime(url, response);
                const preloadNextAll = response.headers.get('X-Preload-Next-All');
                const preloadPrevAll = response.headers.get('X-Preload-Previous-All');

                if (preloadNextAll) {
                    const urls = preloadNextAll.split(',').filter(u => u.trim());
                    console.log('[MODAL] Preloading', urls.length, 'next images');
                    urls.forEach(url => this.preloadImageInBackground(url.trim()));
                } else {
                    const preloadNext = response.headers.get('X-Preload-Next');
                    if (preloadNext) {
                        this.preloadImageInBackground(preloadNext);
                    }
                }

                if (preloadPrevAll) {
                    const urls = preloadPrevAll.split(',').filter(u => u.trim());
                    console.log('[MODAL] Preloading', urls.length, 'previous images');
                    urls.forEach(url => this.preloadImageInBackground(url.trim()));
                } else {
                    const preloadPrev = response.headers.get('X-Preload-Previous');
                    if (preloadPrev) {
                        this.preloadImageInBackground(preloadPrev);
                    }
                }
            }
        } catch (e) {
            console.warn('[MODAL] Failed to fetch preload headers:', e);
        }
    }

    async preloadImageInBackground(url) {
        try {
            const urlObj = new URL(url, window.location.origin);
            const pathMatch = urlObj.pathname.match(/\/api\/media\/(.+)/);
            if (!pathMatch) return;

            const path = decodeURIComponent(pathMatch[1]);

            if (this.preloadCache.has(path)) {
                return;
            }

            fetch(url)
                .then(response => {
                    if (response.ok) {
                        apiService._maybeRecordFileMtime(url, response);
                        return response.blob();
                    }
                    throw new Error('Failed to preload');
                })
                .then(blob => {
                    const blobUrl = URL.createObjectURL(blob);
                    const fileIndex = this.currentFiles.findIndex(f => f.path === path);
                    this.preloadCache.set(path, {
                        blobUrl,
                        index: fileIndex,
                        timestamp: Date.now()
                    });
                    console.log('[MODAL] Preloaded and cached:', path, 'index:', fileIndex);
                    this.cleanupOldCache();
                })
                .catch(e => {
                    console.warn('[MODAL] Failed to preload:', path, e);
                });
        } catch (e) {
            console.warn('[MODAL] Error parsing preload URL:', e);
        }
    }

    cleanupOldCache() {
        const currentIndex = this.currentIndex;
        const toRemove = [];

        for (const [path, entry] of this.preloadCache) {
            const distance = Math.abs(entry.index - currentIndex);
            if (distance > this.maxCacheDistance) {
                URL.revokeObjectURL(entry.blobUrl);
                toRemove.push(path);
                console.log('[MODAL] Evicted from cache:', path, 'distance:', distance);
            }
        }

        toRemove.forEach(path => this.preloadCache.delete(path));

        // Hard count cap: deferred (DEFERRED) responses are full-resolution
        // originals, so distance-based eviction alone can leave many large
        // blobs alive while the user sits on one image. Evict oldest beyond
        // the cap, never the currently displayed path. Cap 0 = unlimited.
        if (this.maxCacheEntries > 0 && this.preloadCache.size > this.maxCacheEntries) {
            const entries = [...this.preloadCache.entries()]
                .filter(([path]) => path !== this.currentFile?.path)
                .sort((a, b) => a[1].timestamp - b[1].timestamp);
            for (const [path, entry] of entries) {
                if (this.preloadCache.size <= this.maxCacheEntries) break;
                URL.revokeObjectURL(entry.blobUrl);
                this.preloadCache.delete(path);
                console.log('[MODAL] Evicted from cache (count cap):', path);
            }
        }
    }

    clearCache() {
        console.log('[MODAL] clearCache called, cache size:', this.preloadCache.size);
        let count = 0;
        for (const [path, entry] of this.preloadCache) {
            console.log('[MODAL] Revoking URL for:', path, entry);
            URL.revokeObjectURL(entry.blobUrl);
            count++;
        }
        this.preloadCache.clear();
        console.log('[MODAL] Cache cleared, was:', count, 'images');
        return count;
    }

    // Apply a new cache cap from the settings dropdown. Lowering it evicts
    // oldest entries immediately so the in-memory cache honors the choice
    // without waiting for the next preload cycle.
    setCacheLimit(limit) {
        const num = parseInt(limit, 10);
        if (Number.isNaN(num) || num < 0) return;
        this.maxCacheEntries = num;
        if (num > 0) this.cleanupOldCache();
    }

    showImage(url) {
        this.vidEl.style.display = 'none';
        this.vidEl.pause();

        // Dispose any prior retry attachment on the reused imgEl before
        // attaching for the new URL (the helper is also idempotent per
        // element, but this drops the old listener eagerly so a pending
        // retry timer from the previous image can never fire and clobber
        // this new src).
        if (this._imgRetryDispose) { this._imgRetryDispose(); this._imgRetryDispose = null; }

        this.imgEl.src = url;
        this.imgEl.style.display = 'block';
        // Auto-retry transient load failures (backend briefly busy during a
        // scan, a dropped connection). The modal previously had no error
        // handler, so a single failed fetch left a broken-image icon with no
        // recovery. The helper forces a fresh fetch by briefly clearing src
        // before re-assigning (no cache-bust param — media endpoints are
        // network-only and the HTTP cache doesn't store error responses).
        this._imgRetryDispose = attachImageRetry(this.imgEl, url, { maxRetries: 2, baseDelayMs: 400 });
    }

    showVideo(url) {
        this.imgEl.style.display = 'none';

        this.vidEl.src = url;
        this.vidEl.style.display = 'block';
        this.vidEl.loop = true;
        this.vidEl.play().catch((e) => console.warn('Video play failed:', e));
    }

    showTagAssignment(file) {
        const container = document.getElementById('active-image-tags');
        container.innerHTML = '';

        // Files without a tags array get no panel — without this early hide,
        // navigating from a tagged image to a tagless one kept displaying the
        // previous file's tag chips (whose click handlers mutated the wrong
        // file's state).
        if (!file.tags) {
            document.getElementById('tag-assignment').style.display = 'none';
            document.getElementById('global-tags-section').style.display = 'block';
            return;
        }

        file.tags.forEach((tag) => {
            const el = document.createElement('span');
            el.className = 'tag';
            el.style.cssText = `
                background-color: rgba(99, 102, 241, 0.1);
                color: var(--primary);
                display: inline-flex;
                align-items: center;
                cursor: pointer;
            `;

            const textSpan = document.createElement('span');
            textSpan.textContent = tag;
            textSpan.style.cursor = 'pointer';

            const deleteBtn = document.createElement('span');
            deleteBtn.innerHTML = ' ×';
            deleteBtn.style.marginLeft = '4px';
            deleteBtn.style.cursor = 'pointer';

            textSpan.addEventListener('click', async () => {
                // Close the modal and filter by the clicked tag, in order.
                // back() is async (popstate fires later), so filtering
                // immediately after it raced the router: setActiveTag's
                // replace() overwrote the still-open modal:open entry, then
                // the pending history.back() popped to the previous gallery
                // entry with its OLD payload — dropping the tag filter. The
                // gallery:filterByTag listener then ran against a gallery
                // that had already re-rendered without the tag.
                // popOverlays() pops modal:open, runs its exit(), and syncs
                // the URL synchronously — awaitable, no popstate race.
                await historyRouter.popOverlays();
                window.dispatchEvent(new CustomEvent('gallery:filterByTag', { detail: tag }));
            });

            deleteBtn.addEventListener('click', async (e) => {
                e.stopPropagation();
                file.tags = file.tags.filter((t) => t !== tag);
                await this.saveFileTags(file);
                this.showTagAssignment(file);
            });

            el.appendChild(textSpan);
            el.appendChild(deleteBtn);
            container.appendChild(el);
        });

        document.getElementById('tag-assignment').style.display = 'block';
        document.getElementById('global-tags-section').style.display = 'none';
    }

    async saveFileTags(file) {
        try {
            await db.put(db.stores.FILES, file.path, {
                path: file.path,
                name: file.name,
                tags: file.tags,
            });
        } catch (e) {
            console.warn('Failed to save file tags:', e);
        }
    }

    next() {
        if (this.currentIndex < this.currentFiles.length - 1) {
            this.cleanupOldCache(); // Clean up old cached images before navigating
            const nextFile = this.currentFiles[this.currentIndex + 1];
            // Sync the URL without running the route's exit (skipEnter) — a
            // full replace would close() the modal, which revokes the entire
            // preload cache. The direct open() reuses preloaded blobs.
            historyRouter.replace('modal:open', {
                file: { path: nextFile.path, type: nextFile.type },
            }, { skipEnter: true });
            this.open(nextFile, this.currentFiles);
        }
    }

    previous() {
        if (this.currentIndex > 0) {
            this.cleanupOldCache(); // Clean up old cached images before navigating
            const prevFile = this.currentFiles[this.currentIndex - 1];
            historyRouter.replace('modal:open', {
                file: { path: prevFile.path, type: prevFile.type },
            }, { skipEnter: true });
            this.open(prevFile, this.currentFiles);
        }
    }

    cleanup() {
        // Drop the retry attachment so its error listener and any pending
        // retry timer are removed when the modal's imgEl is cleared/reused.
        if (this._imgRetryDispose) { this._imgRetryDispose(); this._imgRetryDispose = null; }
        // Don't revoke blob URLs that are in the preload cache
        // Only revoke URLs that were created for non-cached images
        if (this.imgEl.src) {
            // Check if this URL is in our preload cache
            let isCached = false;
            for (const [path, entry] of this.preloadCache) {
                if (entry.blobUrl === this.imgEl.src) {
                    isCached = true;
                    break;
                }
            }
            // Only revoke if not in cache
            if (!isCached) {
                resourceManager.revoke(this.imgEl.src);
            }
            this.imgEl.src = '';
        }
        if (this.vidEl.src) {
            resourceManager.revoke(this.vidEl.src);
            this.vidEl.src = '';
            this.vidEl.pause();
        }
    }

    close() {
        this.cleanup();
        // Revoke all blob URLs held in the preload cache so they don't leak
        // across modal sessions. cleanup() only revokes the current img/video
        // URL; the cache entries would otherwise linger until GC (which may
        // never reclaim the blob URLs, since nothing references the Map keys).
        this.clearCache();
        this.modal.style.display = 'none';

        document.getElementById('tag-assignment').style.display = 'none';
        document.getElementById('global-tags-section').style.display = 'block';

        // Defensive: the modal no longer toggles immersive-mode itself
        // (center taps are a no-op since v29), but clear any body class
        // lingering from a pre-v29 session so the sidebar/header return.
        this.isImmersive = false;
        document.body.classList.remove('immersive-mode');

        this.currentFile = null;
        this.currentFiles = [];
        this.currentIndex = -1;
        this.isOpen = false;

        window.dispatchEvent(new CustomEvent('modal:closed'));
    }

    getOpenState() {
        return this.isOpen;
    }
}
