/*
 * dsh-gateway service worker — app shell only.
 *
 * Deliberately narrow. The contract's data is live and per-device, so caching
 * any of it would show one phone another phone's session or a turn that has
 * already moved on. The rules are therefore:
 *
 *   - `/api/` is never intercepted, in any form. The WebSocket and every fetch
 *     go straight to the network. (The app is mounted at `/m/`, so API calls are
 *     already outside this worker's scope; the check keeps that true if the
 *     mount ever moves.)
 *   - Anything that is not a static asset destination is left alone, which keeps
 *     `/healthz` (its answer is the point) out of the cache.
 *   - Navigations are network-first, so the served shell carries the *current*
 *     per-response CSP nonce; the cached copy is only a fallback for offline,
 *     where a stale nonce costs the inline critical CSS and nothing else.
 *   - Static assets are stale-while-revalidate: instant from cache, refreshed in
 *     the background, so a deployed change lands on the next launch.
 *
 * The precache list is intentionally the shell entry points rather than every
 * ES module: `addAll` rejects the whole install if one entry is missing, and the
 * module graph is cached by the runtime rule on first load anyway.
 */

const VERSION = "1";
const CACHE = `dsh-gateway-shell-v${VERSION}`;

const SHELL = [
  "./",
  "./index.html",
  "./styles.css",
  "./app.js",
  "./manifest.webmanifest",
  "./icon.svg",
  "./icon-maskable.svg",
];

/** Destinations that are, by definition, static files we ship ourselves. */
const STATIC_DESTINATIONS = new Set(["script", "style", "image", "font", "manifest"]);

self.addEventListener("install", (event) => {
  event.waitUntil(
    (async () => {
      const cache = await caches.open(CACHE);
      await cache.addAll(SHELL);
      await self.skipWaiting();
    })(),
  );
});

self.addEventListener("activate", (event) => {
  event.waitUntil(
    (async () => {
      const names = await caches.keys();
      await Promise.all(
        names.filter((name) => name.startsWith("dsh-gateway-shell-") && name !== CACHE).map((name) => caches.delete(name)),
      );
      await self.clients.claim();
    })(),
  );
});

async function networkFirstNavigation(request) {
  try {
    return await fetch(request);
  } catch {
    const cache = await caches.open(CACHE);
    const cached = (await cache.match("./index.html")) ?? (await cache.match("./"));
    if (cached !== undefined) return cached;
    return new Response("dsh-gateway is offline and no shell is cached.", {
      status: 503,
      headers: { "Content-Type": "text/plain; charset=utf-8" },
    });
  }
}

async function staleWhileRevalidate(request) {
  const cache = await caches.open(CACHE);
  const cached = await cache.match(request);
  const network = fetch(request)
    .then((response) => {
      // Opaque and error responses are not worth storing; a 404 cached forever
      // would outlive the deployment that fixed it.
      if (response.ok && response.type === "basic") void cache.put(request, response.clone());
      return response;
    })
    .catch(() => undefined);

  if (cached !== undefined) return cached;
  const response = await network;
  if (response !== undefined) return response;
  return new Response("", { status: 504 });
}

self.addEventListener("fetch", (event) => {
  const request = event.request;
  if (request.method !== "GET") return;

  const url = new URL(request.url);
  if (url.origin !== self.location.origin) return;
  if (url.pathname.startsWith("/api/")) return;

  if (request.mode === "navigate") {
    event.respondWith(networkFirstNavigation(request));
    return;
  }
  if (STATIC_DESTINATIONS.has(request.destination)) {
    event.respondWith(staleWhileRevalidate(request));
  }
  // Everything else — health probes, anything with a query string that carries
  // meaning — falls through to the network untouched.
});

/* ------------------------------------------------------------ notifications */

/*
 * A push message is the only thing that reaches this app while it is closed, so
 * it is handled here rather than in a page: the browser wakes this worker, not
 * the app. The payload is what the gateway encrypted — a title, a sentence, and
 * where a tap should land — and it is already decrypted by the time it arrives.
 */
self.addEventListener("push", (event) => {
  let message = { title: "dsh-gateway", body: "Something happened.", url: "./#/sessions" };
  try {
    if (event.data) message = { ...message, ...event.data.json() };
  } catch {
    // A push with no readable payload still deserves to be shown: it came from
    // this gateway, and staying silent would be worse than a vague sentence.
  }
  event.waitUntil(
    self.registration.showNotification(message.title, {
      body: message.body,
      // The tag is what collapses repeats: a second approval for the same
      // session replaces the first rather than stacking behind it.
      tag: message.tag,
      data: { url: message.url, sessionId: message.sessionId },
      icon: "./icon.svg",
      badge: "./icon-maskable.svg",
    }),
  );
});

/*
 * A tap should land on the session the notification is about, in the app that
 * is already open if there is one — reopening it would lose whatever the reader
 * was in the middle of.
 */
self.addEventListener("notificationclick", (event) => {
  event.notification.close();
  const target = (event.notification.data && event.notification.data.url) || "./#/sessions";

  event.waitUntil(
    (async () => {
      const clients = await self.clients.matchAll({ type: "window", includeUncontrolled: true });
      for (const client of clients) {
        if (!client.url.startsWith(self.registration.scope)) continue;
        if ("focus" in client) await client.focus();
        // The page owns its hash router, so it is told where to go rather than
        // being navigated behind its own back.
        client.postMessage({ type: "notification-click", url: target });
        return;
      }
      if (self.clients.openWindow) await self.clients.openWindow(target);
    })(),
  );
});
