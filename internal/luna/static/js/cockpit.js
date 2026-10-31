// Home page behaviour. Uses event delegation because widget updates morph the DOM.
let openGroup = null;
let cursor = -1;
let query = '';
let typed = '';
let rawTyped = '';
let caret = 0;

// Not web addresses even though they look like one: next.js, README.md, package.json
const FILE_ENDINGS = /\.(js|ts|jsx|tsx|md|json|py|go|rs|rb|txt|html|css|yml|yaml|sh|java|php|sql|csv|pdf|png|jpg|gif|svg|zip)$/i;

// google.com, docs.dorzairi.com/x, https://a.b, localhost:3000 and 10.0.0.200:8000 go straight to the site.
// Local addresses stay on http because they rarely have certificates.
function addressFrom(text) {
    if (/^https?:\/\/\S+$/i.test(text)) return text;
    if (/^(localhost|\d{1,3}(\.\d{1,3}){3})(:\d+)?(\/\S*)?$/i.test(text)) return 'http://' + text;
    if (FILE_ENDINGS.test(text)) return null;
    if (/^([a-z0-9-]+\.)+[a-z]{2,}(:\d+)?([/?#]\S*)?$/i.test(text)) return 'https://' + text;
    return null;
}

const root = () => document.querySelector('.cockpit');

function restoreOpen() {
    document.querySelectorAll('.ck-row').forEach((row) => {
        const open = row.dataset.g === openGroup;
        if (row.classList.contains('open') !== open) row.classList.toggle('open', open);
        const line = row.querySelector('.ck-line');
        if (line.getAttribute('aria-expanded') !== String(open)) line.setAttribute('aria-expanded', String(open));
    });
}

function targets() {
    if (query) return [...document.querySelectorAll('#ck-results .ck-item, .ck-app.hit')];
    return [...document.querySelectorAll('#ck-board .ck-line')];
}

function paintCursor(scroll = true) {
    document.querySelectorAll('.cockpit .cur').forEach((node) => node.classList.remove('cur'));
    const list = targets();
    if (cursor >= list.length) cursor = list.length - 1;
    if (cursor > -1 && list[cursor]) {
        list[cursor].classList.add('cur');
        if (scroll) list[cursor].scrollIntoView({ block: 'nearest' });
    }
}

function runSearch() {
    const results = document.getElementById('ck-results');
    const board = document.getElementById('ck-board');
    if (!results || !board) return;

    document.querySelectorAll('.ck-app').forEach((app) => {
        app.classList.toggle('hit', !!query && app.textContent.toLowerCase().includes(query));
    });

    if (!query) {
        results.hidden = true;
        results.replaceChildren();
        board.hidden = false;
        paintCursor();
        return;
    }

    const hits = [...board.querySelectorAll('.ck-item')].filter((item) => item.textContent.toLowerCase().includes(query));
    const rows = hits.map((item) => item.cloneNode(true));
    const address = addressFrom(typed);
    if (address) {
        const go = document.createElement('a');
        go.className = 'ck-item ck-go';
        go.href = address;
        const name = document.createElement('b');
        name.textContent = 'Go to ' + typed;
        go.append(name);
        rows.unshift(go);
    }
    results.replaceChildren(...rows);
    if (!rows.length && !document.querySelector('.ck-app.hit')) {
        const empty = document.createElement('div');
        empty.className = 'ck-empty';
        empty.textContent = 'Enter to search the web';
        results.append(empty);
    }
    results.hidden = false;
    board.hidden = true;
    paintCursor();
}

function setText(node, text) {
    if (node.textContent !== text) node.textContent = text;
}

// Everything time related follows the device: its time zone, language and 12 or 24 hour setting
const viewerZone = () => Intl.DateTimeFormat().resolvedOptions().timeZone;
const clockText = (date, zone) => date.toLocaleTimeString(undefined, { hour: 'numeric', minute: '2-digit', timeZone: zone });
const hourParts = (date, zone) => {
    const parts = new Intl.DateTimeFormat('en-US', { hour: 'numeric', minute: 'numeric', hourCycle: 'h23', timeZone: zone }).formatToParts(date);
    const part = (type) => Number(parts.find((p) => p.type === type).value);
    return part('hour') * 60 + part('minute');
};
const localDay = (date = new Date()) => {
    const pad = (n) => String(n).padStart(2, '0');
    return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())}`;
};
// Noon keeps a calendar date on its own day in every time zone
const dateFrom = (day) => new Date(`${day}T12:00:00`);
const daysFromToday = (day) => Math.round((dateFrom(day) - dateFrom(localDay())) / 86400000);

function tickClock() {
    const now = new Date();
    document.querySelectorAll('[data-ck-clock]').forEach((n) => setText(n, clockText(now)));
    document.querySelectorAll('[data-ck-date]').forEach((n) => {
        setText(n, now.toLocaleDateString(undefined, { weekday: 'long', day: 'numeric', month: 'long' }));
    });
    document.querySelectorAll('[data-ck-zone]').forEach((n) => {
        setText(n, clockText(now, n.dataset.ckZone));
        // No point listing the zone the device is already in
        n.parentElement.hidden = n.dataset.ckZone === viewerZone() || hourParts(now, n.dataset.ckZone) === hourParts(now);
    });
    document.querySelectorAll('[data-ck-now]').forEach((n) => { n.style.left = (hourParts(now) / 14.4) + '%'; });
    document.querySelectorAll('[data-ck-at]').forEach((n) => {
        const at = new Date(n.dataset.ckAt);
        setText(n, `${at.toLocaleDateString(undefined, { weekday: 'short', day: 'numeric', month: 'short' })}, ${clockText(at)}`);
    });
    document.querySelectorAll('[data-ck-day-only]').forEach((n) => {
        setText(n, dateFrom(n.dataset.ckDayOnly).toLocaleDateString(undefined, { weekday: 'short', day: 'numeric', month: 'short' }));
    });
    document.querySelectorAll('[data-ck-day]').forEach((n) => {
        const date = dateFrom(n.dataset.ckDay);
        setText(n, date.toLocaleDateString(undefined, { weekday: 'narrow' }));
        n.classList.toggle('now', n.dataset.ckDay === localDay());
    });
    document.querySelectorAll('[data-ck-due]').forEach((n) => {
        const days = daysFromToday(n.dataset.ckDue);
        const when = days < 0 ? `${-days}d late` : days === 0 ? 'due today' : `due in ${days}d`;
        setText(n, n.dataset.ckPrefix ? `${n.dataset.ckPrefix}, ${when}` : when);
    });
    document.querySelectorAll('[data-ck-noted]').forEach((n) => {
        const days = daysFromToday(n.dataset.ckNoted);
        const plural = Math.abs(days) === 1 ? 'day' : 'days';
        // The row is sorted by the server's day, so near midnight in another zone days can be off by one
        const text = n.dataset.ckNote === 'oldest'
            ? (days < 0 ? `Oldest is ${-days} ${plural} overdue` : 'Due today')
            : (days > 0 ? `next due in ${days} ${plural}` : days === 0 ? 'next due today' : `next was due ${-days} ${plural} ago`);
        setText(n, 'ckAfter' in n.dataset ? `, ${text.charAt(0).toLowerCase()}${text.slice(1)}` : text.charAt(0).toUpperCase() + text.slice(1));
    });
    paintSky();
}

const WEATHER_WORDS = { 0: 'clear sky', 1: 'mainly clear', 2: 'partly cloudy', 3: 'overcast', 45: 'fog', 48: 'rime fog', 51: 'light drizzle', 53: 'drizzle', 55: 'heavy drizzle', 56: 'freezing drizzle', 57: 'freezing drizzle', 61: 'light rain', 63: 'rain', 65: 'heavy rain', 66: 'freezing rain', 67: 'freezing rain', 71: 'light snow', 73: 'snow', 75: 'heavy snow', 77: 'snow grains', 80: 'light showers', 81: 'showers', 82: 'heavy showers', 85: 'snow showers', 86: 'heavy snow showers', 95: 'thunderstorm', 96: 'thunderstorm with hail', 99: 'thunderstorm with hail' };
const SKY_EVERY = 30 * 60 * 1000;
let sky = null;
let skyAsked = false;

function paintSky() {
    if (!sky) return;
    document.querySelectorAll('[data-ck-sky]').forEach((n) => setText(n, `, ${sky.temperature}° ${sky.words}`));
    const rise = new Date(sky.sunrise * 1000);
    const set = new Date(sky.sunset * 1000);
    document.querySelectorAll('[data-ck-daybar]').forEach((bar) => {
        bar.hidden = false;
        const light = bar.querySelector('u');
        light.style.left = (hourParts(rise) / 14.4) + '%';
        light.style.width = (Math.max(0, hourParts(set) - hourParts(rise)) / 14.4) + '%';
    });
    document.querySelectorAll('[data-ck-daylab]').forEach((n) => { n.hidden = false; });
    document.querySelectorAll('[data-ck-rise]').forEach((n) => setText(n, `sunrise ${clockText(rise)}`));
    document.querySelectorAll('[data-ck-set]').forEach((n) => setText(n, `sunset ${clockText(set)}`));
}

// The city comes from the device's time zone, so there is no location prompt and nothing to configure
async function loadSky() {
    // The widget can still be loading right after a Luna restart, the observer calls this again
    if (!root()) return;
    skyAsked = true;
    const zone = viewerZone();
    const key = `ck-sky-${zone}`;
    try {
        const saved = JSON.parse(localStorage.getItem(key));
        if (saved && Date.now() - saved.at < SKY_EVERY) {
            sky = saved;
            paintSky();
            return;
        }
    } catch { /* storage can be blocked, fetching still works */ }
    try {
        const city = zone.split('/').pop().replaceAll('_', ' ');
        const places = await (await fetch(`https://geocoding-api.open-meteo.com/v1/search?count=10&name=${encodeURIComponent(city)}`)).json();
        // Only a place in this exact zone: a similar name could be anywhere, like "Eastern" or "UTC"
        const place = (places.results || []).find((p) => p.timezone === zone);
        if (!place) return;
        const fahrenheit = new Intl.Locale(navigator.language).region === 'US';
        const query = `latitude=${place.latitude}&longitude=${place.longitude}&timezone=${encodeURIComponent(zone)}&timeformat=unixtime&forecast_days=1&current=temperature_2m,weather_code&daily=sunrise,sunset${fahrenheit ? '&temperature_unit=fahrenheit' : ''}`;
        const data = await (await fetch(`https://api.open-meteo.com/v1/forecast?${query}`)).json();
        sky = {
            at: Date.now(), temperature: Math.round(data.current.temperature_2m),
            words: WEATHER_WORDS[data.current.weather_code] || '', sunrise: data.daily.sunrise[0], sunset: data.daily.sunset[0],
        };
        paintSky();
        try { localStorage.setItem(key, JSON.stringify(sky)); } catch { /* per-device cache only */ }
    } catch (error) {
        console.error('Weather failed', error);
    }
}

