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
