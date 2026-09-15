/**
 * ThumbnailService - Unified thumbnail generation and caching
 */

import { db } from '../../db/db-registry.js';
import { apiService } from '../service-registry.js';
import { resourceManager, attachImageRetry } from '../../utils/utils-registry.js';
import { GALLERY_CONFIG } from '../../core/core-registry.js';

export class ThumbnailService {
    constructor() {
        this.cache = new Map();
        this.maxSize = 200;
        this.quality = 0.5;
    }

    async loadThumbnail(fileObj, container, force = false, section = null, options = null) {
        try {
            if (apiService.isServerMode) {
                return this._loadServerThumbnail(fileObj, container, section, options);
            }
            return this._loadClientThumbnail(fileObj, container, force);
        } catch (e) {
            console.error('Thumbnail load error:', e);
            this._showErrorPlaceholder(container);
        }
    }

    async _loadServerThumbnail(fileObj, container, section = 'images', options = null) {
        // Guard: a missing/empty path would produce /api/thumbnail/undefined
        // (encodeURIComponent(undefined) === "undefined"), which the backend
        // rejects with 403. Skip the request rather than spam the server.
        if (!fileObj?.path) return;

        // For videos, ensure the ▶ badge is present regardless of which
        // branch runs below. Placing this BEFORE the existingImg early-return
        // fixes the bug where a recycled/re-rendered video container that
        // still holds an <img> would skip badge creation.
        if (fileObj.type === 'video') {
            this._showVideoBadge(container);
        }

        // Check if there's already a thumbnail image (loading or loaded)
        const existingImg = container.querySelector('img');
        if (existingImg) {
            return; // Already has a thumbnail (don't duplicate)
        }

        const img = document.createElement('img');
        img.loading = 'lazy';

        // Use simplified thumbnail URL - server uses config-based sizing.
        // For videos, the backend extracts a keyframe via ffmpeg and returns
        // a WebP thumbnail (same endpoint as images). We request it the same
        // way so videos get a real visual thumbnail instead of a static ▶.
        // Sequence context (index/total/next/prev) is forwarded as query
        // params so the backend can attach preload headers — previously the
        // options argument was accepted but silently dropped.
        let url = apiService.getThumbnailUrl(fileObj.path, section);
        if (options) {
            const params = new URLSearchParams();
            if (options.index !== undefined && options.total !== undefined) {
                params.append('index', options.index);
                params.append('total', options.total);
            }
            if (options.nextPath) params.append('nextPaths', options.nextPath);
            if (options.prevPath) params.append('prevPaths', options.prevPath);
            const qs = params.toString();
            if (qs) url += `&${qs}`;
        }
        img.src = url;
        img.style.width = '100%';
        img.style.height = '100%';
        img.style.objectFit = 'cover';

        // Thumbnail responses carry X-File-Mtime — record it so subsequent
        // getThumbnailUrl/getMediaUrl calls version their URLs with ?v=<mtime>.
        img.addEventListener('load', () => {
            // The <img> load itself can't expose headers; piggyback a cached
            // HEAD probe only when the mtime for this path is still unknown.
            if (!apiService.fileVersionParam(fileObj.path)) {
                fetch(url, { method: 'HEAD' })
                    .then(r => r.ok && apiService._maybeRecordFileMtime(url, r))
                    .catch(() => {});
            }
        }, { once: true });

        // Auto-retry transient load failures (backend briefly busy during a
        // scan, a thumbnail still being generated when the gallery first
        // paints, a dropped connection). Only fall back to the placeholder
        // once retries are exhausted, so a momentary blip doesn't leave a
        // permanent ✕ on a tile that would have loaded on the second try.
        attachImageRetry(img, img.src, {
            maxRetries: 2,
            baseDelayMs: 500,
            onGiveUp: () => {
                console.warn(`Failed to load thumbnail after retries: ${fileObj.path}`);
                img.style.display = 'none';
                // For videos, fall back to the ▶ placeholder (e.g. 0-byte files
                // where ffmpeg cannot extract a frame). For images, fall back to
                // the error placeholder.
                if (fileObj.type === 'video') {
                    this._showVideoPlaceholder(container);
                } else {
                    this._showErrorPlaceholder(container);
                }
            },
        });

        container.insertBefore(img, container.firstChild);
    }

    async _loadClientThumbnail(fileObj, container, force) {
        if (!force && fileObj.type !== 'video') {
            const cached = await this._getFromCache(fileObj.path);
            if (cached) {
                const img = document.createElement('img');
                img.src = cached;
                img.loading = 'lazy';
                container.insertBefore(img, container.firstChild);
                return;
            }
        }

        let file;
        if (fileObj.file) {
            file = fileObj.file;
        } else if (fileObj.handle) {
            file = await fileObj.handle.getFile();
        } else {
            return;
        }

        const url = resourceManager.createUrl(file);

        if (fileObj.type === 'video') {
            this._createVideoThumbnail(url, container);
        } else {
            await this._createImageThumbnail(url, fileObj.path, container);
        }
    }

