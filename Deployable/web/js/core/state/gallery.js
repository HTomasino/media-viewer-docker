import { SECTIONS, GALLERY_CONFIG, READER_CONFIG } from '../config.js';
import { naturalCompare } from '../../utils/tags.js';
import { progressStore } from '../../db/progress.js';
import { favoritesStore } from '../../db/favorites.js';
import { historyRouter } from '../history-router.js';

const LAST_VIEW_KEY = 'lastView';
const READER_LAST_VIEW_KEY = 'lastViewReader';

/**
 * Map a SECTIONS constant value to its router route name.
 * SECTIONS.H_MANGA = 'h-manga' → 'hmanga' (route name strips separators).
 * Used by setSection and deep-link parsing to ensure the route name
 * matches what's registered in app.js.
 */
export function _sectionToRoute(section) {
    return 'gallery:' + String(section).replace(/[-_]/g, '');
}

/**
 * Map a router route suffix back to a SECTIONS constant value.
 * 'hmanga' → SECTIONS.H_MANGA ('h-manga'). Returns null if unknown.
 */
export function _routeToSection(suffix) {
    if (suffix === 'images') return SECTIONS.IMAGES;
    if (suffix === 'manga') return SECTIONS.MANGA;
    if (suffix === 'hmanga') return SECTIONS.H_MANGA;
    return null;
}

/**
 * GalleryState - Gallery-specific state
 */
