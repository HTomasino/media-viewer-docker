/**
 * Tag Management App - ES6 Modular Version
 * Enhanced with tag browser, batch operations, thumbnail management
 */

import { apiService, unifiedScanner } from './services/service-registry.js';
import { ApiTimeoutError } from './services/api/index.js';
import { db, cache, handles } from './db/db-registry.js';
import { configState } from './core/core-registry.js';
import { SECTIONS } from './core/core-registry.js';
import { escapeHtml, sanitizeTag, showNotification, showError, getChapterCount } from './utils/utils-registry.js';

class TagManagementApp {
    constructor() {
        this.allImageFiles = [];
        this.allMangaDirs = [];
        this.allDirFormats = new Map();
        this.isServerMode = false;
        this.currentView = 'files';
        this.selectedFiles = new Set();
        this.tagStats = [];
        this.autocompleteIndex = -1;
        this.thumbnailGenerationProgress = { current: 0, total: 0 };
        this._gotifyPriorityDebounce = null;
        this._discordConfigDebounce = null;
        // Tags selected in the Tag Browser view for "Flush Selected Tags"
        this.selectedFlushTags = new Set();
    }

    async init() {
        console.log('[TAG-MGMT] Initializing...');

        await configState.init();
        this.isServerMode = configState.isServerMode();

        this.setupEventListeners();

        // Render the static sections (exclusions, directory names) right away
        // so the page is interactive even before any server data arrives.
        this.renderExcludedTags();
        this.loadDirectoryNames();

        if (!this.isServerMode) {
            // Local/IndexedDB mode: load everything, no busy-backend concern.
            await this.loadData();
            const gotifyPanel = document.getElementById('gotify-settings-panel');
            if (gotifyPanel) gotifyPanel.classList.add('hidden');
            const discordPanel = document.getElementById('discord-settings-panel');
            if (discordPanel) discordPanel.classList.add('hidden');
            console.log('[TAG-MGMT] Initialization complete (local mode)');
            return;
        }

        // Server mode: show per-view loading placeholders, then fire ALL server
        // loads in parallel. Previously init awaited each load sequentially
        // (getFiles ? 2x getFolders ? getTagStats ? getThumbnailStatus ?
        // getGotifyStatus), so a single blocked read froze the whole page ?
        // and because the backend's DB reads take the RWMutex read lock that
        // a concurrent scan starves (Go's RWMutex is write-preferring), the
        // page would hang for the entire scan duration. allSettled + short
        // per-request timeouts means one slow endpoint can't block the others,
        // and each view shows its own loading/error state instead of a
        // frozen page. Each task is self-contained: it renders its own view
        // on success and shows a lightweight "backend busy" placeholder on
        // failure, without throwing up to the top-level handler.
        this.renderFilesView();
        this.renderDirsView();
        this.renderTagStats();
        this.renderThumbnailStatus(null);
        this.renderGotifyStatus({ running: false, enabled: false });
        this.renderDiscordStatus({ enabled: false, ready: false });
        this.markLoading('files-list', 'Loading files...');
        this.markLoading('dirs-list', 'Loading directories...');
        this.markLoading('tag-cloud', 'Loading tags...');
        this.markLoading('thumbnail-status', 'Loading thumbnail status...');
        this.markLoading('gotify-status', 'Loading Gotify status...');
        this.markLoading('discord-status', 'Loading Discord status...');

        const results = await Promise.allSettled([
            this.loadFromServer(),
            this.loadTagStats(),
            this.loadThumbnailStatus(),
            this.loadGotifyStatus(),
            this.loadDiscordStatus(),
        ]);

        // Surface a single summary notification if any load failed, but do
        // NOT block the UI ? each view already shows its own state.
        const rejected = results.filter(r => r.status === 'rejected');
        if (rejected.length > 0) {
            console.warn('[TAG-MGMT] Some loads failed:', rejected);
            showNotification(
                `Backend is busy ? ${rejected.length} of ${results.length} sections couldn't load. They'll retry when you switch tabs.`,
                'warning',
                6000
            );
        }

        console.log('[TAG-MGMT] Initialization complete');
    }

    // markLoading writes a lightweight placeholder into a container while its
    // data is being fetched, so the page never shows a blank/frozen region.
    markLoading(id, message) {
        const el = document.getElementById(id);
        if (!el) return;
        el.innerHTML = `<p class="text-secondary loading-placeholder">${escapeHtml(message)}</p>`;
    }

    setupEventListeners() {
        // View tabs
        document.getElementById('view-files-btn')?.addEventListener('click', (e) => {
            this.switchView('files', e.currentTarget);
        });

        document.getElementById('view-dirs-btn')?.addEventListener('click', (e) => {
            this.switchView('dirs', e.currentTarget);
        });

        document.getElementById('view-tags-btn')?.addEventListener('click', (e) => {
            this.switchView('tags', e.currentTarget);
        });

        document.getElementById('view-thumbnails-btn')?.addEventListener('click', (e) => {
            this.switchView('thumbnails', e.currentTarget);
        });

        // Global exclusions
        document.getElementById('btn-add-exclusion')?.addEventListener('click', () => {
            this.addGlobalExclusion();
        });

        document.getElementById('global-exclude-input')?.addEventListener('keydown', (e) => {
            if (e.key === 'Enter') this.addGlobalExclusion();
        });

        // Directory config
        document.querySelectorAll('.change-dir-btn').forEach(btn => {
            btn.addEventListener('click', () => this.changeSectionDirectory(btn.dataset.section));
        });

        document.querySelectorAll('.clear-dir-btn').forEach(btn => {
            btn.addEventListener('click', () => this.clearSectionDirectory(btn.dataset.section));
        });

        // File selection
        document.getElementById('btn-select-all')?.addEventListener('click', () => this.selectAllFiles());
        document.getElementById('btn-select-none')?.addEventListener('click', () => this.selectNone());

        // Batch operations
        document.getElementById('batch-add-tags')?.addEventListener('click', () => this.batchAddTags());
        document.getElementById('batch-remove-tags')?.addEventListener('click', () => this.batchRemoveTags());
        document.getElementById('batch-clear-tags')?.addEventListener('click', () => this.batchClearTags());

        // Tag browser
        document.getElementById('tag-search')?.addEventListener('input', (e) => {
            this.filterTagCloud(e.target.value);
        });

        document.getElementById('tag-sort')?.addEventListener('change', (e) => {
            this.sortTagCloud(e.target.value);
        });

        // Back to Gallery ? uses history.back() when there's a referrer to
        // preserve gallery scroll/state, otherwise navigates to the gallery
        // root with a cache-bust query so the service worker revalidates
        // instead of serving a stale shell. A plain <a href="/"> left users
        // on a frozen gallery (the SW served a cached index.html and the app
        // re-init stalled on slow API calls); this handler forces a fresh load.
        document.getElementById('btn-back-to-gallery')?.addEventListener('click', () => {
            this.backToGallery();
        });

        // Flush tags
        document.getElementById('btn-flush-all-tags')?.addEventListener('click', () => {
            this.flushAllTags();
        });
        document.getElementById('btn-flush-selected-tags')?.addEventListener('click', () => {
            this.flushSelectedTags();
        });

        // Thumbnails
        document.getElementById('btn-generate-missing')?.addEventListener('click', () => {
            this.generateMissingThumbnails();
        });

        document.getElementById('btn-cull-orphans')?.addEventListener('click', () => {
            this.cullOrphanedThumbnails();
        });

        // Gotify
        document.getElementById('btn-gotify-toggle')?.addEventListener('click', () => {
            this.toggleGotify();
        });

        document.getElementById('btn-gotify-reset')?.addEventListener('click', () => {
            this.resetGotify();
        });

        document.getElementById('btn-gotify-push-pending')?.addEventListener('click', () => {
            this.pushPendingNotifications();
        });

        document.getElementById('gotify-priority')?.addEventListener('input', (e) => {
            const value = parseInt(e.target.value, 10);
            document.getElementById('gotify-priority-value').textContent = value;
            clearTimeout(this._gotifyPriorityDebounce);
            this._gotifyPriorityDebounce = setTimeout(() => this.updateGotifyPriority(value), 500);
        });

        // Discord
        document.getElementById('btn-discord-toggle')?.addEventListener('click', () => {
            this.toggleDiscord();
        });

        document.getElementById('btn-discord-test')?.addEventListener('click', () => {
            this.testDiscord();
        });

        document.getElementById('btn-discord-push-pending')?.addEventListener('click', () => {
            this.pushDiscordPending();
        });

        document.getElementById('btn-discord-save')?.addEventListener('click', () => {
            this.saveDiscordConfig();
        });

        const discordInputs = ['discord-bot-token', 'discord-recipient-id', 'discord-cooldown'];
        discordInputs.forEach(id => {
            document.getElementById(id)?.addEventListener('input', () => {
                clearTimeout(this._discordConfigDebounce);
                this._discordConfigDebounce = setTimeout(() => this.saveDiscordConfig(), 800);
            });
        });
    }

