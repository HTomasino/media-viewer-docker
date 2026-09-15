/**
 * ResourceManager - Centralized object URL lifecycle management
 * Replaces duplicate registry code in script.js
 */

export class ResourceManager {
    constructor() {
        this.registry = new Set();
    }
    
    /**
     * Create an object URL and track it for cleanup.
     * @param {Blob|File} blob - Blob or File to create URL for
     * @returns {string} - Object URL
     */
    createUrl(blob) {
        const url = URL.createObjectURL(blob);
        this.registry.add(url);
        return url;
    }
    
    /**
     * Revoke a specific object URL.
     * @param {string} url - URL to revoke
     */
    revoke(url) {
        if (typeof url === 'string' && url.startsWith('blob:')) {
            URL.revokeObjectURL(url);
            this.registry.delete(url);
        }
    }
    
    /**
     * Revoke all tracked object URLs.
     */
    revokeAll() {
        this.registry.forEach(url => URL.revokeObjectURL(url));
        this.registry.clear();
    }
    
    /**
     * Get count of tracked URLs (for debugging).
     * @returns {number}
     */
    getTrackedCount() {
        return this.registry.size;
    }
}

// Singleton instance for app-wide use
export const resourceManager = new ResourceManager();

/**
 * Attach automatic retry-on-error to a media element (<img> or <video>).
 *
 * When the element's load fails (error event), the src is re-assigned after
 * a short backoff, up to `maxRetries` times. This transparently recovers
 * transient load failures — a dropped connection mid-stream, a backend
 * briefly busy during a scan, a thumbnail still being generated when the
 * gallery first paints. After exhausting retries, `onGiveUp` (if provided)
 * runs so the caller can show its existing error/placeholder UI.
 *
 * NO cache-bust query param: media/thumbnail endpoints are network-only
 * (not SW-cached) and the browser HTTP cache doesn't store error responses,
 * so a plain re-fetch already hits the network. Adding `?_retry=N` would
 * cache a SUCCESSFUL retry under a param'd key that future normal loads
 * (without the param) would miss — pure cache-poisoning loss. Instead, the
 * reload is forced by briefly clearing `src` to '' before re-assigning,
 * which kicks a fresh fetch/decode for every scheme (http, blob:, data:).
 *
 * IDEMPOTENT PER ELEMENT: if called again on the same element (e.g. the
 * modal reuses one <img> across navigations), any prior attachment is torn
 * down first — its listener is removed and its pending timer cancelled — so
 * listeners and retry closures never accumulate, and a stale timer from a
 * previous URL can never fire and clobber the new src. Callers should still
 * invoke the returned disposer on teardown (e.g. component cleanup) to drop
 * the listener when the element is no longer used.
 *
 * CONCURRENCY GATE: a shared per-origin in-flight counter caps simultaneous
 * retries so a gallery full of failed thumbnails during a backend scan
 * doesn't multiply request volume against an already-saturated
 * ConnectionLimiter (which 503s when full — and 503s are themselves load
 * failures the helper would otherwise retry). Retries that would exceed the
 * cap are skipped (counted as exhausted) rather than queued.
 *
 * @param {HTMLImageElement|HTMLVideoElement} el - element with a .src to retry
 * @param {string} url - the original src
 * @param {object} [opts]
 * @param {number} [opts.maxRetries=2] - retry attempts after the initial failure
 * @param {number} [opts.baseDelayMs=400] - initial backoff; doubled each retry (capped)
 * @param {function(): void} [opts.onGiveUp] - called when retries are exhausted
 * @returns {function} disposer that cancels pending retries and removes the listener
 */
// Shared in-flight retry cap. Bounds total concurrent image-retry requests
// across the whole app so a mass thumbnail failure during a scan doesn't
// generate O(tiles × maxRetries) requests against the backend's
// ConnectionLimiter. 8 is well under the default max_concurrent (50),
// leaving headroom for normal traffic.
const MAX_INFLIGHT_RETRIES = 8;
let _inflightRetries = 0;

export function attachImageRetry(el, url, { maxRetries = 2, baseDelayMs = 400, onGiveUp } = {}) {
    // Idempotency: tear down any prior attachment on this element before
    // installing a new one. This prevents listener/closure accumulation on
    // reused elements (the modal's single imgEl, reader imgs) and cancels
    // any pending retry timer from a previous URL so it can't fire and
    // overwrite the new src with a stale URL.
    const prev = el.__imageRetryDispose;
    if (typeof prev === 'function') prev();

    let attempt = 0;
    let timer = null;
    let cancelled = false;
    let countedInflight = false;

    const doRetry = () => {
        if (cancelled || !el.isConnected) return;
        // Force a reload: clearing src first ensures the subsequent
        // re-assignment actually changes the value (re-assigning an
        // identical src is a browser no-op and would neither refetch nor
        // retrigger error). This works for every scheme — http(s) (fresh
        // network fetch), blob: (re-decode), data: (re-decode).
        el.src = '';
        el.src = url;
    };

    const fail = () => {
        if (cancelled || !el.isConnected) return;
        if (attempt >= maxRetries) {
            if (typeof onGiveUp === 'function') onGiveUp();
            return;
        }
        // Concurrency gate: if too many retries are already in flight across
        // the app, skip this one (treat as exhausted) instead of piling onto
        // a saturated backend. This prevents the retry layer from amplifying
        // the very backpressure (503 from ConnectionLimiter) that image load
        // failures signal during a scan.
        if (_inflightRetries >= MAX_INFLIGHT_RETRIES) {
            if (typeof onGiveUp === 'function') onGiveUp();
            return;
        }
        attempt++;
        _inflightRetries++;
        countedInflight = true;
        const delay = Math.min(baseDelayMs * (1 << (attempt - 1)), 3000);
        timer = setTimeout(() => {
            // Release the inflight slot when the timer fires.
            if (countedInflight) { _inflightRetries--; countedInflight = false; }
            if (cancelled || !el.isConnected) return;
            doRetry();
        }, delay);
    };

    const dispose = () => {
        if (cancelled) return;
        cancelled = true;
        if (timer) { clearTimeout(timer); timer = null; }
        if (countedInflight) { _inflightRetries--; countedInflight = false; }
        el.removeEventListener('error', fail);
        if (el.__imageRetryDispose === dispose) delete el.__imageRetryDispose;
    };
    el.__imageRetryDispose = dispose;

    el.addEventListener('error', fail, { once: false });

    return dispose;
}
