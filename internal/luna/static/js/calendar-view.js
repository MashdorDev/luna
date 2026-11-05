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

const VIEW = 'luna-calendar-view';

function savedView() {
    try {
        const view = localStorage.getItem(VIEW);
        if (view === 'week' || view === 'list') return view;
    } catch {
        // fall through to the default
    }
    // Seven columns do not fit a phone, so phones start on the list
    return window.matchMedia('(max-width: 700px)').matches ? 'list' : 'week';
}

const hoursText = (minutes) => {
    const h = Math.floor(minutes / 60);
    const m = minutes % 60;
    return h && m ? `${h}h ${m}m` : h ? `${h}h` : `${m}m`;
};

// Booked time counts overlapping meetings once
function bookedMinutes(items) {
    let total = 0;
    let until = -1;
    [...items].sort((a, b) => a.from - b.from).forEach(({ from, to }) => {
        if (to <= until) return;
        total += to - Math.max(from, until);
        until = to;
    });
    return total;
}

const VIDEO_ICON = '<svg viewBox="0 0 16 16" aria-hidden="true"><path fill="currentColor" d="M2 4.5A1.5 1.5 0 0 1 3.5 3h6A1.5 1.5 0 0 1 11 4.5v1.2l2.4-1.6a.6.6 0 0 1 .9.5v6.8a.6.6 0 0 1-.9.5L11 10.3v1.2A1.5 1.5 0 0 1 9.5 13h-6A1.5 1.5 0 0 1 2 11.5z"/></svg>';

function videoMark() {
    const mark = element('i', 'cw-video');
    mark.innerHTML = VIDEO_ICON;
    mark.title = 'Has a meeting link';
    return mark;
}

function opener(tag, className, event) {
    const node = element(tag, className);
    if (tag === 'button') node.type = 'button';
    node.dataset.calOpen = '';
    node.dataset.calEvent = JSON.stringify(event);
    node.dataset.calEnd = event.end || '';
    return node;
}

// The feeds reach 15 days ahead, so this week and next
const WEEKS = 2;
let weekOffset = 0;

function mondayOf(date) {
    const day = new Date(date.getFullYear(), date.getMonth(), date.getDate());
    return addDays(day, -((day.getDay() + 6) % 7));
}

// The week view runs Monday to Sunday; the list runs from today
function viewDays(root, view, now) {
    if (view === 'week') {
        const monday = addDays(mondayOf(now), weekOffset * 7);
        return [...Array(7)].map((_, i) => addDays(monday, i));
    }
    return [...Array(Number(root.dataset.calDays) || 7)].map((_, i) => addDays(now, i));
}

