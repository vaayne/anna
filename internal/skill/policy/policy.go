// Package policy owns the versioned Agent Skill activation setting.
package policy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
)

const Version = 1

// ErrCommitOutcomeUnknown means PostgreSQL returned an error from COMMIT after
// accepting a policy mutation. The transaction may already be durable; callers
// must reconcile current policy truth instead of assuming a rollback.
var ErrCommitOutcomeUnknown = errors.New("AgentSkillPolicy commit outcome unknown")

// Policy is the decoded, immutable value of agent.enabled_builtin_skills.
// Legacy builtin refs remain readable for compatibility, but builtin Skills do
// not participate in policy decisions and are controlled by their owner plugin.
type Policy struct {
	Disabled []string
}

// Decode reads the strict versioned storage format. The database migration
// canonicalizes historical arrays before this code runs and a constraint keeps
// them from returning; accepting any other shape here would fail open by
// silently changing which instructions the model receives.
func Decode(raw json.RawMessage) (Policy, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return Policy{}, fmt.Errorf("AgentSkillPolicy must be an object")
	}
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	open, err := decoder.Token()
	if err != nil {
		return Policy{}, fmt.Errorf("decode AgentSkillPolicy: %w", err)
	}
	if open != json.Delim('{') {
		return Policy{}, fmt.Errorf("AgentSkillPolicy must be an object")
	}
	var (
		version     int
		disabled    []string
		hasVersion  bool
		hasDisabled bool
	)
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return Policy{}, fmt.Errorf("decode AgentSkillPolicy: %w", err)
		}
		field, ok := token.(string)
		if !ok {
			return Policy{}, fmt.Errorf("decode AgentSkillPolicy: object field name is not a string")
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return Policy{}, fmt.Errorf("decode AgentSkillPolicy: %w", err)
		}
		switch field {
		case "version":
			if hasVersion {
				return Policy{}, fmt.Errorf("AgentSkillPolicy version must appear exactly once")
			}
			hasVersion = true
			if bytes.Equal(value, []byte("null")) || json.Unmarshal(value, &version) != nil {
				return Policy{}, fmt.Errorf("AgentSkillPolicy version must be an integer")
			}
		case "disabled":
			if hasDisabled {
				return Policy{}, fmt.Errorf("AgentSkillPolicy disabled must appear exactly once")
			}
			hasDisabled = true
			if bytes.Equal(value, []byte("null")) || json.Unmarshal(value, &disabled) != nil {
				return Policy{}, fmt.Errorf("AgentSkillPolicy disabled must be a non-null array")
			}
		default:
			return Policy{}, fmt.Errorf("AgentSkillPolicy contains unknown field %q", field)
		}
	}
	close, err := decoder.Token()
	if err != nil {
		return Policy{}, fmt.Errorf("decode AgentSkillPolicy: %w", err)
	}
	if close != json.Delim('}') {
		return Policy{}, fmt.Errorf("decode AgentSkillPolicy: object did not close")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return Policy{}, fmt.Errorf("decode AgentSkillPolicy: trailing JSON value")
		}
		return Policy{}, fmt.Errorf("decode AgentSkillPolicy: %w", err)
	}
	if !hasVersion || !hasDisabled {
		return Policy{}, fmt.Errorf("AgentSkillPolicy requires exactly version and disabled")
	}
	if version != Version {
		return Policy{}, fmt.Errorf("unsupported AgentSkillPolicy version %d", version)
	}
	canonical, err := canonicalDisabled(disabled)
	if err != nil {
		return Policy{}, err
	}
	if len(canonical) != len(disabled) {
		return Policy{}, fmt.Errorf("AgentSkillPolicy disabled refs must be unique canonical refs")
	}
	for i := range canonical {
		if canonical[i] != disabled[i] {
			return Policy{}, fmt.Errorf("AgentSkillPolicy disabled refs must be sorted canonical refs")
		}
	}
	return Policy{Disabled: canonical}, nil
}

// CanonicalJSON returns deterministic bytes for a policy mutation.
func (p Policy) CanonicalJSON() (json.RawMessage, error) {
	disabled, err := canonicalDisabled(p.Disabled)
	if err != nil {
		return nil, err
	}
	// json.Marshal preserves struct declaration order, which is part of the
	// storage contract for this byte-sensitive column.
	data, err := json.Marshal(struct {
		Version  int      `json:"version"`
		Disabled []string `json:"disabled"`
	}{Version: Version, Disabled: disabled})
	if err != nil {
		return nil, fmt.Errorf("encode AgentSkillPolicy: %w", err)
	}
	return data, nil
}

func (p Policy) DisabledRef(ref string) bool {
	return sort.SearchStrings(p.Disabled, ref) < len(p.Disabled) && p.Disabled[sort.SearchStrings(p.Disabled, ref)] == ref
}

// SetEnabled changes exactly one logical ref. Enabling is deliberately allowed
// for a stored dangling ref, which is the sole explicit prune operation.
func (p Policy) SetEnabled(ref string, enabled bool) (Policy, error) {
	if err := ValidateMutationRef(ref); err != nil {
		return Policy{}, err
	}
	next := append([]string(nil), p.Disabled...)
	if enabled {
		next = remove(next, ref)
	} else if !p.DisabledRef(ref) {
		next = append(next, ref)
	}
	canonical, err := canonicalDisabled(next)
	if err != nil {
		return Policy{}, err
	}
	return Policy{Disabled: canonical}, nil
}

// ValidateMutationRef accepts only scopes that remain independently writable.
// ValidateRef intentionally remains broader so old builtin policy bytes can be
// decoded and preserved while their effect is ignored.
func ValidateMutationRef(ref string) error {
	scope, name, ok := strings.Cut(ref, ":")
	if !ok || name == "" || (scope != "system" && scope != "system_agent") {
		return fmt.Errorf("invalid AgentSkillPolicy ref %q", ref)
	}
	return validateName(name, ref)
}

func remove(refs []string, ref string) []string {
	out := refs[:0]
	for _, candidate := range refs {
		if candidate != ref {
			out = append(out, candidate)
		}
	}
	return out
}

// ValidateRef accepts policy-addressable scopes, including the legacy builtin
// scope so old rows remain readable, and the existing Skill name grammar.
func ValidateRef(ref string) error {
	scope, name, ok := strings.Cut(ref, ":")
	if !ok || name == "" || (scope != "builtin" && scope != "system" && scope != "system_agent") {
		return fmt.Errorf("invalid AgentSkillPolicy ref %q", ref)
	}
	return validateName(name, ref)
}

func validateName(name, ref string) error {
	if len(name) > 64 || strings.HasPrefix(name, "-") || strings.HasSuffix(name, "-") || strings.Contains(name, "--") {
		return fmt.Errorf("invalid AgentSkillPolicy ref %q", ref)
	}
	for _, r := range name {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
			return fmt.Errorf("invalid AgentSkillPolicy ref %q", ref)
		}
	}
	return nil
}

func canonicalDisabled(refs []string) ([]string, error) {
	// A v1 policy always writes [] for empty disabled refs. nil would marshal as
	// null and make a strict policy indistinguishable from malformed storage.
	out := make([]string, len(refs))
	copy(out, refs)
	for _, ref := range out {
		if err := ValidateRef(ref); err != nil {
			return nil, err
		}
	}
	sort.Strings(out)
	for i := 1; i < len(out); i++ {
		if out[i] == out[i-1] {
			return nil, fmt.Errorf("AgentSkillPolicy disabled refs must be unique")
		}
	}
	return out, nil
}
