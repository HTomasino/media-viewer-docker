/**
 * Core Index - Central core exports
 */

// Re-export all config constants
export * from './config.js';

// Re-export state modules
export { configState } from './state/config.js';
export { galleryState, readerState } from './state/gallery.js';
