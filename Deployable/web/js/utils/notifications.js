/**
 * Notification utilities - Single notification system
 * Consolidated from script.js
 */

/**
 * Show a non-blocking notification toast.
 * @param {string} message - Message to display
 * @param {string} [type='info'] - Type: 'info', 'success', 'error', 'warning'
 * @param {number} [duration=3000] - Duration in milliseconds
 */
export function showNotification(message, type = 'info', duration = 3000) {
    // Remove existing notification
    const existing = document.querySelector('.notification-toast');
    if (existing) existing.remove();
    
    const notification = document.createElement('div');
    notification.className = `notification-toast notification-${type}`;
    
    const colors = {
        info: '#6366f1',
        success: '#22c55e',
        error: '#ef4444',
        warning: '#f59e0b',
    };
    
    notification.style.cssText = `
        position: fixed;
        bottom: 20px;
        right: 20px;
        padding: 1rem 1.5rem;
        border-radius: 8px;
        background: ${colors[type] || colors.info};
        color: white;
        font-weight: 500;
        z-index: 9999;
        box-shadow: 0 4px 12px rgba(0,0,0,0.3);
        animation: slideIn 0.3s ease;
    `;
    
    notification.textContent = message;
    document.body.appendChild(notification);
    
    setTimeout(() => {
        notification.style.animation = 'fadeOut 0.3s ease';
        setTimeout(() => notification.remove(), 300);
    }, duration);
}

/**
 * Show an error notification with automatic error logging.
 * @param {string} context - Context where error occurred
 * @param {Error} error - Error object
 */
export function showError(context, error) {
    console.error(`[${context}]`, error);
    showNotification(`Error: ${error.message || 'Unknown error'}`, 'error');
}

/**
 * Wrap a function with error boundary.
 * @param {Function} fn - Async function to wrap
 * @param {string} context - Description for error logging
 * @returns {Function} - Wrapped function
 */
export function withErrorBoundary(fn, context) {
    return async function(...args) {
        try {
            return await fn.apply(this, args);
        } catch (error) {
            showError(context, error);
            return null;
        }
    };
}