export const galleryState = {
    section: SECTIONS.IMAGES,
    files: [],
    folders: [],
    /** Which section the loaded files array belongs to. Used by _navShowSection
     *  to skip a redundant refetch when the target section's data is already
     *  in memory (e.g. back-to-gallery from an overlay). */
    filesSection: null,
    /** Which section the loaded folders array belongs to. */
    foldersSection: null,
    displayedItems: [],
    page: 0,
    activeTag: null,
    /** Free-text search filter. Kept here so updateDisplay() can re-derive
     *  displayedItems from the source arrays on every change — filtering the
     *  derived array in place made each keystroke shrink the result set
     *  cumulatively (backspacing could never restore items). */
    searchQuery: '',
    currentArtist: null,
    favoritesOnly: false,
    favoriteIds: new Set(),
    /** Map from chapter name to parent artist, used during H-Manga favorites-only flat view */
    _chapterArtistMap: new Map(),

    setSection(section) {
        this.section = section;
        this.currentArtist = null;
        this.page = 0;
        this.activeTag = null;
        this._chapterArtistMap.clear();
        this.saveLastView();
        // Return the promise so callers can await the transition (data load,
        // scroll restore) before continuing. Useful when the caller needs
        // the new section's data to be ready (e.g. handleReindex).
        return historyRouter.replace(_sectionToRoute(section), null, { resetScroll: true });
    },

    setCurrentArtist(artist) {
        this.currentArtist = artist;
        this.page = 0;
        this.updateDisplay(); // Update displayedItems to show chapters
        this.saveLastView();
        // Return the promise so callers can await the transition.
        return historyRouter.push('gallery:hmanga:artist', { artist: { name: artist.name, path: artist.path } });
    },

    /**
     * Save the current view state (section + artist) to localStorage
     * so the client can restore it on next visit.
     */
    saveLastView() {
        const state = {
            section: this.section,
            artist: null,
        };
        if (this.section === SECTIONS.H_MANGA && this.currentArtist) {
            state.artist = {
                name: this.currentArtist.name,
                path: this.currentArtist.path,
            };
        }
        try {
            localStorage.setItem(LAST_VIEW_KEY, JSON.stringify(state));
        } catch (e) {
            console.warn('[GALLERY] Failed to save last view state:', e);
        }
    },

    /**
     * Restore the last view state from localStorage.
     * Returns { section, artist } or null if no saved state.
     * For H-Manga sections, the artist data includes name and path
     * which must be matched against loaded folder data to find the
     * full artist object.
     */
    restoreLastView() {
        try {
            const raw = localStorage.getItem(LAST_VIEW_KEY);
            if (!raw) return null;
            const state = JSON.parse(raw);
            // Validate the restored section is valid
            if (!Object.values(SECTIONS).includes(state.section)) return null;
            return state;
        } catch (e) {
            console.warn('[GALLERY] Failed to restore last view state:', e);
            return null;
        }
    },

    /**
     * Clear the last view state (e.g. when user explicitly navigates away).
     */
    clearLastView() {
        try {
            localStorage.removeItem(LAST_VIEW_KEY);
        } catch (e) {
            // Ignore
        }
    },

    setFiles(files) {
        this.files = files;
        this.filesSection = this.section;
        this.updateDisplay();
    },

    setFolders(folders) {
        this.folders = folders;
        this.foldersSection = this.section;
        this.updateDisplay();
    },

    setActiveTag(tag) {
        this.activeTag = tag;
        this.page = 0;
        this.updateDisplay();
        // Reflect filter state in the URL so reload preserves it. Use
        // replace (not push) so browser back skips filter changes.
        this._syncFilterToUrl();
    },

    setFavoritesOnly(value) {
        this.favoritesOnly = value;
        this.page = 0;
        this.updateDisplay();
        this._syncFilterToUrl();
    },

    /**
     * Build a filter payload from current filter state and call
     * historyRouter.replace to update the URL. Called by setActiveTag and
     * setFavoritesOnly. Null fields are stripped to keep the URL clean.
     * No resetScroll: a filter change (tag chip, favorites toggle) is not a
     * navigation — the user's scroll position should be preserved. The
     * default replace path keeps the entry's scrollY.
     */
    _syncFilterToUrl() {
        const filter = {};
        if (this.activeTag) filter.tag = this.activeTag;
        if (this.favoritesOnly) filter.favorites = true;
        const payload = Object.keys(filter).length ? filter : null;
        historyRouter.replace(_sectionToRoute(this.section), payload);
    },

    async loadFavorites(section) {
        this.favoriteIds = await favoritesStore.getIdentifiers(section);
    },

    /**
     * True when the item has a favorite identifier AND that identifier is in
     * the loaded favorite set. Artist cards resolve via the "artist:"
     * namespace, book cards via "parent/book", image files via path.
     */
    isItemFavorite(item, artist) {
        const id = this.getItemIdentifier(item, artist);
        return id != null && this.favoriteIds.has(id);
    },

    getItemIdentifier(item, artist) {
        if (this.section === SECTIONS.IMAGES) {
            return item.path;
        }
        if (this.section === SECTIONS.H_MANGA) {
            const parent = artist || this.currentArtist;
            if (parent) {
                return parent.path + '/' + item.name;
            }
            // Artist-level favorite (artist overview grid, no parent). The
            // "artist:" namespace can never collide with book identifiers
            // ("parent/book"). Folder names are unique within the section,
            // so name is a stable key.
            if (item?.chapters) {
                return 'artist:' + (item.path || item.name);
            }
        }
        return null;
    },

    updateDisplay() {
        if (this.section === SECTIONS.IMAGES) {
            this.displayedItems = this.filterAndSortFiles(this.files);
        } else if (this.section === SECTIONS.H_MANGA && this.currentArtist) {
            this.displayedItems = this.currentArtist.chapters;
        } else if (this.section === SECTIONS.H_MANGA) {
            // H-Manga artist grid: favorite artists first, then alphabetical.
            // naturalCompare on names gives the alphabetical order within
            // each group; the favorite flag is the only primary key, so
            // toggling a favorite reorders immediately on the next
            // updateDisplay().
            this.displayedItems = [...this.folders].sort((a, b) => {
                const aFav = this.isItemFavorite(a) ? 1 : 0;
                const bFav = this.isItemFavorite(b) ? 1 : 0;
                if (aFav !== bFav) return bFav - aFav;
                return naturalCompare(a.name, b.name);
            });
        } else {
            // Manga series grid: most recently updated first (newest book/
            // chapter activity at the top). Secondary key: newest-first is
            // the only key — name order is not wanted here.
            this.displayedItems = [...this.folders].sort((a, b) =>
                (b.updated_at || 0) - (a.updated_at || 0)
            );
        }

        // Clear previous artist mapping from favorites-only flat view
        this._chapterArtistMap.clear();

        if (this.favoritesOnly) {
            if (this.section === SECTIONS.H_MANGA && !this.currentArtist) {
                // Favorites view for the artist grid: favorited ARTISTS stay
                // as artist cards, and favorite BOOKS from non-favorited
                // artists are flattened in as book cards (with their parent
                // mapped for the reader). A favorited artist's books are not
                // also flattened — the artist card already surfaces them.
                const favoriteBooks = [];
                const favoriteArtists = [];
                for (const artist of this.folders) {
                    if (this.isItemFavorite(artist)) {
                        favoriteArtists.push(artist);
                        continue;
                    }
                    for (const chapter of artist.chapters || []) {
                        const id = this.getItemIdentifier(chapter, artist);
                        if (id && this.favoriteIds.has(id)) {
                            // Map chapter name to parent artist so the gallery can
                            // render and open the reader with the correct series
                            this._chapterArtistMap.set(chapter, artist);
                            favoriteBooks.push(chapter);
                        }
                    }
                }
                // Favorite artists keep the artist-grid order (favorites
                // first — vacuously true here — then alphabetical); books
                // keep folder order (updated_at), both preserved because
                // this.folders is already sorted by updateDisplay's base
                // branch above.
                this.displayedItems = [...favoriteArtists, ...favoriteBooks];
            } else {
                this.displayedItems = this.displayedItems.filter(item => {
                    const id = this.getItemIdentifier(item);
                    return id && this.favoriteIds.has(id);
                });
            }
        }

        // Free-text search: always applied against the freshly derived list,
        // never by filtering displayedItems in place.
        if (this.searchQuery) {
            const terms = this.searchQuery.toLowerCase().split(/\s+/).filter(Boolean);
            this.displayedItems = this.displayedItems.filter(item =>
                terms.every(term => item.name.toLowerCase().includes(term))
            );
        }
    },

    filterAndSortFiles(files) {
        let result = [...files];
        if (this.activeTag) {
            result = result.filter(f => f.tags?.includes(this.activeTag));
        }
        if (this.activeTag) {
            result.sort((a, b) => (a.number || 0) - (b.number || 0));
        } else {
            result.sort((a, b) => (b.mtime || 0) - (a.mtime || 0));
        }
        return result;
    },

    nextPage() {
        this.page++;
    },

    getChunk() {
        const start = this.page * GALLERY_CONFIG.CHUNK_SIZE;
        const end = Math.min(start + GALLERY_CONFIG.CHUNK_SIZE, this.displayedItems.length);
        return this.displayedItems.slice(start, end);
    },

    hasMore() {
        const end = (this.page + 1) * GALLERY_CONFIG.CHUNK_SIZE;
        return end < this.displayedItems.length;
    },

    reset() {
        this.page = 0;
        this.activeTag = null;
        this.searchQuery = '';
    },

    clear() {
        this.files = [];
        this.folders = [];
        this.displayedItems = [];
        this.currentArtist = null;
        this.page = 0;
        this.activeTag = null;
        this.searchQuery = '';
    },

    // Calculate tag popularity from all files
    getPopularTags(limit = 20) {
        const tagCounts = new Map();

        this.files.forEach(file => {
            if (file.tags) {
                file.tags.forEach(tag => {
                    const count = tagCounts.get(tag) || 0;
                    tagCounts.set(tag, count + 1);
                });
            }
        });

        // Sort by count descending, then by name
        const sorted = Array.from(tagCounts.entries())
            .sort((a, b) => {
                if (b[1] !== a[1]) return b[1] - a[1]; // Sort by count
                return a[0].localeCompare(b[0]); // Then by name
            })
            .slice(0, limit);

        return sorted; // Returns array of [tag, count]
    },
};

