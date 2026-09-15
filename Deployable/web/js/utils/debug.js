/**
 * Debug logging helper.
 *
 * All non-essential debug logging (API, gallery, reader traces) routes through
 * `debug()` so production builds stay quiet. Enable verbose logging by setting
 * `localStorage['mv-debug'] = '1'` in the console; remove it (or set to '0') to
 * silence again. This keeps diagnostics available on demand without leaking
 * implementation noise to end users.
 */

let _enabled;

function isEnabled() {
    if (_enabled === undefined) {
        try {
            _enabled = localStorage.getItem('mv-debug') === '1';
        } catch (e) {
            // localStorage may be unavailable (private mode, sandbox) —
            // default to off so we never spam the console unprompted.
            _enabled = false;
        }
    }
    return _enabled;
}

// Re-check on storage changes so toggling the flag from another tab/window
// is picked up without a reload. Falls back to a no-op if addEventListener is
// unavailable (older browsers / non-window globals).
if (typeof window !== 'undefined' && window.addEventListener) {
    window.addEventListener('storage', (e) => {
        if (e.key === 'mv-debug') {
            _enabled = e.newValue === '1';
        }
    });
}

/**
 * debug(...args) — drop-in replacement for console.log that only logs when
 * debug mode is enabled. Accepts the same arguments as console.log.
 */
export function debug(...args) {
    if (isEnabled()) {
        // Use console.log directly so the call site shows in the console
        // stack trace instead of this wrapper.
        console.log(...args);
    }
}