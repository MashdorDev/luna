// Shows calendar reminders for calendar-view.js. Android Chrome only allows notifications
// from a service worker, so every browser goes through this one.
self.addEventListener('install', () => self.skipWaiting());
self.addEventListener('activate', (event) => event.waitUntil(self.clients.claim()));

// A click opens the meeting's details in an open Luna tab, or opens Luna if none is open
self.addEventListener('notificationclick', (event) => {
    event.notification.close();
    const data = event.notification.data || {};
    event.waitUntil((async () => {
        // Other apps can share the origin, so only Luna's own tabs, and only the page that
        // reminded (query and hash aside), since other Luna pages may have no calendar
        const path = (url) => url.split(/[?#]/)[0];
        const tabs = (await self.clients.matchAll({ type: 'window', includeUncontrolled: true }))
            .filter((client) => client.url.startsWith(self.registration.scope));
        const tab = tabs.find((client) => client.url === data.page)
            || (data.page && tabs.find((client) => path(client.url) === path(data.page)));
        if (tab) {
            tab.postMessage({ calendarOpen: data.event });
            return tab.focus();
        }
        return self.clients.openWindow(data.page || self.registration.scope);
    })());
});
