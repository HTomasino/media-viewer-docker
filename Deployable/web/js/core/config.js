/**
 * Core configuration - Single source of truth
 */

// Database
export const DB_CONFIG = {
    NAME: 'MediaViewerDB',
    VERSION: 6,
};

// Gallery behavior
export const GALLERY_CONFIG = {
    CHUNK_SIZE: 50,
    DEFAULT_THUMBNAIL_SIZE: 200,
    VIRTUALIZATION_MARGIN: '300%',
    PAGINATION_ROOT_MARGIN: '200px',
};

// Manga reader
export const READER_CONFIG = {
    MODES: {
        SINGLE: 'single',
        PAIR: 'pair',
        STRIP: 'strip',
    },
    SCALE_MODES: {
        ORIGINAL: 'original',
        FIT_WIDTH: 'fit-width',
        FIT_HEIGHT: 'fit-height',
    },
    FILL_PERCENTAGES: [50, 60, 70, 75, 80, 85, 90, 95, 100],
    DEFAULT_FILL_PERCENT: 100,
    LOAD_MODES: {
        LAZY: 'lazy',
        SEQUENTIAL: 'sequential',
    },
    DEFAULT_SCALE_MODE: 'original',
    DEFAULT_LOAD_MODE: 'lazy',
    SCROLL_SAVE_INTERVAL: 500,
    COMPLETION_THRESHOLD: 0.9,
};

// Tag processing
export const TAG_CONFIG = {
    MAX_TAG_LENGTH: 50,
    ALLOWED_TAG_PATTERN: /^[a-zA-Z0-9_\-]+$/,
};

// Sections
export const SECTIONS = {
    IMAGES: 'images',
    MANGA: 'manga',
    H_MANGA: 'h-manga',
};

// File types
export const MEDIA_EXTENSIONS = new Set([
    'jpg', 'jpeg', 'png', 'gif', 'webp', 'bmp', 'svg',
    'mp4', 'webm', 'avi', 'mov', 'mkv'
]);

export const VIDEO_EXTENSIONS = new Set([
    'mp4', 'webm', 'avi', 'mov', 'mkv'
]);

// Stop words for tag filtering
export const STOP_WORDS = new Set([
    'of', 'the', 'is', 'a', 'and', 'in', 'on', 'at', 'to', 'for', 'with',
    'i', 'you', 'me', 'my', 'like', 'has', 'set', 'love', 'from', 'some',
    'sexy', 'be', 'have', 'edit', 'so', 'do', 'etc',
    'png', 'jpg', 'jpeg', 'mp4', 'gif', 'webm',
    'webp', 'bmp', 'svg', 'avi', 'mov', 'mkv'
]);

// Keyboard shortcuts
export const KEYBOARD_SHORTCUTS = {
    FOCUS_SEARCH: 'f',
    TOGGLE_THEME: 'd',
    ESCAPE: 'Escape',
    ARROW_LEFT: 'ArrowLeft',
    ARROW_RIGHT: 'ArrowRight',
};

// Server configuration
export const SERVER_CONFIG = {
    DEFAULT_PORT: 3000,
    RATE_LIMIT: 100,
    RATE_WINDOW_MS: 60000,
    THUMBNAIL_CACHE_DIR: '.thumbnails',
    THUMBNAIL_MAX_DIM: 200,
    THUMBNAIL_QUALITY: 50,
};

// MIME type mapping
export const MIME_TYPES = {
    '.jpg': 'image/jpeg',
    '.jpeg': 'image/jpeg',
    '.png': 'image/png',
    '.gif': 'image/gif',
    '.webp': 'image/webp',
    '.bmp': 'image/bmp',
    '.svg': 'image/svg+xml',
    '.mp4': 'video/mp4',
    '.webm': 'video/webm',
    '.avi': 'video/x-msvideo',
    '.mov': 'video/quicktime',
};
