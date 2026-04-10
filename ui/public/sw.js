// Ghostcam service worker — minimal shell cache for PWA installability.
// Live data (HLS segments, SSE, API) is always fetched from the network;
// only the app shell is cached so the UI can boot offline and satisfy
// install criteria.

const SHELL_CACHE = 'ghostcam-shell-v1';

self.addEventListener('install', (event) => {
  event.waitUntil(
    caches.open(SHELL_CACHE).then((cache) => cache.add('/')),
  );
  self.skipWaiting();
});

self.addEventListener('activate', (event) => {
  event.waitUntil(
    caches
      .keys()
      .then((keys) =>
        Promise.all(
          keys
            .filter((key) => key !== SHELL_CACHE)
            .map((key) => caches.delete(key)),
        ),
      )
      .then(() => self.clients.claim()),
  );
});

self.addEventListener('fetch', (event) => {
  const req = event.request;
  if (req.method !== 'GET') return;

  const url = new URL(req.url);
  if (url.origin !== self.location.origin) return;

  // Never intercept live data paths.
  if (
    url.pathname.startsWith('/api') ||
    url.pathname.startsWith('/hls') ||
    url.pathname.startsWith('/events')
  ) {
    return;
  }

  // Network-first with shell fallback for navigations so fresh builds ship
  // immediately when online, but the app still boots when offline.
  if (req.mode === 'navigate') {
    event.respondWith(
      fetch(req)
        .then((res) => {
          const copy = res.clone();
          caches.open(SHELL_CACHE).then((cache) => cache.put('/', copy));
          return res;
        })
        .catch(() =>
          caches
            .match('/')
            .then(
              (cached) =>
                cached ||
                new Response('Offline', {
                  status: 503,
                  statusText: 'Offline',
                }),
            ),
        ),
    );
  }
});
