import { db } from './index.js';

/**
 * FavoritesStore - Persistent favorite tracking for Images and H-Manga
 *
 * Key format: "section:identifier"
 *   - Images: "images:path/to/image.jpg"
 *   - H-Manga: "h-manga:ArtistName/BookTitle"
 */
export const favoritesStore = {
    async add(section, identifier, name) {
        const key = `${section}:${identifier}`;
        await db.put(db.stores.FAVORITES, null, {
            key,
            section,
            identifier,
            name,
            addedAt: Date.now(),
        });
    },

    async remove(section, identifier) {
        const key = `${section}:${identifier}`;
        await db.delete(db.stores.FAVORITES, key);
    },

    async isFavorite(section, identifier) {
        const key = `${section}:${identifier}`;
        const result = await db.get(db.stores.FAVORITES, key);
        return !!result;
    },

    async getAll(section) {
        const all = await db.getAllFromIndex(db.stores.FAVORITES, 'section', section);
        return all;
    },

    async getIdentifiers(section) {
        const all = await this.getAll(section);
        return new Set(all.map(f => f.identifier));
    },
};