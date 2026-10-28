// Service worker of the Astovia web UI: shows Web Push notifications and
// opens the screen when one is clicked. It is also served at /admin/sw.js so
// that workers registered before the UI moved from /admin/ to / update to
// this code; nothing here depends on the registration scope.
'use strict';

const FALLBACK = { title: 'Astovia', body: '新しいお知らせがあります。' };

self.addEventListener('install', () => {
  self.skipWaiting();
});

self.addEventListener('activate', (event) => {
  event.waitUntil(self.clients.claim());
});

// A push must always show a notification: some browsers revoke the
// permission of sites that receive pushes without showing one. A payload
// that cannot be read is shown with a generic text.
self.addEventListener('push', (event) => {
  let data = {};
  try {
    const parsed = event.data ? event.data.json() : null;
    if (parsed && typeof parsed === 'object') data = parsed;
  } catch {
    data = {};
  }
  const title = typeof data.title === 'string' && data.title ? data.title : FALLBACK.title;
  const body = typeof data.body === 'string' && data.body ? data.body : FALLBACK.body;
  const options = { body, icon: '/icon-192.png', data: { url: typeof data.url === 'string' ? data.url : '/' } };
  if (typeof data.tag === 'string' && data.tag) options.tag = data.tag;
  event.waitUntil(self.registration.showNotification(title, options));
});

// legacyPath rewrites a path of the old /admin/ UI to the current one.
function legacyPath(pathname) {
  if (pathname === '/admin') return '/';
  if (pathname.startsWith('/admin/')) return pathname.slice('/admin'.length);
  return pathname;
}

// target returns the path to open: the notification's URL when it is a
// same-origin path (old /admin/ paths are rewritten), and / otherwise.
function target(raw) {
  if (typeof raw !== 'string') return '/';
  try {
    const url = new URL(raw, self.location.origin);
    if (url.origin !== self.location.origin) return '/';
    const pathname = legacyPath(url.pathname);
    if (pathname.startsWith('/') && !pathname.startsWith('//')) return pathname + url.search;
  } catch {
    // Fall through to the default.
  }
  return '/';
}

// screenWindow reports whether an open window shows a screen that may be
// reused. Other pages such as the OAuth consent page are left alone.
function screenWindow(raw) {
  const url = new URL(raw);
  if (url.origin !== self.location.origin) return false;
  const p = url.pathname;
  return p === '/' || p === '/candidates' || p.startsWith('/candidates/') || p === '/auto' || p === '/auto/' ||
    p.startsWith('/tasks/') || p === '/settings' || p.startsWith('/settings/');
}

self.addEventListener('notificationclick', (event) => {
  event.notification.close();
  const path = target(event.notification.data && event.notification.data.url);
  event.waitUntil((async () => {
    const windows = await self.clients.matchAll({ type: 'window', includeUncontrolled: true });
    const client = windows.find((c) => screenWindow(c.url));
    if (client) {
      try {
        const focused = await client.focus();
        // navigate resolves to null, or throws, for a window this worker
        // does not control; open a new window then.
        if (await focused.navigate(path)) return;
      } catch {
        // Fall through to a new window.
      }
    }
    await self.clients.openWindow(path);
  })());
});