    async loadData() {
        try {
            if (this.isServerMode) {
                await this.loadFromServer();
            } else {
                await this.loadFromIndexedDB();
            }
        } catch (e) {
            console.error('[TAG-MGMT] Failed to load data:', e);
            showError('Tag Management Init', e);
        }
    }

    // fetchWithRetry wraps an async operation with a bounded retry + backoff.
    // Use it for the lightweight read endpoints so a transient block (e.g.
    // the tail end of a directory scan holding the backend's write lock)
    // doesn't freeze the UI for the full default 15s timeout. Each attempt
    // uses a short timeout so we give up fast and retry instead of hanging.
    //
    // IMPORTANT: retries are limited to TRANSPORT/timeout failures only
    // (ApiTimeoutError or a network TypeError from fetch). HTTP error
    // responses (4xx/5xx, including 429 RateLimited and 503 "Server busy")
    // are NOT retried ? those are explicit backpressure signals from the
    // backend, and retrying them would defeat the rate/connection limiters
    // and amplify load during the exact condition they protect against.
    // The `shouldRetry` predicate lets a caller override this if needed.
    async fetchWithRetry(fn, { attempts = 2, timeoutMs = 4000, backoffMs = 500, shouldRetry } = {}) {
        const defaultShouldRetry = (err) => {
            // ApiTimeoutError: our own AbortController timeout ? retryable.
            if (err instanceof ApiTimeoutError || err?.isTimeout) return true;
            // fetch() throws a TypeError on network failure / connection
            // refused / aborted (server not reachable, still binding, etc.).
            // HTTP error responses are NOT thrown here ? they resolve and
            // are turned into Error() by the api methods, which we don't
            // classify as retryable below.
            if (err instanceof TypeError) return true;
            return false;
        };
        const retry = shouldRetry || defaultShouldRetry;
        let lastErr;
        for (let i = 0; i < attempts; i++) {
            try {
                return await fn(timeoutMs);
            } catch (e) {
                lastErr = e;
                // Only retry if the error is transport/timeout AND we have
                // attempts left; otherwise bail with the last error.
                if (i < attempts - 1 && retry(e)) {
                    await new Promise(r => setTimeout(r, backoffMs));
                    continue;
                }
                break;
            }
        }
        throw lastErr;
    }

    async loadFromServer() {
        try {
            // Fire files + both folder sections in parallel. Each is an
            // independent read; awaiting them serially only amplified the
            // hang when the backend was busy. Clear allMangaDirs first so
            // repeated calls (refresh / post-flush reload) don't accumulate
            // duplicates.
            const [files, mangaFolders, hmangaFolders] = await Promise.all([
                this.fetchWithRetry(() => apiService.getFiles('images'), { timeoutMs: 4000 }),
                this.fetchWithRetry(() => apiService.getFolders(SECTIONS.MANGA), { timeoutMs: 4000 }),
                this.fetchWithRetry(() => apiService.getFolders(SECTIONS.H_MANGA), { timeoutMs: 4000 }),
            ]);

            this.allImageFiles = Array.isArray(files) ? files : [];
            this.allMangaDirs = [];
            if (Array.isArray(mangaFolders)) this.allMangaDirs.push(...mangaFolders);
            if (Array.isArray(hmangaFolders)) this.allMangaDirs.push(...hmangaFolders);

            this.renderFilesView();
            this.renderDirsView();
        } catch (e) {
            console.error('[TAG-MGMT] Server load error:', e);
            // Show an explicit error state instead of leaving the loading
            // placeholder forever. Users can switch tabs to retry.
            const filesList = document.getElementById('files-list');
            if (filesList) {
                filesList.innerHTML = '<p class="text-secondary">Couldn\'t load files ? the backend may be busy scanning. Try switching to another tab and back.</p>';
            }
            const dirsList = document.getElementById('dirs-list');
            if (dirsList) {
                dirsList.innerHTML = '<p class="text-secondary">Couldn\'t load directories ? the backend may be busy scanning. Try switching to another tab and back.</p>';
            }
            // Re-throw so Promise.allSettled in init() records the rejection
            // for the summary notification; each view already shows its state.
            throw e;
        }
    }

    async loadFromIndexedDB() {
        try {
            const dbInstance = await db.open();

            const filesData = await db.getAll(db.stores.FILES);
            this.allImageFiles = filesData || [];

            // Clear before appending so repeated calls don't accumulate
            // duplicates (mirrors the loadFromServer fix).
            this.allMangaDirs = [];
            for (const section of [SECTIONS.MANGA, SECTIONS.H_MANGA]) {
                const cached = await cache.load(section);
                if (cached) {
                    this.allMangaDirs.push(...cached);
                }
            }

            this.renderFilesView();
            this.renderDirsView();
        } catch (e) {
            console.error('[TAG-MGMT] IndexedDB load error:', e);
        }
    }

    async loadTagStats() {
        if (!this.isServerMode) return;

        try {
            this.tagStats = await this.fetchWithRetry(
                (t) => apiService.getTagStats(),
                { attempts: 2, timeoutMs: 5000, backoffMs: 400 }
            );
            this.renderTagStats();
            this.renderTagCloud();
        } catch (e) {
            console.error('[TAG-MGMT] Failed to load tag stats:', e);
            this.tagStats = [];
            this.renderTagStats();
            this.renderTagCloud();
            const cloud = document.getElementById('tag-cloud');
            if (cloud) {
                cloud.innerHTML = '<p class="text-secondary">Couldn\'t load tags ? the backend may be busy. Switch away and back to retry.</p>';
            }
        }
    }

