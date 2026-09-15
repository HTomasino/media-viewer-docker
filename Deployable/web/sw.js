// Media Viewer Service Worker v39
// v39:
//  - Strict sequential image loading: in reader sequential mode each image
//    must finish downloading before the next src is set (initial batch,
//    delayed remaining load, and scroll-driven prefetch all gated), so the
//    backend is never hit with parallel transcode requests; adjacent-image
//    preload headers are disabled in this mode for the same reason.
// v35:
//  - Hamburger hidden in reader immersive mode (consolidated rule:
//    body.reader-open and body.immersive-mode both hide the floating
//    hamburger inside the mobile media query).
// v34:
//  - Search-bar overlap fix: mobile top-header gains left padding (3.6rem)
//    so the fixed floating hamburger no longer covers the search input;
//    the hamburger is hidden while the reader is open (it's a gallery
//    affordance and overlapped the reader's own top bar).
// v33:
//  - Mobile sidebar rebuilt as a fixed slide-in drawer: the old in-flow top
//    strip (max-height 110px, overflow hidden) could not hold nav +
//    favorites + tags — content below the clip line was invisible and the
//    strip could appear behind/under gallery content. The drawer is
//    position:fixed (80vw/320px, full height, z-index 200) with a tap-to-
//    close backdrop; the floating hamburger (fixed top-left, z-index 201)
//    is always visible outside the reader and opens it. All sidebar
//    sections are reachable in the drawer; drawer is closed by default.
// v32:
//  - Sidebar flicker fix: the reader no longer toggles the sidebar's
//    .collapsed class (that class is the user's persisted gallery
//    preference). Reader open/close now uses a dedicated body.reader-open
//    class — CSS hides the sidebar while reading and restores the user's
//    preference on close, ending the "pops up behind the gallery then
//    immediately hides" flicker. _applySectionUI re-asserts the
//    preference on every section entry.
// v31:
//  - Toggleable mobile sidebar: the old "always show sidebar" override made
//    .collapsed a no-op on mobile (both hamburgers hidden => no way to
//    toggle). Collapsed now slides the sidebar fully away; the in-sidebar
//    hamburger collapses it and a floating hamburger (top-left, only while
//    collapsed) reopens it. Reader immersive-mode unchanged (hides all).
// v30:
// v29:
//  - Exit-handle fix: the handle button shipped with an inline
//    style="display:none" that beat the media-query reveal rule, so the
//    floating exit handle never appeared. Visibility is now CSS-driven
//    only (base display:none, body.immersive-mode reveals it).
// v28:
//  - Fix regression from v26: the reader's isMobile() method was shadowed
//    by the constructor's this.isMobile boolean property, so toggleUI()
//    threw "isMobile is not a function" on every center tap — immersive
//    mode (and the exit handle/hint) never activated. toggleUI now reads
//    the media query directly.
// v27:
//  - CRITICAL mobile fix: the section nav row was crushed to 0px height.
//    The mobile sidebar is a column flex container with max-height +
//    overflow:hidden; .sidebar-nav's overflow-x:auto gave it an automatic
//    min-height of 0, so flexbox absorbed the entire height deficit by
//    shrinking the nav — the ONLY way to switch sections was untappable.
//    Fixed with flex-shrink:0 + min-height:44px on the nav, a tighter
//    header margin, and shrinkable scan-status.
// v26:
//  - Mobile UI parity with modal: reader immersive mode keeps its own top
//    bar visible (back button/chapter dropdown always reachable) and adds
//    a floating exit handle + one-time "tap center" hint; reader toggleUI
//    no longer writes inline top-header styles (single body-class
//    mechanism, stale inline styles scrubbed on open); mobile sidebar
//    strip slimmed 150px -> 110px.
// v25:
//  - Mobile sidebar accessibility fix: reader/modal no longer set inline
//    sidebar display styles — body.immersive-mode CSS is the single
//    source of truth (an unmatched inline display:none !important left
//    the sidebar permanently unreachable, and mobile has no hamburger to
//    recover it). Favorites/Random and tag-filter sections are now
//    revealed in the mobile sidebar strip (compact-mobile class); manga
//    settings and tag assignment remain hidden for space.
// v24:
//  - "Mark read at bottom" fix: progressStore._upsert now serializes
//    writes per record key — the scroll handler's throttled saveProgress
//    could interleave with markComplete's read-modify-write and wipe the
//    just-written isComplete=true. Also: when every recorded chapter is
//    complete, the Continue position synthesizes the NEXT chapter (past
//    the list = series fully read, badge suppressed) instead of pointing
//    back at the just-finished chapter.
// v23:
//  - Mobile "latest chapter read" fix: reader:open route restores saved
//    progress on replay/deep-link entry (mobile tab reload previously
//    reopened the reader at the originally-tapped chapter, ignoring the
//    progress record). Splash explicit chapter picks keep restore disabled
//    via a payload flag.
// v22:
//  - Section sort split: H-Manga artist grid keeps favorites-first then
//    alphabetical; Manga series grid sorts by most recently updated
//    (updated_at desc) instead of alphabetical.
// v21:
//  - Stale manga section fix: loadServerData re-binds the displayed artist
//    against the fresh folder list at fetch RESOLUTION time (previously only
//    when set before the fetch started, leaving artist views on stale
//    chapters after section round-trips); _navShowArtist refreshes in the
//    background instead of serving cached folders indefinitely; reindex
//    auto-refresh pops the artist drill-down before applying, keeping route
//    and UI in sync.
// v20:
//  - Artist favorites in H-Manga: star badge on artist cards ("artist:"
//    identifier namespace), artist grid sorts favorites-first then
//    alphabetical, favorites-only view keeps favorited artists as cards and
//    flattens favorite books from non-favorited artists.
// v19:
//  - Bugfix sweep: reader back button double-binding removed; modal
//    next/prev/tag-click route through the history router; search filter
//    re-derives from source (backspace restores results); reader scroll
//    listener no longer accumulates per chapter; chunker observers created
//    once (no per-render leak); stale tag panel hidden for tagless files;
//    filter changes preserve scroll; media/thumbnail responses drop
//    `immutable` and gain X-File-Mtime versioning (edited files no longer
//    stick for 7 days); preload log spam moved to debug logging.
// v18:
//  - SPA history router with scroll preservation. New
//    core/history-router.js owns a nav:stack in localStorage; section
//    switches and filter changes use replace, drill-downs and overlays use
//    push, browser back/forward walks the stack and closes overlays before
//    leaving the app.
// v17:
//  - Stop the cover-fallback infinite loop: track tried sources and clear
//    onerror in the terminal branch, otherwise a broken local + unreachable
//    remote would hammer the server with hundreds of 404s per second.
//  - Wrap progressStore.getContinuePosition in a try/catch in splash.open()
//    so an IndexedDB failure doesn't leave the splash hanging on the
//    skeleton forever.
//  - Guard the splash:openReader and gallery:openSeriesSplash listeners
//    against missing e.detail.
//  - Make getChapterCount's non-numeric skip opt-in (skipNonNumeric) so
//    H-Manga books with free-form names are still counted individually.
//  - parseChapterName returns the stripped name in non-numeric fallback
//    branches for consistency with its numeric branches.

