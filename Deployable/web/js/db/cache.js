import { db } from './index.js';

/**
 * CacheStore - Section data caching
 */
export const cacheStore = {
    async save(section, data) {
        await db.put(db.stores.CACHE, section, data);
    },
    
    async load(section) {
        return await db.get(db.stores.CACHE, section);
    },
    
    async clear(section) {
        await db.delete(db.stores.CACHE, section);
    },
};
