import { apiService } from '../../services/service-registry.js';
import { progress as progressStore } from '../../db/db-registry.js';
import { configState, historyRouter } from '../../core/core-registry.js';
import { galleryState } from '../../core/state/gallery.js';
import { parseChapterName, escapeHtml } from '../../utils/utils-registry.js';

export class SeriesSplash {
    constructor(container) {
        this.container = container;
        this.currentSeries = null;
        this.currentInfo = null;
        this.currentProgress = null;

        this.cache = new Map();
        this.abortController = null;

        // Guard: if the splash container or its required buttons aren't in the
        // DOM (e.g. a future page that doesn't include #series-splash-container)
        // we silently no-op rather than throwing during init. The open() method
        // is also a safe no-op in that state.
        if (!this.container) return;
        const backBtn = this.container.querySelector('#splash-back-btn');
        const primaryBtn = this.container.querySelector('#splash-primary-btn');
        if (backBtn) backBtn.addEventListener('click', () => historyRouter.back());
        if (primaryBtn) primaryBtn.addEventListener('click', () => this.openContinue());
        this.container.addEventListener('click', (e) => {
            if (e.target === this.container) historyRouter.back();
        });
    }

    async open(series) {
        // Defensive guard — see constructor. If the splash container isn't
        // in the DOM there is nothing to show; bail out silently rather than
        // throwing on the first `this.container.style` access.
        if (!this.container) return;
        this.currentSeries = series;
        this.currentInfo = null;
        this.currentProgress = null;

        this.container.style.display = 'flex';
        this.container.setAttribute('aria-hidden', 'false');
        document.getElementById('gallery-container').style.display = 'none';
        document.getElementById('manga-view-container').style.display = 'none';
        document.getElementById('gallery-breadcrumbs').style.display = 'none';

        const sidebar = document.getElementById('sidebar');
        if (sidebar) sidebar.classList.add('collapsed');

        this.renderSkeleton();

        this.abortController?.abort();
        this.abortController = new AbortController();
        const signal = this.abortController.signal;

        const cacheKey = `${configState.isServerMode() ? 'srv' : 'cli'}:${series.name}`;
        let info;
        if (this.cache.has(cacheKey)) {
            info = this.cache.get(cacheKey);
        } else if (configState.isServerMode()) {
            try {
                info = await apiService.getSeriesInfo('manga', series.name);
                this.cache.set(cacheKey, info);
            } catch (e) {
                info = this.fallbackInfo(series);
            }
        } else {
            info = this.fallbackInfo(series);
        }
        if (signal.aborted) return;

        this.currentInfo = info;
        // Wrapped in try/catch so an IndexedDB or store failure doesn't leave
        // the splash hanging on the skeleton forever (open() is `async` and
        // callers wouldn't see the rejection if no listener awaits it).
        try {
            this.currentProgress = await progressStore.getContinuePosition(series.name, galleryState.section);
        } catch (e) {
            console.warn('[SPLASH] Failed to load continue position:', e);
            this.currentProgress = null;
        }
        if (signal.aborted) return;
        this.render(info);
    }

    fallbackInfo(series) {
        return {
            title: series.name,
            description: '',
            author: '',
            status: '',
            genres: [],
            source: '',
            coverUrl: '',
            coverPath: '',
        };
    }

    renderSkeleton() {
        this.container.querySelector('#splash-cover-img').removeAttribute('src');
        this.container.querySelector('#splash-title').textContent = this.currentSeries?.name || '';
        this.container.querySelector('#splash-meta').innerHTML = '';
        this.container.querySelector('#splash-primary-btn').textContent = 'Loading...';
        this.container.querySelector('#splash-description').textContent = '';
        this.container.querySelector('#splash-source').innerHTML = '';
        this.container.querySelector('#splash-chapter-list').innerHTML =
            '<div class="splash-loading">Loading chapters...</div>';
    }

