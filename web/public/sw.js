// Service worker of the Astovia management screen: shows Web Push
// notifications and opens the screen when one is clicked.
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
  const options = { body, icon: '/admin/icon-192.png', data: { url: typeof data.url === 'string' ? data.url : '/admin/' } };
  if (typeof data.tag === 'string' && data.tag) options.tag = data.tag;
  event.waitUntil(self.registration.showNotification(title, options));
});

// target returns the path to open: the notification's URL when it is a
// same-origin path under /admin/, and /admin/ otherwise.
function target(raw) {
  if (typeof raw !== 'string' || !raw.startsWith('/admin/')) return '/admin/';
  try {
    const url = new URL(raw, self.location.origin);
    if (url.origin === self.location.origin && url.pathname.startsWith('/admin/')) return url.pathname + url.search;
  } catch {
    // Fall through to the default.
  }
  return '/admin/';
}

self.addEventListener('notificationclick', (event) => {
  event.notification.close();
  const path = target(event.notification.data && event.notification.data.url);
  event.waitUntil((async () => {
    const windows = await self.clients.matchAll({ type: 'window', includeUncontrolled: true });
    for (const client of windows) {
      const url = new URL(client.url);
      if (url.origin !== self.location.origin || !url.pathname.startsWith('/admin/')) continue;
      try {
        const focused = await client.focus();
        await focused.navigate(path);
        return;
      } catch {
        // An uncontrolled window cannot be navigated; open a new one.
        break;
      }
    }
    await self.clients.openWindow(path);
  })());
});