const CACHE_NAME = 'media-viewer-v44';
const API_CACHE_NAME = 'media-viewer-api-v2';
const API_CACHE_MAX_ENTRIES = 50;

// Read-only GET endpoints eligible for stale-while-revalidate. Only these are
// cached; everything else under /api/ (mutations, media streams, thumbnails)
// falls through to network-only.
const API_CACHE_PATHS = [
  '/api/files',
  '/api/folders',
  '/api/tags',
  '/api/tags/stats',
];

// Media/thumbnail GETs ARE cacheable by the SW — but only when the URL is
// version-stamped with ?v=<mtime> (bumped on every file edit). Unversioned
// media URLs (paths are mutable) stay network-only so an edited file's stale
// bytes can't be served from the cache. The ?v= param arrives once
// apiService._maybeRecordFileMtime learns the mtime from an X-File-Mtime
// response, which the backend now stamps on every media/thumbnail response
// including the streamed nocache variants.
const MEDIA_CACHE_PATHS = ['/api/media/', '/api/thumbnail/'];

function isApiCacheable(pathname) {
  // Exact match against the allowlist (the tags endpoints have sub-paths like
  // /api/tags/stats and /api/tags/:tag/files — only /api/tags and /api/tags/stats
  // are read-only; /api/tags/:path is a POST mutation handled by the network path).
  return API_CACHE_PATHS.includes(pathname);
}

