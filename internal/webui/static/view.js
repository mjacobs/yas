// View-option helpers: duplicate-run collapsing and default view filters.
// Pure module (no DOM, no fetch, no storage) so it is table-testable via
// `node --test` (view_test.mjs), like tokens.js.

// collapseRuns groups CONSECUTIVE records with an identical command into one
// {record, count} row. `keep` selects which occurrence represents the run:
// 'first' (default — the most recent one in the newest-first timeline) or
// 'last' (the most recent one in the oldest-first session view).
export function collapseRuns(records, keep = 'first') {
  const groups = [];
  for (const record of records) {
    const prev = groups[groups.length - 1];
    if (prev && prev.record.command === record.command) {
      prev.count++;
      if (keep === 'last') prev.record = record;
    } else {
      groups.push({ record, count: 1 });
    }
  }
  return groups;
}

// applyDefaultFilters merges the persisted default view filters (executor,
// host) into the parsed search params. Explicit search-box tokens always win;
// empty-string prefs mean "no default". Returns a new object.
export function applyDefaultFilters(params, prefs) {
  const merged = { ...params };
  for (const key of ['executor', 'host']) {
    if (prefs[key] && !(key in merged)) merged[key] = prefs[key];
  }
  return merged;
}

// dropFailures removes records that finished with a non-zero exit code.
// Successes (0) and still-running records (no exit_code yet) stay.
export function dropFailures(records) {
  return records.filter((r) => r.exit_code == null || r.exit_code === 0);
}
