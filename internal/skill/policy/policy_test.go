package policy

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestDecodeStrict(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    []string
		wantErr bool
	}{
		{name: "missing", raw: "", wantErr: true},
		{name: "null", raw: "null", wantErr: true},
		{name: "empty array", raw: "[]", wantErr: true},
		{name: "non-empty array", raw: `["formerly-an-allowlist"]`, wantErr: true},
		{name: "canonical", raw: `{"version":1,"disabled":["system:alpha","system_agent:beta"]}`, want: []string{"system:alpha", "system_agent:beta"}},
		{name: "missing version", raw: `{"disabled":[]}`, wantErr: true},
		{name: "missing disabled", raw: `{"version":1}`, wantErr: true},
		{name: "null version", raw: `{"version":null,"disabled":[]}`, wantErr: true},
		{name: "null disabled", raw: `{"version":1,"disabled":null}`, wantErr: true},
		{name: "duplicate version", raw: `{"version":1,"version":1,"disabled":[]}`, wantErr: true},
		{name: "duplicate disabled", raw: `{"version":1,"disabled":[],"disabled":[]}`, wantErr: true},
		{name: "trailing value", raw: `{"version":1,"disabled":[]} null`, wantErr: true},
		{name: "unknown field", raw: `{"version":1,"disabled":[],"future":true}`, wantErr: true},
		{name: "unknown version", raw: `{"version":2,"disabled":[]}`, wantErr: true},
		{name: "malformed object", raw: `{"version":1,"disabled":`, wantErr: true},
		{name: "unsorted", raw: `{"version":1,"disabled":["system_agent:beta","system:alpha"]}`, wantErr: true},
		{name: "duplicate", raw: `{"version":1,"disabled":["system:alpha","system:alpha"]}`, wantErr: true},
		{name: "invalid scope", raw: `{"version":1,"disabled":["user:alpha"]}`, wantErr: true},
		{name: "invalid name", raw: `{"version":1,"disabled":["system:Not-valid"]}`, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Decode(json.RawMessage(tt.raw))
			if tt.wantErr {
				if err == nil {
					t.Fatal("Decode() error = nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("Decode() error = %v", err)
			}
			if !reflect.DeepEqual(got.Disabled, tt.want) {
				t.Fatalf("Decode() = %#v; want disabled=%#v", got, tt.want)
			}
		})
	}
}

func TestPolicyCanonicalJSONAndDanglingClear(t *testing.T) {
	empty, err := (Policy{}).CanonicalJSON()
	if err != nil {
		t.Fatalf("CanonicalJSON(empty) error = %v", err)
	}
	if want := `{"version":1,"disabled":[]}`; string(empty) != want {
		t.Fatalf("CanonicalJSON(empty) = %s, want %s", empty, want)
	}
	p := Policy{Disabled: []string{"system_agent:zeta", "system:alpha"}}
	got, err := p.CanonicalJSON()
	if err != nil {
		t.Fatalf("CanonicalJSON() error = %v", err)
	}
	const want = `{"version":1,"disabled":["system:alpha","system_agent:zeta"]}`
	if string(got) != want {
		t.Fatalf("CanonicalJSON() = %s, want %s", got, want)
	}

	// A dangling disabled ref has no execution effect at resolution time, but its
	// explicit enable mutation is the only permitted way to remove it from storage.
	next, err := Policy{Disabled: []string{"system:removed-skill"}}.SetEnabled("system:removed-skill", true)
	if err != nil {
		t.Fatalf("SetEnabled(dangling clear) error = %v", err)
	}
	if len(next.Disabled) != 0 {
		t.Fatalf("SetEnabled(dangling clear) = %#v, want empty", next.Disabled)
	}
	cleared, err := next.CanonicalJSON()
	if err != nil || string(cleared) != `{"version":1,"disabled":[]}` {
		t.Fatalf("CanonicalJSON(dangling clear) = %s, %v; want canonical empty", cleared, err)
	}
}

func TestLegacyBuiltinRefsDecodeButCannotMutate(t *testing.T) {
	legacy, err := Decode(json.RawMessage(`{"version":1,"disabled":["builtin:stella"]}`))
	if err != nil || !legacy.DisabledRef("builtin:stella") {
		t.Fatalf("legacy builtin policy = %#v, %v; want readable", legacy, err)
	}
	if _, err := legacy.SetEnabled("builtin:stella", false); err == nil {
		t.Fatal("SetEnabled accepted a new builtin policy mutation")
	}
	if err := ValidateMutationRef("builtin:stella"); err == nil {
		t.Fatal("ValidateMutationRef accepted a builtin ref")
	}
	if err := ValidateMutationRef("system:stella"); err != nil {
		t.Fatalf("ValidateMutationRef rejected managed ref: %v", err)
	}
}