    async loadThumbnailStatus() {
        if (!this.isServerMode) return;

        try {
            const status = await this.fetchWithRetry(
                (t) => apiService.getThumbnailStatus('images'),
                { attempts: 2, timeoutMs: 5000, backoffMs: 400 }
            );
            this.renderThumbnailStatus(status);
        } catch (e) {
            console.error('[TAG-MGMT] Failed to load thumbnail status:', e);
            const container = document.getElementById('thumbnail-status');
            if (container) {
                container.innerHTML = '<p class="text-secondary">Couldn\'t load thumbnail status ? the backend may be busy.</p>';
            }
        }
    }

    switchView(view, btn) {
        this.currentView = view;

        document.querySelectorAll('.view-btn').forEach(b => b.classList.remove('active'));
        btn?.classList.add('active');

        // Hide all views
        document.getElementById('files-view').style.display = 'none';
        document.getElementById('dirs-view').style.display = 'none';
        document.getElementById('tag-browser-view')?.classList.add('hidden');
        document.getElementById('thumbnails-view')?.classList.add('hidden');

        // Show selected view
        switch (view) {
            case 'files':
                document.getElementById('files-view').style.display = 'block';
                this.renderFilesView();
                break;
            case 'dirs':
                document.getElementById('dirs-view').style.display = 'block';
                this.renderDirsView();
                break;
            case 'tags':
                document.getElementById('tag-browser-view')?.classList.remove('hidden');
                break;
            case 'thumbnails':
                document.getElementById('thumbnails-view')?.classList.remove('hidden');
                this.loadThumbnailStatus();
                break;
        }
    }

    // File Management
    renderFilesView() {
        const container = document.getElementById('files-list');
        if (!container) return;

        container.innerHTML = '';

        if (!this.allImageFiles.length) {
            container.innerHTML = '<p>No files found. Scan a directory first.</p>';
            return;
        }

        const table = document.createElement('table');
        table.className = 'data-table';

        table.innerHTML = `
            <thead>
                <tr>
                    <th class="checkbox-cell"><input type="checkbox" id="select-all-header"></th>
                    <th>Name</th>
                    <th>Path</th>
                    <th>Tags</th>
                    <th>Actions</th>
                </tr>
            </thead>
        `;

        const tbody = document.createElement('tbody');

        this.allImageFiles.forEach((file, index) => {
            const row = document.createElement('tr');
            row.dataset.path = file.path;
            row.dataset.index = index;

            const isSelected = this.selectedFiles.has(file.path);
            if (isSelected) row.classList.add('selected');

            row.innerHTML = `
                <td class="checkbox-cell"><input type="checkbox" ${isSelected ? 'checked' : ''}></td>
                <td>${escapeHtml(file.name)}</td>
                <td>${escapeHtml(file.path)}</td>
                <td class="tags-cell">${this.renderTagsCell(file.tags || [], file.path)}</td>
                <td>
                    <button class="btn-icon edit-tags-btn" data-path="${escapeHtml(file.path)}" title="Edit tags">
                        <svg viewBox="0 0 24 24" width="16" height="16"><path fill="currentColor" d="M20.71 7.04c.39-.39.39-1.04 0-1.41l-2.34-2.34c-.37-.39-1.02-.39-1.41 0l-1.84 1.83 3.75 3.75M3 17.25V21h3.75L17.81 9.93l-3.75-3.75L3 17.25z"/></svg>
                    </button>
                    <button class="btn-icon generate-thumb-btn" data-path="${escapeHtml(file.path)}" title="Generate thumbnail">
                        <svg viewBox="0 0 24 24" width="16" height="16"><path fill="currentColor" d="M5 3C3.89 3 3 3.89 3 5V19C3 20.1 3.9 21 5 21H19C20.1 21 21 20.1 21 19V5C21 3.9 20.1 3 19 3M5 19V5H19V19M14.5 7L11 13L9 11L7 14H17"/></svg>
                    </button>
                </td>
            `;

            tbody.appendChild(row);
        });

        table.appendChild(tbody);
        container.appendChild(table);

        // Event listeners
        tbody.querySelectorAll('tr').forEach(row => {
            const checkbox = row.querySelector('input[type="checkbox"]');
            checkbox.addEventListener('change', () => {
                const path = row.dataset.path;
                if (checkbox.checked) {
                    this.selectedFiles.add(path);
                    row.classList.add('selected');
                } else {
                    this.selectedFiles.delete(path);
                    row.classList.remove('selected');
                }
                this.updateBatchBar();
            });
        });

        tbody.querySelectorAll('.edit-tags-btn').forEach(btn => {
            btn.addEventListener('click', (e) => {
                const path = e.currentTarget.closest('tr').dataset.path;
                this.editFileTagsInline(path);
            });
        });

        tbody.querySelectorAll('.generate-thumb-btn').forEach(btn => {
            btn.addEventListener('click', async (e) => {
                const path = e.currentTarget.dataset.path;
                await this.generateThumbnail(path);
            });
        });

        // Header checkbox
        const headerCheckbox = table.querySelector('#select-all-header');
        headerCheckbox.addEventListener('change', () => {
            const checkboxes = tbody.querySelectorAll('input[type="checkbox"]');
            checkboxes.forEach(cb => {
                cb.checked = headerCheckbox.checked;
                const row = cb.closest('tr');
                const path = row.dataset.path;
                if (headerCheckbox.checked) {
                    this.selectedFiles.add(path);
                    row.classList.add('selected');
                } else {
                    this.selectedFiles.delete(path);
                    row.classList.remove('selected');
                }
            });
            this.updateBatchBar();
        });
    }

    renderTagsCell(tags, filePath) {
        if (!tags || tags.length === 0) {
            return '<span class="text-secondary">No tags</span>';
        }
        return tags.map(t => `<span class="tag" data-tag="${escapeHtml(t)}">${escapeHtml(t)}</span>`).join(' ');
    }

