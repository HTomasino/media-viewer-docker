import { db } from './index.js';

/**
 * progressStore - Reading progress tracking
 *
 * Read-state semantics:
 *   - `lastViewedAt` only advances on actual reading events (page turn in
 *     page mode, scroll-save in strip mode, or markComplete). Opening a
 *     chapter without reading does NOT bump it, so a brief preview no longer
 *     hijacks the "Continue Reading" position.
 *   - `isComplete=true` is sticky: any save that doesn't explicitly set it
 *     preserves the prior value. This stops routine saveProgress() calls
 *     from silently unmarking a chapter the user just finished.
 *   - `getContinuePosition` prefers an incomplete chapter over a completed
 *     one, so once a chapter is marked done it stops being a "Continue"
 *     candidate.
 *
 * All record-taking methods accept an optional `section` (a SECTIONS value)
 * and internally key records as "<section>|<seriesName>" so a manga series
 * and an h-manga book that happen to share a folder name never collide.
 * Callers that omit `section` get the legacy unprefixed key, so records
 * written before this change keep working.
 */

// _scopedName prefixes a series name with its section for the IndexedDB key.
// A missing/null section yields the unprefixed name (back-compat with
// records saved before namespacing was introduced).
function _scopedName(section, seriesName) {
    return section ? `${section}|${seriesName}` : String(seriesName ?? '');
}

export const progressStore = {
    // Serializes _upsert calls per record key ("series|chapter"). The scroll
    // handler fires saveProgress (throttled) and markComplete as
    // fire-and-forget asyncs; without serialization their read-modify-write
    // transactions interleave and a stale saveProgress put can clobber the
    // isComplete=true that markComplete just wrote.
    _pendingUpserts: new Map(),

    /**
     * Record actual reading activity: bumps `lastViewedAt`, sets `isComplete`
     * only when explicitly true OR when the existing record was already
     * complete (sticky). Use this for scroll-save, page-turn, and the
     * explicit `markComplete` path.
     */
    async save(seriesName, chapterIndex, pageIndex, isComplete = false, section = null) {
        await this._upsert(
            _scopedName(section, seriesName),
            chapterIndex,
            pageIndex,
            (existing) => ({
                isComplete: isComplete || (existing?.isComplete === true),
                lastViewedAt: Date.now(),
            })
        );
    },

    /**
     * Record a position without bumping `lastViewedAt`. Use for chapter
     * open/close/advance where the user has not yet demonstrated any
     * reading activity. Preserves any existing `isComplete=true`.
     */
    async updatePosition(seriesName, chapterIndex, pageIndex, section = null) {
        await this._upsert(
            _scopedName(section, seriesName),
            chapterIndex,
            pageIndex,
            (existing) => ({
                isComplete: existing?.isComplete === true,
                lastViewedAt: existing?.lastViewedAt || 0,
            })
        );
    },

    /**
     * Read-modify-write in a single `readwrite` transaction. Halves the
     * IndexedDB round-trips compared to a separate get + put — meaningful
     * on scroll-save, which fires every 500ms in strip mode.
     *
     * Calls for the same record are serialized through a per-key promise
     * chain. Without this, saveProgress() (every 500ms while scrolling) and
     * markComplete() fire concurrently in the scroll handler; their
     * get→put sequences interleave and saveProgress's put — carrying
     * isComplete from a STALE read — can land after markComplete's,
     * wiping the just-written complete flag. That was the "reaching the
     * bottom doesn't mark the chapter read" bug.
     */
    _upsert(seriesName, chapterIndex, pageIndex, merge) {
        const key = `${seriesName}|${chapterIndex}`;
        const prev = this._pendingUpserts.get(key) || Promise.resolve();
        const next = prev.then(() =>
            this._upsertNow(seriesName, chapterIndex, pageIndex, merge)
        );
        // Swallow rejections on the chain link so one failed write doesn't
        // poison subsequent calls (the original caller still gets the
        // rejection via `next`). The map holds one settled promise per
        // record ever written — bounded by chapter count, so no cleanup
        // is needed.
        this._pendingUpserts.set(key, next.catch(() => {}));
        return next;
    },

    async _upsertNow(seriesName, chapterIndex, pageIndex, merge) {
        const dbInstance = await db.open();
        return new Promise((resolve, reject) => {
            const tx = dbInstance.transaction(db.stores.READ_STATUS, 'readwrite');
            const store = tx.objectStore(db.stores.READ_STATUS);
            // Array keyPath — key is the same composite array.
            const getReq = store.get([seriesName, chapterIndex]);
            getReq.onsuccess = () => {
                const existing = getReq.result || null;
                const patch = merge(existing);
                store.put({
                    seriesName,
                    chapterIndex,
                    pageIndex,
                    isComplete: patch.isComplete,
                    lastViewedAt: patch.lastViewedAt,
                });
            };
            tx.oncomplete = () => resolve();
            tx.onerror = () => reject(tx.error);
        });
    },

    async getBySeries(seriesName, section = null) {
        const dbInstance = await db.open();
        return new Promise((resolve) => {
            const tx = dbInstance.transaction(db.stores.READ_STATUS, 'readonly');
            const store = tx.objectStore(db.stores.READ_STATUS);
            const index = store.index('seriesName');
            const req = index.getAll(_scopedName(section, seriesName));
            req.onsuccess = () => resolve(req.result || []);
            req.onerror = () => resolve([]);
        });
    },

    async getContinuePosition(seriesName, section = null) {
        const progress = await this.getBySeries(seriesName, section);
        if (!progress.length) return null;
        // Prefer the most-recent INCOMPLETE entry — once a chapter is done
        // it is no longer a "Continue Reading" candidate.
        //
        // When EVERY recorded chapter is complete (user binged to the end,
        // or finished the only chapter they opened), pointing at the
        // most-recent completed chapter would suggest re-reading it — the
        // "mark as read at the bottom doesn't work" symptom from the user's
        // perspective. Instead return the NEXT chapter after the
        // highest completed index: the natural reading order. Its page
        // starts at 0; the record doesn't exist yet, so synthesize it.
        const incomplete = progress.filter((p) => !p.isComplete);
        if (incomplete.length) {
            return incomplete.reduce((best, curr) =>
                (curr.lastViewedAt || 0) > (best.lastViewedAt || 0) ? curr : best
            );
        }
        const highestComplete = Math.max(...progress.map((p) => p.chapterIndex));
        return {
            seriesName,
            chapterIndex: highestComplete + 1,
            pageIndex: 0,
            isComplete: false,
            synthesized: true,
        };
    },

    async isChapterComplete(seriesName, chapterIndex, section = null) {
        const progress = await this.getBySeries(seriesName, section);
        const chapter = progress.find((p) => p.chapterIndex === chapterIndex);
        return chapter?.isComplete || false;
    },
};