function weekData(root, events, shown, days) {
    const keys = days.map(dayKey);
    const timed = keys.map(() => []);
    const allDay = keys.map(() => []);
    const marks = keys.map(() => []);
    events.filter((event) => event.calendars.some(shown)).forEach((event) => {
        // Working location and away label the day instead of sitting in it
        if (event.marker) {
            const from = event.day || dayKey(new Date(event.start));
            const to = event.endDay || dayKey(new Date(new Date(event.end) - 1));
            keys.forEach((key, i) => {
                const inside = event.day ? from <= key && key < to : from <= key && key <= to;
                if (inside && !marks[i].includes(event.marker)) marks[i].push(event.marker);
            });
            return;
        }
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
    // Calendars without reminders (a kid's school day) sit side by side with meetings,
    // but only meetings count as an overlap, toward booked time, or as Now and Next
    timed.forEach((list) => {
        list.sort((a, b) => a.from - b.from || b.to - a.to);
        lanes(list);
        list.forEach((item) => {
            item.clash = !item.event.quiet && list.some((other) => other !== item && !other.event.quiet
                && other.from < item.to && other.to > item.from);
        });
    });
    const meetings = (list) => list.filter((item) => !item.event.quiet);
    const stats = timed.map((list) => ({ count: meetings(list).length, minutes: bookedMinutes(meetings(list)), clashes: list.filter((item) => item.clash).length }));
    return { days, keys, timed, allDay, stats, marks };
}

function dayStats(stat) {
    return stat.count ? `${stat.count} · ${hoursText(stat.minutes)}` : 'Free';
}

function buildSummary(root, data) {
    const summary = root.querySelector('[data-cal-summary]');
    const count = data.stats.reduce((sum, stat) => sum + stat.count, 0);
    const minutes = data.stats.reduce((sum, stat) => sum + stat.minutes, 0);
    const clashes = data.timed.flat().filter((item) => item.clash);
    const parts = [element('b', '', `${count} ${count === 1 ? 'meeting' : 'meetings'}, ${hoursText(minutes)} booked`)];
    const busiest = data.stats.reduce((best, stat, i) => (stat.minutes > data.stats[best].minutes ? i : best), 0);
    if (count) parts.push(element('span', '', `Busiest ${data.days[busiest].toLocaleDateString(undefined, { weekday: 'long' })}, ${hoursText(data.stats[busiest].minutes)}`));
    if (clashes.length) parts.push(element('span', 'warn', `${clashes.length} overlapping`));
    const now = new Date();
    // Next week's view is for planning; Now and Next stay on this week
    const next = weekOffset ? null : data.timed.flat().filter((item) => item.end > now && !item.event.quiet).sort((a, b) => a.start - b.start)[0];
    if (next) {
        const button = opener('button', 'cw-next', next.event);
        button.append(element('span', '', next.start <= now ? 'Now: ' : 'Next: '), element('b', '', next.event.name));
        const left = element('span', 'cw-left');
        left.dataset.calNextStart = next.event.start;
        left.dataset.calNextEnd = next.event.end;
        button.append(left);
        parts.push(button);
    }
    summary.replaceChildren(...parts);
}

function buildWeek(root, data) {
    const home = root.dataset.calHome;
    const { days, timed, allDay, stats } = data;
    // The whole day, so the now line is there at any hour; the box scrolls to it
    const first = 0;
    const last = 24 * 60;
    const span = last - first;

    const grid = root.querySelector('[data-cal-grid]');
    const today = dayKey(new Date());
    const nodes = [element('div', 'cw-corner')];
    days.forEach((day, i) => {
        const head = element('div', 'cw-head' + (dayKey(day) === today ? ' today' : ''));
        const date = element('div', 'cw-date');
        date.append(element('b', '', day.toLocaleDateString(undefined, { weekday: 'short' })), element('span', '', String(day.getDate())));
        head.append(date, element('small', stats[i].clashes ? 'warn' : '', dayStats(stats[i])));
        if (data.marks[i].length) head.append(element('span', 'cw-mark', data.marks[i].join(', ')));
        nodes.push(head);
    });
    nodes.push(element('div', 'cw-corner'));
    allDay.forEach((list) => {
        const cell = element('div', 'cw-allday');
        list.forEach((event) => {
            const chip = opener('button', `cw-chip cal-c${event.color}${event.holiday ? ' hol' : ''}${event.task ? ' task' : ''}`, event);
            chip.textContent = event.name;
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
        const isToday = dayKey(days[i]) === today;
        const column = element('div', 'cw-day' + (isToday ? ' today' : ''));
        list.forEach((item) => {
            const { event } = item;
            const flags = [event.tentative && 'tent', event.changed && 'moved', item.clash && 'clash', item.to - item.from < 45 && 'short'].filter(Boolean).join(' ');
            const block = opener('button', `cw-event cal-c${event.color} ${flags}`, event);
            block.style.top = `${((item.from - first) / span) * 100}%`;
            block.style.height = `${((item.to - item.from) / span) * 100}%`;
            block.style.left = `${(item.lane / item.lanes) * 100}%`;
            block.style.width = `${100 / item.lanes}%`;
            block.title = [event.name, timeRange(item.start, item.end, home), event.location, event.calendars.join(', '),
                event.tentative && 'Tentative', event.changed && `Moved ${ago(new Date(event.changed))}`, item.clash && 'Overlaps another meeting']
                .filter(Boolean).join('\n');
            const name = element('b', '', event.name);
            name.dir = 'auto';
            // A short block only has room for the name and when it starts
            const time = element('span', 'cw-time', item.to - item.from < 45 ? clock(item.start) : `${clock(item.start)} - ${clock(item.end)}`);
            if (event.link) time.prepend(videoMark());
            block.append(name, time, element('span', 'cw-where', event.location && !/^https?:/i.test(event.location) ? event.location : event.calendars.join(', ')));
            column.append(block);
        });
        if (isToday) {
            const line = element('i', 'cw-now');
            line.append(element('span', '', shortClock(new Date())));
            column.append(line);
        }
        nodes.push(column);
    });
    grid.style.setProperty('--days', days.length);
    grid.style.setProperty('--rows', span / 60);
    grid.dataset.first = first;
    grid.dataset.span = span;
    grid.replaceChildren(...nodes);
    scrollWeek(root, grid, first, span);
}

// Once per page and week shown, so rebuilds at each meeting boundary keep where Dor scrolled:
// a third of the box above now on today's week, 7 AM on other weeks
const scrolledWeeks = new WeakMap();
function scrollWeek(root, grid, first, span) {
    const box = root.querySelector('[data-cal-week-wrap]');
    if (!box || box.hidden || scrolledWeeks.get(root) === weekOffset) return;
    scrolledWeeks.set(root, weekOffset);
    const line = grid.querySelector('.cw-now');
    const column = grid.querySelector('.cw-day');
    if (!column) return;
    const minute = line ? minutesOf(new Date()) : 7 * 60;
    const top = column.getBoundingClientRect().top - box.getBoundingClientRect().top + box.scrollTop;
    const head = grid.querySelector('.cw-head')?.offsetHeight || 0;
    const y = top + ((minute - first) / span) * column.offsetHeight;
    box.scrollTop = Math.max(0, y - head - (line ? (box.clientHeight - head) / 3 : 12));
}

// The hour column only fits the digits, so AM and PM go
const shortClock = (date) => clock(date).replace(/\s*[ap]\.?m\.?$/i, '');

function nowLine(now) {
    const line = element('div', 'cw-lnow');
    line.append(element('span', '', `Now ${clock(now)}`));
    return line;
}

function buildList(root, data) {
    const list = root.querySelector('[data-cal-list]');
    const now = new Date();
    const nodes = [];
    data.days.forEach((day, i) => {
        const items = [...data.allDay[i].map((event) => ({ event })), ...data.timed[i]];
        const head = element('div', 'cw-lday' + (i === 0 ? ' today' : ''));
        const label = i === 0 ? 'Today' : i === 1 ? 'Tomorrow' : day.toLocaleDateString(undefined, { weekday: 'long' });
        const title = element('b', '', `${label}, ${day.toLocaleDateString(undefined, { day: 'numeric', month: 'short' })}`);
        if (data.marks[i].length) title.append(element('span', 'cw-mark', data.marks[i].join(', ')));
        head.append(title, element('small', data.stats[i].clashes ? 'warn' : '', dayStats(data.stats[i])));
        nodes.push(head);
        if (!items.length) {
            nodes.push(element('div', 'cw-lfree', 'Nothing booked'));
            return;
        }
        // Today gets a line at the current time, ahead of the first meeting not started yet
        const nowAt = i === 0 ? items.findIndex(({ start }) => start && start > now) : -2;
        items.forEach(({ event, start, end, clash }, index) => {
            if (index === nowAt) nodes.push(nowLine(now));
            const row = opener('button', `cw-lrow cal-c${event.color}${event.holiday ? ' hol' : ''}${event.task ? ' task' : ''}${end && end < now ? ' past' : ''}`, event);
            const time = element('span', 'cw-ltime', start ? `${clock(start)} - ${clock(end)}` : event.task ? 'Due' : 'All day');
            const body = element('span', 'cw-lbody');
            const name = element('b', '', event.name);
            name.dir = 'auto';
            const line = element('span', 'cw-lline');
            if (event.link) line.append(videoMark());
            if (event.tentative) line.append(element('em', '', 'Tentative'));
            if (event.changed) line.append(element('em', 'warn', `Moved ${ago(new Date(event.changed))}`));
            if (clash) line.append(element('em', 'warn', 'Overlaps'));
            const where = event.location && !/^https?:/i.test(event.location) ? `${event.location}, ` : '';
            line.append(`${where}${event.calendars.join(', ')}`);
            body.append(name, line);
            row.append(element('i', 'cw-lbar'), time, body);
            nodes.push(row);
        });
    });
    if (data.timed[0].length && !nodes.some((node) => node.classList.contains('cw-lnow'))) {
        // Nothing left today, so the line goes after today's last meeting
        const nextDay = nodes.findIndex((node, index) => index > 0 && node.classList.contains('cw-lday'));
        nodes.splice(nextDay === -1 ? nodes.length : nextDay, 0, nowLine(now));
    }
    list.replaceChildren(...nodes);
}

function paintWeek(root) {
    const grid = root.querySelector('[data-cal-grid]');
    if (!grid) return;
    const events = parseList(root.dataset.calEvents);
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
    if (away) setText(zone, `Times in ${place(Intl.DateTimeFormat().resolvedOptions().timeZone)}, open a meeting for ${place(home)}`);

    const view = savedView();
    root.querySelectorAll('[data-cal-view]').forEach((button) => {
        const on = String(button.dataset.calView === view);
        if (button.getAttribute('aria-pressed') !== on) button.setAttribute('aria-pressed', on);
    });
    setHidden(root.querySelector('[data-cal-week-wrap]'), view !== 'week');
    setHidden(root.querySelector('[data-cal-list]'), view !== 'list');

    // Rebuilding on every tick would fire the observer again, so only when something changed.
    // Counting the starts and ends already passed puts every meeting boundary in the key,
    // so "Now" and "Next" move on the moment a meeting starts or ends.
    const passed = events.reduce((sum, event) => sum + (new Date(event.start) <= now) + (new Date(event.end) <= now), 0);
    // The hour keeps "Moved 5 min ago" from going stale after the day's last meeting
    const days = viewDays(root, view, now);
    const nav = root.querySelector('[data-cal-weeknav]');
    setHidden(nav, view !== 'week');
    if (nav) {
        const range = `${days[0].toLocaleDateString(undefined, { month: 'short', day: 'numeric' })} - ${days[6]?.toLocaleDateString(undefined, { month: 'short', day: 'numeric' })}`;
        setText(nav.querySelector('[data-cal-week-label]'), `${weekOffset ? 'Next week' : 'This week'}, ${range}`);
        nav.querySelectorAll('[data-cal-week-step]').forEach((button) => {
            const target = weekOffset + Number(button.dataset.calWeekStep);
            const off = target < 0 || target >= WEEKS;
            if (button.disabled !== off) button.disabled = off;
        });
    }
    const key = [root.dataset.calEvents, dayKey(now), now.getHours(), passed, view, weekOffset, JSON.stringify(choices())].join('#');
    // Only the shown view is built, so check that one; the empty week grid in list view
    // otherwise rebuilt the list on every paint, and the observer looped until the tab hung
    const built = view === 'week' ? grid : root.querySelector('[data-cal-list]');
    if (grid.dataset.key !== key || !built?.firstChild) {
        const data = weekData(root, events, shown, days);
        buildSummary(root, data);
        if (view === 'week') buildWeek(root, data);
        else buildList(root, data);
        grid.dataset.key = key;
    }
    root.querySelectorAll('.cw-event, .cw-lrow').forEach((node) => {
        const past = node.dataset.calEnd !== '' && new Date(node.dataset.calEnd) < now;
        if (node.classList.contains('past') !== past) node.classList.toggle('past', past);
        const event = node.dataset.calEvent && JSON.parse(node.dataset.calEvent);
        const live = !past && !!event?.start && new Date(event.start) <= now;
        if (node.classList.contains('live') !== live) node.classList.toggle('live', live);
    });
    root.querySelectorAll('.cw-lnow span').forEach((label) => setText(label, `Now ${clock(now)}`));
    const left = root.querySelector('[data-cal-next-start]');
    if (left) {
        const start = new Date(left.dataset.calNextStart);
        const end = new Date(left.dataset.calNextEnd);
        setText(left, now >= end ? 'ended' : now >= start ? `${duration(end - now)} left` : `in ${duration(start - now)}`);
    }
    const line = grid.querySelector('.cw-now');
    if (line) {
        const top = `${((minutesOf(now) - Number(grid.dataset.first)) / Number(grid.dataset.span)) * 100}%`;
        if (line.style.top !== top) line.style.top = top;
        setText(line.firstChild, shortClock(now));
    }
}

document.addEventListener('click', (click) => {
    const step = click.target.closest('[data-cal-week-step]');
    if (step) {
        weekOffset = Math.min(WEEKS - 1, Math.max(0, weekOffset + Number(step.dataset.calWeekStep)));
        paint();
        return;
    }
    const button = click.target.closest('[data-cal-view]');
    if (!button) return;
    try {
        localStorage.setItem(VIEW, button.dataset.calView);
    } catch {
        // The choice then lasts until reload
    }
    paint();
});

export function paint() {
    document.querySelectorAll('.ck-agenda[data-cal-root]').forEach(paintAgenda);
    document.querySelectorAll('.cw[data-cal-root]').forEach(paintWeek);
    paintBells();
    tickDialog();
}

// Meeting details, so an invite can be read before its link opens Zoom or Meet
const LINK_TEXT = /(https?:\/\/[^\s<>"]+[^\s<>".,;:!?)\]]|\+\d[\d ().-]{7,}\d)/g;

function linkify(text) {
    const fragment = document.createDocumentFragment();
    text.split(LINK_TEXT).forEach((part, i) => {
        if (i % 2 === 0) {
            if (part) fragment.append(part);
            return;
        }
        const link = element('a', '', part);
        // Dial-in numbers call straight from a phone
        const phone = part.startsWith('+');
        link.href = phone ? `tel:${part.replace(/[^\d+]/g, '')}` : part;
        if (!phone) {
            link.target = '_blank';
            link.rel = 'noreferrer';
        }
        fragment.append(link);
    });
    return fragment;
}

function parseList(text) {
    try {
        return JSON.parse(text || '[]');
    } catch {
        return [];
    }
}

function rootEvents(root) {
    if (!root) return [];
    if (root.dataset.calEvents) return parseList(root.dataset.calEvents);
    return [...root.querySelectorAll('[data-cal-event]')].map((node) => parseList(node.dataset.calEvent));
}

function ago(date) {
    const minutes = Math.round((Date.now() - date) / 60000);
    if (minutes < 60) return `${Math.max(1, minutes)} min ago`;
    if (minutes < 1440) return `${Math.round(minutes / 60)} h ago`;
    return `${Math.round(minutes / 1440)} days ago`;
}

function countdown(start, end) {
    const now = new Date();
    if (now >= end) return 'Ended';
    if (now >= start) return `Happening now, ${duration(end - now)} left`;
    if (start - now < 24 * 60 * 60 * 1000) return `Starts in ${duration(start - now)}`;
    const days = Math.round((start - now) / 86400000);
    return `Starts in ${days} ${days === 1 ? 'day' : 'days'}`;
}

function section(label, ...children) {
    const node = element('div', 'cal-dsec');
    node.append(element('div', 'cal-dlabel', label), ...children);
    return node;
}

function joinLabel(link) {
    let host = '';
    try {
        host = new URL(link).hostname;
    } catch {
        return 'Join';
    }
    if (host.endsWith('meet.google.com')) return 'Join Google Meet';
    if (host.endsWith('zoom.us')) return 'Join Zoom';
    if (host.includes('teams.')) return 'Join Teams';
    if (host.includes('discord')) return 'Open Discord';
    return `Join on ${host}`;
}

const ANSWERS = {
    accepted: { word: 'Going', you: 'You are going', cls: 'yes' },
    tentative: { word: 'Maybe', you: 'You said maybe', cls: 'maybe' },
    declined: { word: 'Not going', you: 'You declined', cls: 'no' },
    needsAction: { word: 'No reply', you: 'You have not answered', cls: 'wait' },
};
const ANSWER_ORDER = ['accepted', 'tentative', 'needsAction', 'declined'];

function link(className, text, href) {
    const node = element('a', className, text);
    node.href = href;
    if (/^https?:/i.test(href)) {
        node.target = '_blank';
        node.rel = 'noreferrer';
    }
    return node;
}

// Who is invited and what each answered, organizer first
function peopleSection(event) {
    const answerOf = (person) => person.status || 'needsAction';
    const people = [...event.attendees].sort((a, b) => Number(!!b.organizer) - Number(!!a.organizer)
        || ANSWER_ORDER.indexOf(answerOf(a)) - ANSWER_ORDER.indexOf(answerOf(b)));
    const tally = people.length < 2 ? [] : ANSWER_ORDER
        .map((status) => [status, people.filter((person) => answerOf(person) === status).length])
        .filter(([, n]) => n)
        .map(([status, n]) => `${n} ${ANSWERS[status].word.toLowerCase()}`);
    const list = element('ul', 'cal-dpeople');
    people.forEach((person) => {
        const answer = ANSWERS[answerOf(person)] || ANSWERS.needsAction;
        const row = element('li');
        const name = person.name || (person.email || '?').split('@')[0];
        const avatar = element('span', `cal-dav ${answer.cls}`, name.slice(0, 1).toUpperCase());
        if (person.photo) {
            const photo = element('img');
            photo.src = person.photo;
            photo.alt = '';
            photo.referrerPolicy = 'no-referrer';
            // A picture that fails to load leaves the letter
            photo.addEventListener('error', () => photo.remove());
            avatar.append(photo);
        }
        row.append(avatar);
        const who = element('span', 'cal-dwho');
        const top = element('span', 'cal-dname');
        top.append(element('b', '', person.self ? `${name} (you)` : name));
        if (person.organizer) top.append(element('em', '', 'Organizer'));
        if (person.optional) top.append(element('em', '', 'Optional'));
        who.append(top);
        if (person.email && person.email !== name) {
            who.append(person.self ? element('small', '', person.email) : link('cal-dmail', person.email, `mailto:${person.email}`));
        }
        if (person.comment) who.append(element('q', '', person.comment));
        row.append(who, element('span', `cal-dans ${answer.cls}`, person.status ? answer.word : ''));
        list.append(row);
    });
    const others = people.filter((person) => !person.self && person.email).map((person) => person.email);
    const label = element('div', 'cal-dlabel');
    label.append(`People${tally.length ? `, ${tally.join(', ')}` : ''}`);
    if (others.length > 1) label.append(' ', link('cal-dmail', 'Email everyone', `mailto:${others.join(',')}?subject=${encodeURIComponent(event.name)}`));
    const node = element('div', 'cal-dsec');
    node.append(label, list);
    return node;
}

// Going, Maybe and No buttons that answer the invite in Google for Dor
function answerButtons(event, root) {
    const box = element('div', 'cal-danswer');
    const status = element('span', 'cal-dastatus');
    box.append(element('span', 'cal-dalabel', 'Going?'));
    [['accepted', 'Yes'], ['tentative', 'Maybe'], ['declined', 'No']].forEach(([answer, label]) => {
        const button = element('button', `cal-dabtn ${ANSWERS[answer].cls}`, label);
        button.type = 'button';
        button.setAttribute('aria-pressed', String(event.response === answer));
        button.addEventListener('click', async () => {
            if (event.response === answer) return;
            box.querySelectorAll('button').forEach((other) => { other.disabled = true; });
            status.textContent = 'Saving...';
            try {
                const response = await fetch(`${BASE}/api/calendar/respond`, {
                    method: 'POST',
                    headers: { 'Content-Type': 'application/json', 'X-Luna-Calendar': '1' },
                    body: JSON.stringify({ id: event.respond, answer }),
                });
                if (!response.ok) throw new Error((await response.text()).trim() || `HTTP ${response.status}`);
                event.response = answer;
                (event.attendees || []).forEach((person) => { if (person.self) person.status = answer; });
                openDetails(event, root);
                if (answer === 'declined') {
                    setText(dialog.querySelector('.cal-dastatus'), 'Declined. Google told the organizer, and it leaves Luna on the next refresh.');
                } else {
                    setText(dialog.querySelector('.cal-dastatus'), 'Saved. Google told the organizer.');
                }
            } catch (error) {
                status.textContent = `Could not save: ${error.message}`;
                box.querySelectorAll('button').forEach((other) => { other.disabled = false; });
            }
        });
        box.append(button);
    });
    box.append(status);
    return box;
}

let dialog = null;

function openDetails(event, root) {
    if (!dialog) {
        dialog = element('dialog', 'cal-dialog');
        dialog.addEventListener('click', (click) => {
            if (click.target === dialog) dialog.close();
        });
        document.body.append(dialog);
    }
    const home = root?.dataset.calHome;
    const close = element('button', 'cal-dclose', '×');
    close.type = 'button';
    close.setAttribute('aria-label', 'Close');
    close.addEventListener('click', () => dialog.close());
    const title = element('h3', '', event.name);
    title.dir = 'auto';
    const head = element('div', 'cal-dhead');
    head.append(title, close);
    const nodes = [head];
    const chips = element('div', 'cal-dchips');
    if (event.response) {
        const answer = ANSWERS[event.response] || ANSWERS.needsAction;
        chips.append(element('span', `cal-dchip ${answer.cls}`, answer.you));
    } else if (event.tentative) chips.append(element('span', 'cal-dchip', 'Tentative'));
    if (event.recurring) chips.append(element('span', 'cal-dchip', 'Repeats'));
    if (event.task) chips.append(element('span', 'cal-dchip', 'Task'));
    if (event.changed) chips.append(element('span', 'cal-dchip warn', `Moved ${ago(new Date(event.changed))}`));
    if (chips.childNodes.length) nodes.push(chips);

    const longDate = (date) => date.toLocaleDateString(undefined, { weekday: 'long', day: 'numeric', month: 'long' });
    let start = null;
    let end = null;
    delete dialog.dataset.start;
    if (event.day) {
        const first = new Date(`${event.day}T12:00:00`);
        const last = addDays(new Date(`${event.endDay}T12:00:00`), -1);
        nodes.push(element('div', 'cal-dwhen', dayKey(first) === dayKey(last) ? `${longDate(first)}, all day` : `${longDate(first)} to ${longDate(last)}`));
    } else {
        start = new Date(event.start);
        end = new Date(event.end);
        nodes.push(element('div', 'cal-dwhen', `${longDate(start)}, ${clock(start)} - ${clock(end)}, ${duration(end - start)}`));
        const left = element('div', 'cal-dleft', countdown(start, end));
        left.dataset.calDleft = '';
        dialog.dataset.start = event.start;
        dialog.dataset.end = event.end;
        nodes.push(left);

        // The same meeting in each zone that matters, checked against the device's own clock
        const zones = [{ zone: home, label: place(home || '') }, ...parseList(root?.dataset.calZones)];
        const seen = new Set([`${clock(start)}|${dayKey(start)}`]);
        const rows = element('div', 'cal-dzones');
        zones.forEach(({ zone, label }) => {
            if (!zone) return;
            const day = start.toLocaleDateString('en-CA', { timeZone: zone });
            const key = `${clock(start, zone)}|${day}`;
            if (seen.has(key)) return;
            seen.add(key);
            const shift = day === dayKey(start) ? '' : day > dayKey(start) ? ', next day' : ', day before';
            const row = element('span');
            row.append(element('b', '', label), `${clock(start, zone)} - ${clock(end, zone)}${shift}`);
            rows.append(row);
        });
        if (rows.childNodes.length) nodes.push(rows);
    }

    const cals = element('div', `cal-dcals cal-c${event.color}`);
    cals.append(element('i'), event.calendars.join(', '));
    nodes.push(cals);

    if (event.link || event.open) {
        const actions = element('div', 'cal-dactions');
        if (event.link) {
            const join = element('a', 'cal-djoin', joinLabel(event.link));
            join.href = event.link;
            join.target = '_blank';
            join.rel = 'noreferrer';
            const copy = element('button', 'cal-dbtn', 'Copy link');
            copy.type = 'button';
            copy.addEventListener('click', () => {
                navigator.clipboard?.writeText(event.link).then(() => { copy.textContent = 'Copied'; }, () => { copy.textContent = 'Copy failed'; });
            });
            actions.append(join, copy);
        }
        if (event.open) actions.append(link('cal-dbtn', event.task ? 'Open in Google Tasks' : 'Open in Google Calendar', event.open));
        nodes.push(actions);
        if (event.dial) {
            const dial = element('div', 'cal-ddial');
            dial.append('Or call ', link('', event.dial.number, event.dial.uri.split(',')[0]), event.dial.pin ? `, PIN ${event.dial.pin}#` : '');
            nodes.push(dial);
        }
    }

    if (event.location && event.location !== event.link) {
        const where = element('div');
        where.append(linkify(event.location));
        if (!/^https?:/i.test(event.location)) {
            const map = element('a', 'cal-dmap', 'Map');
            map.href = `https://www.google.com/maps/search/?api=1&query=${encodeURIComponent(event.location)}`;
            map.target = '_blank';
            map.rel = 'noreferrer';
            where.append(' ', map);
        }
        nodes.push(section('Where', where));
    }

    if (event.respond && start && end > new Date()) nodes.push(answerButtons(event, root));
    if (event.attendees?.length) nodes.push(peopleSection(event));
    if (event.files?.length) {
        const files = element('ul', 'cal-dfiles');
        event.files.forEach((file) => {
            const item = element('li');
            const anchor = link('', '', file.url);
            if (file.icon) {
                const icon = element('img');
                icon.src = file.icon;
                icon.alt = '';
                anchor.append(icon);
            }
            anchor.append(file.title || 'Attachment');
            item.append(anchor);
            files.append(item);
        });
        nodes.push(section('Files', files));
    }

    if (start) {
        const shown = root ? shownFor(root) : () => true;
        const clashes = event.quiet ? [] : rootEvents(root).filter((other) => other.start && other.name !== event.name && !other.quiet
            && new Date(other.start) < end && new Date(other.end) > start && other.calendars.some(shown));
        if (clashes.length) {
            const list = element('ul', 'cal-dclash');
            clashes.forEach((other) => list.append(element('li', '', `${other.name}, ${clock(new Date(other.start))} - ${clock(new Date(other.end))}`)));
            nodes.push(section('Overlaps with', list));
        }
        const minutes = parseList(root?.dataset.calRemind);
        let reminder;
        if (!minutes.length) reminder = 'No reminders set up';
        else if (event.quiet) reminder = 'No reminders for this calendar';
        else if (!('Notification' in window)) reminder = 'This browser cannot show reminders';
        else if (Notification.permission !== 'granted') reminder = 'Reminders are off in this browser';
        else {
            const all = [...new Set([...minutes, ...(event.reminders || [])])].sort((a, b) => b - a);
            reminder = `Reminder ${all.join(', ').replace(/, (\d+)$/, ' and $1')} min before`;
        }
        nodes.push(element('div', 'cal-dremind', reminder));
    }

    if (event.notes) {
        const notes = element('div', 'cal-dnotes');
        notes.dir = 'auto';
        notes.append(linkify(event.notes));
        nodes.push(section('Notes', notes));
    }
    dialog.replaceChildren(...nodes);
    if (!dialog.open) dialog.showModal();
    close.focus();
}

function tickDialog() {
    if (!dialog?.open || !dialog.dataset.start) return;
    setText(dialog.querySelector('[data-cal-dleft]'), countdown(new Date(dialog.dataset.start), new Date(dialog.dataset.end)));
}

document.addEventListener('click', (click) => {
    const opener = click.target.closest('[data-cal-open]');
    if (!opener) return;
    try {
        openDetails(JSON.parse(opener.closest('[data-cal-event]').dataset.calEvent), opener.closest('[data-cal-root]'));
    } catch (error) {
        // A widget update can swap the node mid-click, the next click works
        console.error('Calendar details failed', error);
    }
});

// Reminders are browser notifications from whichever Luna tab is open. Tabs share what was
// already sent through storage, so two open tabs notify once.
const SENT = 'luna-reminded';
const LATE = 2 * 60 * 1000;

function sentList() {
    try {
        return JSON.parse(localStorage.getItem(SENT)) || {};
    } catch {
        return {};
    }
}

// Android Chrome throws on new Notification() and only shows them from a service worker.
// Every browser goes through the worker when it registers; the constructor is the fallback.
const BASE = new URL(import.meta.url).pathname.split('/static/')[0];
let worker = null;
let notifyFailed = false;
// Waits on this registration rather than serviceWorker.ready, which never resolves on a page
// outside the scope (such as /luna without the trailing slash)
const workerReady = !('serviceWorker' in navigator) ? Promise.resolve()
    : navigator.serviceWorker.register(`${BASE}/calendar-sw.js`, { scope: `${BASE}/` })
        .then((registration) => {
            const pending = registration.installing || registration.waiting;
            if (registration.active || !pending) return registration;
            return new Promise((resolve) => pending.addEventListener('statechange', () => {
                if (pending.state === 'activated') resolve(registration);
            }));
        })
        .then((registration) => { if (registration.active) worker = registration; })
        .catch(() => {});
if ('serviceWorker' in navigator) {
    navigator.serviceWorker.addEventListener('message', (message) => {
        const wanted = message.data?.calendarOpen;
        if (!wanted) return;
        for (const root of document.querySelectorAll('[data-cal-root]')) {
            const event = rootEvents(root).find((item) => `${item.name}|${item.start}` === wanted);
            if (event) return openDetails(event, root);
        }
    });
}

// Resolves to whether the browser showed it
function notify(event, root, title, options) {
    const failed = () => {
        notifyFailed = true;
        paintBells();
        return false;
    };
    if (worker) {
        return worker.showNotification(title, { ...options, data: { event: `${event.name}|${event.start}`, page: location.href } })
            .then(() => true, failed);
    }
    try {
        const note = new Notification(title, options);
        note.onclick = () => {
            window.focus();
            openDetails(event, root);
            note.close();
        };
        return Promise.resolve(true);
    } catch {
        return Promise.resolve(failed());
    }
}

function saveSent(sent) {
    try {
        localStorage.setItem(SENT, JSON.stringify(sent));
    } catch {
        // Without storage each tab reminds on its own
    }
}

function remind() {
    if (!('Notification' in window) || Notification.permission !== 'granted') return;
    const now = Date.now();
    const sent = sentList();
    let changed = false;
    document.querySelectorAll('[data-cal-root]').forEach((root) => {
        const shown = shownFor(root);
        const usual = parseList(root.dataset.calRemind);
        rootEvents(root).forEach((event) => {
            // Times Dor set on the meeting in Google come on top of the usual ones
            [...new Set([...usual, ...(event.reminders || [])])].forEach((minutes) => {
                if (!event.start || event.quiet || event.holiday || !event.calendars.some(shown)) return;
                const start = new Date(event.start);
                const at = start - minutes * 60000;
                const key = `${event.name}|${event.start}|${minutes}`;
                if (now < at || now - at > LATE || sent[key]) return;
                sent[key] = start.getTime();
                changed = true;
                notify(event, root, event.name, {
                    body: `In ${minutes} min, ${timeRange(start, new Date(event.end), root.dataset.calHome)}\n${event.calendars.join(', ')}`,
                    tag: key,
                    requireInteraction: minutes <= 1,
                }).then((shown) => {
                    if (shown) return;
                    // Not shown, so another tab or a later tick may still try
                    const latest = sentList();
                    delete latest[key];
                    saveSent(latest);
                });
            });
        });
    });
    Object.keys(sent).forEach((key) => {
        if (now - sent[key] > 24 * 60 * 60 * 1000) {
            delete sent[key];
            changed = true;
        }
    });
    if (changed) saveSent(sent);
}

function paintBells() {
    document.querySelectorAll('[data-cal-notify]').forEach((bell) => {
        const root = bell.closest('[data-cal-root]');
        const minutes = parseList(root?.dataset.calRemind);
        const supported = 'Notification' in window && minutes.length > 0;
        setHidden(bell, !supported);
        if (!supported) return;
        const state = notifyFailed ? 'failed' : Notification.permission;
        const labels = { granted: 'Reminders on', denied: 'Reminders blocked', failed: 'Reminders not working here' };
        setText(bell, labels[state] || 'Turn on reminders');
        const tip = state === 'denied' ? 'Allow notifications for this site in the browser settings'
            : state === 'failed' ? 'This browser refused to show a reminder. Try Luna in another browser.'
                : `${minutes.join(' and ')} min before each meeting, while a Luna tab is open`;
        if (bell.title !== tip) bell.title = tip;
        if (bell.classList.contains('on') !== (state === 'granted')) bell.classList.toggle('on', state === 'granted');
    });
}

document.addEventListener('click', (click) => {
    if (!click.target.closest('[data-cal-notify]') || Notification.permission !== 'default') return;
    Notification.requestPermission().then(() => {
        paintBells();
        remind();
    });
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
// The first reminder waits for the worker, or Android drops it; 5 seconds caps the wait
Promise.race([workerReady, new Promise((resolve) => setTimeout(resolve, 5000))]).then(() => {
    remind();
    setInterval(remind, 20000);
});
