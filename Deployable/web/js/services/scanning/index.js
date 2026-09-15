import { apiService } from '../service-registry.js';
import { cacheStore } from '../../db/cache.js';
import { isMediaFile, parseFilename, naturalCompare } from '../../utils/tags.js';
import { GALLERY_CONFIG, VIDEO_EXTENSIONS } from '../../core/config.js';

/**
 * BaseScanner - Common scanning functionality
 */
export class BaseScanner {
    constructor() {
        this.isFileSystemAPISupported = 'showDirectoryPicker' in window;
    }
    
    /**
     * Recursively traverse directory.
     * @param {FileSystemDirectoryHandle} dirHandle
     * @param {string} path
     * @yields {Object} - { entry, path, kind }
     */
    async* traverse(dirHandle, path = '') {
        if (!this.isFileSystemAPISupported) return;
        
        try {
            for await (const entry of dirHandle.values()) {
                const entryPath = path + (path ? '/' : '') + entry.name;
                
                yield { entry, path: entryPath, kind: entry.kind };
                
                if (entry.kind === 'directory') {
                    yield* this.traverse(entry, entryPath);
                }
            }
        } catch (e) {
            console.warn('Skipping inaccessible folder:', e);
        }
    }
    
    /**
     * Natural sort comparator for filenames.
     */
    naturalCompare(a, b) {
        return naturalCompare(a, b);
    }
}

/**
 * ImageScanner - Scans images directory with nested folders
 */
export class ImageScanner extends BaseScanner {
    async scan(directoryHandle, onProgress, dirFormats = new Map(), excludedTags = new Set()) {
        const files = [];
        let count = 0;
        
        for await (const { entry, path } of this.traverse(directoryHandle)) {
            if (entry.kind !== 'file' || !isMediaFile(entry.name)) continue;
            
            const type = this._getMediaType(entry.name);
            const folderName = path.split('/').pop() || '';
            
            // Parse tags from filename
            const tagsData = type === 'video' 
                ? { number: null, tags: parseFolderTags(folderName, dirFormats) }
                : parseFilename(entry.name, folderName, dirFormats, excludedTags);
            
            // Add folder name as first tag
            if (folderName && !tagsData.tags.includes(folderName.toLowerCase().replace(/\s+/g, '_'))) {
                tagsData.tags.unshift(folderName.toLowerCase().replace(/\s+/g, '_'));
            }
            
            const fileInfo = await entry.getFile();
            files.push({
                handle: entry,
                path,
                name: entry.name,
                type,
                mtime: fileInfo.lastModified,
                ...tagsData,
            });
            
            count++;
            if (count % 50 === 0 && onProgress) {
                onProgress(Math.min(90, 10 + count / 10));
            }
        }
        
        return files;
    }
    
    _getMediaType(filename) {
        const ext = filename.split('.').pop().toLowerCase();
        return VIDEO_EXTENSIONS.has(ext) ? 'video' : 'image';
    }
}

/**
 * MangaScanner - Scans manga directory structure
 * Series/Chapter/Image hierarchy
 */
export class MangaScanner extends BaseScanner {
    async scan(directoryHandle) {
        const series = [];
        
        for await (const seriesEntry of directoryHandle.values()) {
            if (seriesEntry.kind !== 'directory') continue;
            
            const seriesData = await this._scanSeries(seriesEntry);
            if (seriesData.chapters.length > 0) {
                series.push(seriesData);
            }
        }
        
        return series;
    }
    
    async _scanSeries(seriesHandle) {
        const series = {
            name: seriesHandle.name,
            handle: seriesHandle,
            chapters: [],
            updated_at: 0,
        };
        
        for await (const entry of seriesHandle.values()) {
            if (entry.kind === 'directory') {
                const chapter = await this._scanChapter(entry);
                if (chapter.images.length > 0) {
                    series.chapters.push(chapter);
                    if (chapter.maxMtime > series.updated_at) {
                        series.updated_at = chapter.maxMtime;
                    }
                }
            } else if (entry.kind === 'file' && isMediaFile(entry.name)) {
                const mtime = await this._getFileMtime(entry);
                this._addRootImages(series, seriesHandle, entry);
                if (mtime > series.updated_at) {
                    series.updated_at = mtime;
                }
            }
        }
        
        series.chapters.sort((a, b) => naturalCompare(a.name, b.name));
        
        return series;
    }
    
    async _scanChapter(chapterHandle) {
        const chapter = {
            name: chapterHandle.name,
            handle: chapterHandle,
            images: [],
            maxMtime: 0,
        };
        
        for await (const entry of chapterHandle.values()) {
            if (entry.kind === 'file' && isMediaFile(entry.name)) {
                const mtime = await this._getFileMtime(entry);
                chapter.images.push({
                    handle: entry,
                    name: entry.name,
                    path: `${chapterHandle.name}/${entry.name}`,
                });
                if (mtime > chapter.maxMtime) {
                    chapter.maxMtime = mtime;
                }
            }
        }
        
        chapter.images.sort((a, b) => naturalCompare(a.name, b.name));
        return chapter;
    }
    