function isVersionedMediaUrl(url) {
  if (!MEDIA_CACHE_PATHS.some((p) => url.pathname.startsWith(p))) return false;
  return url.searchParams.has('v');
}

// Query params that vary with the request's POSITION in a listing (index,
// total, next/prev path context, preload count) but do NOT change the image
// bytes the server returns � the backend only uses them to compute the
// X-Preload-* response headers. Without normalizing these away, the same
// image viewed from two different positions (tag-filtered list, different
// chapter length, modal vs reader) would be stored under two distinct URLs
// and counted twice against the media-cache limit.
const MEDIA_KEY_IGNORED_PARAMS = ['index', 'total', 'nextPaths', 'prevPaths', 'preloadCount'];

// mediaCacheKey returns the canonical URL string used as the SW cache key:
// the media path plus only the byte-affecting params (section, width,
// height, dpr, nocache, v). Position-only params are stripped so the same
// image always maps to exactly one cache entry.
function mediaCacheKey(url) {
  const canonical = new URL(url);
  MEDIA_KEY_IGNORED_PARAMS.forEach((p) => canonical.searchParams.delete(p));
  // Sort params so keys are insensitive to insertion order: the backend-built
  // preload URLs ('width=..&dpr=..&nocache=1&index=..') and client-built
  // getMediaUrl URLs ('section=..&width=..&v=..') order params differently,
  // and Cache Storage keys are exact strings � without sorting, the same
  // param set in a different order would be two entries.
  const sorted = [...canonical.searchParams.entries()].sort(([a], [b]) =>
    a < b ? -1 : a > b ? 1 : 0
  );
  canonical.search = '';
  sorted.forEach(([k, v]) => canonical.searchParams.append(k, v));
  return canonical.toString();
}

// Track insertion order for LRU eviction since Cache API entries don't expose
// mtime. Keys are full request URLs; values are insertion timestamps.
const apiCacheOrder = new Map();

// Media entries get their own cache + LRU so a batch of large image blobs
// can't evict the small JSON entries (files/folders/tags) that the UI
// depends on for fast section loads.
const MEDIA_CACHE_NAME = 'media-viewer-media-v1';
const MEDIA_CACHE_MAX_ENTRIES = 200;
const mediaCacheOrder = new Map();
// User-configurable cap (SET_MEDIA_CACHE_LIMIT message from the page).
// 0 means unlimited. Starts at the compiled default; the app pushes its
// persisted value on boot and whenever the user changes the setting.
let mediaCacheLimit = MEDIA_CACHE_MAX_ENTRIES;

// Only cache shell, not the module scripts (they have their own cache headers)
const SHELL_ASSETS = [
  './',
  './index.html',
  './tag-management.html',
  './manifest.json'
];

// Install - cache shell assets only
self.addEventListener('install', (event) => {
  event.waitUntil(
    caches.open(CACHE_NAME).then((cache) => {
      return Promise.allSettled(
        SHELL_ASSETS.map(url => cache.add(url).catch(() => {
          // Ignore errors for assets that may not exist yet
          console.log('[SW] Failed to cache:', url);
        }))
      );
    })
  );
  // Skip waiting so the new SW activates immediately
  self.skipWaiting();
});

// Activate - clean up old caches (both the old shell cache AND the API cache
// from a previous version, so a v7→v8 upgrade purges everything).
self.addEventListener('activate', (event) => {
  event.waitUntil(
    caches.keys().then((cacheNames) => {
      // Purge every cache that isn't the current shell, API, or media cache.
      // This also drops any prior media-viewer-api-*/media-viewer-media-* version.
      return Promise.all(
        cacheNames.filter((name) => name !== CACHE_NAME && name !== API_CACHE_NAME && name !== MEDIA_CACHE_NAME).map((name) => {
          console.log('[SW] Deleting old cache:', name);
          return caches.delete(name);
        })
      );
    }).then(() => {
      // Reset the in-SW order trackers to match the freshly-purged caches.
      apiCacheOrder.clear();
      mediaCacheOrder.clear();
      return Promise.all([
        caches.open(API_CACHE_NAME).then((cache) => cache.keys()),
        caches.open(MEDIA_CACHE_NAME).then((cache) => cache.keys()),
      ]);
    }).then(([apiKeys, mediaKeys]) => {
      const now = Date.now();
      apiKeys.forEach((req) => apiCacheOrder.set(req.url, now));
      mediaKeys.forEach((req) => mediaCacheOrder.set(req.url, now));
    })
  );
  // Take control of all clients immediately
  self.clients.claim();
});

