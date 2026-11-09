"use strict";
// Habakkuk's service worker. It exists so the dashboard can be installed to
// a home screen and open without a network. It keeps only the app shell
// (this page's own static files, which hold no data); API responses,
// transcripts and everything else private are never cached.
const CACHE = "habakkuk-shell-v3";
const SHELL = ["./", "app.js", "nodes.js", "pair.html", "app.css", "icon.svg", "icon-192.png", "manifest.webmanifest"];

self.addEventListener("install", e => {
  e.waitUntil(caches.open(CACHE).then(c => c.addAll(SHELL)).then(() => self.skipWaiting()));
});

self.addEventListener("activate", e => {
  e.waitUntil(caches.keys()
    .then(keys => Promise.all(keys.filter(k => k !== CACHE).map(k => caches.delete(k))))
    .then(() => self.clients.claim()));
});

self.addEventListener("fetch", e => {
  const url = new URL(e.request.url);
  const scope = new URL(self.registration.scope);
  const inShell = SHELL.some(p => new URL(p, scope).pathname === url.pathname);
  if (e.request.method !== "GET" || url.origin !== scope.origin || url.search || !inShell) {
    return; // the network handles it, untouched
  }
  // Network first, so an update shows at once; the cache only answers when
  // the network is down.
  e.respondWith(fetch(e.request).then(res => {
    if (res.ok) {
      const copy = res.clone();
      caches.open(CACHE).then(c => c.put(e.request, copy));
    }
    return res;
  }).catch(() => caches.match(e.request)));
});
