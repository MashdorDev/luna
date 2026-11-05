// The agenda on Home and the week grid. Times follow the device's zone, with the calendar's home zone alongside
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
    const shownItems = [];

    root.querySelectorAll('.ck-ag').forEach((item) => {
        let day;
        let visible;
        let at = 0;
        const left = item.querySelector('[data-cal-left]');
        if (item.dataset.calDay) {
            day = item.dataset.calDay <= today ? today : item.dataset.calDay;
            visible = item.dataset.calEndDay > today && day <= tomorrow;
            setText(item.querySelector('[data-cal-time]'), 'All day');
        } else {
            const start = new Date(item.dataset.calStart);
            const end = new Date(item.dataset.calEnd);
            at = start.getTime();
            day = dayKey(start) < today ? today : dayKey(start);
            visible = end > now && day <= tomorrow;
            setText(item.querySelector('[data-cal-time]'), timeRange(start, end, home));
            const live = start <= now;
            setText(left, live ? `now, ${duration(end - now)} left` : start - now < SOON ? `in ${duration(start - now)}` : '');
            if (item.classList.contains('live') !== live) item.classList.toggle('live', live);
        }
        visible = visible && item.dataset.calNames.split('|').some(shown);
        setHidden(item, !visible);
        setHidden(item.querySelector('[data-cal-label]'), true);
        if (visible) shownItems.push({ item, day, at });
    });

    // The server sorts by the home zone's days. CSS order puts them in this device's days
    // without moving nodes, which the next widget update would undo.
    shownItems.sort((a, b) => a.day.localeCompare(b.day) || a.at - b.at);
    let lastDay = null;
    shownItems.forEach(({ item, day }, index) => {
        const order = String(index + 1);
        if (item.style.order !== order) item.style.order = order;
        if (day === lastDay) return;
        const label = item.querySelector('[data-cal-label]');
        setHidden(label, false);
        setText(label, day === today ? 'Today' : 'Tomorrow');
        lastDay = day;
    });
    setHidden(root.querySelector('[data-cal-empty]'), shownItems.length > 0);
}

const minutesOf = (date) => date.getHours() * 60 + date.getMinutes();

// Side by side lanes for meetings that overlap, each cluster as wide as it needs
function lanes(events) {
    let cluster = [];
    let clusterEnd = 0;
    const finish = () => cluster.forEach((event) => { event.lanes = Math.max(...cluster.map((e) => e.lane)) + 1; });
    events.forEach((event) => {
        if (event.from >= clusterEnd) {
            finish();
            cluster = [];
        }
        const used = cluster.filter((e) => e.to > event.from).map((e) => e.lane);
        event.lane = 0;
        while (used.includes(event.lane)) event.lane++;
        cluster.push(event);
        clusterEnd = Math.max(clusterEnd, event.to);
    });
    finish();
}

function element(tag, className, text) {
    const node = document.createElement(tag);
    if (className) node.className = className;
    if (text) node.textContent = text;
    return node;
}

