/**
 * Service Registry - Central singleton manager
 * Ensures single instances across all modules
 */

// Services
import { ApiService } from './api/index.js';
import { ThumbnailService } from './thumbnails/index.js';
import { UnifiedScanner, ImageScanner, MangaScanner, HMangaScanner } from './scanning/index.js';

// Create singleton instances
const apiService = new ApiService();
const thumbnailService = new ThumbnailService();
const unifiedScanner = new UnifiedScanner();

// Export singletons
export {
    apiService,
    thumbnailService,
    unifiedScanner,
    // Export classes for testing
    ApiService,
    ThumbnailService,
    UnifiedScanner,
    ImageScanner,
    MangaScanner,
    HMangaScanner
};
