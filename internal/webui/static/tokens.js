// Search-token parser: hishtory-style tokens -> /v1/search params.
// The grammar's source of truth is ../testdata/token-vectors.json, replayed
// against the real API by a Go test and against this module by
// tokens_test.mjs (node --test). Pure module: no DOM, no fetch.

const PREFIXES = {
  host: 'host',
  cwd: 'cwd',
  exit: 'exit',
  executor: 'executor',
  session: 'session',
  after: 'since',
  before: 'until',
};

const DATE_ONLY = /^\d{4}-\d{2}-\d{2}$/;

// parseTokens turns search-box input into a plain object of /v1/search query
// params. Unrecognized words (including unknown prefix:value forms) join the
// free-text `q`; a later duplicate token overwrites an earlier one.
export function parseTokens(input) {
  const params = {};
  const text = [];
  for (const word of input.trim().split(/\s+/).filter(Boolean)) {
    if (word === 'failed') {
      params.failed = 'true';
      continue;
    }
    const i = word.indexOf(':');
    const param = i > 0 && PREFIXES[word.slice(0, i)];
    if (param) {
      let value = word.slice(i + 1);
      // The API wants RFC3339; normalize date-only before:/after: values to
      // UTC day boundaries.
      if ((param === 'since' || param === 'until') && DATE_ONLY.test(value)) {
        value += 'T00:00:00Z';
      }
      params[param] = value;
    } else {
      text.push(word);
    }
  }
  if (text.length) params.q = text.join(' ');
  return params;
}
