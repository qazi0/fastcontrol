// Network-first shell cache so the app opens instantly even before the socket connects.
const CACHE = "fastcontrol-v11";
const SHELL = ["./", "index.html", "messages.js", "app.js", "style.css", "manifest.json", "icon.svg", "fonts/Ubuntu-R.woff2", "fonts/Ubuntu-M.woff2", "fonts/Ubuntu-B.woff2", "fonts/UbuntuMono-R.woff2"];
self.addEventListener("install", (e) => e.waitUntil(caches.open(CACHE).then((c) => c.addAll(SHELL)).then(() => self.skipWaiting())));
self.addEventListener("activate", (e) => e.waitUntil(self.clients.claim()));
self.addEventListener("fetch", (e) => {
  const u = new URL(e.request.url);
  if (e.request.method !== "GET" || u.pathname.startsWith("/ws")) return;
  e.respondWith(fetch(e.request).then((r) => {
    const copy = r.clone();
    caches.open(CACHE).then((c) => c.put(e.request, copy));
    return r;
  }).catch(() => caches.match(e.request)));
});