    async editFileTagsInline(filePath) {
        const file = this.allImageFiles.find(f => f.path === filePath);
        if (!file) return;

        const cell = document.querySelector(`tr[data-path="${CSS.escape(filePath)}"] .tags-cell`);
        if (!cell) return;

        const editor = document.createElement('div');
        editor.className = 'tag-editor';

        const tags = file.tags || [];
        editor.innerHTML = tags.map(t => `
            <span class="tag" data-tag="${escapeHtml(t)}">
                ${escapeHtml(t)}
                <span class="remove" data-tag="${escapeHtml(t)}">×</span>
            </span>
        `).join('') + '<input type="text" placeholder="Add tag...">';

        cell.innerHTML = '';
        cell.appendChild(editor);

        const input = editor.querySelector('input');
        input.focus();

        // Autocomplete
        const allTags = this.getAllUniqueTags();
        let autocompleteEl = null;

        const showAutocomplete = (value) => {
            if (autocompleteEl) {
                autocompleteEl.remove();
                autocompleteEl = null;
            }

            if (!value) return;

            const matches = allTags.filter(t => t.includes(value.toLowerCase()) && !tags.includes(t)).slice(0, 10);
            if (matches.length === 0) return;

            autocompleteEl = document.createElement('div');
            autocompleteEl.className = 'tag-autocomplete';
            autocompleteEl.innerHTML = matches.map(t => `<div class="tag-autocomplete-item" data-tag="${escapeHtml(t)}">${escapeHtml(t)}</div>`).join('');

            const rect = input.getBoundingClientRect();
            autocompleteEl.style.position = 'fixed';
            autocompleteEl.style.left = rect.left + 'px';
            autocompleteEl.style.top = (rect.bottom + 2) + 'px';
            autocompleteEl.style.width = rect.width + 'px';

            document.body.appendChild(autocompleteEl);

            autocompleteEl.querySelectorAll('.tag-autocomplete-item').forEach(item => {
                item.addEventListener('click', () => {
                    this.addTagToEditor(editor, item.dataset.tag);
                    input.value = '';
                    autocompleteEl.remove();
                    autocompleteEl = null;
                });
            });
        };

        input.addEventListener('input', (e) => {
            showAutocomplete(e.target.value.toLowerCase());
        });

        input.addEventListener('keydown', async (e) => {
            if (e.key === 'Enter') {
                e.preventDefault();
                const tag = sanitizeTag(input.value);
                if (tag) {
                    await this.addTagToFile(filePath, tag);
                    this.addTagToEditor(editor, tag);
                    input.value = '';
                }
            } else if (e.key === 'Escape') {
                if (autocompleteEl) {
                    autocompleteEl.remove();
                    autocompleteEl = null;
                } else {
                    this.renderFilesView();
                }
            }
        });

        // Remove tags
        editor.querySelectorAll('.tag .remove').forEach(btn => {
            btn.addEventListener('click', async () => {
                const tag = btn.dataset.tag;
                await this.removeTagFromFile(filePath, tag);
                btn.parentElement.remove();
            });
        });
    }

    addTagToEditor(editor, tag) {
        const existingTags = Array.from(editor.querySelectorAll('.tag')).map(t => t.dataset.tag);
        if (existingTags.includes(tag)) return;

        const tagSpan = document.createElement('span');
        tagSpan.className = 'tag';
        tagSpan.dataset.tag = tag;
        tagSpan.innerHTML = `${escapeHtml(tag)}<span class="remove" data-tag="${escapeHtml(tag)}">×</span>`;

        tagSpan.querySelector('.remove').addEventListener('click', async () => {
            const path = editor.closest('tr').dataset.path;
            await this.removeTagFromFile(path, tag);
            tagSpan.remove();
        });

        editor.insertBefore(tagSpan, editor.querySelector('input'));
    }

    async addTagToFile(filePath, tag) {
        tag = sanitizeTag(tag);
        if (!tag) return;

        const file = this.allImageFiles.find(f => f.path === filePath);
        if (!file) return;

        if (!file.tags) file.tags = [];
        if (file.tags.includes(tag)) return;

        file.tags.push(tag);

        if (this.isServerMode) {
            try {
                await apiService.updateFileTags(filePath, file.tags);
            } catch (e) {
                console.error('Failed to update tags:', e);
            }
        } else {
            await db.put(db.stores.FILES, filePath, file);
        }
    }

    async removeTagFromFile(filePath, tag) {
        const file = this.allImageFiles.find(f => f.path === filePath);
        if (!file || !file.tags) return;

        file.tags = file.tags.filter(t => t !== tag);

        if (this.isServerMode) {
            try {
                await apiService.updateFileTags(filePath, file.tags);
            } catch (e) {
                console.error('Failed to update tags:', e);
            }
        } else {
            await db.put(db.stores.FILES, filePath, file);
        }
    }

    // Batch Operations
    selectAllFiles() {
        this.allImageFiles.forEach(f => this.selectedFiles.add(f.path));
        this.renderFilesView();
        this.updateBatchBar();
    }

    selectNone() {
        this.selectedFiles.clear();
        this.renderFilesView();
        this.updateBatchBar();
    }

    updateBatchBar() {
        const bar = document.getElementById('batch-bar');
        const count = document.getElementById('selected-count');

        if (this.selectedFiles.size === 0) {
            bar.classList.add('hidden');
        } else {
            bar.classList.remove('hidden');
            count.textContent = this.selectedFiles.size;
        }
    }

    async batchAddTags() {
        const tags = prompt('Enter tags to add (space-separated):');
        if (!tags) return;

        const newTags = tags.split(/\s+/).filter(t => t).map(sanitizeTag);

        if (this.isServerMode) {
            try {
                await apiService.bulkUpdateTags(Array.from(this.selectedFiles), { add: newTags });
                showNotification(`Added tags to ${this.selectedFiles.size} files`, 'success');
            } catch (e) {
                showError('Batch Add Tags', e);
            }
        }

        // Update local state
        for (const path of this.selectedFiles) {
            const file = this.allImageFiles.find(f => f.path === path);
            if (file) {
                if (!file.tags) file.tags = [];
                newTags.forEach(tag => {
                    if (!file.tags.includes(tag)) file.tags.push(tag);
                });
            }
        }

        this.selectedFiles.clear();
        this.updateBatchBar();
        this.renderFilesView();
    }

    async batchRemoveTags() {
        const tags = prompt('Enter tags to remove (space-separated):');
        if (!tags) return;

        const tagsToRemove = tags.split(/\s+/).filter(t => t).map(t => t.toLowerCase());

        if (this.isServerMode) {
            try {
                await apiService.bulkUpdateTags(Array.from(this.selectedFiles), { remove: tagsToRemove });
                showNotification(`Removed tags from ${this.selectedFiles.size} files`, 'success');
            } catch (e) {
                showError('Batch Remove Tags', e);
            }
        }

        // Update local state
        for (const path of this.selectedFiles) {
            const file = this.allImageFiles.find(f => f.path === path);
            if (file && file.tags) {
                file.tags = file.tags.filter(t => !tagsToRemove.includes(t.toLowerCase()));
            }
        }

        this.selectedFiles.clear();
        this.updateBatchBar();
        this.renderFilesView();
    }

    async batchClearTags() {
        if (!confirm(`Clear all tags from ${this.selectedFiles.size} files?`)) return;

        if (this.isServerMode) {
            try {
                await apiService.bulkUpdateTags(Array.from(this.selectedFiles), { set: [] });
                showNotification(`Cleared tags from ${this.selectedFiles.size} files`, 'success');
            } catch (e) {
                showError('Batch Clear Tags', e);
            }
        }

        // Update local state
        for (const path of this.selectedFiles) {
            const file = this.allImageFiles.find(f => f.path === path);
            if (file) file.tags = [];
        }

        this.selectedFiles.clear();
        this.updateBatchBar();
        this.renderFilesView();
    }

