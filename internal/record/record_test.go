package record

import (
	"encoding/json"
	"reflect"
	"sort"
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

// Golden freeze: a fully-populated record serializes to exactly ContractFields.
func TestRecordJSON_ContractFields(t *testing.T) {
	r := Record{
		ID: "i", Command: "c", CWD: "d", Hostname: "h", Session: "s", Shell: "zsh",
		Username: "u", ExitCode: ptr(0), StartTime: base, DurationMS: ptr(int64(1)),
		CreatedAt: base, Deleted: true, Executor: "human", CorrID: "x",
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(mustJSON(t, r), &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got := make([]string, 0, len(m))
	for k := range m {
		got = append(got, k)
	}
	want := append([]string(nil), ContractFields...)
	sort.Strings(got)
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("record JSON keys drifted from the v1 contract:\n got %v\nwant %v\n"+
			"(change ContractFields + ContractVersion + docs/api/query-api-v1.md only on a DELIBERATE breaking change)", got, want)
	}
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
		"id":       func(r *Record, v string) { r.ID = v },
		"cwd":      func(r *Record, v string) { r.CWD = v },
		"hostname": func(r *Record, v string) { r.Hostname = v },
		"session":  func(r *Record, v string) { r.Session = v },
		"shell":    func(r *Record, v string) { r.Shell = v },
		"username": func(r *Record, v string) { r.Username = v },
		"executor": func(r *Record, v string) { r.Executor = v },
		"corr_id":  func(r *Record, v string) { r.CorrID = v },
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
