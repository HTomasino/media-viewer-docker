/**
 * History-API-based SPA router.
 *
 * Single source of truth for nav transitions: popstate (and a synthetic
 * dispatch after pushState/replaceState, which do NOT fire popstate).
 * push/replace/back only mutate the stack and history; all enter()/exit()
 * calls run in _applyTransition.
 *
 * Scroll position is captured per entry on scroll (in-memory only, debounced
 * to localStorage) and restored on back navigation via a double-rAF +
 * router:scrollRestored event so the gallery can materialize chunks first.
 *
 * Persists to localStorage under `nav:stack`. On first boot, if `nav:stack`
 * is missing but legacy `lastView` is present, seeds from it (Option B
 * safety net — legacy keys are never deleted).
 */

const STACK_KEY = 'nav:stack';
const LEGACY_KEY = 'lastView';

/**
 * Monotonic per-document sequence counter stamped into history.state on
 * every push/replace. popstate uses it to match the browser history cursor
 * to a stack entry positionally — necessary because entries whose URL fell
 * back to a payload-less form (URL-too-long fallback) can't be matched via
 * _toUrl string comparison.
 */
let _seqCounter = 0;
function nextSeq() { return ++_seqCounter; }

const encodePayload = (p) => p == null ? '' : encodeURIComponent(JSON.stringify(p));
const decodePayload = (s) => {
    if (!s) return null;
    try { return JSON.parse(decodeURIComponent(s)); }
    catch { return null; }
};

