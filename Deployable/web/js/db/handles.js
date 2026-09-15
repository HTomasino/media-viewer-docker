import { db } from './index.js';

/**
 * HandleStore - Directory handle persistence
 */
export const handleStore = {
    async save(id, handle) {
        await db.put(db.stores.HANDLES, null, { id, handle });
    },
    
    async get(id) {
        const result = await db.get(db.stores.HANDLES, id);
        return result?.handle || null;
    },
    
    async delete(id) {
        await db.delete(db.stores.HANDLES, id);
    },
};
