/**
 * VirtualizedChunker - Unified pagination and virtualization
 * Uses viewport-based IntersectionObserver for maximum compatibility
 */

export class VirtualizedChunker {
    constructor(container, options = {}) {
        this.container = container;
        this.options = {
            chunkSize: options.chunkSize || 50,
            viewportMargin: options.viewportMargin || '300px',
        };

        this.currentPage = 0;
        this.items = [];
        this.renderedCount = 0;
        this.paginationObserver = null;
        this.visibilityObserver = null;
        this.onItemRender = options.onItemRender || (() => {});
        this.onItemEnterViewport = options.onItemEnterViewport || null;
        this.onItemLeaveViewport = options.onItemLeaveViewport || null;
        this.onSentinelVisible = options.onSentinelVisible || (() => {});
        this.isLoadingChunk = false;

        // Observers are created once for the chunker's lifetime. init() used
        // to build two fresh IntersectionObservers per call, and Gallery
        // .render() calls reset()+init() on every render — the old observers
        // (never disconnected) kept observed detached nodes reachable.
        this.setupObservers();
    }

    // Idempotent: the observers are created once in the constructor and live
    // for the chunker's lifetime, so re-init after reset() is a no-op.
    init() {
    }

    setupObservers() {
        // Pagination observer - triggers when sentinel enters viewport
        this.paginationObserver = new IntersectionObserver((entries) => {
            const entry = entries[0];
            if (entry.isIntersecting && !this.isLoadingChunk) {
                if (this.renderedCount >= this.items.length) return;

                // Set the re-entrance guard SYNCHRONOUSLY, before scheduling the
                // rAF. Previously this was set inside the rAF callback, so a
                // second observer fire (e.g. a rapid scroll past the sentinel)
                // could pass the !isLoadingChunk check and schedule a second
                // rAF that double-incremented currentPage. Setting it here
                // blocks any concurrent callback until the rAF resets it.
                this.isLoadingChunk = true;
                requestAnimationFrame(() => {
                    this.currentPage++;
                    this.renderChunk();
                    this.onSentinelVisible();
                    this.isLoadingChunk = false;
                });
            }
        }, { rootMargin: '200px' });

        // Visibility observer - tracks items entering/leaving viewport
        this.visibilityObserver = new IntersectionObserver((entries) => {
            entries.forEach((entry) => {
                const el = entry.target;
                
                if (entry.isIntersecting) {
                    // Item entered viewport - load it
                    if (this.onItemEnterViewport) {
                        this.onItemEnterViewport(el);
                    }
                } else {
                    // Item left viewport - unload it
                    if (this.onItemLeaveViewport) {
                        this.onItemLeaveViewport(el);
                    }
                }
            });
        }, { rootMargin: this.options.viewportMargin });
    }

    setItems(items) {
        this.items = items;
        this.currentPage = 0;
        this.renderedCount = 0;
    }

    renderChunk() {
        const startIndex = this.currentPage * this.options.chunkSize;
        const endIndex = Math.min(startIndex + this.options.chunkSize, this.items.length);

        if (startIndex >= this.items.length) return;

        // Remove old sentinel
        const oldSentinel = document.getElementById('scroll-sentinel');
        if (oldSentinel) {
            this.paginationObserver.unobserve(oldSentinel);
            oldSentinel.remove();
        }

        const fragment = document.createDocumentFragment();
        let count = 0;
        for (let i = startIndex; i < endIndex; i++) {
            const item = this.items[i];
            const el = this.onItemRender(item, i);
            if (el) {
                el.dataset.index = i;
                this.visibilityObserver.observe(el);
                fragment.appendChild(el);
                count++;
            }
        }

        this.container.appendChild(fragment);
        this.renderedCount += count;

        // Add new sentinel
        if (endIndex < this.items.length) {
            const sentinel = document.createElement('div');
            sentinel.id = 'scroll-sentinel';
            sentinel.style.cssText = 'height: 100px; width: 100%;';
            this.container.appendChild(sentinel);
            this.paginationObserver.observe(sentinel);
        }
    }

    destroy() {
        this.paginationObserver?.disconnect();
        this.visibilityObserver?.disconnect();
    }

    /**
     * Materialize chunks so the scrolling container is tall enough for
     * scrollTop = y to land at the given offset. Renders pages until
     * scrollHeight exceeds y + viewport + margin. Robust to variable
     * row heights — no rowHeight guessing needed.
     */
    rechunkAtOffset(y) {
        if (!this.items?.length) return;
        const margin = 800;
        const target = y + (window.innerHeight || 0) + margin;
        while (this.renderedCount < this.items.length && this.container.scrollHeight < target) {
            this.currentPage++;
            this.renderChunk();
        }
    }

    reset() {
        this.currentPage = 0;
        this.renderedCount = 0;
        this.isLoadingChunk = false;
        this.container.innerHTML = '';
    }
}
