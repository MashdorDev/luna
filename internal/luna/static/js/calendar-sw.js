// Shows calendar reminders for calendar-view.js. Android Chrome only allows notifications
// from a service worker, so every browser goes through this one.
self.addEventListener('install', () => self.skipWaiting());
self.addEventListener('activate', (event) => event.waitUntil(self.clients.claim()));

// A click opens the meeting's details in an open Luna tab, or opens Luna if none is open
self.addEventListener('notificationclick', (event) => {
    event.notification.close();
    const data = event.notification.data || {};
    event.waitUntil((async () => {
        const tabs = await self.clients.matchAll({ type: 'window', includeUncontrolled: true });
        const tab = tabs.find((client) => client.url === data.page) || tabs[0];
        if (tab) {
            tab.postMessage({ calendarOpen: data.event });
            return tab.focus();
        }
        return self.clients.openWindow(data.page || self.registration.scope);
    })());
});