    // Directory Management
    renderDirsView() {
        const container = document.getElementById('dirs-list');
        if (!container) return;

        container.innerHTML = '';

        if (!this.allMangaDirs.length) {
            container.innerHTML = '<p>No directories found. Scan a directory first.</p>';
            return;
        }

        this.allMangaDirs.forEach(dir => {
            const div = document.createElement('div');
            div.className = 'dir-item';
            // Manga dirs are expected to be numbered (skip named extras);
            // H-Manga dirs commonly have free-form book names that should
            // each count as one entry. Default to manga when section is
            // missing ? matches the section default elsewhere in this file.
            const count = getChapterCount(dir.chapters, {
                skipNonNumeric: dir.section !== 'h-manga'
            });
            div.innerHTML = `
                <div>
                    <div class="dir-name">${escapeHtml(dir.name)}</div>
                    <div class="dir-info">${count} chapters</div>
                </div>
                <div class="dir-actions">
                    <button class="btn-secondary btn-small rescan-btn" data-dir="${escapeHtml(dir.name)}">
                        Rescan
                    </button>
                    <button class="btn-secondary btn-small quick-rescan-btn" data-dir="${escapeHtml(dir.name)}">
                        Quick Scan
                    </button>
                </div>
            `;
            container.appendChild(div);
        });

        container.querySelectorAll('.rescan-btn').forEach(btn => {
            btn.addEventListener('click', () => this.rescanDirectory(btn.dataset.dir, false));
        });

        container.querySelectorAll('.quick-rescan-btn').forEach(btn => {
            btn.addEventListener('click', () => this.rescanDirectory(btn.dataset.dir, true));
        });
    }

    async rescanDirectory(folder, quick) {
        if (!this.isServerMode) {
            showNotification('Rescan only available in server mode', 'warning');
            return;
        }

        try {
            // Find section for this folder
            let section = 'manga';
            for (const dir of this.allMangaDirs) {
                if (dir.name === folder) {
                    section = dir.section || 'manga';
                    break;
                }
            }

            showNotification(`${quick ? 'Quick' : 'Full'} rescan started for ${folder}...`, 'info');
            await apiService.rescanDirectory(section, folder, { quick });

            // Poll for completion (simplified)
            setTimeout(async () => {
                await this.loadFromServer();
                showNotification(`Rescan complete for ${folder}`, 'success');
            }, 2000);
        } catch (e) {
            showError('Rescan Directory', e);
        }
    }

    // Tag Browser
    renderTagStats() {
        const grid = document.getElementById('tag-stats-grid');
        if (!grid) return;

        const totalTags = this.tagStats.length;
        const totalFiles = this.allImageFiles.length;
        const avgTagsPerFile = totalFiles > 0
            ? (this.tagStats.reduce((sum, s) => sum + s.count, 0) / totalFiles).toFixed(1)
            : 0;

        // Tags with only 1 file
        const orphanedTags = this.tagStats.filter(s => s.count === 1).length;

        grid.innerHTML = `
            <div class="stat-card">
                <div class="number">${totalTags}</div>
                <div class="label">Total Tags</div>
            </div>
            <div class="stat-card">
                <div class="number">${totalFiles}</div>
                <div class="label">Total Files</div>
            </div>
            <div class="stat-card">
                <div class="number">${avgTagsPerFile}</div>
                <div class="label">Avg Tags/File</div>
            </div>
            <div class="stat-card">
                <div class="number">${orphanedTags}</div>
                <div class="label">Orphaned Tags</div>
            </div>
        `;
    }

    renderTagCloud() {
        const container = document.getElementById('tag-cloud');
        if (!container) return;

        container.innerHTML = '';

        if (!this.tagStats.length) {
            container.innerHTML = '<p class="text-secondary">No tags found</p>';
            this.updateFlushSelection();
            return;
        }

        const maxCount = Math.max(...this.tagStats.map(s => s.count), 1);

        this.tagStats.forEach(stat => {
            const btn = document.createElement('button');
            btn.className = 'tag-cloud-item';
            // Preserve selection state across re-renders (tagStats tags are
            // already lowercased from the backend, matching selectedFlushTags).
            if (this.selectedFlushTags.has(stat.tag)) {
                btn.classList.add('selected');
            }

            // Calculate font size based on count (0.75rem to 1.5rem)
            const size = 0.75 + (stat.count / maxCount) * 0.75;
            btn.style.setProperty('--tag-size', `${size}rem`);

            // The tag name + count label, plus a small ? button that removes
            // the tag from every file AND adds it to global exclusions (so it
            // won't be re-extracted on future scans). The ? uses
            // stopPropagation so clicking it doesn't toggle the flush selection.
            btn.innerHTML = `${escapeHtml(stat.tag)} <span class="count">(${stat.count})</span>` +
                `<button class="tag-exclude-btn" data-tag="${escapeHtml(stat.tag)}" title="Remove this tag from all files and add to exclusions">?</button>`;
            btn.title = `${stat.tag} - ${stat.count} files (click to select for flush)`;

            btn.addEventListener('click', () => {
                if (this.selectedFlushTags.has(stat.tag)) {
                    this.selectedFlushTags.delete(stat.tag);
                    btn.classList.remove('selected');
                } else {
                    this.selectedFlushTags.add(stat.tag);
                    btn.classList.add('selected');
                }
                this.updateFlushSelection();
            });

            // Wire the per-tag ? button: flush the tag from all files and
            // add it to the global exclusion list in one action.
            const excludeBtn = btn.querySelector('.tag-exclude-btn');
            excludeBtn.addEventListener('click', async (e) => {
                e.stopPropagation();
                await this.removeAndExcludeTag(stat.tag);
            });

            container.appendChild(btn);
        });

        this.updateFlushSelection();
    }

    filterTagCloud(query) {
        const items = document.querySelectorAll('.tag-cloud-item');
        items.forEach(item => {
            const tagName = item.textContent.split(' ')[0].toLowerCase();
            item.style.display = tagName.includes(query.toLowerCase()) ? '' : 'none';
        });
    }

    sortTagCloud(sortMode) {
        const container = document.getElementById('tag-cloud');
        const items = Array.from(container.querySelectorAll('.tag-cloud-item'));

        items.sort((a, b) => {
            const aName = a.textContent.split(' ')[0];
            const bName = b.textContent.split(' ')[0];
            const aCount = parseInt(a.querySelector('.count').textContent.slice(1, -1));
            const bCount = parseInt(b.querySelector('.count').textContent.slice(1, -1));

            switch (sortMode) {
                case 'count-desc': return bCount - aCount;
                case 'count-asc': return aCount - bCount;
                case 'name-asc': return aName.localeCompare(bName);
                case 'name-desc': return bName.localeCompare(aName);
                default: return 0;
            }
        });

        items.forEach(item => container.appendChild(item));
    }

    // Back to Gallery ? prefer history.back() to preserve gallery scroll/state,
    // otherwise navigate to the gallery root with a cache-bust query so the
    // service worker revalidates instead of serving a stale shell.
    backToGallery() {
        if (window.history.length > 1 && document.referrer &&
            new URL(document.referrer).origin === window.location.origin) {
            window.history.back();
            return;
        }
        // Fallback: hard navigate with cache-bust. The ?back=1 param forces
        // the SW to treat this as a fresh request (different URL) and fetch
        // from network, avoiding a stale cached index.html.
        window.location.href = '/?back=1';
    }

