// yas web UI — search + timeline page. A client of the /v1 JSON contract
// only: the sole data path is same-origin fetch('/v1/search?...'). All view
// state lives in the URL query string (?q=<raw search input>, plus
// ?session=<id> for the session-detail view), so every view is a shareable
// link.
// The exception is the view-options panel (duplicate-collapsing + default
// filters): those are per-browser preferences, persisted in localStorage,
// not part of the shareable view.
import { parseTokens } from './tokens.js';
import { collapseRuns, applyDefaultFilters, dropFailures } from './view.js';

const PAGE_SIZE = 50;

// --- view options (persisted per browser) ------------------------------------

const PREFS_KEY = 'yas.view';
const DEFAULT_PREFS = { collapse: false, hideFailures: false, executor: '', host: '' };

function loadPrefs() {
  try {
    return { ...DEFAULT_PREFS, ...JSON.parse(localStorage.getItem(PREFS_KEY) || '{}') };
  } catch {
    return { ...DEFAULT_PREFS };
  }
}

function savePrefs() {
  try {
    localStorage.setItem(PREFS_KEY, JSON.stringify(prefs));
  } catch {
    // storage unavailable (private mode etc.) — options still apply this page
  }
}

const prefs = loadPrefs();

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
  const params = new URLSearchParams(applyDefaultFilters(parseTokens(box.value), prefs));
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
    const rendered = appendRecords(records);
    offset += records.length;
    if (records.length < PAGE_SIZE) exhausted = true;
    status.textContent = '';
    if (offset === 0) {
      status.textContent = '=^..^=  no matching history';
      status.classList.add('empty-state');
    } else {
      status.classList.remove('empty-state');
    }
    // A page can render nothing (all rows hidden or merged into the previous
    // page's tail); the sentinel is already intersecting so the observer
    // won't re-fire — keep paging until something lands or the API is done.
    if (rendered === 0 && !exhausted) {
      loading = false;
      loadPage();
      return;
    }
  } catch (err) {
    if (gen === generation) status.textContent = `error: ${err.message}`;
  } finally {
    if (gen === generation) loading = false;
  }
}

// appendRecords applies the view options (hide-failures filter, duplicate-run
// collapsing) and appends the result to the timeline. Collapsing must survive
// page seams: if a page starts with the same command the previous page ended
// with, the counts merge into the existing row instead of adding a new one.
// Returns how many rows it touched (appended or merged into).
function appendRecords(records) {
  let visible = prefs.hideFailures ? dropFailures(records) : records;
  if (!prefs.collapse) {
    for (const rec of visible) timeline.append(renderRecord(rec));
    return visible.length;
  }
  // Session view is oldest-first, so the run's most recent occurrence is its
  // last record; the default timeline is newest-first, so it's the first.
  const groups = collapseRuns(visible, sessionView ? 'last' : 'first');
  let touched = 0;
  const tail = timeline.lastElementChild;
  if (groups.length && tail && tail.dataset.command === groups[0].record.command) {
    setRunCount(tail, Number(tail.dataset.count) + groups[0].count);
    groups.shift();
    touched++;
  }
  for (const { record, count } of groups) {
    const li = renderRecord(record);
    setRunCount(li, count);
    timeline.append(li);
    touched++;
  }
  return touched;
}

// setRunCount stamps a collapsed-run row with its occurrence count and shows
// the ×N badge (only when the run really has more than one occurrence).
function setRunCount(li, count) {
  li.dataset.count = String(count);
  let badge = li.querySelector('.dupcount');
  if (count < 2) {
    if (badge) badge.remove();
    return;
  }
  if (!badge) {
    badge = el('span', 'dupcount');
    badge.title = 'consecutive identical runs (most recent shown)';
    li.querySelector('.command').append(badge);
  }
  badge.textContent = ` ×${count}`;
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

// shortenHome collapses the record's home directory to "~" for display.
// The client has no $HOME, so it infers the conventional home from the
// record's username (Linux /home/<u>, macOS /Users/<u>). Display-only —
// the record JSON and the detail view keep the full path.
function shortenHome(path, username) {
  if (!username) return path;
  for (const home of ['/home/' + username, '/Users/' + username]) {
    if (path === home) return '~';
    if (path.startsWith(home + '/')) return '~' + path.slice(home.length);
  }
  return path;
}

function renderRecord(rec) {
  const li = el('li', 'record');
  li.dataset.command = rec.command; // page-seam merge key for dup-collapsing
  const cmd = el('code', 'command', rec.command);
  const meta = el('span', 'meta');
  const exit = rec.exit_code;
  if (exit !== undefined && exit !== null) {
    meta.append(el('span', exit === 0 ? 'badge ok' : 'badge fail', String(exit)));
  } else {
    meta.append(el('span', 'badge running', '…'));
  }
  // Every meta cell is always rendered (empty when absent) so the shared
  // grid columns line up across rows.
  meta.append(el('span', 'host', rec.hostname || ''));
  // cwd is display-shortened (~ for the user's home, tail-visible ellipsis
  // via CSS) — the full path lives in the tooltip and the record detail.
  const cwd = el('span', 'cwd');
  if (rec.cwd) {
    // <bdi> isolates the LTR path text from the cell's RTL direction (which
    // exists only to put the ellipsis on the LEFT so the leaf dirs stay
    // visible) — without it punctuation reshuffles.
    cwd.append(el('bdi', null, shortenHome(rec.cwd, rec.username)));
    cwd.title = rec.cwd;
  }
  meta.append(cwd);
  const dur =
    rec.duration_ms === undefined || rec.duration_ms === null
      ? ''
      : humanDuration(rec.duration_ms);
  meta.append(el('span', 'duration', dur));
  if (rec.session) {
    const link = el('a', 'session', rec.session.slice(0, 8));
    // Preserve the rest of the view state (q etc.) so the banner's
    // "back to search" can restore it by removing only `session`.
    const url = new URL(location);
    url.searchParams.set('session', rec.session);
    link.href = url.search;
    link.title = 'session ' + rec.session;
    meta.append(link);
  } else {
    meta.append(el('span', 'session', ''));
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
  ['cwd', (r) => r.cwd],
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

// --- view-options panel -------------------------------------------------------

// Controls reflect the persisted prefs on load; any change saves and re-runs
// the search so the new view applies immediately.
const optControls = [
  ['opt-collapse', 'collapse', 'checked'],
  ['opt-hide-failures', 'hideFailures', 'checked'],
  ['opt-executor', 'executor', 'value'],
  ['opt-host', 'host', 'value'],
];
for (const [id, key, prop] of optControls) {
  const control = document.getElementById(id);
  control[prop] = prefs[key];
  control.addEventListener('change', () => {
    prefs[key] = prop === 'value' ? control[prop].trim() : control[prop];
    savePrefs();
    newSearch();
  });
}

new IntersectionObserver((entries) => {
  if (entries.some((e) => e.isIntersecting)) loadPage();
}).observe(sentinel);

readURL();
newSearch();