// Messages from the page: the settings dropdown can lower/raise the media
// cache cap. Setting a smaller limit evicts oldest entries immediately so
// the cache shrinks to the new bound right away (0 = unlimited, no eviction).
async function trimMediaCacheToLimit() {
  if (mediaCacheLimit <= 0) return;
  const cache = await caches.open(MEDIA_CACHE_NAME);
  while (mediaCacheOrder.size > mediaCacheLimit) {
    const oldest = mediaCacheOrder.keys().next().value;
    if (oldest === undefined) break;
    mediaCacheOrder.delete(oldest);
    await cache.delete(new Request(oldest)).catch(() => {});
  }
}

self.addEventListener('message', (event) => {
  const data = event.data || {};
  if (data.type === 'SET_MEDIA_CACHE_LIMIT') {
    const num = parseInt(data.limit, 10);
    if (!Number.isNaN(num) && num >= 0) {
      mediaCacheLimit = num;
      console.log('[SW] media cache limit set to:', num === 0 ? 'unlimited' : num);
      event.waitUntil(trimMediaCacheToLimit());
    }
  }
});

// Stale-while-revalidate for cacheable API GETs: respond with the cached
// response immediately (if any) and fire a background fetch to update the
// cache. If nothing is cached, go to the network; on failure return 503.
async function apiStaleWhileRevalidate(request) {
  const cache = await caches.open(API_CACHE_NAME);
  const cached = await cache.match(request);

  // Background revalidation (only if there's a cached response to serve
  // immediately, otherwise we await the network fetch below).
  if (cached) {
    fetch(request).then((response) => {
      if (response && response.ok) {
        putApiCacheEntry(cache, request, response);
      }
    }).catch(() => {
      // Offline — keep serving stale; that's the whole point of SWR.
    });
    return cached;
  }

  // No cache — go to the network. On success cache the response; on failure
  // there's nothing to fall back to, so return 503.
  try {
    const response = await fetch(request);
    if (response && response.ok) {
      // Clone before caching since the original body will be consumed by
      // the browser when we return response.
      putApiCacheEntry(cache, request, response.clone());
    }
    return response;
  } catch (e) {
    return new Response(JSON.stringify({ error: 'Offline' }), {
      status: 503,
      headers: { 'Content-Type': 'application/json' }
    });
  }
}

// putApiCacheEntry stores a response and enforces the LRU bound. Eviction
// uses the apiCacheOrder Map (insertion-ordered) to drop the oldest entry
// when the cache exceeds API_CACHE_MAX_ENTRIES.
function putApiCacheEntry(cache, request, response) {
  const url = request.url;
  cache.put(request, response).then(() => {
    // Refresh insertion order: delete-then-set moves an existing key to the
    // end (most-recent), mirroring an LRU touch.
    apiCacheOrder.delete(url);
    apiCacheOrder.set(url, Date.now());
    if (apiCacheOrder.size > API_CACHE_MAX_ENTRIES) {
      // Evict the oldest (first) entry. Use keys().next() to avoid
      // converting the whole map to an array on every insert.
      const oldest = apiCacheOrder.keys().next().value;
      if (oldest !== undefined) {
        apiCacheOrder.delete(oldest);
        // cache.delete accepts a Request or URL string; build a Request to
        // match the stored entry's key precisely.
        cache.delete(new Request(oldest)).catch(() => {});
      }
    }
  }).catch((e) => {
    console.log('[SW] API cache put failed:', url, e);
  });
}

// putMediaCacheEntry stores a version-stamped media response and enforces a
// dedicated LRU bound (mediaCacheLimit, user-configurable), separate from the
// JSON API LRU so image blobs can't evict files/folders/tags entries.
// `key` is the canonical mediaCacheKey(url) string � position-only params
// (index/total/nextPaths/prevPaths/preloadCount) are already stripped so the
// same image maps to exactly one entry regardless of where it was viewed.
function putMediaCacheEntry(cache, key, response) {
  cache.put(new Request(key), response).then(() => {
    mediaCacheOrder.delete(key);
    mediaCacheOrder.set(key, Date.now());
    if (mediaCacheLimit > 0 && mediaCacheOrder.size > mediaCacheLimit) {
      const oldest = mediaCacheOrder.keys().next().value;
      if (oldest !== undefined) {
        mediaCacheOrder.delete(oldest);
        cache.delete(new Request(oldest)).catch(() => {});
      }
    }
  }).catch((e) => {
    console.log('[SW] media cache put failed:', key, e);
  });
}