    _addRootImages(series, seriesHandle, fileEntry) {
        let rootChapter = series.chapters.find(c => c.name === 'Root');
        if (!rootChapter) {
            rootChapter = { name: 'Root', handle: seriesHandle, images: [] };
            series.chapters.push(rootChapter);
        }
        
        rootChapter.images.push({
            handle: fileEntry,
            name: fileEntry.name,
            path: `Root/${fileEntry.name}`,
        });
    }

    async _getFileMtime(fileEntry) {
        try {
            const file = await fileEntry.getFile();
            return Math.floor(file.lastModified / 1000);
        } catch {
            return 0;
        }
    }
}

/**
 * HMangaScanner - Scans H-Manga directory structure
 * Artist/Book/Chapter hierarchy
 */
export class HMangaScanner extends BaseScanner {
    async scan(directoryHandle) {
        const books = [];
        
        for await (const bookEntry of directoryHandle.values()) {
            if (bookEntry.kind !== 'directory') continue;
            
            const book = await this._scanBook(bookEntry);
            if (book.chapters.length > 0) {
                books.push(book);
            }
        }
        
        return books;
    }
    
    async _scanBook(bookHandle) {
        const book = {
            name: bookHandle.name,
            handle: bookHandle,
            chapters: [],
            updated_at: 0,
        };
        
        let hasSubDirs = false;
        const rootImages = [];
        
        for await (const entry of bookHandle.values()) {
            if (entry.kind === 'directory') {
                hasSubDirs = true;
                const chapter = await this._scanChapter(entry, bookHandle.name);
                if (chapter.images.length > 0) {
                    book.chapters.push(chapter);
                    if (chapter.maxMtime > book.updated_at) {
                        book.updated_at = chapter.maxMtime;
                    }
                }
            } else if (entry.kind === 'file' && isMediaFile(entry.name)) {
                const mtime = await this._getFileMtime(entry);
                rootImages.push({
                    handle: entry,
                    name: entry.name,
                    path: `Root/${entry.name}`,
                });
                if (mtime > book.updated_at) {
                    book.updated_at = mtime;
                }
            }
        }
        
        if (!hasSubDirs && rootImages.length > 0) {
            rootImages.sort((a, b) => naturalCompare(a.name, b.name));
            rootImages.forEach(img => { img.path = `${bookHandle.name}/${img.name}`; });
            book.chapters.push({
                name: bookHandle.name,
                handle: bookHandle,
                images: rootImages,
            });
        } else if (hasSubDirs && rootImages.length > 0) {
            rootImages.sort((a, b) => naturalCompare(a.name, b.name));
            book.chapters.unshift({ name: 'Root', handle: bookHandle, images: rootImages });
        }
        
        book.chapters.sort((a, b) => naturalCompare(a.name, b.name));
        return book;
    }
    
    async _scanChapter(chapterHandle, bookPath) {
        const chapter = {
            name: chapterHandle.name,
            handle: chapterHandle,
            images: [],
            maxMtime: 0,
        };
        
        for await (const entry of chapterHandle.values()) {
            if (entry.kind === 'file' && isMediaFile(entry.name)) {
                const mtime = await this._getFileMtime(entry);
                chapter.images.push({
                    handle: entry,
                    name: entry.name,
                    path: `${bookPath}/${chapterHandle.name}/${entry.name}`,
                });
                if (mtime > chapter.maxMtime) {
                    chapter.maxMtime = mtime;
                }
            } else if (entry.kind === 'directory') {
                await this._scanNested(entry, chapter, `${bookPath}/${chapterHandle.name}`);
            }
        }
        
        chapter.images.sort((a, b) => naturalCompare(a.name, b.name));
        return chapter;
    }
    
    async _scanNested(dirHandle, chapter, pathPrefix) {
        for await (const entry of dirHandle.values()) {
            if (entry.kind === 'file' && isMediaFile(entry.name)) {
                const mtime = await this._getFileMtime(entry);
                chapter.images.push({
                    handle: entry,
                    name: entry.name,
                    path: `${pathPrefix}/${dirHandle.name}/${entry.name}`,
                });
                if (mtime > chapter.maxMtime) {
                    chapter.maxMtime = mtime;
                }
            } else if (entry.kind === 'directory') {
                await this._scanNested(entry, chapter, `${pathPrefix}/${dirHandle.name}`);
            }
        }
    }
    
    async _getFileMtime(fileEntry) {
        try {
            const file = await fileEntry.getFile();
            return Math.floor(file.lastModified / 1000);
        } catch {
            return 0;
        }
    }
}

/**
 * UnifiedScanner - Handles both client and server scanning
 * Eliminates duplicate scan functions in script.js
 */
export class UnifiedScanner {
    constructor() {
        this.scanners = {
            images: new ImageScanner(),
            manga: new MangaScanner(),
            'h-manga': new HMangaScanner(),
        };
    }
    
    async scan(section, handle, options = {}) {
        const scanner = this.scanners[section];
        if (!scanner) throw new Error(`Unknown section: ${section}`);
        
        return scanner.scan(handle, options.onProgress, options.dirFormats, options.excludedTags);
    }
}

// Singleton instance
export const unifiedScanner = new UnifiedScanner();