function buildWeek(root, events, shown) {
    const home = root.dataset.calHome;
    const now = new Date();
    const days = [...Array(Number(root.dataset.calDays) || 7)].map((_, i) => addDays(now, i));
    const keys = days.map(dayKey);
    const visible = events.filter((event) => event.calendars.some(shown));

    const timed = keys.map(() => []);
    const allDay = keys.map(() => []);
    visible.forEach((event) => {
        if (event.day) {
            keys.forEach((key, i) => { if (event.day <= key && key < event.endDay) allDay[i].push(event); });
            return;
        }
        const start = new Date(event.start);
        const end = new Date(event.end);
        keys.forEach((key, i) => {
            const dayStart = new Date(`${key}T00:00:00`);
            const dayEnd = addDays(dayStart, 1);
            if (start >= dayEnd || end <= dayStart) return;
            const from = start < dayStart ? 0 : minutesOf(start);
            const to = end >= dayEnd ? 1440 : Math.max(from + 15, minutesOf(end));
            timed[i].push({ event, start, end, from, to });
        });
    });

    // Office hours unless a meeting falls outside them
    const all = timed.flat();
    const first = Math.min(8 * 60, ...all.map((e) => Math.floor(e.from / 60) * 60));
    const last = Math.max(18 * 60, ...all.map((e) => Math.ceil(e.to / 60) * 60));
    const span = last - first;

    const grid = root.querySelector('[data-cal-grid]');
    const nodes = [element('div', 'cw-corner')];
    days.forEach((day, i) => {
        const head = element('div', 'cw-head' + (i === 0 ? ' today' : ''));
        head.append(element('b', '', day.toLocaleDateString(undefined, { weekday: 'short' })), element('span', '', String(day.getDate())));
        nodes.push(head);
    });
    nodes.push(element('div', 'cw-corner'));
    allDay.forEach((list) => {
        const cell = element('div', 'cw-allday');
        list.forEach((event) => {
            const chip = element('div', `cw-chip cal-c${event.color}${event.holiday ? ' hol' : ''}`, event.name);
            chip.title = `${event.name}, ${event.calendars.join(', ')}`;
            cell.append(chip);
        });
        nodes.push(cell);
    });

    const hours = element('div', 'cw-hours');
    for (let minute = first; minute < last; minute += 60) {
        const label = element('span', '', new Date(2000, 0, 1, minute / 60).toLocaleTimeString(undefined, { hour: 'numeric' }));
        label.style.top = `${((minute - first) / span) * 100}%`;
        hours.append(label);
    }
    nodes.push(hours);
    timed.forEach((list, i) => {
        const column = element('div', 'cw-day' + (i === 0 ? ' today' : ''));
        list.sort((a, b) => a.from - b.from || b.to - a.to);
        lanes(list);
        list.forEach((item) => {
            const { event } = item;
            const block = element('button', `cw-event cal-c${event.color}`);
            block.type = 'button';
            block.dataset.calOpen = '';
            block.dataset.calEvent = JSON.stringify(event);
            block.style.top = `${((item.from - first) / span) * 100}%`;
            block.style.height = `${((item.to - item.from) / span) * 100}%`;
            block.style.left = `${(item.lane / item.lanes) * 100}%`;
            block.style.width = `${100 / item.lanes}%`;
            block.dataset.calEnd = event.end;
            block.title = `${event.name}\n${timeRange(item.start, item.end, home)}\n${event.calendars.join(', ')}`;
            block.append(element('b', '', event.name), element('span', '', clock(item.start)));
            column.append(block);
        });
        if (i === 0) column.append(element('i', 'cw-now'));
        nodes.push(column);
    });
    grid.style.setProperty('--days', days.length);
    grid.style.setProperty('--rows', span / 60);
    grid.dataset.first = first;
    grid.dataset.span = span;
    grid.replaceChildren(...nodes);
}

function paintWeek(root) {
    const grid = root.querySelector('[data-cal-grid]');
    if (!grid) return;
    let events = [];
    try {
        events = JSON.parse(root.dataset.calEvents || '[]');
    } catch {
        events = [];
    }
    const shown = shownFor(root);
    root.querySelectorAll('[data-cal-toggle]').forEach((box) => {
        const on = shown(box.dataset.calToggle);
        if (box.checked !== on) box.checked = on;
    });
    const home = root.dataset.calHome;
    const now = new Date();
    const zone = root.querySelector('[data-cal-zone]');
    const away = home && clock(now, home) !== clock(now);
    setHidden(zone, !away);
    if (away) setText(zone, `Times in ${place(Intl.DateTimeFormat().resolvedOptions().timeZone)}, hover a meeting for ${place(home)}`);

    // Rebuilding on every tick would fire the observer again, so only when something changed
    const key = [root.dataset.calEvents, dayKey(now), JSON.stringify(choices())].join('#');
    if (grid.dataset.key !== key || !grid.firstChild) {
        buildWeek(root, events, shown);
        grid.dataset.key = key;
    }
    grid.querySelectorAll('.cw-event').forEach((block) => {
        const past = new Date(block.dataset.calEnd) < now;
        if (block.classList.contains('past') !== past) block.classList.toggle('past', past);
    });
    const line = grid.querySelector('.cw-now');
    if (line) {
        const top = `${((minutesOf(now) - Number(grid.dataset.first)) / Number(grid.dataset.span)) * 100}%`;
        if (line.style.top !== top) line.style.top = top;
    }
}