// apiCacheFirst serves version-stamped media URLs from the cache when known,
// otherwise goes to the network and caches. ?v=<mtime> in the URL guarantees
// byte-identity for a given version, so no revalidation is needed on hit.
// A background refresh is intentionally NOT fired on cache hits: a media GET
// is a transcode+stream request, and re-fetching every cached image after it
// was just served would double the server load the cache exists to avoid.
async function apiCacheFirst(request) {
  const cache = await caches.open(MEDIA_CACHE_NAME);
  // Canonical key: strips position-only params so the same image viewed from
  // different listing positions hits one entry (see mediaCacheKey).
  const key = mediaCacheKey(request.url);
  const cached = await cache.match(new Request(key));
  if (cached) {
    // LRU touch on hit � derive from the same function that built the stored
    // key so a future param-list change can't desync the order map.
    mediaCacheOrder.delete(key);
    mediaCacheOrder.set(key, Date.now());
    return cached;
  }
  try {
    // Fetch the ORIGINAL request (never the canonical URL): index/total/
    // nextPaths must reach the server for its preload-header computation.
    const response = await fetch(request);
    if (response && response.ok) {
      putMediaCacheEntry(cache, key, response.clone());
    }
    return response;
  } catch (e) {
    return new Response(JSON.stringify({ error: 'Offline' }), {
      status: 503,
      headers: { 'Content-Type': 'application/json' }
    });
  }
}

// Fetch - network first for mutations, stale-while-revalidate for cacheable
// API GETs, cache-first with background refresh for static assets.
self.addEventListener('fetch', (event) => {
  const url = new URL(event.request.url);

  // API requests: cache the read-only GET allowlist via SWR, version-stamped
  // media/thumbnail GETs cache-first, and everything else (POST
  // scan/reindex/tags, unversioned media) network-only.
  if (url.pathname.startsWith('/api/')) {
    if (event.request.method === 'GET' && isVersionedMediaUrl(url)) {
      // Cache-first: the ?v=<mtime> version param guarantees freshness, so
      // a cache hit can be served without revalidation. Background update
      // keeps the LRU order fresh.
      event.respondWith(apiCacheFirst(event.request));
    } else if (event.request.method === 'GET' && isApiCacheable(url.pathname)) {
      event.respondWith(apiStaleWhileRevalidate(event.request));
    } else {
      // Mutations and streaming endpoints: never cache. On network failure
      // return 503 so the UI shows an error instead of hanging.
      event.respondWith(
        fetch(event.request).catch(() => {
          return new Response(JSON.stringify({ error: 'Offline' }), {
            status: 503,
            headers: { 'Content-Type': 'application/json' }
          });
        })
      );
    }
    return;
  }

  // Don't cache JS modules - always fetch fresh for instant updates
  if (url.pathname.endsWith('.js') || url.pathname.endsWith('.mjs')) {
    event.respondWith(
      fetch(event.request).catch(() => {
        return caches.match(event.request);
      })
    );
    return;
  }

  // Don't cache CSS files - always fetch fresh (versioned via query string)
  if (url.pathname.endsWith('.css')) {
    event.respondWith(
      fetch(event.request).catch(() => {
        return caches.match(event.request);
      })
    );
    return;
  }

  // Don't cache cross-origin requests
  if (url.origin !== self.location.origin) {
    event.respondWith(fetch(event.request));
    return;
  }

  // For static assets: cache first, then network
  event.respondWith(
    caches.match(event.request).then((cached) => {
      if (cached) {
        // Return cached, but also update cache in background
        fetch(event.request).then((response) => {
          if (response && response.ok) {
            caches.open(CACHE_NAME).then((cache) => {
              cache.put(event.request, response);
            });
          }
        }).catch(() => {});
        return cached;
      }
      // Not in cache - fetch and cache
      return fetch(event.request).then((response) => {
        if (response && response.ok) {
          const responseClone = response.clone();
          caches.open(CACHE_NAME).then((cache) => {
            cache.put(event.request, responseClone);
          });
        }
        return response;
      });
    })
  );
});
