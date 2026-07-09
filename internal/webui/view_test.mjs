// Tests for the pure view-option helpers (static/view.js): duplicate-run
// collapsing and default view filters. Like tokens_test.mjs this runs via
// `node --test internal/webui/` or `make test-js`; NOT part of `make test`,
// so the repo stays Node-free for build/CI.
import test from 'node:test';
import assert from 'node:assert/strict';
import { collapseRuns, applyDefaultFilters, dropFailures } from './static/view.js';

const rec = (command, extra = {}) => ({ command, ...extra });

test('collapseRuns: empty list', () => {
  assert.deepEqual(collapseRuns([]), []);
});

test('collapseRuns: no duplicates passes through with count 1', () => {
  const records = [rec('a'), rec('b'), rec('c')];
  assert.deepEqual(
    collapseRuns(records),
    records.map((record) => ({ record, count: 1 })),
  );
});

test('collapseRuns: consecutive identical commands collapse into one row', () => {
  const records = [rec('make', { id: '1' }), rec('make', { id: '2' }), rec('ls')];
  assert.deepEqual(collapseRuns(records), [
    { record: records[0], count: 2 },
    { record: records[2], count: 1 },
  ]);
});

test('collapseRuns: non-consecutive duplicates stay separate rows', () => {
  const records = [rec('make'), rec('ls'), rec('make')];
  assert.deepEqual(collapseRuns(records), [
    { record: records[0], count: 1 },
    { record: records[1], count: 1 },
    { record: records[2], count: 1 },
  ]);
});

// The timeline is newest-first, so the first record of a run IS the most
// recent occurrence — the default keeps it.
test('collapseRuns: keeps the first (most recent) record of a run by default', () => {
  const records = [rec('make', { id: 'newest' }), rec('make', { id: 'older' })];
  assert.equal(collapseRuns(records)[0].record.id, 'newest');
});

// The session-detail view is oldest-first (reverse=true), so the most recent
// occurrence of a run is its LAST record; keep:'last' selects it.
test('collapseRuns: keep last selects the final record of a run', () => {
  const records = [rec('make', { id: 'older' }), rec('make', { id: 'newest' }), rec('ls', { id: 'x' })];
  assert.deepEqual(collapseRuns(records, 'last'), [
    { record: records[1], count: 2 },
    { record: records[2], count: 1 },
  ]);
});

// --- default filters ---------------------------------------------------------

test('applyDefaultFilters: empty prefs leave params untouched', () => {
  assert.deepEqual(applyDefaultFilters({ q: 'git' }, {}), { q: 'git' });
});

test('applyDefaultFilters: executor and host defaults fill absent params', () => {
  assert.deepEqual(
    applyDefaultFilters({ q: 'git' }, { executor: '$all-human', host: 'pine' }),
    { q: 'git', executor: '$all-human', host: 'pine' },
  );
});

test('applyDefaultFilters: explicit search tokens beat defaults', () => {
  assert.deepEqual(
    applyDefaultFilters({ executor: 'claude', host: 'baox' }, { executor: '$all-human', host: 'pine' }),
    { executor: 'claude', host: 'baox' },
  );
});

test('applyDefaultFilters: empty-string prefs are not defaults', () => {
  assert.deepEqual(applyDefaultFilters({}, { executor: '', host: '' }), {});
});

test('applyDefaultFilters: does not mutate its input', () => {
  const params = {};
  applyDefaultFilters(params, { host: 'pine' });
  assert.deepEqual(params, {});
});

test('dropFailures: removes non-zero exits, keeps successes and running', () => {
  const records = [
    { command: 'ok', exit_code: 0 },
    { command: 'boom', exit_code: 1 },
    { command: 'running' },
    { command: 'null-exit', exit_code: null },
    { command: 'sigint', exit_code: 130 },
  ];
  assert.deepEqual(dropFailures(records).map((r) => r.command), [
    'ok',
    'running',
    'null-exit',
  ]);
});