    render(info) {
        const titleEl = this.container.querySelector('#splash-title');
        const metaEl = this.container.querySelector('#splash-meta');
        const descEl = this.container.querySelector('#splash-description');
        const sourceEl = this.container.querySelector('#splash-source');
        const coverEl = this.container.querySelector('#splash-cover-img');
        const primaryBtn = this.container.querySelector('#splash-primary-btn');
        const listEl = this.container.querySelector('#splash-chapter-list');

        titleEl.textContent = info.title || this.currentSeries.name;

        const meta = [];
        if (info.author) meta.push(`<span class="meta-item"><strong>Author:</strong> ${escapeHtml(info.author)}</span>`);
        if (info.status) meta.push(`<span class="meta-item"><strong>Status:</strong> ${escapeHtml(info.status)}</span>`);
        if (info.genres?.length) meta.push(`<span class="meta-item"><strong>Genres:</strong> ${escapeHtml(info.genres.join(', '))}</span>`);
        metaEl.innerHTML = meta.join('');

        descEl.textContent = info.description || '';
        descEl.style.display = info.description ? 'block' : 'none';

        if (info.source) {
            sourceEl.innerHTML = `Source: <a href="${escapeHtml(info.source)}" target="_blank" rel="noopener">${escapeHtml(info.source)}</a>`;
        } else {
            sourceEl.innerHTML = '';
        }

        // Prefer the local coverPath (always reachable, served by the server's
        // media endpoint) over the remote coverUrl. The remote URL can be
        // blocked, throttled, or simply fail to load — leaving an empty cover
        // frame on the user's screen. The remote URL is only used as a
        // fallback when no local cover image exists. We also attach an
        // onerror handler so if the chosen URL fails to load we fall back to
        // the other source rather than showing a broken-image frame.
        //
        // The onerror handler MUST be installed before setCover() runs the
        // first time. Otherwise setCover's `coverEl.onerror = null` would
        // clear the handler we just assigned, leaving the image with no
        // fallback at all (the src then loads without any error recovery).
        //
        // We deliberately do NOT clear `onerror` in setCover or in the two
        // URL branches — the handler must remain attached so it can fire
        // when a URL fails and hop to the other source. The terminal `else`
        // branch (both sources exhausted) is the only place `onerror` is
        // cleared, which breaks the fallback cycle.
        //
        // `tried` tracks which sources we've already attempted so we don't
        // oscillate local→remote→local→remote forever when both fail. The
        // browser fires onerror once per failed load, but each `setCover`
        // call triggers a new load, so without this guard a broken local +
        // unreachable remote would hammer the server with hundreds of
        // requests per second until the tab is closed.
        const tried = { local: false, remote: false };
        const setCover = (url) => {
            coverEl.src = url;
        };
        coverEl.onerror = () => {
            // Compare against getAttribute('src') (the raw string we assigned)
            // rather than coverEl.src (which the browser resolves against the
            // document base to an absolute URL — it would never match a
            // relative `this._localCoverUrl` and the local→remote fallback
            // branch would silently never fire).
            const current = coverEl.getAttribute('src');
            if (current === this._localCoverUrl) tried.local = true;
            if (current === info.coverUrl) tried.remote = true;

            if (current === this._localCoverUrl && info.coverUrl && !tried.remote) {
                setCover(info.coverUrl);
            } else if (current === info.coverUrl && info.coverPath && configState.isServerMode() && !tried.local) {
                setCover(this._localCoverUrl);
            } else {
                // Both sources exhausted (or the failed URL doesn't match
                // either). Clear the handler so the next load attempt (e.g.
                // from a re-render) can attach a fresh one, and drop the src
                // so the placeholder shows.
                coverEl.onerror = null;
                coverEl.removeAttribute('src');
            }
        };
        if (info.coverPath && configState.isServerMode()) {
            this._localCoverUrl = `/api/media/${encodeURIComponent(info.coverPath).replace(/%2F/g, '/')}?section=manga`;
            setCover(this._localCoverUrl);
        } else if (info.coverUrl) {
            setCover(info.coverUrl);
        } else {
            coverEl.onerror = null;
            this._localCoverUrl = null;
            coverEl.removeAttribute('src');
        }

        if (this.currentProgress) {
            primaryBtn.textContent = 'Continue Reading';
        } else {
            primaryBtn.textContent = 'Read First Chapter';
        }

        listEl.innerHTML = '';
        const chapters = this.currentSeries.chapters || [];
        if (!chapters.length) {
            listEl.innerHTML = '<div class="splash-empty">No chapters found.</div>';
            return;
        }

        const fragment = document.createDocumentFragment();
        // Newest first: iterate from the last chapter down to the first so the
        // most recent chapter appears at the top of the list. The original index
        // is preserved for the reader (progress lookup uses the original index).
        for (let i = chapters.length - 1; i >= 0; i--) {
            const chapter = chapters[i];
            const idx = i;
            const { chapter: mainCh, part } = parseChapterName(chapter.name);
            const isNumeric = typeof mainCh === 'number';
            const label = isNumeric
                ? (part > 0 ? `Ch.${mainCh}.${part}` : `Ch.${mainCh}`)
                : escapeHtml(chapter.name);
            const row = document.createElement('button');
            row.className = 'splash-chapter-row';
            if (this.currentProgress && this.currentProgress.chapterIndex === idx) {
                row.classList.add('is-continue');
            }
            row.innerHTML = `<span class="chapter-label">${label}</span><span class="chapter-pages">${chapter.images?.length || 0} pages</span>`;
            row.addEventListener('click', () => this.openChapter(idx));
            fragment.appendChild(row);
        }
        listEl.appendChild(fragment);
    }

    openContinue() {
        const idx = this.currentProgress?.chapterIndex ?? 0;
        this.openReader(idx);
    }

    openChapter(index) {
        this.openReader(index);
    }

    openReader(chapterIndex) {
        window.dispatchEvent(new CustomEvent('splash:openReader', {
            detail: { series: this.currentSeries, chapterIndex }
        }));
    }

    close() {
        if (!this.container) return;
        this.abortController?.abort();
        this.container.style.display = 'none';
        this.container.setAttribute('aria-hidden', 'true');
        document.getElementById('gallery-container').style.display = 'grid';

        const sidebar = document.getElementById('sidebar');
        if (sidebar) sidebar.classList.remove('collapsed');

        this.currentSeries = null;
        this.currentInfo = null;
        this.currentProgress = null;

        window.dispatchEvent(new CustomEvent('splash:closed'));
    }

    isOpen() {
        return this.container && this.container.style.display !== 'none';
    }
}
