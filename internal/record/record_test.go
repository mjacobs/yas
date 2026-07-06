package record

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

func ptr[T any](v T) *T { return &v }

var base = time.UnixMilli(1_700_000_000_000)

func TestRecordJSON_ExecutorAndCorrID(t *testing.T) {
	r := Record{ID: "i", Command: "c", StartTime: base, CreatedAt: base, Executor: "claude-code", CorrID: "sess-7"}
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(b)
	if !strings.Contains(s, `"executor":"claude-code"`) {
		t.Errorf("executor missing: %s", s)
	}
	if !strings.Contains(s, `"corr_id":"sess-7"`) {
		t.Errorf("corr_id missing: %s", s)
	}
}

func TestRecordJSON_OmitsEmptyExecutorCorrID(t *testing.T) {
	r := Record{ID: "i", Command: "c", StartTime: base, CreatedAt: base}
	s := string(mustJSON(t, r))
	if strings.Contains(s, "executor") || strings.Contains(s, "corr_id") {
		t.Errorf("empty executor/corr_id must be omitted: %s", s)
	}
}

func TestRecordJSON_RepoRootAndBranch(t *testing.T) {
	set := Record{ID: "i", Command: "c", StartTime: base, CreatedAt: base, RepoRoot: "/work/x/proj", Branch: "feat/y"}
	s := string(mustJSON(t, set))
	if !strings.Contains(s, `"repo_root":"/work/x/proj"`) {
		t.Errorf("repo_root missing: %s", s)
	}
	if !strings.Contains(s, `"branch":"feat/y"`) {
		t.Errorf("branch missing: %s", s)
	}
	empty := string(mustJSON(t, Record{ID: "i", Command: "c", StartTime: base, CreatedAt: base}))
	if strings.Contains(empty, "repo_root") || strings.Contains(empty, "branch") {
		t.Errorf("empty repo_root/branch must be omitted: %s", empty)
	}
}

func TestIsAgent(t *testing.T) {
	for _, c := range []struct {
		exec string
		want bool
	}{{"", false}, {"human", false}, {"claude-code", true}, {"codex", true}, {"ci", true}} {
		if got := (Record{Executor: c.exec}).IsAgent(); got != c.want {
			t.Errorf("IsAgent(%q)=%v want %v", c.exec, got, c.want)
		}
	}
}

// Golden freeze: a fully-populated record serializes to exactly ContractFields,
// in that exact order. encoding/json marshals struct fields in declaration
// order (not alphabetically, not map-random), so the order Record's JSON tags
// are declared in is itself part of the contract — this asserts order, not
// just the key set (unmarshaling into a map would lose the order and only
// prove the set matches).
func TestRecordJSON_ContractFields(t *testing.T) {
	r := Record{
		ID: "i", Command: "c", CWD: "d", Hostname: "h", Session: "s", Shell: "zsh",
		Username: "u", ExitCode: ptr(0), StartTime: base, DurationMS: ptr(int64(1)),
		CreatedAt: base, Deleted: true, Executor: "human", CorrID: "x",
		RepoRoot: "/repo", Branch: "main",
	}
	got := jsonKeysInOrder(t, mustJSON(t, r))
	want := ContractFields()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("record JSON keys/order drifted from the v1 contract:\n got %v\nwant %v\n"+
			"(change ContractFields + ContractVersion + docs/api/query-api-v1.md only on a DELIBERATE breaking change)", got, want)
	}
}

// ContractFields is documented as returning a fresh copy every call so a
// caller can't mutate the frozen contract for anyone else. Prove it: mutating
// one call's result must not leak into the next call's result.
func TestContractFields_ReturnsFreshCopy(t *testing.T) {
	got := ContractFields()
	got[0] = "MUTATED"
	if again := ContractFields(); again[0] != "id" {
		t.Fatalf("ContractFields must return a fresh copy each call: mutating one "+
			"result affected a later call, got %v", again)
	}
}

// jsonKeysInOrder returns the top-level object keys of b in serialization
// order. json.Decoder.Token walks the raw byte stream in document order —
// unlike Unmarshal into a map, which loses order — so this is the one
// reliable way to observe field order. Every Record field is a scalar
// (string/number/bool/null), so a flat key/value/key/value walk suffices;
// this would need to recurse for a nested object or array value.
func jsonKeysInOrder(t *testing.T, b []byte) []string {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(string(b)))
	tok, err := dec.Token()
	if err != nil {
		t.Fatalf("token: %v", err)
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		t.Fatalf("expected a JSON object, got %v", tok)
	}
	var keys []string
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			t.Fatalf("key token: %v", err)
		}
		keys = append(keys, kt.(string))
		if _, err := dec.Token(); err != nil { // skip the scalar value
			t.Fatalf("value token: %v", err)
		}
	}
	return keys
}

// Validate bounds Command so a hostile sync push can't store unbounded blobs,
// but the cap must be far beyond any real interactive command line: the record
// path is sacred — `yas record start` must never fail on real input.
func TestValidate_CommandLengthCap(t *testing.T) {
	at := Record{ID: "i", Command: strings.Repeat("a", MaxCommandBytes), StartTime: base, CreatedAt: base}
	if err := at.Validate(); err != nil {
		t.Errorf("command at cap (%d bytes) must validate, got: %v", MaxCommandBytes, err)
	}
	over := at
	over.Command = strings.Repeat("a", MaxCommandBytes+1)
	if err := over.Validate(); err == nil {
		t.Errorf("command over cap (%d bytes) must fail validation", MaxCommandBytes+1)
	}
	// The cap applies to tombstones too — Deleted must not bypass the bound.
	over.Deleted = true
	if err := over.Validate(); err == nil {
		t.Errorf("deleted record with over-cap command must still fail validation")
	}
}

// Every other string field is bounded too, so a record that passes Validate
// has a bounded encoded size — the sync client relies on this to guarantee a
// single record always fits the push body cap. Caps must stay far beyond real
// values (PATH_MAX cwd, RFC hostnames): the record path is sacred.
func TestValidate_FieldLengthCaps(t *testing.T) {
	valid := Record{ID: "i", Command: "c", StartTime: base, CreatedAt: base}
	set := map[string]func(*Record, string){
		"id":        func(r *Record, v string) { r.ID = v },
		"cwd":       func(r *Record, v string) { r.CWD = v },
		"hostname":  func(r *Record, v string) { r.Hostname = v },
		"session":   func(r *Record, v string) { r.Session = v },
		"shell":     func(r *Record, v string) { r.Shell = v },
		"username":  func(r *Record, v string) { r.Username = v },
		"executor":  func(r *Record, v string) { r.Executor = v },
		"corr_id":   func(r *Record, v string) { r.CorrID = v },
		"repo_root": func(r *Record, v string) { r.RepoRoot = v },
		"branch":    func(r *Record, v string) { r.Branch = v },
	}
	for name, apply := range set {
		at := valid
		apply(&at, strings.Repeat("a", MaxFieldBytes))
		if err := at.Validate(); err != nil {
			t.Errorf("%s at cap (%d bytes) must validate, got: %v", name, MaxFieldBytes, err)
		}
		over := valid
		apply(&over, strings.Repeat("a", MaxFieldBytes+1))
		if err := over.Validate(); err == nil {
			t.Errorf("%s over cap must fail validation", name)
		} else if !strings.Contains(err.Error(), name) {
			t.Errorf("%s over cap: error should name the field, got: %v", name, err)
		}
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}
