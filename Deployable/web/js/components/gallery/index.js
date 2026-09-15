/**
 * Gallery component with memory-managed lazy loading
 */

import { VirtualizedChunker } from './chunker.js';
import { galleryState } from '../../core/core-registry.js';
import { thumbnailService } from '../../services/service-registry.js';
import { showNotification, escapeHtml } from '../../utils/utils-registry.js';
import { getChapterCount, parseChapterName } from '../../utils/utils-registry.js';
import { progress } from '../../db/db-registry.js';
import { favoritesStore } from '../../db/favorites.js';
import { SECTIONS } from '../../core/core-registry.js';
import { debug } from '../../utils/debug.js';

export class Gallery {
    // Cap on how many rendered-item entries the Map retains between full
    // render() calls. Virtualization unloads DOM nodes that scroll out of
    // view, but renderedItems kept every index ever rendered, leaking memory
    // on long scroll sessions. Evicting the oldest entry keeps recent ones
    // (still needed for thumbnail reloads on scroll-back) while bounding size.
    static MAX_RENDERED_ITEMS = 200;

    constructor(container) {
        this.container = container;
        // The gallery's actual scrolling element is .main-content (CSS
        // overflow-y: auto), not the window. window.scrollY is always 0
        // for this layout, so the router's window-scroll listener can't
        // track position. Track the right container here and tell the
        // router to use it via a custom event.
        this._scrollContainer = container.closest('.main-content') || document.documentElement;
        this._routerScrollAbort = null;
        // Suppresses scroll-tracker updates during render() so the
        // browser's auto-clamp to 0 (when innerHTML is cleared) doesn't
        // clobber the router's saved scrollY.
        this._suppressScrollTracking = false;

        this.chunker = new VirtualizedChunker(container, {
            chunkSize: 50,
            viewportMargin: '300px', // Load 300px before entering, unload 300px after leaving
            onItemRender: (item, index) => this.renderItem(item, index),
            onItemEnterViewport: (el) => this.loadItemThumbnail(el),
            onItemLeaveViewport: (el) => this.unloadItemThumbnail(el),
            onSentinelVisible: () => this.onLoadMore()
        });

        this.renderedItems = new Map();
        this._routerAbort = null;
        this._installScrollRestoreListener();
    }

    _installScrollRestoreListener() {
        this._routerAbort = new AbortController();
        this._routerScrollAbort = new AbortController();
        // Track the actual scrolling container's scrollTop and tell the
        // router via a custom event so the entry's scrollY matches the
        // gallery's position (not the window's, which is always 0 here).
        this._scrollContainer.addEventListener('scroll', () => {
            if (this._suppressScrollTracking) return;
            // Skip dispatch when the gallery is hidden (splash/reader open,
            // modal overlay). main-content auto-clamps scrollTop to 0 when
            // its only visible child disappears, and the resulting scroll
            // event would clobber the router's saved scrollY with 0 —
            // leaving the user at the top after they close the overlay.
            if (getComputedStyle(this.container).display === 'none') return;
            window.dispatchEvent(new CustomEvent('router:scroll', {
                detail: { scrollTop: this._scrollContainer.scrollTop }
            }));
        }, { passive: true, signal: this._routerScrollAbort.signal });

        window.addEventListener('router:scrollRestored', (e) => {
            const y = e.detail?.y || 0;
            // Suppress scroll-tracker updates for the programmatic
            // scrollTop assignment below so the resulting scroll event
            // doesn't overwrite y back into the entry (no-op write here,
            // but a robust scrollRestored should not perturb state).
            this._suppressScrollTracking = true;
            try {
                // Materialize chunks so the scrolling container is tall enough
                // for scrollTop to actually reach y.
                this.chunker.rechunkAtOffset(y);
                // Force layout so scrollHeight reflects the newly appended
                // chunks before we set scrollTop. Without this, the browser
                // clamps scrollTop to the pre-render max and the user lands
                // at the bottom of the initial chunk instead of the saved
                // scroll position.
                void this._scrollContainer.offsetHeight;
                // Scroll the actual container (the window.scrollTo the router
                // does after dispatch is a no-op for this layout).
                this._scrollContainer.scrollTop = y;
            } finally {
                // Re-enable tracker after a rAF so the synchronous scroll
                // event fired by scrollTop = y has already been delivered
                // and ignored. Subsequent user scrolls are tracked normally.
                requestAnimationFrame(() => {
                    this._suppressScrollTracking = false;
                });
            }
        }, { signal: this._routerAbort.signal });
    }

