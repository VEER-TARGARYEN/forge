/* FORGE service worker.
 *
 * Its job is narrow: make the shell installable and let it paint when the
 * server is not answering yet — the window opens before the Go process has
 * finished binding, and a "cannot reach site" flash on every launch would make
 * a local app feel broken.
 *
 * It must never cache /api/. Agent state changes every second; a stale session
 * list is worse than a slow one, and a cached SSE stream is meaningless.
 */

const VERSION = 'forge-shell-v1';
const SHELL = [
  '/',
  '/app.css',
  '/app.js',
  '/manifest.webmanifest',
  '/icon-192.png',
  '/icon-512.png',
];

self.addEventListener('install', event => {
  event.waitUntil(
    caches.open(VERSION)
      // Individually, so one missing asset does not fail the whole install.
      .then(c => Promise.allSettled(SHELL.map(u => c.add(u))))
      .then(() => self.skipWaiting())
  );
});

self.addEventListener('activate', event => {
  event.waitUntil(
    caches.keys()
      .then(keys => Promise.all(keys.filter(k => k !== VERSION).map(k => caches.delete(k))))
      .then(() => self.clients.claim())
  );
});

self.addEventListener('fetch', event => {
  const req = event.request;
  if (req.method !== 'GET') return;

  const url = new URL(req.url);
  if (url.origin !== self.location.origin) return;
  // Live data and streams go straight to the network, always.
  if (url.pathname.startsWith('/api/')) return;

  // Network first so an upgraded binary's assets win immediately; the cache is
  // only the fallback for a server that has not come up yet.
  event.respondWith(
    fetch(req)
      .then(res => {
        if (res && res.ok) {
          const copy = res.clone();
          caches.open(VERSION).then(c => c.put(req, copy)).catch(() => {});
        }
        return res;
      })
      .catch(async () => {
        const hit = await caches.match(req);
        if (hit) return hit;
        // A client-side route that was never cached still needs the shell.
        if (req.mode === 'navigate') {
          const shell = await caches.match('/');
          if (shell) return shell;
        }
        return new Response('FORGE is not running.', {
          status: 503,
          headers: { 'Content-Type': 'text/plain' },
        });
      })
  );
});
