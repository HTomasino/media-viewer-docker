/**
 * Database Registry - Central database singletons
 */

import { Database } from './database.js';
import { handleStore as handles } from './handles.js';
import { cacheStore as cache } from './cache.js';
import { progressStore as progress } from './progress.js';
import { favoritesStore as favorites } from './favorites.js';

// Create singleton database instance
const db = new Database();

// Export registry
export {
    db,
    handles,
    cache,
    progress,
    favorites
};
