// yas web UI — search + timeline page. A client of the /v1 JSON contract
// only: the sole data path is same-origin fetch('/v1/search?...'). All view
// state lives in the URL query string (?q=<raw search input>, plus
// ?session=<id> for the session-detail view), so every view is a shareable
// link.
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

// Session-detail view (/ui/?session=<id>): the same timeline filtered to one
// session, oldest-first. Ordering comes from the API (reverse=true), never
// from client-side reshuffling — infinite-scroll pagination depends on it.
let sessionView = '';

function queryFromBox() {
  const params = new URLSearchParams(parseTokens(box.value));
  if (sessionView) {
    params.set('session', sessionView);
    params.set('reverse', 'true');
  }
  return params;
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
  if (rec.session) {
    const link = el('a', 'session', rec.session.slice(0, 8));
    // Preserve the rest of the view state (q etc.) so the banner's
    // "back to search" can restore it by removing only `session`.
    const url = new URL(location);
    url.searchParams.set('session', rec.session);
    link.href = url.search;
    link.title = 'session ' + rec.session;
    meta.append(link);
  }
  const when = el('time', 'when', relativeTime(rec.start_time));
  when.dateTime = rec.start_time;
  when.title = rec.start_time;
  meta.append(when);
  li.append(cmd, meta);
  // Record detail: clicking the command expands the full record inline.
  // Expanded state is not URL-persisted in v1.
  cmd.addEventListener('click', () => toggleDetail(li, rec));
  return li;
}

const DETAIL_FIELDS = [
  ['id', (r) => r.id],
  ['session', (r) => r.session],
  ['executor', (r) => r.executor || 'human'],
  ['corr_id', (r) => r.corr_id],
  ['shell', (r) => r.shell],
  ['username', (r) => r.username],
  ['exit_code', (r) => (r.exit_code === undefined || r.exit_code === null ? '(running)' : String(r.exit_code))],
  ['duration', (r) => (r.duration_ms === undefined || r.duration_ms === null ? '' : `${humanDuration(r.duration_ms)} (${r.duration_ms}ms)`)],
  ['start_time', (r) => r.start_time],
  ['created_at', (r) => r.created_at],
  ['repo_root', (r) => r.repo_root],
  ['branch', (r) => r.branch],
];

function toggleDetail(li, rec) {
  const existing = li.querySelector('.detail');
  if (existing) {
    existing.remove();
    return;
  }
  const dl = el('dl', 'detail');
  for (const [label, value] of DETAIL_FIELDS) {
    const v = value(rec);
    if (!v) continue;
    dl.append(el('dt', null, label), el('dd', null, v));
  }
  li.append(dl);
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
  const params = new URLSearchParams(location.search);
  box.value = params.get('q') || '';
  sessionView = params.get('session') || '';
  renderSessionBanner();
}

function renderSessionBanner() {
  const banner = document.getElementById('session-banner');
  banner.replaceChildren();
  if (!sessionView) return;
  banner.append('session ', el('code', null, sessionView), ' — oldest first · ');
  const clear = el('a', null, 'back to search');
  const url = new URL(location);
  url.searchParams.delete('session');
  clear.href = url.search || './';
  banner.append(clear);
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
