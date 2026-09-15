/**
 * Tag parsing utilities
 * Consolidated from utils.js
 */

import { STOP_WORDS, MEDIA_EXTENSIONS } from '../core/config.js';

/**
 * Check if a filename has a supported media extension.
 * @param {string} filename 
 * @returns {boolean}
 */
export function isMediaFile(filename) {
    const ext = filename.split('.').pop().toLowerCase();
    return MEDIA_EXTENSIONS.has(ext);
}

/**
 * Filter tags by removing stop words, user exclusions, and pure numbers.
 * @param {string[]} tagsArray - Array of tag strings to filter
 * @param {Set<string>} [userExcludedTags] - Optional set of user-excluded tags
 * @returns {string[]}
 */
export function filterTags(tagsArray, userExcludedTags = new Set()) {
    return tagsArray
        .map(t => t.toLowerCase().trim())
        .filter(t => {
            if (t.length === 0) return false;
            if (STOP_WORDS.has(t)) return false;
            if (userExcludedTags.has(t)) return false;
            if (/^-?\d+([.,]\d+)?$/.test(t)) return false;
            return true;
        });
}

/**
 * Extract tags from a folder name based on configured format.
 * @param {string} folderName 
 * @param {Map<string, string>} [dirFormats] - Map of folder path to format
 * @returns {string[]} Array of extracted tags
 */
export function parseFolderTags(folderName, dirFormats = new Map()) {
    const format = dirFormats.get(folderName) || 'standard';
    let extracted = [];

    if (format === 'bracket') {
        const match = folderName.match(/^\[(.*?)\]\s*(.*)$/);
        if (match) extracted = [match[1], match[2]];
    } else if (format === 'suffix') {
        const match = folderName.match(/^(.*?)\s*\[(.*?)\]$/);
        if (match) extracted = [match[1], match[2]];
    } else if (format === 'parentheses') {
        const match = folderName.match(/^(.*?)\s*\((.*?)\)$/);
        if (match) extracted = [match[1], match[2]];
    }

    if (extracted.length === 0) {
        extracted = [folderName];
    }

    extracted = extracted.map(e => e.trim().replace(/[-\s]+/g, '_')).filter(e => e.length > 0);
    return filterTags(extracted);
}

/**
 * Extract number and tags from a filename.
 * @param {string} filename - Full filename with extension
 * @param {string} [folderName] - Optional folder name for fallback tags
 * @param {Map<string, string>} [dirFormats] - Optional map for folder tag parsing
 * @param {Set<string>} [userExcludedTags] - Optional set of user-excluded tags
 * @returns {{number: number|null, tags: string[]}}
 */
export function parseFilename(filename, folderName, dirFormats = new Map(), userExcludedTags = new Set()) {
    const nameWithoutExt = (filename.substring(0, filename.lastIndexOf('.')) || filename).replace(/\.+$/, '');
    let number = null;
    let tags = [];

    const isPureNumbers = /^\d+$/.test(nameWithoutExt);
    const isHashOrID = /^[a-zA-Z0-9_]{6,}$/.test(nameWithoutExt) && /\d/.test(nameWithoutExt) && /[a-zA-Z]/.test(nameWithoutExt);

    if (isPureNumbers || isHashOrID) {
        const numMatch = nameWithoutExt.match(/^(\d+)/);
        if (numMatch) number = parseInt(numMatch[1], 10);
    } else {
        const numMatch = nameWithoutExt.match(/^(\d+)/);
        if (numMatch) {
            number = parseInt(numMatch[1], 10);
            let rest = nameWithoutExt.substring(numMatch[1].length).trim();
            rest = rest.replace(/^[-\s]+/, '');
            if (rest) {
                tags = rest.split(/[-\s]+/).filter(t => t.length > 0);
            }
        } else {
            tags = nameWithoutExt.split(/[-\s]+/).filter(t => t.length > 0);
        }
    }

    let finalTags = filterTags(tags, userExcludedTags);

    if (finalTags.length === 0 && folderName) {
        finalTags.push(...parseFolderTags(folderName, dirFormats));
    }

    return { number, tags: finalTags };
}

/**
 * Natural sort comparator for filenames.
 * @param {string} a 
 * @param {string} b 
 * @returns {number}
 */
export function naturalCompare(a, b) {
    return a.localeCompare(b, undefined, { numeric: true, sensitivity: 'base' });
}

export function parseChapterName(name) {
    // Strip a leading "Chapter " (or similar) prefix so the numeric match works
    // on names like "Chapter 37.5" — the previous version failed on these
    // because Number("Chapter 37") is NaN, which made the function fall back
    // to treating the whole "Chapter 37.5" string as a single chapter.
    const stripped = String(name).replace(/^\s*chapter\s+/i, '').trim();
    const dotIndex = stripped.indexOf('.');
    if (dotIndex === -1) {
        const num = Number(stripped);
        if (isNaN(num)) return { chapter: stripped, part: 0 };
        return { chapter: num, part: 0 };
    }
    const chapter = Number(stripped.substring(0, dotIndex));
    const part = parseInt(stripped.substring(dotIndex + 1), 10);
    if (isNaN(chapter)) return { chapter: stripped, part: 0 };
    return { chapter, part: isNaN(part) ? 0 : part };
}

/**
 * Count unique "main" entries in a chapter list.
 *
 * @param {Array<{name: string}>} chapters
 * @param {Object} [options]
 * @param {boolean} [options.skipNonNumeric=false] When true, skip entries
 *   whose name doesn't parse to a numeric chapter (e.g. "Extra",
 *   "Special"). Pass true for the Manga section where every chapter is
 *   expected to be numbered and named extras would inflate the count;
 *   leave false (or default) for H-Manga where books are standalone and
 *   commonly have non-numeric names like
 *   "An Erotic Novel Kind of Girl! (Comic X-Eros #89)" — in that case
 *   each non-numeric entry still counts as 1.
 * @returns {number}
 */
export function getChapterCount(chapters, { skipNonNumeric = false } = {}) {
    if (!chapters?.length) return 0;
    const mainChapters = new Set();
    for (const ch of chapters) {
        const { chapter } = parseChapterName(ch.name);
        if (typeof chapter !== 'number') {
            if (skipNonNumeric) continue;
            // Non-numeric name: treat as its own entry so every unique
            // label is counted (H-Manga book names are commonly free-form).
            mainChapters.add(chapter);
            continue;
        }
        // Skip chapter 0 entirely (e.g. "Chapter 0", "Chapter 0.1") — these
        // are prequels/intros, not part of the main chapter numbering. Sub-
        // chapters (X.Y) collapse into their parent main number via the
        // Set, so they don't inflate the count.
        if (chapter === 0) continue;
        mainChapters.add(chapter);
    }
    return mainChapters.size;
}