    _createVideoThumbnail(url, container) {
        const vid = document.createElement('video');
        vid.muted = true;
        vid.loop = true;

        vid.addEventListener('mouseover', () => vid.play().catch(() => {}));
        vid.addEventListener('mouseout', () => {
            vid.pause();
            vid.currentTime = 0;
        });
        vid.addEventListener('error', () => {
            resourceManager.revoke(url);
        });

        vid.src = url;
        container.insertBefore(vid, container.firstChild);
    }

    async _createImageThumbnail(url, path, container) {
        const placeholder = document.createElement('div');
        placeholder.className = 'thumb-placeholder';
        placeholder.style.cssText = `
            width: 100%;
            height: 100%;
            background: rgba(255,255,255,0.05);
            display: flex;
            align-items: center;
            justify-content: center;
            color: var(--text-secondary);
            font-size: 0.8rem;
        `;
        placeholder.textContent = '...';
        container.insertBefore(placeholder, container.firstChild);

        const thumbImg = new Image();

        thumbImg.onerror = () => {
            resourceManager.revoke(url);
            placeholder.textContent = 'Error';
        };

        thumbImg.onload = () => {
            try {
                const canvas = document.createElement('canvas');
                const MAX = this.maxSize;
                let w = thumbImg.naturalWidth;
                let h = thumbImg.naturalHeight;

                if (w > 0 && h > 0) {
                    if (w > h) {
                        if (w > MAX) {
                            h *= MAX / w;
                            w = MAX;
                        }
                    } else {
                        if (h > MAX) {
                            w *= MAX / h;
                            h = MAX;
                        }
                    }

                    canvas.width = w;
                    canvas.height = h;
                    const ctx = canvas.getContext('2d');
                    ctx.drawImage(thumbImg, 0, 0, w, h);

                    const dataUrl = canvas.toDataURL('image/jpeg', this.quality);
                    this._saveToCache(path, dataUrl);

                    const finalImg = document.createElement('img');
                    finalImg.src = dataUrl;
                    finalImg.loading = 'lazy';
                    if (placeholder.isConnected) {
                        placeholder.replaceWith(finalImg);
                    }
                }
            } catch (err) {
                console.warn('Canvas compression failed:', err);
            } finally {
                resourceManager.revoke(url);
            }
        };

        thumbImg.src = url;
    }

    async _getFromCache(path) {
        try {
            return await db.get(db.stores.THUMBNAILS, path);
        } catch (e) {
            return null;
        }
    }

    async _saveToCache(path, dataUrl) {
        try {
            await db.put(db.stores.THUMBNAILS, path, dataUrl);
        } catch (e) {
            console.warn('Could not save thumbnail:', e);
        }
    }

    _showVideoPlaceholder(container) {
        // Don't add a duplicate placeholder if one already exists.
        if (container.querySelector('[data-video-placeholder]')) {
            return;
        }
        const placeholder = document.createElement('div');
        placeholder.setAttribute('data-video-placeholder', '');
        placeholder.style.cssText = `
            width: 100%;
            height: 100%;
            display: flex;
            align-items: center;
            justify-content: center;
            background: var(--bg-elevated);
            color: var(--text-secondary);
            font-size: 2rem;
        `;
        placeholder.textContent = '▶';
        container.insertBefore(placeholder, container.firstChild);
    }

    // _showVideoBadge overlays a small ▶ in the corner of a video's
    // generated thumbnail frame, so users can tell at a glance that the
    // gallery item is a playable video (not just another image). The badge
    // is absolutely positioned and pointer-events:none so it never blocks
    // clicks on the underlying gallery item.
    _showVideoBadge(container) {
        // Don't add a duplicate badge if one already exists.
        if (container.querySelector('[data-video-badge]')) {
            return;
        }
        const badge = document.createElement('div');
        badge.setAttribute('data-video-badge', '');
        badge.style.cssText = `
            position: absolute;
            bottom: 0.3rem;
            right: 0.3rem;
            width: 1.4rem;
            height: 1.4rem;
            display: flex;
            align-items: center;
            justify-content: center;
            background: rgba(0, 0, 0, 0.6);
            color: #fff;
            border-radius: 50%;
            font-size: 0.7rem;
            pointer-events: none;
            z-index: 2;
        `;
        badge.textContent = '▶';
        container.appendChild(badge);
    }

    _showErrorPlaceholder(container) {
        // Don't add multiple error placeholders
        if (container.querySelector('[data-error-placeholder]')) {
            return;
        }
        const errorDiv = document.createElement('div');
        errorDiv.style.cssText = `
            width: 100%;
            height: 100%;
            display: flex;
            align-items: center;
            justify-content: center;
            background: var(--bg-elevated);
            color: var(--text-secondary);
        `;
        errorDiv.textContent = '✕';
        errorDiv.dataset.errorPlaceholder = 'true';
        container.insertBefore(errorDiv, container.firstChild);
    }
}
