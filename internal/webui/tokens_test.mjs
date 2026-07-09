// Table-test for the search-token parser against the shared grammar vectors
// (testdata/token-vectors.json — same file the Go contract test replays).
// Optional: run with `node --test internal/webui/` or `make test-js`; NOT part
// of `make test`, so the repo stays Node-free for build/CI.
import test from 'node:test';
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import { parseTokens } from './static/tokens.js';

const vectors = JSON.parse(
  await readFile(new URL('./testdata/token-vectors.json', import.meta.url), 'utf8'),
);

for (const { name, input, params } of vectors) {
  test(name, () => {
    assert.deepEqual(parseTokens(input), params);
  });
}