    // Update the "selected tags" info text and the Flush Selected button state.
    updateFlushSelection() {
        const info = document.getElementById('flush-selection-info');
        const flushSelectedBtn = document.getElementById('btn-flush-selected-tags');
        const count = this.selectedFlushTags.size;
        if (info) {
            if (count > 0) {
                info.classList.remove('hidden');
                info.textContent = `${count} tag${count !== 1 ? 's' : ''} selected for flush`;
            } else {
                info.classList.add('hidden');
            }
        }
        if (flushSelectedBtn) {
            flushSelectedBtn.disabled = count === 0;
        }
    }

    // Shared flush flow: runs the given async flush operation, shows a
    // result notification, clears any tag-cloud selection, and refreshes
    // tag stats + the file list. Used by both flushAllTags and
    // flushSelectedTags so the result-parsing/refresh/error path lives in
    // one place (dedupes the per-mode duplication flagged in review).
    async performFlush(operation, successMessage) {
        try {
            const result = await operation();
            const updated = result.updated ?? 0;
            const removed = result.removed_tag_instances ?? 0;
            showNotification(`${successMessage}: ${removed} instance${removed !== 1 ? 's' : ''} removed from ${updated} file${updated !== 1 ? 's' : ''}.`, 'success');
            // Clear selection and refresh tag stats + local file state.
            this.selectedFlushTags.clear();
            await this.loadTagStats();
            await this.loadFromServer();
        } catch (e) {
            showError('Flush Tags', e);
        }
    }

    // Remove a single tag from every file AND add it to the global tag
    // exclusions list, so it won't be re-extracted on future scans. This is
    // the action behind the per-tag ? button in the Tag Browser cloud.
    // Routes the flush+notify+refresh+error flow through performFlush (the
    // shared helper) so it stays unified with flushAllTags/flushSelectedTags;
    // the exclusion step runs before the await so it persists even if refresh
    // fails.
    async removeAndExcludeTag(tag) {
        if (!this.isServerMode) {
            showNotification('Tag removal only available in server mode', 'warning');
            return;
        }
        const lcTag = String(tag).toLowerCase();
        if (!confirm(`Remove the tag "${lcTag}" from EVERY file and add it to the global exclusions?\n\nThis removes all instances of the tag and prevents it from being re-extracted on future scans. Continue?`)) {
            return;
        }
        // Add to exclusions and drop from selection BEFORE the flush, so the
        // exclusion persists even if the flush or refresh throws.
        configState.addExcludedTag(lcTag);
        this.renderExcludedTags();
        this.selectedFlushTags.delete(lcTag);
        await this.performFlush(
            () => apiService.flushTags([lcTag]),
            `Removed "${lcTag}" (and added to exclusions)`
        );
    }

    // Flush ALL tags from every file (across all sections). Asks for
    // confirmation because it is destructive and global. Sends an explicit
    // { all: true } opt-in so the backend never infers a global wipe from
    // a missing/nil tags array.
    async flushAllTags() {
        if (!this.isServerMode) {
            showNotification('Tag flush only available in server mode', 'warning');
            return;
        }
        if (!confirm('This will remove ALL tags from EVERY file across all sections.\n\nFiles themselves are not deleted, but every tag will be gone. Continue?')) {
            return;
        }
        await this.performFlush(
            () => apiService.flushAllTags(),
            'Flushed all tags'
        );
    }

    // Flush only the tags selected in the Tag Browser view, from every file.
    async flushSelectedTags() {
        if (!this.isServerMode) {
            showNotification('Tag flush only available in server mode', 'warning');
            return;
        }
        const tags = Array.from(this.selectedFlushTags);
        if (tags.length === 0) {
            showNotification('Select one or more tags first by clicking them in the cloud.', 'info');
            return;
        }
        if (!confirm(`Remove the ${tags.length} selected tag${tags.length !== 1 ? 's' : ''} from EVERY file across all sections?\n\nThis affects all files, not just selected ones. Continue?`)) {
            return;
        }
        await this.performFlush(
            () => apiService.flushTags(tags),
            `Flushed ${tags.length} tag${tags.length !== 1 ? 's' : ''}`
        );
    }

    // Thumbnail Management
    renderThumbnailStatus(status) {
        const container = document.getElementById('thumbnail-status');
        if (!container) return;

        const generated = status?.generated || 0;
        const missing = status?.missing || 0;
        const failed = status?.failed || 0;
        const orphaned = status?.orphaned || 0;

        container.innerHTML = `
            <div class="status-item generated">
                <div class="value">${generated}</div>
                <div class="label">Generated</div>
            </div>
            <div class="status-item missing">
                <div class="value">${missing}</div>
                <div class="label">Missing</div>
            </div>
            <div class="status-item failed">
                <div class="value">${failed}</div>
                <div class="label">Failed</div>
            </div>
            <div class="status-item orphaned">
                <div class="value">${orphaned}</div>
                <div class="label">Orphaned</div>
            </div>
        `;
    }

    async generateMissingThumbnails() {
        if (!this.isServerMode) {
            showNotification('Thumbnail generation only available in server mode', 'warning');
            return;
        }

        try {
            showNotification('Starting thumbnail generation...', 'info');
            const result = await apiService.generateMissingThumbnails('images');

            if (result.count === 0) {
                showNotification('No missing thumbnails found', 'success');
                return;
            }

            const progressContainer = document.getElementById('thumbnail-progress');
            const progressFill = document.getElementById('thumbnail-progress-fill');
            const progressText = document.getElementById('thumbnail-progress-text');

            progressContainer.classList.remove('hidden');

            const initialMissing = result.count;
            const pollInterval = setInterval(async () => {
                try {
                    const status = await apiService.getThumbnailStatus('images');
                    this.renderThumbnailStatus(status);

                    const generated = status?.generated || 0;
                    const currentMissing = status?.missing || 0;
                    const done = Math.max(0, initialMissing - currentMissing);
                    const percent = initialMissing > 0 ? (done / initialMissing) * 100 : 100;

                    progressFill.style.width = `${Math.min(percent, 100)}%`;
                    progressText.textContent = `${done} of ${initialMissing} generated`;

                    if (currentMissing === 0 || percent >= 100) {
                        clearInterval(pollInterval);
                        showNotification('Thumbnail generation complete', 'success');
                        setTimeout(() => progressContainer.classList.add('hidden'), 3000);
                        this.loadThumbnailStatus();
                    }
                } catch (e) {
                    console.error('[TAG-MGMT] Poll error:', e);
                }
            }, 2000);

        } catch (e) {
            showError('Generate Thumbnails', e);
        }
    }

    async generateThumbnail(path) {
        if (!this.isServerMode) {
            showNotification('Thumbnail generation only available in server mode', 'warning');
            return;
        }

        try {
            await apiService.generateThumbnail(path, 'images');
            showNotification('Thumbnail generated', 'success');
        } catch (e) {
            showError('Generate Thumbnail', e);
        }
    }