    render() {
        // Suppress the scroll-position-tracker while we tear down and
        // rebuild the gallery. Clearing innerHTML collapses the container,
        // the browser auto-clamps main-content's scrollTop to ~0, and the
        // resulting scroll event would clobber the router's saved scrollY
        // with 0 — leaving the user at the top after navigation. We
        // re-enable the tracker in a rAF tick once layout has settled
        // around the new content.
        this._suppressScrollTracking = true;
        const restoreTracker = () => {
            requestAnimationFrame(() => {
                this._suppressScrollTracking = false;
            });
        };

        this.container.innerHTML = '';
        this.renderedItems.clear();
        this.chunker.reset();
        this.chunker.init();
        const items = galleryState.displayedItems;
        this.chunker.setItems(items);
        this.chunker.renderChunk();
        restoreTracker();

        // Show/hide breadcrumbs for H-Manga book view
        const breadcrumbs = document.getElementById('gallery-breadcrumbs');
        if (breadcrumbs) {
            if (galleryState.section === SECTIONS.H_MANGA && galleryState.currentArtist) {
                breadcrumbs.style.display = 'block';
                const breadcrumbName = document.getElementById('breadcrumb-artist-name');
                if (breadcrumbName) breadcrumbName.textContent = galleryState.currentArtist.name;
            } else if (galleryState.section === SECTIONS.H_MANGA && galleryState.favoritesOnly && !galleryState.currentArtist) {
                breadcrumbs.style.display = 'block';
                const breadcrumbName = document.getElementById('breadcrumb-artist-name');
                if (breadcrumbName) breadcrumbName.textContent = '★ Favorites';
            } else {
                breadcrumbs.style.display = 'none';
            }
        }
    }

    renderItem(item, index) {
        const section = galleryState.section;
        const div = document.createElement('div');
        div.className = 'gallery-item';
        div.dataset.index = index;
        
        // Bound the Map so it doesn't grow unbounded across a long scroll
        // session. Evict the oldest inserted entry (Map preserves insertion
        // order, so the first key is the oldest). Recent entries are kept so
        // scroll-back can reload thumbnails without a re-render.
        if (this.renderedItems.size > Gallery.MAX_RENDERED_ITEMS) {
            const oldestKey = this.renderedItems.keys().next().value;
            if (oldestKey !== undefined) {
                this.renderedItems.delete(oldestKey);
            }
        }
        this.renderedItems.set(index, { item, section });
        
        if (section === SECTIONS.IMAGES) {
            return this.createImageStructure(div, item);
        } else if (section === SECTIONS.H_MANGA && (galleryState.currentArtist || galleryState._chapterArtistMap.has(item))) {
            return this.createBookStructure(div, item);
        } else {
            return this.createSeriesStructure(div, item);
        }
    }

    createImageStructure(div, file) {
        const title = document.createElement('div');
        title.className = 'gallery-item-info';
        title.innerHTML = `<div class="gallery-item-title">${escapeHtml(file.name)}</div>`;
        div.appendChild(title);

        this.addFavoriteBadge(div, file);

        div.addEventListener('click', () => {
            window.dispatchEvent(new CustomEvent('gallery:openMedia', { detail: file }));
        });

        return div;
    }

    createSeriesStructure(div, series) {
        const title = document.createElement('div');
        title.className = 'gallery-item-info';
        const label = galleryState.section === SECTIONS.H_MANGA ? 'Books' : 'Chapters';
        // Manga chapters are expected to be numbered; named extras inflate
        // the count. H-Manga books are commonly free-form ("An Erotic
        // Novel Kind of Girl! (Comic X-Eros #89)") and should still be
        // counted individually.
        const count = getChapterCount(series.chapters, {
            skipNonNumeric: galleryState.section === SECTIONS.MANGA
        });
        title.innerHTML = `
            <div class="gallery-item-title">${escapeHtml(series.name)}</div>
            <div class="text-sm" style="color:#ccc;">${count} ${label}</div>
        `;
        div.appendChild(title);

        // Add a placeholder badge slot up front so the layout reserves space
        // and the badge appears the moment its async progress query resolves,
        // rather than popping in after the item may have scrolled past. The
        // promise is intentionally NOT awaited here — createSeriesStructure
        // must stay synchronous so the chunker's renderItem pipeline works.
        this.addContinueBadge(div, series);

        // Artist-level favorite badge (H-Manga overview):getItemIdentifier
        // resolves "artist:<path|name>" for chapter-bearing folder items.
        this.addFavoriteBadge(div, series);

        div.addEventListener('click', () => {
            if (galleryState.section === SECTIONS.H_MANGA) {
                galleryState.setCurrentArtist(series);
                this.render();
            } else if (galleryState.section === SECTIONS.MANGA) {
                window.dispatchEvent(new CustomEvent('gallery:openSeriesSplash', { detail: { series } }));
            } else {
                window.dispatchEvent(new CustomEvent('gallery:openReader', {
                    detail: { series, chapterIndex: 0 }
                }));
            }
        });

        return div;
    }