export function paint() {
    document.querySelectorAll('.ck-agenda[data-cal-root]').forEach(paintAgenda);
    document.querySelectorAll('.cw[data-cal-root]').forEach(paintWeek);
}

// Meeting details, so an invite can be read before its link opens Zoom or Meet
const LINK_TEXT = /(https?:\/\/[^\s<>"]+[^\s<>".,;:!?)\]])/g;

function linkify(text) {
    const fragment = document.createDocumentFragment();
    text.split(LINK_TEXT).forEach((part, i) => {
        if (i % 2 === 0) {
            if (part) fragment.append(part);
            return;
        }
        const link = element('a', '', part);
        link.href = part;
        link.target = '_blank';
        link.rel = 'noreferrer';
        fragment.append(link);
    });
    return fragment;
}

function joinLabel(link) {
    const host = new URL(link).hostname;
    if (host.endsWith('meet.google.com')) return 'Join Google Meet';
    if (host.endsWith('zoom.us')) return 'Join Zoom';
    if (host.includes('teams.')) return 'Join Teams';
    if (host.includes('discord')) return 'Open Discord';
    return `Join on ${host}`;
}

let dialog = null;

function openDetails(event, home) {
    if (!dialog) {
        dialog = element('dialog', 'cal-dialog');
        dialog.addEventListener('click', (click) => {
            if (click.target === dialog) dialog.close();
        });
        document.body.append(dialog);
    }
    const close = element('button', 'cal-dclose', '×');
    close.type = 'button';
    close.setAttribute('aria-label', 'Close');
    close.addEventListener('click', () => dialog.close());
    const title = element('h3', '', event.name);
    title.dir = 'auto';
    const head = element('div', 'cal-dhead');
    head.append(title, close);

    let when;
    if (event.day) {
        const first = new Date(`${event.day}T12:00:00`);
        const last = addDays(new Date(`${event.endDay}T12:00:00`), -1);
        const format = (date) => date.toLocaleDateString(undefined, { weekday: 'long', day: 'numeric', month: 'long' });
        when = dayKey(first) === dayKey(last) ? `${format(first)}, all day` : `${format(first)} to ${format(last)}`;
    } else {
        const start = new Date(event.start);
        when = `${start.toLocaleDateString(undefined, { weekday: 'long', day: 'numeric', month: 'long' })}, ${timeRange(start, new Date(event.end), home)}`;
    }
    const cals = element('div', `cal-dcals cal-c${event.color}`);
    cals.append(element('i'), event.calendars.join(', '));
    const nodes = [head, element('div', 'cal-dwhen', when), cals];

    if (event.location && event.location !== event.link) {
        const where = element('div', 'cal-dloc');
        where.append(linkify(event.location));
        nodes.push(where);
    }
    if (event.link) {
        const join = element('a', 'cal-djoin', joinLabel(event.link));
        join.href = event.link;
        join.target = '_blank';
        join.rel = 'noreferrer';
        nodes.push(join);
    }
    if (event.notes) {
        const notes = element('div', 'cal-dnotes');
        notes.dir = 'auto';
        notes.append(linkify(event.notes));
        nodes.push(notes);
    }
    dialog.replaceChildren(...nodes);
    if (!dialog.open) dialog.showModal();
    close.focus();
}

document.addEventListener('click', (click) => {
    const opener = click.target.closest('[data-cal-open]');
    if (!opener) return;
    const holder = opener.closest('[data-cal-event]');
    try {
        openDetails(JSON.parse(holder.dataset.calEvent), opener.closest('[data-cal-root]')?.dataset.calHome);
    } catch {
        // A widget update can swap the node mid-click, the next click works
    }
});

document.addEventListener('change', (event) => {
    const box = event.target.closest('[data-cal-toggle]');
    if (box) saveChoice(box.dataset.calToggle, box.checked);
});

// Widget updates morph the DOM, which wipes the text this script set
new MutationObserver(paint).observe(document.documentElement, { childList: true, subtree: true });
window.addEventListener('storage', (event) => {
    if (event.key === STORE) paint();
});
paint();
setInterval(paint, 15000);