/**
 * ReaderState - Manga reader-specific state
 */
export const readerState = {
    series: null,
    chapterIndex: 0,
    pageIndex: 0,
    mode: READER_CONFIG.MODES.STRIP,
    scaleMode: READER_CONFIG.DEFAULT_SCALE_MODE,
    fillPercent: READER_CONFIG.DEFAULT_FILL_PERCENT,
    loadMode: READER_CONFIG.DEFAULT_LOAD_MODE,
    controlsVisible: true,

    loadSeries(series, chapterIndex = 0) {
        this.series = series;
        this.chapterIndex = chapterIndex;
        this.pageIndex = 0;
        this.saveLastView();
    },

    async restoreProgress() {
        if (!this.series) return;
        const progress = await progressStore.getContinuePosition(this.series.name, galleryState.section);
        if (progress) {
            // For H-Manga, each "chapter" is a standalone book. Don't override
            // the user's explicit book selection — only restore the page offset
            // within the selected book.
            if (galleryState.section === SECTIONS.H_MANGA) {
                this.pageIndex = progress.pageIndex || 0;
            } else {
                // Clamp chapterIndex to valid range for the current series
                // (series data may have changed since progress was saved)
                const maxChapter = (this.series.chapters?.length || 1) - 1;
                this.chapterIndex = Math.min(progress.chapterIndex, Math.max(0, maxChapter));
                this.pageIndex = progress.pageIndex || 0;
            }
        }
    },

    async saveProgress() {
        if (!this.series) return;
        await progressStore.save(
            this.series.name,
            this.chapterIndex,
            this.pageIndex,
            false,
            galleryState.section
        );
    },

    /**
     * Record the user's current position without bumping `lastViewedAt`.
     * Used for chapter open/close/advance where no reading has actually
     * happened yet — opening a chapter is not the same as reading it,
     * and the "Continue" position must not jump to a chapter the user
     * merely previewed.
     */
    async updateProgress() {
        if (!this.series) return;
        await progressStore.updatePosition(
            this.series.name,
            this.chapterIndex,
            this.pageIndex,
            galleryState.section
        );
    },

    async markComplete() {
        if (!this.series) return;
        await progressStore.save(
            this.series.name,
            this.chapterIndex,
            this.pageIndex,
            true,
            galleryState.section
        );
    },

    /**
     * True when the user is on the last page of the current chapter in
     * single/pair mode. Used by the reader to decide whether advancing
     * or closing should mark the chapter complete.
     */
    isAtLastPage() {
        if (this.mode === READER_CONFIG.MODES.STRIP) return false;
        const total = this.getTotalPages();
        if (!total) return false;
        const step = this.mode === READER_CONFIG.MODES.PAIR ? 2 : 1;
        return this.pageIndex + step >= total;
    },

    /**
     * Save the current reader state to localStorage so the client
     * can restore its section context on next visit.
     * Only section and (for h-manga) artist are saved — the gallery
     * restore uses these to navigate to the right section overview,
     * not to auto-open a specific series.
     */
    saveLastView() {
        if (!this.series) return;

        const section = galleryState.section;
        if (!section) return;

        const state = {
            section,
        };

        // For h-manga, also save the current artist so we can navigate
        // back to the artist's chapter listing
        if (section === SECTIONS.H_MANGA && galleryState.currentArtist) {
            state.artist = {
                name: galleryState.currentArtist.name,
                path: galleryState.currentArtist.path,
            };
        }

        try {
            localStorage.setItem(READER_LAST_VIEW_KEY, JSON.stringify(state));
        } catch (e) {
            console.warn('[READER] Failed to save last view state:', e);
        }
    },

    /**
     * Restore the reader's last view state from localStorage.
     * Returns { section, artist? } or null.
     */
    restoreLastView() {
        try {
            const raw = localStorage.getItem(READER_LAST_VIEW_KEY);
            if (!raw) return null;
            const state = JSON.parse(raw);
            if (!Object.values(SECTIONS).includes(state.section)) return null;
            return state;
        } catch (e) {
            console.warn('[READER] Failed to restore last view state:', e);
            return null;
        }
    },

    setMode(mode) {
        this.mode = mode;
        if (mode === READER_CONFIG.MODES.SINGLE) {
            this.scaleMode = READER_CONFIG.SCALE_MODES.FIT_HEIGHT;
            this.fillPercent = READER_CONFIG.DEFAULT_FILL_PERCENT;
        } else if (mode === READER_CONFIG.MODES.STRIP) {
            this.scaleMode = READER_CONFIG.SCALE_MODES.FIT_WIDTH;
            this.fillPercent = 75;
        }
        localStorage.setItem('mangaScaleMode', this.scaleMode);
        localStorage.setItem('mangaFillPercent', this.fillPercent);
    },

    setScaleMode(scaleMode) {
        this.scaleMode = scaleMode;
        localStorage.setItem('mangaScaleMode', scaleMode);
        // Reset fill percent to 100 when switching to original
        if (scaleMode === READER_CONFIG.SCALE_MODES.ORIGINAL) {
            this.fillPercent = READER_CONFIG.DEFAULT_FILL_PERCENT;
            localStorage.setItem('mangaFillPercent', this.fillPercent);
        }
    },

    setFillPercent(percent) {
        this.fillPercent = percent;
        localStorage.setItem('mangaFillPercent', percent);
    },

    loadScaleMode() {
        // Load fill percent first so backward-compat mappings can override it
        this.loadFillPercent();

        const saved = localStorage.getItem('mangaScaleMode');
        if (saved) {
            // Backward compatibility: map old values to new system
            // Only override fillPercent if it hasn't been saved yet (first migration)
            if (saved === 'fit-width-75') {
                this.scaleMode = READER_CONFIG.SCALE_MODES.FIT_WIDTH;
                if (localStorage.getItem('mangaFillPercent') === null) {
                    this.fillPercent = 75;
                    localStorage.setItem('mangaFillPercent', this.fillPercent);
                }
                localStorage.setItem('mangaScaleMode', this.scaleMode);
            } else if (saved === 'fit-both') {
                // 'fit-both' mapped to 'fit-width' at 100% — user can adjust
                this.scaleMode = READER_CONFIG.SCALE_MODES.FIT_WIDTH;
                if (localStorage.getItem('mangaFillPercent') === null) {
                    this.fillPercent = 100;
                    localStorage.setItem('mangaFillPercent', this.fillPercent);
                }
                localStorage.setItem('mangaScaleMode', this.scaleMode);
            } else {
                this.scaleMode = saved;
            }
        }
    },

    loadFillPercent() {
        const saved = localStorage.getItem('mangaFillPercent');
        if (saved !== null) {
            const parsed = parseInt(saved, 10);
            if (READER_CONFIG.FILL_PERCENTAGES.includes(parsed)) {
                this.fillPercent = parsed;
            }
        }
        // Ensure fillPercent is always a valid number (never undefined)
        if (this.fillPercent === undefined || this.fillPercent === null) {
            this.fillPercent = READER_CONFIG.DEFAULT_FILL_PERCENT;
        }
    },

    setLoadMode(loadMode) {
        this.loadMode = loadMode;
        localStorage.setItem('mangaLoadMode', loadMode);
    },

    loadLoadMode() {
        const saved = localStorage.getItem('mangaLoadMode');
        if (saved && Object.values(READER_CONFIG.LOAD_MODES).includes(saved)) {
            this.loadMode = saved;
        }
    },

    getChapter() {
        return this.series?.chapters[this.chapterIndex];
    },

    getTotalPages() {
        return this.getChapter()?.images.length || 0;
    },

    isLastChapter() {
        return this.chapterIndex >= (this.series?.chapters.length || 0) - 1;
    },

    nextPage() {
        const total = this.getTotalPages();
        const step = this.mode === READER_CONFIG.MODES.PAIR ? 2 : 1;
        const newIndex = this.pageIndex + step;
        if (newIndex < total) {
            this.pageIndex = newIndex;
            return true;
        }
        return false;
    },

    previousPage() {
        const step = this.mode === READER_CONFIG.MODES.PAIR ? 2 : 1;
        const newIndex = this.pageIndex - step;
        if (newIndex >= 0) {
            this.pageIndex = newIndex;
            return true;
        }
        return false;
    },

    nextChapter() {
        if (this.chapterIndex < (this.series?.chapters.length || 0) - 1) {
            this.chapterIndex++;
            this.pageIndex = 0;
            // Use updateProgress here — advancing to a new chapter is not
            // itself a reading event and should not bump lastViewedAt for
            // the new chapter (otherwise previewing the next chapter would
            // immediately become the user's "Continue Reading" position).
            this.updateProgress();
            this.saveLastView();
            return true;
        }
        return false;
    },

    previousChapter() {
        if (this.chapterIndex > 0) {
            this.chapterIndex--;
            this.pageIndex = 0;
            this.updateProgress();
            this.saveLastView();
            return true;
        }
        return false;
    },

    toggleControls() {
        this.controlsVisible = !this.controlsVisible;
    },

    showControls() {
        this.controlsVisible = true;
    },

    reset() {
        this.series = null;
        this.chapterIndex = 0;
        this.pageIndex = 0;
        this.mode = READER_CONFIG.MODES.STRIP;
        this.scaleMode = READER_CONFIG.DEFAULT_SCALE_MODE;
        this.fillPercent = READER_CONFIG.DEFAULT_FILL_PERCENT;
        this.loadMode = READER_CONFIG.DEFAULT_LOAD_MODE;
    },
};