    createBookStructure(div, book) {
        const artist = galleryState._chapterArtistMap.get(book) || galleryState.currentArtist;
        const title = document.createElement('div');
        title.className = 'gallery-item-info';
        const pageCount = book.images?.length || 0;
        const mappedArtist = galleryState._chapterArtistMap.get(book);
        const artistLabel = mappedArtist && !galleryState.currentArtist
            ? `<div class="text-sm" style="color:#aaa;">${escapeHtml(mappedArtist.name)}</div>`
            : '';
        title.innerHTML = `
            <div class="gallery-item-title">${escapeHtml(book.name)}</div>
            ${artistLabel}
            <div class="text-sm" style="color:#ccc;">${pageCount} Pages</div>
        `;
        div.appendChild(title);

        this.addFavoriteBadge(div, book);

        const series = artist;
        const chapterIndex = series?.chapters?.indexOf(book) ?? -1;

        div.addEventListener('click', () => {
            // Guard: -1 means the chapter object identity no longer matches
            // (e.g. favorites flat view after a folder refresh). Opening the
            // reader with -1 renders an empty shell — resolve by name instead,
            // or skip the open when it can't be resolved.
            let resolvedIdx = chapterIndex;
            if (resolvedIdx < 0 && series?.chapters) {
                resolvedIdx = series.chapters.findIndex(c => c.name === book.name);
            }
            if (resolvedIdx < 0) {
                showNotification('Could not resolve chapter for this book', 'warning');
                return;
            }
            window.dispatchEvent(new CustomEvent('gallery:openReader', {
                detail: { series, chapterIndex: resolvedIdx }
            }));
        });

        return div;
    }

    // Called when item enters viewport (+margin) - load thumbnail
    loadItemThumbnail(el) {
        const index = parseInt(el.dataset.index);
        if (isNaN(index)) return;
        
        const data = this.renderedItems.get(index);
        if (!data) return;
        
        const { item, section } = data;
        
        // Skip if already has an img element
        if (el.querySelector('img')) {
            return;
        }
        
        // Get total count for preloading context
        const total = galleryState.displayedItems.length;
        
        // Get next/prev items for preloading based on actual sort order
        const nextItem = index < galleryState.displayedItems.length - 1 ? galleryState.displayedItems[index + 1] : null;
        const prevItem = index > 0 ? galleryState.displayedItems[index - 1] : null;
        
        debug('[GALLERY DEBUG] loadItemThumbnail:', { 
            index, 
            total, 
            nextItemPath: nextItem?.path, 
            prevItemPath: prevItem?.path,
            itemPath: item.path 
        });
        
        // Load thumbnail based on type
        if (section === SECTIONS.IMAGES) {
            // Guard: an Images-section item is expected to have a path.
            // Skip the request if missing to avoid /api/thumbnail/undefined 403s.
            if (!item.path) return;
            const options = { index, total };
            if (nextItem?.path) options.nextPath = nextItem.path;
            if (prevItem?.path) options.prevPath = prevItem.path;
            debug('[GALLERY DEBUG] Calling loadThumbnail with options:', options);
            thumbnailService.loadThumbnail(item, el, false, section, options);
        } else if (section === SECTIONS.H_MANGA && (galleryState.currentArtist || galleryState._chapterArtistMap.has(item))) {
            if (item.images?.length > 0 && item.images[0].path) {
                const firstImg = item.images[0];
                const options = { index, total };
                if (nextItem?.images?.[0]?.path) options.nextPath = nextItem.images[0].path;
                if (prevItem?.images?.[0]?.path) options.prevPath = prevItem.images[0].path;
                thumbnailService.loadThumbnail({
                    path: firstImg.path,
                    handle: firstImg.handle,
                    type: 'image'
                }, el, false, section, options);
            }
        } else {
            if (item.chapters?.[0]?.images?.[0]?.path) {
                const thumbImg = item.chapters[0].images[0];
                const options = { index, total };
                if (nextItem?.chapters?.[0]?.images?.[0]?.path) options.nextPath = nextItem.chapters[0].images[0].path;
                if (prevItem?.chapters?.[0]?.images?.[0]?.path) options.prevPath = prevItem.chapters[0].images[0].path;
                thumbnailService.loadThumbnail({
                    path: thumbImg.path,
                    handle: thumbImg.handle,
                    type: 'image'
                }, el, false, section, options);
            }
        }
    }

