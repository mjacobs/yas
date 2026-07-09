// yas web UI — digest dashboard. A client of GET /v1/digest only: today's
// commands grouped by host and project/directory, failures flagged. Optional
// ?since/?until (RFC3339) pass straight through to the API, so a shared link
// reproduces the same window; with neither the API defaults to "today".

const digestEl = document.getElementById('digest');
const windowEl = document.getElementById('window');
const status = document.getElementById('status');

function el(tag, className, text) {
  const node = document.createElement(tag);
  if (className) node.className = className;
  if (text !== undefined) node.textContent = text;
  return node;
}

// Render one (host, location) group as a card: the location, the command
// count, a failure badge when any command failed, and the sampled failing
// commands (already deduped/capped/truncated by the API).
function renderGroup(group) {
  const card = el('article', 'digest-group');
  const head = el('div', 'digest-group-head');
  const loc = el('span', 'cwd');
  // <bdi> + the .cwd RTL trick (see app.css): ellipsis on the LEFT so the
  // leaf directories stay visible. Full path in the tooltip.
  loc.append(Object.assign(el('bdi'), { textContent: group.cwd }));
  loc.title = group.repo_root ? `${group.cwd} (git project)` : group.cwd;
  head.append(loc);
  head.append(
    el('span', 'digest-count', `${group.count} command${group.count === 1 ? '' : 's'}`),
  );
  if (group.failures > 0) {
    head.append(el('span', 'badge fail', `${group.failures} failed`));
  }
  card.append(head);
  if (group.failed_commands.length) {
    const list = el('ul', 'digest-failures');
    for (const cmd of group.failed_commands) {
      const li = el('li');
      li.append(el('span', 'badge fail', '✗'), ' ', el('code', 'command', cmd));
      list.append(li);
    }
    card.append(list);
  }
  return card;
}

// Groups arrive host-asc then location-asc (the API's deterministic order);
// render one section per host, preserving that order.
function renderDigest(env) {
  windowEl.textContent = `${fmtTime(env.since)} → ${fmtTime(env.until)}`;
  digestEl.replaceChildren();
  if (!env.groups.length) {
    status.textContent = '=^..^=  no commands in this window';
    status.classList.add('empty-state');
    return;
  }
  status.textContent = '';
  status.classList.remove('empty-state');
  let section = null;
  let currentHost = null;
  for (const group of env.groups) {
    if (group.host !== currentHost) {
      currentHost = group.host;
      section = el('section', 'digest-host');
      section.append(el('h2', 'host', group.host));
      digestEl.append(section);
    }
    section.append(renderGroup(group));
  }
}

export function fmtTime(iso) {
  const t = new Date(iso);
  if (Number.isNaN(t.getTime())) return iso;
  return t.toLocaleString(undefined, {
    year: 'numeric', month: '2-digit', day: '2-digit',
    hour: '2-digit', minute: '2-digit',
  });
}

async function load() {
  const params = new URLSearchParams();
  const page = new URLSearchParams(location.search);
  for (const key of ['since', 'until']) {
    const v = page.get(key);
    if (v) params.set(key, v);
  }
  status.textContent = 'loading…';
  try {
    const resp = await fetch('/v1/digest' + (params.size ? '?' + params : ''));
    if (!resp.ok) {
      const body = await resp.json().catch(() => ({}));
      status.textContent = `error: ${body.error || resp.status}`;
      return;
    }
    renderDigest(await resp.json());
  } catch (err) {
    status.textContent = `error: ${err.message}`;
  }
}

load();
