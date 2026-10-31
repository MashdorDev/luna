// Home page behaviour. Uses event delegation because widget updates morph the DOM.
let openGroup = null;
let cursor = -1;
let query = '';

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

function paintCursor() {
    document.querySelectorAll('.cockpit .cur').forEach((node) => node.classList.remove('cur'));
    const list = targets();
    if (cursor >= list.length) cursor = list.length - 1;
    if (cursor > -1 && list[cursor]) {
        list[cursor].classList.add('cur');
        list[cursor].scrollIntoView({ block: 'nearest' });
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
    results.replaceChildren(...hits.map((item) => item.cloneNode(true)));
    if (!hits.length && !document.querySelector('.ck-app.hit')) {
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

function tickClock() {
    const now = new Date();
    const time = (zone) => now.toLocaleTimeString('en-GB', { hour: '2-digit', minute: '2-digit', timeZone: zone });
    document.querySelectorAll('[data-ck-clock]').forEach((n) => setText(n, time()));
    document.querySelectorAll('[data-ck-date]').forEach((n) => {
        setText(n, now.toLocaleDateString('en-GB', { weekday: 'long', day: 'numeric', month: 'long' }));
    });
    document.querySelectorAll('[data-ck-zone]').forEach((n) => setText(n, time(n.dataset.ckZone)));
    document.querySelectorAll('[data-ck-now]').forEach((n) => {
        n.style.left = ((now.getHours() * 60 + now.getMinutes()) / 14.4) + '%';
    });
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
    query = event.target.value.trim().toLowerCase();
    cursor = -1;
    runSearch();
});

document.addEventListener('keydown', (event) => {
    if (!root() || event.metaKey || event.ctrlKey || event.altKey) return;
    const inSearch = event.target.id === 'ck-q';

    if (inSearch) {
        if (event.key === 'ArrowDown' || event.key === 'ArrowUp') {
            event.preventDefault();
            cursor = Math.max(-1, cursor + (event.key === 'ArrowDown' ? 1 : -1));
            paintCursor();
        } else if (event.key === 'Enter' && query) {
            const list = targets();
            const pick = list[Math.max(cursor, 0)];
            window.open(pick ? pick.href : 'https://duckduckgo.com/?q=' + encodeURIComponent(event.target.value), '_blank', 'noreferrer');
        } else if (event.key === 'Escape') {
            event.target.value = '';
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
    restoreOpen();
    tickClock();
    if (query) {
        const results = document.getElementById('ck-results');
        if (results && results.hidden) runSearch();
    }
    if (cursor > -1 && !document.querySelector('.cockpit .cur')) paintCursor();
}).observe(document.documentElement, { childList: true, subtree: true });

tickClock();
setInterval(tickClock, 15000);