    async cullOrphanedThumbnails() {
        if (!this.isServerMode) {
            showNotification('Orphan culling only available in server mode', 'warning');
            return;
        }

        try {
            showNotification('Culling orphaned thumbnails...', 'info');
            const result = await apiService.cullOrphans('images');

            if (result.deleted === 0) {
                showNotification('No orphaned thumbnails found', 'success');
            } else {
                showNotification(`Deleted ${result.deleted} orphaned thumbnail(s)`, 'success');
            }
            this.loadThumbnailStatus();
        } catch (e) {
            showError('Cull Orphans', e);
        }
    }

    // Utility
    getAllUniqueTags() {
        const tags = new Set();
        this.allImageFiles.forEach(file => {
            if (file.tags) {
                file.tags.forEach(tag => tags.add(tag.toLowerCase()));
            }
        });
        return Array.from(tags).sort();
    }

    renderExcludedTags() {
        const container = document.getElementById('excluded-tags-list');
        if (!container) return;

        container.innerHTML = '';

        configState.excludedTags.forEach(tag => {
            const span = document.createElement('span');
            span.className = 'excluded-tag';
            span.innerHTML = `${escapeHtml(tag)} <button class="remove-btn" data-tag="${escapeHtml(tag)}">×</button>`;
            container.appendChild(span);
        });

        container.querySelectorAll('.remove-btn').forEach(btn => {
            btn.addEventListener('click', (e) => {
                configState.removeExcludedTag(e.target.dataset.tag);
                this.renderExcludedTags();
            });
        });
    }

    addGlobalExclusion() {
        const input = document.getElementById('global-exclude-input');
        const tag = sanitizeTag(input.value);

        if (!tag) {
            showNotification('Invalid tag', 'error');
            return;
        }

        configState.addExcludedTag(tag);
        input.value = '';
        this.renderExcludedTags();
        showNotification(`Added "${tag}" to exclusions`, 'success');
    }

    async loadDirectoryNames() {
        for (const section of Object.values(SECTIONS)) {
            const nameEl = document.getElementById(`${section}-dir-name`);
            if (nameEl) {
                const handle = await configState.getHandle(section);
                nameEl.textContent = handle?.name || 'Not selected';
            }
        }
    }

    async changeSectionDirectory(section) {
        if (this.isServerMode) {
            showNotification('Cannot change directory in server mode', 'warning');
            return;
        }

        try {
            const handle = await window.showDirectoryPicker({ mode: 'read' });
            await configState.setHandle(section, handle);
            await this.loadDirectoryNames();
            showNotification(`Directory updated for ${section}`, 'success');
        } catch (e) {
            console.log('Directory selection cancelled');
        }
    }

    async clearSectionDirectory(section) {
        if (this.isServerMode) {
            showNotification('Cannot clear directory in server mode', 'warning');
            return;
        }

        await configState.clearHandle(section);
        await this.loadDirectoryNames();
        showNotification(`Directory cleared for ${section}`, 'success');
    }

    async loadGotifyStatus() {
        if (!this.isServerMode) return;

        try {
            const status = await this.fetchWithRetry(
                (t) => apiService.getGotifyStatus(),
                { attempts: 2, timeoutMs: 5000, backoffMs: 400 }
            );
            this.renderGotifyStatus(status);

            const prioritySlider = document.getElementById('gotify-priority');
            const priorityValue = document.getElementById('gotify-priority-value');
            if (prioritySlider && status.default_priority !== undefined) {
                prioritySlider.value = status.default_priority;
                priorityValue.textContent = status.default_priority;
            }

            const toggleBtn = document.getElementById('btn-gotify-toggle');
            if (toggleBtn) {
                toggleBtn.textContent = status.running ? 'Stop' : 'Start';
            }
        } catch (e) {
            console.error('[TAG-MGMT] Failed to load Gotify status:', e);
            const statusEl = document.getElementById('gotify-status');
            if (statusEl) statusEl.innerHTML = '<span class="text-secondary">Failed to load status (backend busy?)</span>';
        }
    }

    async loadDiscordStatus() {
        if (!this.isServerMode) return;

        try {
            const status = await this.fetchWithRetry(
                (t) => apiService.getDiscordStatus(),
                { attempts: 2, timeoutMs: 5000, backoffMs: 400 }
            );
            this.renderDiscordStatus(status);

            const recipientInput = document.getElementById('discord-recipient-id');
            const cooldownInput = document.getElementById('discord-cooldown');
            if (recipientInput && status.recipient_id) recipientInput.value = status.recipient_id;
            if (cooldownInput && status.cooldown_sec) cooldownInput.value = status.cooldown_sec;
        } catch (e) {
            console.error('[TAG-MGMT] Failed to load Discord status:', e);
            const statusEl = document.getElementById('discord-status');
            if (statusEl) statusEl.innerHTML = '<span class="text-secondary">Failed to load status (backend busy?)</span>';
        }
    }

    renderGotifyStatus(status) {
        const statusEl = document.getElementById('gotify-status');
        if (!statusEl) return;

        const running = status.running;
        const dotClass = running ? 'running' : 'stopped';
        const statusText = running ? 'Running' : 'Stopped';
        const enabledText = status.enabled ? 'Enabled' : 'Disabled';

        statusEl.innerHTML = `
            <div class="gotify-status-indicator">
                <span class="gotify-status-dot ${dotClass}"></span>
                <span>${statusText}</span>
                <span class="text-secondary">(${enabledText})</span>
            </div>
        `;

        const grid = document.getElementById('gotify-settings-grid');
        if (grid) {
            grid.innerHTML = `
                <div class="gotify-setting-item">
                    <div class="value">${status.port || 8180}</div>
                    <div class="label">Port</div>
                </div>
                <div class="gotify-setting-item">
                    <div class="value">${status.default_priority || 5}</div>
                    <div class="label">Priority</div>
                </div>
                <div class="gotify-setting-item">
                    <div class="value">${status.cooldown_sec || 3600}s</div>
                    <div class="label">Cooldown</div>
                </div>
                <div class="gotify-setting-item">
                    <div class="value" style="color: ${(status.pending_count || 0) > 0 ? '#f59e0b' : '#22c55e'}">${status.pending_count || 0}</div>
                    <div class="label">Unsent</div>
                </div>
            `;
        }

        // Show/hide pending notification warning
        const pendingInfo = document.getElementById('gotify-pending-info');
        const pendingCountEl = document.getElementById('gotify-pending-count');
        const pendingCount = status.pending_count || 0;
        if (pendingInfo && pendingCountEl) {
            if (pendingCount > 0 && running) {
                pendingInfo.classList.remove('hidden');
                pendingCountEl.textContent = `${pendingCount} chapter${pendingCount !== 1 ? 's have' : ' has'} not been notified yet`;
            } else {
                pendingInfo.classList.add('hidden');
            }
        }

        // Enable/disable push button
        const pushBtn = document.getElementById('btn-gotify-push-pending');
        if (pushBtn) {
            pushBtn.disabled = !running || pendingCount === 0;
        }
    }

