// The agenda on Home. Times follow the device's zone, with the calendar's home zone alongside
// when the two differ. Which calendars show is kept on each device.
const STORE = 'luna-calendars';
const SOON = 3 * 60 * 60 * 1000;

function choices() {
    try {
        return JSON.parse(localStorage.getItem(STORE)) || {};
    } catch {
        return {};
    }
}

export function saveChoice(name, shown) {
    const saved = choices();
    saved[name] = shown;
    try {
        localStorage.setItem(STORE, JSON.stringify(saved));
    } catch {
        // Private windows can refuse storage, the choice then lasts until reload
    }
    paint();
}

export function shownFor(root) {
    let hidden = [];
    try {
        hidden = JSON.parse(root.dataset.calHidden || '[]');
    } catch {
        hidden = [];
    }
    const saved = choices();
    return (name) => (name in saved ? saved[name] : !hidden.includes(name));
}

const pad = (n) => String(n).padStart(2, '0');
export const dayKey = (date) => `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())}`;
export const addDays = (date, days) => {
    const next = new Date(date);
    next.setDate(next.getDate() + days);
    return next;
};
export const clock = (date, zone) => date.toLocaleTimeString(undefined, { hour: 'numeric', minute: '2-digit', timeZone: zone });
const place = (zone) => zone.split('/').pop().replace(/_/g, ' ');

// "3:00 PM - 3:30 PM (1:00 PM Vancouver)" when the device is away from the home zone
export function timeRange(start, end, home) {
    let text = `${clock(start)} - ${clock(end)}`;
    if (home && clock(start, home) !== clock(start)) text += ` (${clock(start, home)} ${place(home)})`;
    return text;
}

function duration(ms) {
    const minutes = Math.max(1, Math.round(ms / 60000));
    if (minutes < 60) return `${minutes} min`;
    return `${Math.floor(minutes / 60)} h${minutes % 60 ? ` ${minutes % 60} min` : ''}`;
}

function setText(node, text) {
    if (node && node.textContent !== text) node.textContent = text;
}

function setHidden(node, hidden) {
    if (node && node.hidden !== hidden) node.hidden = hidden;
}

function paintAgenda(root) {
    const shown = shownFor(root);
    const home = root.dataset.calHome;
    const now = new Date();
    const today = dayKey(now);
    const tomorrow = dayKey(addDays(now, 1));
    let lastDay = null;
    let any = false;

    root.querySelectorAll('.ck-ag').forEach((item) => {
        let day;
        let visible;
        const left = item.querySelector('[data-cal-left]');
        if (item.dataset.calDay) {
            day = item.dataset.calDay <= today ? today : item.dataset.calDay;
            visible = item.dataset.calEndDay > today && day <= tomorrow;
            setText(item.querySelector('[data-cal-time]'), 'All day');
        } else {
            const start = new Date(item.dataset.calStart);
            const end = new Date(item.dataset.calEnd);
            day = dayKey(start) < today ? today : dayKey(start);
            visible = end > now && day <= tomorrow;
            setText(item.querySelector('[data-cal-time]'), timeRange(start, end, home));
            const live = start <= now;
            setText(left, live ? `now, ${duration(end - now)} left` : start - now < SOON ? `in ${duration(start - now)}` : '');
            if (item.classList.contains('live') !== live) item.classList.toggle('live', live);
        }
        visible = visible && item.dataset.calNames.split('|').some(shown);
        setHidden(item, !visible);

        const label = item.querySelector('[data-cal-label]');
        const first = visible && day !== lastDay;
        setHidden(label, !first);
        if (first) {
            setText(label, day === today ? 'Today' : 'Tomorrow');
            lastDay = day;
        }
        any = any || visible;
    });
    setHidden(root.querySelector('[data-cal-empty]'), any);
}

export function paint() {
    document.querySelectorAll('.ck-agenda[data-cal-root]').forEach(paintAgenda);
}

// Widget updates morph the DOM, which wipes the text this script set
new MutationObserver(paint).observe(document.documentElement, { childList: true, subtree: true });
window.addEventListener('storage', (event) => {
    if (event.key === STORE) paint();
});
paint();
setInterval(paint, 15000);
