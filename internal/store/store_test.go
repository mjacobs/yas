package store

import "testing"

// ApplyExecutorToken is THE definition of the client-facing executor token
// vocabulary ($all-agent / $all-human / bare "human" / exact name). The CLI,
// query API, and MCP surfaces must all map tokens through it so they cannot
// drift (0zw1: the mapping was previously duplicated in three places, and the
// MCP copy treated bare "human" as an exact match, silently dropping legacy
// rows recorded with a NULL/empty executor).
func TestApplyExecutorToken(t *testing.T) {
	cases := []struct {
		token string
		want  Query
	}{
		{"", Query{}},
		{"$all-agent", Query{AgentsOnly: true}},
		{"$all-human", Query{HumansOnly: true}},
		// Bare "human" folds legacy untagged rows (executor NULL/empty) into
		// the human class — it must NOT be an exact match.
		{"human", Query{HumansOnly: true}},
		{"codex", Query{Executor: "codex"}},
		{"claude-code", Query{Executor: "claude-code"}},
	}
	for _, c := range cases {
		var q Query
		q.ApplyExecutorToken(c.token)
		if q != c.want {
			t.Errorf("ApplyExecutorToken(%q) = %+v, want %+v", c.token, q, c.want)
		}
	}
}

// The token is applied on top of an already-populated query without clobbering
// unrelated fields.
func TestApplyExecutorTokenPreservesOtherFields(t *testing.T) {
	q := Query{Text: "git", Host: "h", Limit: 5}
	q.ApplyExecutorToken("$all-agent")
	if q.Text != "git" || q.Host != "h" || q.Limit != 5 || !q.AgentsOnly {
		t.Errorf("unrelated fields clobbered: %+v", q)
	}
}
