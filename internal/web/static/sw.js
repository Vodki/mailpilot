// Service worker: caches only the static shell (styles, script, icons).
// Pages and mail content are never cached: they always come from the server.
const CACHE = 'mailpilot-static-v3';
const ASSETS = ['/static/app.css', '/static/app.js', '/static/icons.svg', '/static/icon.svg', '/static/icon-192.png'];

self.addEventListener('install', (e) => {
  e.waitUntil(caches.open(CACHE).then((c) => c.addAll(ASSETS)).then(() => self.skipWaiting()));
});

self.addEventListener('activate', (e) => {
  e.waitUntil(caches.keys().then((keys) => Promise.all(keys.filter((k) => k !== CACHE).map((k) => caches.delete(k))))
    .then(() => self.clients.claim()));
});

self.addEventListener('fetch', (e) => {
  const url = new URL(e.request.url);
  if (e.request.method !== 'GET' || url.origin !== self.location.origin || !url.pathname.startsWith('/static/')) {
    return; // network only
  }
  e.respondWith(caches.match(e.request).then((hit) => hit || fetch(e.request)));
});