    renderDiscordStatus(status) {
        const statusEl = document.getElementById('discord-status');
        if (!statusEl) return;

        const ready = status.ready;
        const dotClass = ready ? 'running' : 'stopped';
        const statusText = ready ? 'Ready' : 'Not ready';
        const enabledText = status.enabled ? 'Enabled' : 'Disabled';
        const tokenText = status.bot_token_configured ? 'configured' : 'not set';

        statusEl.innerHTML = `
            <div class="discord-status-indicator">
                <span class="discord-status-dot ${dotClass}"></span>
                <span>${statusText}</span>
                <span class="text-secondary">(${enabledText}, token ${tokenText})</span>
            </div>
        `;

        const grid = document.getElementById('discord-settings-grid');
        if (grid) {
            grid.innerHTML = `
                <div class="discord-setting-item">
                    <div class="value">${status.recipient_id || '?'}</div>
                    <div class="label">Recipient ID</div>
                </div>
                <div class="discord-setting-item">
                    <div class="value">${status.cooldown_sec || 3600}s</div>
                    <div class="label">Cooldown</div>
                </div>
                <div class="discord-setting-item">
                    <div class="value" style="color: ${(status.pending_count || 0) > 0 ? '#f59e0b' : '#22c55e'}">${status.pending_count || 0}</div>
                    <div class="label">Unsent</div>
                </div>
            `;
        }

        const pendingInfo = document.getElementById('discord-pending-info');
        const pendingCountEl = document.getElementById('discord-pending-count');
        const pendingCount = status.pending_count || 0;
        if (pendingInfo && pendingCountEl) {
            if (pendingCount > 0 && status.enabled) {
                pendingInfo.classList.remove('hidden');
                pendingCountEl.textContent = `${pendingCount} chapter${pendingCount !== 1 ? 's have' : ' has'} not been notified yet`;
            } else {
                pendingInfo.classList.add('hidden');
            }
        }

        const pushBtn = document.getElementById('btn-discord-push-pending');
        if (pushBtn) {
            pushBtn.disabled = !ready || pendingCount === 0;
        }

        const toggleBtn = document.getElementById('btn-discord-toggle');
        if (toggleBtn) {
            toggleBtn.textContent = status.enabled ? 'Disable' : 'Enable';
        }
    }

    async toggleDiscord() {
        if (!this.isServerMode) {
            showNotification('Discord management only available in server mode', 'warning');
            return;
        }

        try {
            const status = await apiService.getDiscordStatus();
            const newEnabled = !status.enabled;
            await apiService.toggleDiscord(newEnabled);
            showNotification(newEnabled ? 'Discord enabling...' : 'Discord disabling...', 'info');
            await this.loadDiscordStatus();
        } catch (e) {
            showError('Toggle Discord', e);
        }
    }

    async testDiscord() {
        if (!this.isServerMode) {
            showNotification('Discord management only available in server mode', 'warning');
            return;
        }

        const testBtn = document.getElementById('btn-discord-test');
        try {
            if (testBtn) {
                testBtn.disabled = true;
                testBtn.textContent = 'Sending...';
            }
            await apiService.testDiscord();
            showNotification('Discord test message sent', 'success');
        } catch (e) {
            showError('Test Discord', e);
        } finally {
            if (testBtn) {
                testBtn.disabled = false;
                testBtn.textContent = 'Test';
            }
            try { await this.loadDiscordStatus(); } catch (e) { /* best effort */ }
        }
    }

    async saveDiscordConfig() {
        if (!this.isServerMode) return;

        const botToken = document.getElementById('discord-bot-token')?.value.trim();
        const recipientID = document.getElementById('discord-recipient-id')?.value.trim();
        const cooldown = parseInt(document.getElementById('discord-cooldown')?.value || '3600', 10);

        const settings = {};
        if (botToken) settings.bot_token = botToken;
        if (recipientID) settings.recipient_id = recipientID;
        if (!isNaN(cooldown) && cooldown > 0) settings.cooldown_sec = cooldown;

        if (Object.keys(settings).length === 0) return;

        try {
            await apiService.updateDiscordConfig(settings);
            showNotification('Discord config saved', 'success');
            await this.loadDiscordStatus();
        } catch (e) {
            showError('Save Discord Config', e);
        }
    }

    async pushDiscordPending() {
        if (!this.isServerMode) {
            showNotification('Discord management only available in server mode', 'warning');
            return;
        }

        const pushBtn = document.getElementById('btn-discord-push-pending');
        try {
            if (pushBtn) {
                pushBtn.disabled = true;
                pushBtn.textContent = 'Pushing...';
            }
            const result = await apiService.pushDiscordPending();
            showNotification(result.message || `Pushed ${result.sent} notification(s)`, 'success');
            await this.loadDiscordStatus();
        } catch (e) {
            showError('Push Discord Notifications', e);
        } finally {
            if (pushBtn) {
                pushBtn.textContent = 'Push Unsent';
                pushBtn.disabled = false;
            }
            try { await this.loadDiscordStatus(); } catch (e) { /* best effort */ }
        }
    }

    async toggleGotify() {
        if (!this.isServerMode) {
            showNotification('Gotify management only available in server mode', 'warning');
            return;
        }

        try {
            const status = await apiService.getGotifyStatus();
            const newEnabled = !status.running;
            await apiService.toggleGotify(newEnabled);
            showNotification(newEnabled ? 'Gotify starting...' : 'Gotify stopping...', 'info');
            await this.loadGotifyStatus();
        } catch (e) {
            showError('Toggle Gotify', e);
        }
    }

    async resetGotify() {
        if (!this.isServerMode) {
            showNotification('Gotify management only available in server mode', 'warning');
            return;
        }

        try {
            const resetBtn = document.getElementById('btn-gotify-reset');
            if (resetBtn) {
                resetBtn.disabled = true;
                resetBtn.textContent = 'Restarting...';
            }

            await apiService.resetGotify();
            showNotification('Gotify server reset', 'success');
            await this.loadGotifyStatus();
        } catch (e) {
            showError('Reset Gotify', e);
        } finally {
            const resetBtn = document.getElementById('btn-gotify-reset');
            if (resetBtn) {
                resetBtn.disabled = false;
                resetBtn.textContent = 'Reset';
            }
        }
    }

    async updateGotifyPriority(value) {
        if (!this.isServerMode) return;

        try {
            await apiService.updateGotifyConfig({ default_priority: value });
        } catch (e) {
            console.error('[TAG-MGMT] Failed to update Gotify priority:', e);
        }
    }

    async pushPendingNotifications() {
        if (!this.isServerMode) {
            showNotification('Gotify management only available in server mode', 'warning');
            return;
        }

        const pushBtn = document.getElementById('btn-gotify-push-pending');
        try {
            if (pushBtn) {
                pushBtn.disabled = true;
                pushBtn.textContent = 'Pushing...';
            }

            const result = await apiService.pushPendingNotifications();
            showNotification(result.message || `Pushed ${result.sent} notification(s)`, 'success');
            await this.loadGotifyStatus();
        } catch (e) {
            showError('Push Notifications', e);
        } finally {
            if (pushBtn) {
                pushBtn.textContent = 'Push Unsent';
                pushBtn.disabled = false;
            }
            // Refresh status to update pending count and button state
            try { await this.loadGotifyStatus(); } catch (e) { /* best effort */ }
        }
    }
}

const app = new TagManagementApp();

if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', () => app.init());
} else {
    app.init();
}

window.TagManagementApp = app;