document.addEventListener('click', (event) => {
    const line = event.target.closest('.ck-line');
    if (!line) return;
    const group = line.closest('.ck-row').dataset.g;
    openGroup = openGroup === group ? null : group;
    restoreOpen();
});

document.addEventListener('input', (event) => {
    if (event.target.id !== 'ck-q') return;
    rawTyped = event.target.value;
    caret = event.target.selectionStart;
    typed = rawTyped.trim();
    query = typed.toLowerCase();
    cursor = -1;
    runSearch();
});

document.addEventListener('keydown', (event) => {
    if (!root() || event.metaKey || event.ctrlKey || event.altKey) return;
    const inSearch = event.target.id === 'ck-q';
    if (inSearch) queueMicrotask(() => { caret = event.target.selectionStart; });

    if (inSearch) {
        if (event.key === 'ArrowDown' || event.key === 'ArrowUp') {
            event.preventDefault();
            cursor = Math.max(-1, cursor + (event.key === 'ArrowDown' ? 1 : -1));
            paintCursor();
        } else if (event.key === 'Enter' && query) {
            const pick = targets()[Math.max(cursor, 0)];
            if (pick && pick.classList.contains('ck-go')) window.location.assign(pick.href);
            else if (pick) window.open(pick.href, '_blank', 'noreferrer');
            else window.location.assign(root().dataset.ckSearch.replace('{QUERY}', encodeURIComponent(typed)));
        } else if (event.key === 'Enter' && cursor > -1) {
            event.preventDefault();
            targets()[cursor]?.click();
        } else if (event.key === 'Escape') {
            event.target.value = '';
            typed = '';
            rawTyped = '';
            query = '';
            event.target.blur();
            runSearch();
        }
        return;
    }

    if (event.target.matches('input, textarea, select')) return;
    if (event.key === '/') {
        event.preventDefault();
        document.getElementById('ck-q').focus();
    } else if (event.key === 'j' || event.key === 'k') {
        cursor = Math.max(0, cursor + (event.key === 'j' ? 1 : -1));
        paintCursor();
    } else if (event.key === 'Enter' && cursor > -1 && !event.target.closest('button, a')) {
        const pick = targets()[cursor];
        if (pick) pick.click();
    } else if (event.key === 'Escape') {
        openGroup = null;
        restoreOpen();
    }
});

new MutationObserver(() => {
    if (!root()) return;
    // A widget update can rebuild the box, which would lose what was being typed
    const box = document.getElementById('ck-q');
    if (box && rawTyped && !box.value) {
        box.value = rawTyped;
        box.setSelectionRange(caret, caret);
    }
    restoreOpen();
    tickClock();
    if (!skyAsked) loadSky();
    if (query) {
        const results = document.getElementById('ck-results');
        if (results && results.hidden) runSearch();
    }
    if (cursor > -1 && !document.querySelector('.cockpit .cur')) paintCursor(false);
}).observe(document.documentElement, { childList: true, subtree: true });

tickClock();
setInterval(tickClock, 15000);
loadSky();
setInterval(loadSky, SKY_EVERY);
if (window.matchMedia('(min-width: 861px)').matches) document.getElementById('ck-q')?.focus();
