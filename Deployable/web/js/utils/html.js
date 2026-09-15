/**
 * HTML utilities - Single source of truth
 * Consolidated from script.js and tag-management.js
 */

/**
 * Escape HTML special characters to prevent XSS.
 * @param {string} str - String to escape
 * @returns {string} - Escaped string
 */
export function escapeHtml(str) {
    if (typeof str !== 'string') return '';
    
    const htmlEscapes = {
        '&': '&amp;',
        '<': '&lt;',
        '>': '&gt;',
        '"': '&quot;',
        "'": '&#39;',
    };
    
    return str.replace(/[&<>"']/g, char => htmlEscapes[char]);
}

/**
 * Validate and sanitize tag input.
 * @param {string} tag - Raw tag input
 * @returns {string|null} - Sanitized tag or null if invalid
 */
export function sanitizeTag(tag) {
    if (typeof tag !== 'string') return null;
    
    const sanitized = tag.trim().toLowerCase();
    
    // Reject tags with suspicious characters
    if (/[<>'"&]/.test(sanitized)) return null;
    if (sanitized.length === 0 || sanitized.length > 50) return null;
    
    return sanitized;
}

/**
 * Create a document fragment from an HTML string.
 * @param {string} html - HTML string
 * @returns {DocumentFragment}
 */
export function htmlToFragment(html) {
    const template = document.createElement('template');
    template.innerHTML = html.trim();
    return template.content;
}