    // Called when item leaves viewport (+margin) - unload to free memory
    unloadItemThumbnail(el) {
        const img = el.querySelector('img');
        const video = el.querySelector('video');
        
        if (img) {
            // Set min-height before removing to preserve layout
            if (!el.style.minHeight) {
                el.style.minHeight = `${el.offsetHeight}px`;
            }
            
            // Revoke blob URL to free memory
            if (img.src?.startsWith('blob:')) {
                URL.revokeObjectURL(img.src);
            }
            
            img.remove();
        }
        
        if (video) {
            video.pause();
            if (video.src?.startsWith('blob:')) {
                URL.revokeObjectURL(video.src);
            }
            video.remove();
        }
        
        // Remove placeholders (image thumbnail placeholder, error placeholder,
        // and the video ▶ fallback placeholder)
        const placeholder = el.querySelector('.thumb-placeholder, [data-error-placeholder], [data-video-placeholder]');
        if (placeholder) {
            placeholder.remove();
        }

        // Remove the video ▶ badge added by _showVideoBadge
        const videoBadge = el.querySelector('[data-video-badge]');
        if (videoBadge) {
            videoBadge.remove();
        }
    }

    async addContinueBadge(container, series) {
        // Insert an invisible placeholder span immediately so the container
        // reserves its layout slot; the span is populated (or removed) once
        // the async progress query resolves. This means the badge never
        // "jumps in" after the item has scrolled past — it's either filled
        // in-place or silently removed with no layout shift.
        const info = container.querySelector('.gallery-item-info');
        if (!info) return;
        const placeholder = document.createElement('span');
        placeholder.className = 'continue-badge placeholder';
        placeholder.setAttribute('aria-hidden', 'true');
        info.appendChild(placeholder);

        const progressData = await progress.getContinuePosition(series.name, galleryState.section);
        // Synthesized "next chapter" position (all recorded chapters
        // complete) past the actual chapter list means the series is fully
        // read — no Continue badge.
        if (!progressData || progressData.chapterIndex >= (series.chapters?.length || 0)) {
            // No progress recorded — remove the placeholder with no visible change.
            placeholder.remove();
            return;
        }

        const badge = document.createElement('span');
        badge.className = 'continue-badge';
        const chapName = series.chapters?.[progressData.chapterIndex]?.name;
        const { chapter: mainCh, part } = parseChapterName(chapName || String(progressData.chapterIndex + 1));
        const isNumeric = typeof mainCh === 'number';
        badge.textContent = isNumeric
            ? (part > 0 ? `Continue Ch.${mainCh}.${part}` : `Continue Ch.${mainCh}`)
            : `Continue ${mainCh}`;
        badge.addEventListener('click', (e) => {
            e.stopPropagation();
            window.dispatchEvent(new CustomEvent('gallery:openReader', {
                detail: { series, chapterIndex: progressData.chapterIndex }
            }));
        });

        // Swap the placeholder for the real badge. If the container was
        // detached (e.g. virtualized away) during the await, placeholder may
        // no longer have a parent — guard against that.
        if (placeholder.parentNode) {
            placeholder.parentNode.replaceChild(badge, placeholder);
        }
    }

    addFavoriteBadge(div, item) {
        const section = galleryState.section;
        if (section === SECTIONS.MANGA) return;

        const identifier = galleryState.getItemIdentifier(item, galleryState._chapterArtistMap.get(item));
        if (!identifier) return;

        const isFavorite = galleryState.favoriteIds.has(identifier);

        const badge = document.createElement('span');
        badge.className = 'favorite-badge';
        badge.textContent = '★';
        if (isFavorite) badge.classList.add('is-favorite');
        badge.title = isFavorite ? 'Remove from favorites' : 'Add to favorites';
        badge.addEventListener('click', (e) => {
            e.stopPropagation();
            this.toggleFavorite(identifier, section, item, badge);
        });

        div.appendChild(badge);
    }

    async toggleFavorite(identifier, section, item, badge) {
        const isFavorite = galleryState.favoriteIds.has(identifier);
        if (isFavorite) {
            await favoritesStore.remove(section, identifier);
            galleryState.favoriteIds.delete(identifier);
            badge.classList.remove('is-favorite');
        } else {
            const name = item.name || identifier;
            await favoritesStore.add(section, identifier, name);
            galleryState.favoriteIds.add(identifier);
            badge.classList.add('is-favorite');
        }

        // Artist grid sorts favorites-first: any toggle changes order, so a
        // re-render is required regardless of the favorites-only filter.
        // Book/image toggles only reorder when favoritesOnly is active.
        const isArtistCard = section === SECTIONS.H_MANGA && identifier.startsWith('artist:');
        if (galleryState.favoritesOnly || isArtistCard) {
            galleryState.updateDisplay();
            this.render();
        }
    }

    onLoadMore() {
        // Handled by chunker
    }

    destroy() {
        this.chunker.destroy();
        this.renderedItems.clear();
        this._routerAbort?.abort();
        this._routerScrollAbort?.abort();
    }
}
