// yas web UI — search + timeline page. A client of the /v1 JSON contract
// only: the sole data path is same-origin fetch('/v1/search?...'). All view
// state lives in the URL query string (?q=<raw search input>), so every
// search is a shareable link.
import { parseTokens } from './tokens.js';

const PAGE_SIZE = 50;

const box = document.getElementById('search-box');
const form = document.getElementById('search-form');
const timeline = document.getElementById('timeline');
const status = document.getElementById('status');
const sentinel = document.getElementById('sentinel');

// One generation counter guards against out-of-order responses: only the
// newest search's pages may touch the DOM.
let generation = 0;
let offset = 0;
let exhausted = false;
let loading = false;

function queryFromBox() {
  return new URLSearchParams(parseTokens(box.value));
}

async function loadPage() {
  if (loading || exhausted) return;
  loading = true;
  const gen = generation;
  const params = queryFromBox();
  params.set('limit', String(PAGE_SIZE));
  params.set('offset', String(offset));
  status.textContent = offset === 0 ? 'searching…' : 'loading more…';
  try {
    const resp = await fetch('/v1/search?' + params);
    if (gen !== generation) return;
    if (!resp.ok) {
      const body = await resp.json().catch(() => ({}));
      status.textContent = `error: ${body.error || resp.status}`;
      exhausted = true;
      return;
    }
    const { records } = await resp.json();
    if (gen !== generation) return;
    for (const rec of records) timeline.append(renderRecord(rec));
    offset += records.length;
    if (records.length < PAGE_SIZE) exhausted = true;
    status.textContent = '';
    if (offset === 0) {
      status.textContent = '=^..^=  no matching history';
      status.classList.add('empty-state');
    } else {
      status.classList.remove('empty-state');
    }
  } catch (err) {
    if (gen === generation) status.textContent = `error: ${err.message}`;
  } finally {
    if (gen === generation) loading = false;
  }
}

function newSearch() {
  generation++;
  offset = 0;
  exhausted = false;
  loading = false;
  timeline.replaceChildren();
  loadPage();
}

function el(tag, className, text) {
  const node = document.createElement(tag);
  if (className) node.className = className;
  if (text !== undefined) node.textContent = text;
  return node;
}

function renderRecord(rec) {
  const li = el('li', 'record');
  const cmd = el('code', 'command', rec.command);
  const meta = el('span', 'meta');
  const exit = rec.exit_code;
  if (exit !== undefined && exit !== null) {
    meta.append(el('span', exit === 0 ? 'badge ok' : 'badge fail', String(exit)));
  } else {
    meta.append(el('span', 'badge running', '…'));
  }
  if (rec.hostname) meta.append(el('span', 'host', rec.hostname));
  if (rec.cwd) meta.append(el('span', 'cwd', rec.cwd));
  if (rec.duration_ms !== undefined && rec.duration_ms !== null) {
    meta.append(el('span', 'duration', humanDuration(rec.duration_ms)));
  }
  const when = el('time', 'when', relativeTime(rec.start_time));
  when.dateTime = rec.start_time;
  when.title = rec.start_time;
  meta.append(when);
  li.append(cmd, meta);
  return li;
}

export function humanDuration(ms) {
  if (ms < 1000) return `${ms}ms`;
  if (ms < 60_000) return `${(ms / 1000).toFixed(1).replace(/\.0$/, '')}s`;
  const m = Math.floor(ms / 60_000);
  const s = Math.round((ms % 60_000) / 1000);
  if (m < 60) return s ? `${m}m${s}s` : `${m}m`;
  const h = Math.floor(m / 60);
  return m % 60 ? `${h}h${m % 60}m` : `${h}h`;
}

export function relativeTime(iso) {
  const then = Date.parse(iso);
  if (Number.isNaN(then)) return '';
  const s = Math.round((Date.now() - then) / 1000);
  if (s < 60) return 'just now';
  const units = [
    [60 * 60 * 24 * 365, 'y'],
    [60 * 60 * 24 * 30, 'mo'],
    [60 * 60 * 24 * 7, 'w'],
    [60 * 60 * 24, 'd'],
    [60 * 60, 'h'],
    [60, 'm'],
  ];
  for (const [secs, label] of units) {
    if (s >= secs) return `${Math.floor(s / secs)}${label} ago`;
  }
  return 'just now';
}

// --- URL <-> search box sync -------------------------------------------------

function syncURL(replace) {
  const url = new URL(location);
  if (box.value.trim()) url.searchParams.set('q', box.value.trim());
  else url.searchParams.delete('q');
  const method = replace ? 'replaceState' : 'pushState';
  if (url.href !== location.href) history[method](null, '', url);
}

function readURL() {
  box.value = new URLSearchParams(location.search).get('q') || '';
}

let debounce;
box.addEventListener('input', () => {
  clearTimeout(debounce);
  debounce = setTimeout(() => {
    syncURL(true);
    newSearch();
  }, 250);
});
form.addEventListener('submit', (e) => {
  e.preventDefault();
  clearTimeout(debounce);
  syncURL(false);
  newSearch();
});
window.addEventListener('popstate', () => {
  readURL();
  newSearch();
});

new IntersectionObserver((entries) => {
  if (entries.some((e) => e.isIntersecting)) loadPage();
}).observe(sentinel);

readURL();
newSearch();
