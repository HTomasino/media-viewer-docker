/**
 * Database - IndexedDB abstraction layer
 * Extracted from script.js database operations
 */

import { DB_CONFIG } from '../core/config.js';

export class Database {
    constructor() {
        this.db = null;
        this.stores = {
            HANDLES: 'handles',
            FILES: 'files',
            CACHE: 'cache',
            THUMBNAILS: 'thumbnails',
            READ_STATUS: 'readStatus',
            DIR_FORMATS: 'dirFormats',
            ARTIST_THUMBNAILS: 'artistThumbnails',
            FAVORITES: 'favorites',
        };
    }

    async open() {
        if (this.db) return this.db;

        return new Promise((resolve, reject) => {
            const request = indexedDB.open(DB_CONFIG.NAME, DB_CONFIG.VERSION);

            request.onupgradeneeded = (e) => {
                const db = e.target.result;
                this._createStores(db);
            };

            request.onsuccess = () => {
                this.db = request.result;
                resolve(this.db);
            };

            request.onerror = () => reject(request.error);
        });
    }

    _createStores(db) {
        if (!db.objectStoreNames.contains(this.stores.HANDLES)) {
            db.createObjectStore(this.stores.HANDLES, { keyPath: 'id' });
        }

        if (!db.objectStoreNames.contains(this.stores.FILES)) {
            db.createObjectStore(this.stores.FILES, { keyPath: 'path' });
        }

        if (!db.objectStoreNames.contains(this.stores.READ_STATUS)) {
            const store = db.createObjectStore(this.stores.READ_STATUS, {
                keyPath: ['seriesName', 'chapterIndex']
            });
            store.createIndex('seriesName', 'seriesName', { unique: false });
        }

        if (!db.objectStoreNames.contains('tagsData')) {
            db.createObjectStore('tagsData', { keyPath: 'name' });
        }

        if (!db.objectStoreNames.contains(this.stores.CACHE)) {
            db.createObjectStore(this.stores.CACHE);
        }

        if (!db.objectStoreNames.contains(this.stores.THUMBNAILS)) {
            db.createObjectStore(this.stores.THUMBNAILS);
        }

        if (!db.objectStoreNames.contains(this.stores.DIR_FORMATS)) {
            db.createObjectStore(this.stores.DIR_FORMATS);
        }

        if (!db.objectStoreNames.contains(this.stores.ARTIST_THUMBNAILS)) {
            db.createObjectStore(this.stores.ARTIST_THUMBNAILS);
        }

        if (!db.objectStoreNames.contains(this.stores.FAVORITES)) {
            const store = db.createObjectStore(this.stores.FAVORITES, { keyPath: 'key' });
            store.createIndex('section', 'section', { unique: false });
        }
    }

    async get(storeName, key) {
        const db = await this.open();
        return new Promise((resolve, reject) => {
            const tx = db.transaction(storeName, 'readonly');
            const store = tx.objectStore(storeName);
            const req = store.get(key);
            req.onsuccess = () => resolve(req.result);
            req.onerror = () => reject(req.error);
        });
    }

    async put(storeName, key, value) {
        const db = await this.open();
        return new Promise((resolve, reject) => {
            const tx = db.transaction(storeName, 'readwrite');
            const store = tx.objectStore(storeName);
            // If keyPath is defined (in-line keys), don't pass key to put()
            if (store.keyPath !== null) {
                store.put(value);
            } else {
                store.put(value, key);
            }
            tx.oncomplete = () => resolve();
            tx.onerror = () => reject(tx.error);
        });
    }

    async delete(storeName, key) {
        const db = await this.open();
        return new Promise((resolve, reject) => {
            const tx = db.transaction(storeName, 'readwrite');
            const store = tx.objectStore(storeName);
            store.delete(key);
            tx.oncomplete = () => resolve();
            tx.onerror = () => reject(tx.error);
        });
    }

    async getAll(storeName) {
        const db = await this.open();
        return new Promise((resolve, reject) => {
            const tx = db.transaction(storeName, 'readonly');
            const store = tx.objectStore(storeName);
            const req = store.getAll();
            req.onsuccess = () => resolve(req.result);
            req.onerror = () => reject(req.error);
        });
    }

    async getAllFromIndex(storeName, indexName, query) {
        const db = await this.open();
        return new Promise((resolve, reject) => {
            const tx = db.transaction(storeName, 'readonly');
            const store = tx.objectStore(storeName);
            const index = store.index(indexName);
            const req = index.getAll(query);
            req.onsuccess = () => resolve(req.result);
            req.onerror = () => reject(req.error);
        });
    }
}

// Singleton instance
export const db = new Database();
