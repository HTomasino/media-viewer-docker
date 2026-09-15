/**
 * Services Index - Central service registry
 * Ensures singleton instances across all modules
 */

import { ApiService } from './api/index.js';
import { ThumbnailService } from './thumbnails/index.js';
import { UnifiedScanner } from './scanning/index.js';

// Create single instances
const apiService = new ApiService();
const thumbnailService = new ThumbnailService();
const unifiedScanner = new UnifiedScanner();

// Export singletons
export { apiService, thumbnailService, unifiedScanner };

// Also export classes for testing
export { ApiService } from './api/index.js';
export { ThumbnailService } from './thumbnails/index.js';
export { UnifiedScanner, ImageScanner, MangaScanner, HMangaScanner } from './scanning/index.js';