export const historyRouter = {
    _stack: [],
    _routes: new Map(),
    _persistScheduled: false,
    _init: false,

    register(route, { enter, exit } = {}) {
        this._routes.set(route, { enter, exit });
    },

    _persist() {
        if (this._persistScheduled) return;
        this._persistScheduled = true;
        setTimeout(() => {
            this._persistScheduled = false;
            try {
                localStorage.setItem(STACK_KEY, JSON.stringify(this._stack));
            } catch (e) {
                console.warn('[ROUTER] persist failed:', e);
            }
        }, 150);
    },

    _load() {
        try {
            const raw = localStorage.getItem(STACK_KEY);
            if (raw) {
                const parsed = JSON.parse(raw);
                if (Array.isArray(parsed)) return parsed;
            }
        } catch (e) {
            console.warn('[ROUTER] seed failed:', e);
        }
        // Option B safety net: seed from legacy lastView on first boot.
        try {
            const raw = localStorage.getItem(LEGACY_KEY);
            if (!raw) return [{ route: 'home', payload: null, scrollY: 0 }];
            const state = JSON.parse(raw);
            const sectionRoute = state.section ? `gallery:${String(state.section).replace(/[-_]/g, '')}` : null;
            if (!sectionRoute || !this._routes.has(sectionRoute)) {
                return [{ route: 'home', payload: null, scrollY: 0 }];
            }
            // Drop the artist payload — the legacy key only records it for
            // restore, but the new router restores section overview, not the
            // drill-down (plan: "synthesize a one-element stack from lastView").
            return [
                { route: 'home', payload: null, scrollY: 0 },
                { route: sectionRoute, payload: null, scrollY: 0 }
            ];
        } catch {
            return [{ route: 'home', payload: null, scrollY: 0 }];
        }
    },

    _toUrl(entry) {
        return `#/${entry.route}${entry.payload ? '/' + encodePayload(entry.payload) : ''}`;
    },

    _fromHash() {
        const hash = window.location.hash;
        if (!hash || hash === '#' || hash === '#/') return null;
        const stripped = hash.startsWith('#/') ? hash.slice(2) : hash.slice(1);
        const slash = stripped.indexOf('/');
        const route = slash < 0 ? stripped : stripped.slice(0, slash);
        const payloadStr = slash < 0 ? '' : stripped.slice(slash + 1);
        const payload = decodePayload(payloadStr);
        // Human-readable deep-link aliases for sharing/bookmarking.
        // #/reader/<encoded-name>/<idx> → { series: { name }, chapterIndex: idx }
        // #/splash/<encoded-name>         → { series: { name } }
        if (payload === null && payloadStr) {
            if (route === 'reader') {
                const m = payloadStr.match(/^(.+)\/(\d+)$/);
                if (m) {
                    return {
                        route: 'reader:open',
                        payload: {
                            series: { name: decodeURIComponent(m[1]) },
                            chapterIndex: parseInt(m[2], 10),
                        },
                    };
                }
            }
            if (route === 'splash') {
                return {
                    route: 'splash:open',
                    payload: { series: { name: decodeURIComponent(payloadStr) } },
                };
            }
            if (route === 'modal') {
                return {
                    route: 'modal:open',
                    payload: { file: { path: decodeURIComponent(payloadStr), type: null } },
                };
            }
        }
        return { route, payload };
    },

    /**
     * Single dispatch point for enter()/exit(). Runs exit() on the popped
     * entry (if the stack shrank) and enter() on the new top. Returns the
     * Promise from enter() so callers can await async enters (data loads
     * etc.) before scheduling scroll restore.
     */
    async _applyTransition(prevTop, newTop) {
        const stackChanged = !prevTop || prevTop !== newTop;
        if (prevTop && stackChanged) {
            const outgoing = this._routes.get(prevTop.route);
            if (outgoing?.exit) {
                try { await outgoing.exit(prevTop.payload); } catch (e) { console.warn('[ROUTER] exit failed:', e); }
            }
        }
        const handler = this._routes.get(newTop.route);
        if (handler?.enter) {
            try { return await handler.enter(newTop.payload); }
            catch (e) { console.warn('[ROUTER] enter failed:', e); }
        }
    },

    _scheduleScrollRestore(y) {
        // Double-rAF: first frame lets layout commit the new DOM, second
        // lets scrollHeight become measurable. The gallery listens for
        // router:scrollRestored and scrolls its own _scrollContainer
        // (main-content, since the window doesn't scroll in this layout).
        requestAnimationFrame(() => requestAnimationFrame(() => {
            window.dispatchEvent(new CustomEvent('router:scrollRestored', { detail: { y } }));
        }));
    },

    push(route, payload = null) {
        const top = this._stack[this._stack.length - 1];
        // Note: we deliberately do NOT overwrite the outgoing entry's
        // scrollY here. The scroll listener (window.scroll for the rare
        // case where the page scrolls, router:scroll for child containers
        // like .main-content) keeps the entry's scrollY up-to-date. Writing
        // window.scrollY here would clobber the gallery's position to 0
        // on every push (since main-content scrolls internally, not the
        // window).
        // seq is a per-document monotonic counter stored on both the stack
        // entry and history.state, letting popstate match the browser
        // history cursor positionally even when the URL fell back to a
        // payload-less form (URL-too-long fallback) and can't be matched
        // via _toUrl string comparison.
        const entry = { route, payload, scrollY: 0, seq: nextSeq() };
        this._stack.push(entry);
        // pushState can throw (SecurityError) if the URL exceeds browser
        // length limits (~2MB Chrome, stricter in others). A large series
        // payload (chapters array) can blow this. Fall back to a payload-less
        // URL so the entry still lands in the history; the stack still
        // holds the full payload for in-memory navigation.
        let url = this._toUrl(entry);
        try {
            history.pushState({ route, seq: entry.seq }, '', url);
        } catch (e) {
            console.warn('[ROUTER] pushState URL too long, dropping payload from URL:', e);
            url = `#/${route}`;
            try {
                history.pushState({ route, seq: entry.seq }, '', url);
            } catch (e2) {
                console.warn('[ROUTER] pushState failed entirely:', e2);
                this._stack.pop();
                return;
            }
        }
        this._persist();
        // pushState doesn't fire popstate, so dispatch a synthetic
        // transition. await it so scroll restore happens after the overlay
        // is actually open (enter may be async, e.g. splash.open).
        this._applyTransition(top, entry).finally(() => {
            this._scheduleScrollRestore(0);
        });
    },

    replace(route, payload = null, opts = {}) {
        if (this._stack.length === 0) {
            this._stack.push({ route: 'home', payload: null, scrollY: 0 });
        }
        // Preserve the outgoing entry's scrollY unless the caller asked
        // for a reset. Don't read window.scrollY — main-content scrolls
        // internally, so window.scrollY is always 0 for this layout.
        const top = this._stack[this._stack.length - 1];
        const entry = { route, payload, scrollY: opts.resetScroll ? 0 : (top.scrollY || 0), seq: nextSeq() };
        this._stack[this._stack.length - 1] = entry;
        history.replaceState({ route, seq: entry.seq }, '', this._toUrl(entry));
        this._persist();
        if (opts.skipEnter) {
            this._scheduleScrollRestore(entry.scrollY || 0);
            return;
        }
        this._applyTransition(top, entry).finally(() => {
            // After replace, scroll to the captured y (or 0 if the view
            // changed and we asked for a reset). Section replaces need an
            // explicit scrollTo because enter() resets the DOM.
            this._scheduleScrollRestore(entry.scrollY || 0);
        });
    },

    back() {
        if (this._stack.length <= 1) {
            history.back();
            return;
        }
        // Persist the current stack state before the browser pops. popstate
        // will truncate the stack to the matching entry and run the transition.
        // scrollY is kept up-to-date by the router:scroll listener, so we don't
        // need to capture it here (window.scrollY would be wrong for this layout).
        this._persist();
        history.back();
    },

    /**
     * True if any stack entry has the given route name.
     */
    hasRoute(route) {
        return this._stack.some(e => e.route === route);
    },

    /**
     * Parse the current URL hash without registering routes or installing
     * listeners. Safe to call before replay(). Returns null if the hash is
     * empty or points at an unknown route.
     */
    peekHash() {
        const parsed = this._fromHash();
        if (!parsed) return null;
        if (!this._routes.has(parsed.route)) return null;
        return parsed;
    },

    /**
     * Pop every entry above the first gallery:* entry, calling exit() on
     * each. Returns true if any entries were popped. Used by section change
     * to close overlays without re-running the gallery route's enter.
     */
    async popOverlays() {
        // Find the index of the topmost gallery:* entry.
        let galleryIdx = -1;
        for (let i = this._stack.length - 1; i >= 0; i--) {
            if (this._stack[i].route.startsWith('gallery:')) { galleryIdx = i; break; }
        }
        if (galleryIdx < 0) return false;
        if (galleryIdx === this._stack.length - 1) return false;
        const popped = this._stack.splice(galleryIdx + 1);
        // Sync the browser URL to the new top so history cursor matches stack.
        // Use replaceState so the popped overlays are not reachable via
        // browser-forward (they were closed by an explicit section switch,
        // not a back gesture).
        const newTop = this._stack[this._stack.length - 1];
        history.replaceState({ route: newTop.route, seq: nextSeq() }, '', this._toUrl(newTop));
        newTop.seq = history.state?.seq;
        this._persist();
        // Run exit() on each popped entry, in reverse (closest-to-gallery first).
        for (let i = popped.length - 1; i >= 0; i--) {
            const handler = this._routes.get(popped[i].route);
            if (handler?.exit) {
                try { await handler.exit(popped[i].payload); }
                catch (e) { console.warn('[ROUTER] exit failed:', e); }
            }
        }
        return true;
    },

    replay() {
        if (this._init) return;
        this._init = true;

        window.addEventListener('scroll', () => {
            const top = this._stack[this._stack.length - 1];
            if (top) top.scrollY = window.scrollY;
            this._persist();
        }, { passive: true });

        // Custom event from gallery/page surfaces that scroll inside a
        // child container (e.g. .main-content has overflow-y: auto). The
        // window-scroll listener above can't capture those positions, so
        // emit a router:scroll event with the inner scrollTop to capture
        // it here. Both listeners share the same stack entry update path.
        window.addEventListener('router:scroll', (e) => {
            const top = this._stack[this._stack.length - 1];
            if (top && e.detail && typeof e.detail.scrollTop === 'number') {
                top.scrollY = e.detail.scrollTop;
                this._persist();
            }
        }, { passive: true });

        this._stack = this._load();
        const deep = this._fromHash();
        if (deep && this._routes.has(deep.route)) {
            this._stack = [
                { route: 'home', payload: null, scrollY: 0, seq: nextSeq() },
                { route: deep.route, payload: deep.payload, scrollY: 0, seq: nextSeq() }
            ];
            history.replaceState({ route: deep.route, seq: this._stack[1].seq }, '', this._toUrl(this._stack[1]));
            this._persist();
        }

        window.addEventListener('popstate', (event) => {
            const parsed = this._fromHash();
            // External nav (browser leaving SPA) — do nothing.
            if (!parsed) return;

            const prevTop = this._stack[this._stack.length - 1];

            // Match the browser history cursor to a stack entry. Prefer the
            // seq counter stamped into history.state (works even for entries
            // whose URL fell back to a payload-less form and can't be matched
            // by _toUrl string comparison); fall back to URL comparison for
            // entries created before seq existed or by another tab.
            let idx = -1;
            const seq = event.state?.seq;
            if (seq != null) {
                idx = this._stack.findIndex(e => e.seq === seq);
            }
            if (idx < 0) {
                idx = this._stack.findIndex(e => this._toUrl(e) === window.location.hash);
            }

            if (idx >= 0) {
                // Matches an existing entry → truncate to it.
                if (idx < this._stack.length - 1) {
                    this._stack.length = idx + 1;
                } else {
                    // Forward nav to the same top — re-enter to refresh.
                    this._persist();
                    this._applyTransition(null, prevTop).finally(() => {
                        this._scheduleScrollRestore(prevTop.scrollY || 0);
                    });
                    return;
                }
            } else if (event.state?.seq == null && this._routes.has(parsed.route)) {
                // Manual URL edit / forward into a route we truncated (no seq
                // state → not one of our own history entries, so it can't be
                // a forward-ghost of a popped overlay). Push it and re-enter.
                const entry = { route: parsed.route, payload: parsed.payload, scrollY: 0, seq: nextSeq() };
                this._stack.push(entry);
                history.replaceState({ route: entry.route, seq: entry.seq }, '', window.location.hash);
                this._persist();
                this._applyTransition(prevTop, entry).finally(() => {
                    this._scheduleScrollRestore(0);
                });
                return;
            } else {
                // Unknown route or a forward-ghost entry (seq present but no
                // matching stack entry — e.g. an overlay closed by an explicit
                // section switch). Ignore so closed overlays can't resurrect.
                return;
            }

            this._persist();
            const newTop = this._stack[this._stack.length - 1];
            this._applyTransition(prevTop, newTop).finally(() => {
                this._scheduleScrollRestore(newTop.scrollY || 0);
            });
        });

        const top = this._stack[this._stack.length - 1];
        this._applyTransition(null, top).finally(() => {
            this._scheduleScrollRestore(top.scrollY || 0);
        });
    }
};