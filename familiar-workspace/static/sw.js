// Familiar service worker — enables PWA install + caches the
// mobile app shell. Network-first for API calls, cache-first for
// static assets. Familiar is inherently online; aggressive offline
// caching is wasted complexity. The shell precache is just enough
// to skip a white flash on cold launch.

const CACHE = 'familiar-v8';

// Shell assets precached on install. Cache-busting query params on
// the CSS/JS <link>/<script> tags in mobile.html bypass these
// entries naturally during dev — the URL doesn't match the
// precached path, so the SW falls through to network.
const SHELL = [
  '/',
  '/mobile.css',
  '/mobile.js',
  '/favicon.svg',
  '/apple-touch-icon.png',
  '/icon-192.png',
  '/manifest.json',
  '/vendor/toastui/toastui-editor.min.css',
  '/vendor/toastui/toastui-editor-dark.min.css',
  '/vendor/toastui/toastui-editor-all.min.js',
  '/vendor/mermaid/mermaid.min.js',
  '/mermaid-blocks.js',
  '/image-resize.js',
  '/wikilink.js',
];

self.addEventListener('install', (e) => {
  e.waitUntil(
    caches.open(CACHE)
      .then((c) => c.addAll(SHELL))
      .then(() => self.skipWaiting())
  );
});

self.addEventListener('activate', (e) => {
  // Purge old caches when CACHE bumps. Bump on UI updates that
  // need shell assets to invalidate; the cache-busting query
  // params on the script tags handle most day-to-day changes.
  e.waitUntil(
    caches.keys()
      .then((keys) => Promise.all(
        keys.filter((k) => k !== CACHE).map((k) => caches.delete(k))
      ))
      .then(() => self.clients.claim())
  );
});

self.addEventListener('fetch', (e) => {
  const url = new URL(e.request.url);

  // API calls, SSE streams, auth endpoints: always network.
  // Stale auth or chat data is worse than offline; let the SW
  // fall through to the browser's default network fetch.
  // /api/* is the native chat protocol (CHAT-REARCH §"Phase 0");
  // /events/* is the memlog SSE stream.
  // /sw.js itself must never be served from CacheStorage, or a new
  // service worker (e.g. one that adds push handlers) can't propagate.
  if (url.pathname.startsWith('/v1/') ||
      url.pathname.startsWith('/api/') ||
      url.pathname.startsWith('/events/') ||
      url.pathname.startsWith('/console/api/') ||
      url.pathname === '/sw.js') {
    return;
  }

  // HTML documents (navigations): NETWORK-FIRST. The shell carries
  // cache-busting ?v= query params on its <script>/<link> tags, but
  // those only work if the document itself is fresh — serving a
  // cached index.html/mobile.html pins every asset to whatever it
  // referenced at cache time, so a frontend deploy stays invisible
  // until the cache is manually cleared (the v6 wiki-rail-refresh
  // regression). Familiar is inherently online; fetch the document
  // and fall back to cache only when the network is unreachable.
  if (e.request.mode === 'navigate' || e.request.destination === 'document') {
    e.respondWith(
      fetch(e.request)
        .then((resp) => {
          if (resp.ok && url.origin === self.location.origin) {
            const clone = resp.clone();
            caches.open(CACHE).then((c) => c.put(e.request, clone));
          }
          return resp;
        })
        .catch(() => caches.match(e.request).then((c) => c || caches.match('/')))
    );
    return;
  }

  // Static assets: cache-first with network fallback. Cache
  // misses (new asset, post-purge fetch) populate the cache for
  // next time.
  e.respondWith(
    caches.match(e.request).then((cached) => {
      if (cached) return cached;
      return fetch(e.request).then((resp) => {
        if (resp.ok && url.origin === self.location.origin) {
          const clone = resp.clone();
          caches.open(CACHE).then((c) => c.put(e.request, clone));
        }
        return resp;
      });
    })
  );
});

// ── Web Push (PWA notifications) ──────────────────────────────────────
// A scheduled action with a `push` target sends a notification whose
// payload is JSON {title, body, url, tag}. We show it; tapping it routes
// the app to `url` (a /#chat/<id> deep link into the action's thread).

self.addEventListener('push', (e) => {
  let data = {};
  try {
    data = e.data ? e.data.json() : {};
  } catch (_) {
    data = { body: e.data ? e.data.text() : '' };
  }
  const title = data.title || 'Familiar';
  e.waitUntil(self.registration.showNotification(title, {
    body: data.body || '',
    tag: data.tag || undefined, // collapse repeats from the same action
    data: { url: data.url || '/' },
    icon: '/icon-192.png',
    badge: '/icon-192.png',
  }));
});

// Constrain the click target to a same-origin path so a compromised or
// spoofed push payload can't open-redirect the user to an attacker URL.
// Deep links are app routes like "/#chat/<id>" — anything that doesn't
// resolve to this origin's own path is dropped back to "/".
function safeNotificationTarget(raw) {
  try {
    const u = new URL(raw || '/', self.location.origin);
    if (u.origin !== self.location.origin) return '/';
    return u.pathname + u.search + u.hash;
  } catch (_) {
    return '/';
  }
}

self.addEventListener('notificationclick', (e) => {
  e.notification.close();
  const target = safeNotificationTarget(e.notification.data && e.notification.data.url);
  e.waitUntil((async () => {
    // Prefer focusing an already-open app window and routing it there
    // (a hash change fires the in-app router); else open a new one.
    const wins = await self.clients.matchAll({ type: 'window', includeUncontrolled: true });
    for (const client of wins) {
      if ('focus' in client) {
        await client.focus();
        if ('navigate' in client) {
          try { await client.navigate(target); } catch (_) { /* cross-origin guard */ }
        }
        return;
      }
    }
    if (self.clients.openWindow) await self.clients.openWindow(target);
  })());
});
