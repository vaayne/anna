package plugin

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/CherryHQ/stella/internal/authz"
)

func TestResolveExhaustive256WinnerFirst(t *testing.T) {
	def := Definition{
		ID: "builtin/email", Namespace: "email", DisplayName: "Email",
		Backend: BackendGo, Source: SourceBuiltin, ImplementationKey: "email", Revision: 1,
		DefaultEnabled: true, Spec: json.RawMessage(`{"schema":1}`),
	}
	states := []struct {
		name    string
		enabled *bool
		payload json.RawMessage
	}{
		{name: "absent"},
		{name: "inherit", payload: json.RawMessage(`{}`)},
		{name: "false", enabled: boolPtr(false)},
		{name: "true", enabled: boolPtr(true), payload: json.RawMessage(`{}`)},
	}
	scopes := []Scope{ScopeSystem, ScopeSystemAgent, ScopeUser, ScopeUserAgent}
	for s := range 4 {
		for sa := range 4 {
			for u := range 4 {
				for ua := range 4 {
					configs := make([]Config, 0, 4)
					for i, state := range []int{s, sa, u, ua} {
						if state == 0 {
							continue
						}
						config := Config{
							ID: string(scopes[i]) + "-id", PluginID: def.ID, Namespace: def.Namespace,
							Scope: scopes[i], Payload: states[state].payload,
							Enabled: states[state].enabled, Revision: 1,
						}
						if scopes[i] == ScopeUser || scopes[i] == ScopeUserAgent {
							config.UserID = "user"
						}
						if scopes[i] == ScopeSystemAgent || scopes[i] == ScopeUserAgent {
							config.AgentID = "agent"
						}
						configs = append(configs, config)
					}
					got, err := Resolve(def, configs, "user", "agent")
					if err != nil {
						t.Fatalf("states %d/%d/%d/%d: %v", s, sa, u, ua, err)
					}
					want := states[0]
					wantScope := Scope("")
					for i, state := range []int{ua, u, sa, s} {
						if state != 0 {
							want = states[state]
							wantScope = []Scope{ScopeUserAgent, ScopeUser, ScopeSystemAgent, ScopeSystem}[i]
							break
						}
					}
					wantEnabled := def.DefaultEnabled
					if want.enabled != nil {
						wantEnabled = *want.enabled
					}
					if s == 2 || sa == 2 {
						wantEnabled = false
						if s == 2 {
							wantScope = ScopeSystem
						} else {
							wantScope = ScopeSystemAgent
						}
					}
					if got.IsEffectivelyEnabled != wantEnabled || got.SourceScope != wantScope {
						t.Fatalf("states %d/%d/%d/%d: got enabled=%v scope=%q, want enabled=%v scope=%q", s, sa, u, ua, got.IsEffectivelyEnabled, got.SourceScope, wantEnabled, wantScope)
					}
				}
			}
		}
	}
}

func TestResolveCapsCannotBeBypassed(t *testing.T) {
	def := testDefinition()
	falseValue := false
	trueValue := true
	configs := []Config{
		{ID: "system", PluginID: def.ID, Namespace: def.Namespace, Scope: ScopeSystem, Enabled: &falseValue, Revision: 1},
		{ID: "agent", PluginID: def.ID, Namespace: def.Namespace, Scope: ScopeSystemAgent, AgentID: "agent", Enabled: &trueValue, Payload: json.RawMessage(`{}`), Revision: 1},
		{ID: "user-agent", PluginID: def.ID, Namespace: def.Namespace, Scope: ScopeUserAgent, UserID: "user", AgentID: "agent", Enabled: &trueValue, Payload: json.RawMessage(`{}`), Revision: 1},
	}
	got, err := Resolve(def, configs, "user", "agent")
	if err != nil {
		t.Fatal(err)
	}
	if got.IsEffectivelyEnabled || got.SourceScope != ScopeSystem {
		t.Fatalf("system cap = %#v, want disabled system winner", got)
	}
}

func TestResolveRejectsMismatchedOwner(t *testing.T) {
	def := testDefinition()
	value := true
	got, err := Resolve(def, []Config{{ID: "u", PluginID: def.ID, Namespace: def.Namespace, Scope: ScopeUser, UserID: "other", Enabled: &value, Payload: json.RawMessage(`{}`), Revision: 1}}, "user", "agent")
	if err != nil {
		t.Fatal(err)
	}
	if got.IsEffectivelyEnabled || got.SourceScope != "" {
		t.Fatalf("mismatched owner was selected: %#v", got)
	}
}

func TestResolveAbsentUsesShippedPayload(t *testing.T) {
	def := testDefinition()
	def.DefaultEnabled = true
	def.Spec = json.RawMessage(`{"description":"shipped"}`)
	got, err := Resolve(def, nil, "u", "")
	if err != nil {
		t.Fatal(err)
	}
	if string(got.Payload) != string(def.Spec) {
		t.Fatalf("shipped payload = %s, want %s", got.Payload, def.Spec)
	}
	def.Spec[16] = 'X'
	if string(got.Payload) != `{"description":"shipped"}` {
		t.Fatalf("effective payload retained definition alias: %s", got.Payload)
	}
}

func TestCustomDefinitionCannotDefaultEnabled(t *testing.T) {
	def := testDefinition()
	def.Source, def.DefaultEnabled = SourceCustom, true
	if err := def.Validate(); err == nil {
		t.Fatal("custom definition defaulted enabled")
	}
}

func TestResolveBackendSourceMatrix(t *testing.T) {
	backends := []Backend{BackendCLI, BackendMCP, BackendGo}
	sources := []Source{SourceBuiltin, SourceCustom}
	for _, backend := range backends {
		for _, source := range sources {
			for _, defaultEnabled := range []bool{false, true} {
				name := string(backend) + "/" + string(source) + "/" + fmt.Sprint(defaultEnabled)
				t.Run(name, func(t *testing.T) {
					def := Definition{
						ID: "matrix/" + name, Namespace: "matrix-" + string(backend) + "-" + string(source), DisplayName: name,
						Backend: backend, Source: source, ImplementationKey: name, Spec: json.RawMessage(`{"kind":"matrix"}`),
						DefaultEnabled: defaultEnabled, Revision: 1,
					}
					if source == SourceCustom && backend == BackendGo {
						if err := def.Validate(); err == nil {
							t.Fatal("custom Go definition accepted")
						}
						return
					}
					if source == SourceCustom {
						def.DefaultEnabled = false
					}
					if err := def.Validate(); err != nil {
						t.Fatal(err)
					}
					got, err := Resolve(def, nil, "user", "agent")
					if err != nil {
						t.Fatal(err)
					}
					if got.IsEffectivelyEnabled != def.DefaultEnabled {
						t.Fatalf("default enabled = %v, want %v", got.IsEffectivelyEnabled, def.DefaultEnabled)
					}
					trueValue, falseValue := true, false
					got, err = Resolve(def, []Config{
						{ID: "system", PluginID: def.ID, Namespace: def.Namespace, Scope: ScopeSystem, Enabled: &falseValue, Revision: 1},
						{ID: "user-agent", PluginID: def.ID, Namespace: def.Namespace, Scope: ScopeUserAgent, UserID: "user", AgentID: "agent", Enabled: &trueValue, Payload: json.RawMessage(`{}`), Revision: 1},
					}, "user", "agent")
					if err != nil {
						t.Fatal(err)
					}
					if got.IsEffectivelyEnabled || got.SourceScope != ScopeSystem {
						t.Fatalf("system deny bypassed for %s: %#v", name, got)
					}
				})
			}
		}
	}
}

func TestResolveNamespaceChoosesPayloadOwnerByScope(t *testing.T) {
	catalog := NewCatalog()
	systemDef := testDefinition()
	userDef := systemDef
	userDef.ID = "custom/user"
	if err := catalog.Register(systemDef); err != nil {
		t.Fatal(err)
	}
	if err := catalog.Register(userDef); err != nil {
		t.Fatal(err)
	}
	systemEnabled, userEnabled := true, true
	configs := []Config{
		{ID: "system", PluginID: systemDef.ID, Namespace: systemDef.Namespace, Scope: ScopeSystem, Enabled: &systemEnabled, Payload: json.RawMessage(`{"base":true}`), Revision: 1},
		{ID: "user", PluginID: userDef.ID, Namespace: userDef.Namespace, Scope: ScopeUser, UserID: "u", Enabled: &userEnabled, Payload: json.RawMessage(`{"private":true}`), Revision: 1},
	}
	got, err := ResolveNamespace(catalog, configs, systemDef.Namespace, "u", "")
	if err != nil {
		t.Fatal(err)
	}
	if got.PluginID != userDef.ID || got.SourceScope != ScopeUser {
		t.Fatalf("namespace winner = %#v", got)
	}
	if string(got.Payload) != `{"private":true}` {
		t.Fatalf("namespace payload = %s", got.Payload)
	}
}

func TestResolveNamespacePreservesWinningDefinitionCaps(t *testing.T) {
	catalog := NewCatalog()
	first := testDefinition()
	second := first
	second.ID = "custom/lower"
	if err := catalog.Register(first); err != nil {
		t.Fatal(err)
	}
	if err := catalog.Register(second); err != nil {
		t.Fatal(err)
	}
	falseValue, trueValue := false, true
	configs := []Config{
		{ID: "first-system", PluginID: first.ID, Namespace: first.Namespace, Scope: ScopeSystem, Enabled: &falseValue, Revision: 1},
		{ID: "first-ua", PluginID: first.ID, Namespace: first.Namespace, Scope: ScopeUserAgent, UserID: "u", AgentID: "a", Enabled: &trueValue, Payload: json.RawMessage(`{"ua":true}`), Revision: 1},
		{ID: "second-user", PluginID: second.ID, Namespace: second.Namespace, Scope: ScopeUser, UserID: "u", Enabled: &trueValue, Payload: json.RawMessage(`{"user":true}`), Revision: 1},
	}
	got, err := ResolveNamespace(catalog, configs, first.Namespace, "u", "a")
	if err != nil {
		t.Fatal(err)
	}
	if got.PluginID != first.ID || got.IsEffectivelyEnabled || got.SourceScope != ScopeSystem {
		t.Fatalf("winning definition caps were bypassed: %#v", got)
	}
}

func TestCatalogAllowsSameNamespaceAcrossDefinitions(t *testing.T) {
	catalog := NewCatalog()
	first := testDefinition()
	second := first
	second.ID = "custom/2"
	if err := catalog.Register(first); err != nil {
		t.Fatal(err)
	}
	if err := catalog.Register(second); err != nil {
		t.Fatal(err)
	}
	if got, ok := catalog.Get(first.ID); !ok || got.ID != first.ID {
		t.Fatal("definition ID lookup failed")
	}
}

func TestNamespaceAndExportedNameContract(t *testing.T) {
	for _, namespace := range []string{"Email_2", "MCP-Remote"} {
		if err := ValidateNamespace(namespace); err != nil {
			t.Fatalf("namespace %q rejected: %v", namespace, err)
		}
	}
	for _, namespace := range []string{"has.dot", "has/slash", "bad__separator", ""} {
		if err := ValidateNamespace(namespace); err == nil {
			t.Fatalf("namespace %q accepted", namespace)
		}
	}
	name, err := ExportedToolName("MCP-Remote", "list_items")
	if err != nil || name != "MCP-Remote__list_items" {
		t.Fatalf("exported name = %q, err=%v", name, err)
	}
	if _, err := ExportedToolName("12345678901234567890123456789012345678901234567890123456789012", "x"); err == nil {
		t.Fatal("overlong exported name accepted")
	}
}

func TestDefinitionValidationDoesNotNormalizeEmptySpec(t *testing.T) {
	def := testDefinition()
	def.Spec = nil
	if err := def.Validate(); err == nil {
		t.Fatal("empty spec accepted")
	}
	if def.Spec != nil {
		t.Fatalf("Validate mutated empty spec to %s", def.Spec)
	}
}

func TestCatalogAndResolverDefensivelyCopyMutableFields(t *testing.T) {
	spec := json.RawMessage(`{"key":"original"}`)
	def := testDefinition()
	def.Spec = spec
	catalog := NewCatalog()
	if err := catalog.Register(def); err != nil {
		t.Fatal(err)
	}
	spec[8] = 'X'
	got, ok := catalog.Get(def.ID)
	if !ok || string(got.Spec) != `{"key":"original"}` {
		t.Fatalf("catalog retained caller alias: %s", got.Spec)
	}
	got.Spec[8] = 'Y'
	again, _ := catalog.Get(def.ID)
	if string(again.Spec) != `{"key":"original"}` {
		t.Fatalf("catalog returned internal alias: %s", again.Spec)
	}
	def.Spec = json.RawMessage(`{"key":"original"}`)
	value := true
	payload := json.RawMessage(`{"safe":true}`)
	config := Config{ID: "c", PluginID: def.ID, Namespace: def.Namespace, Scope: ScopeUser, UserID: "u", Enabled: &value, Payload: payload, Revision: 1, CreatedAt: time.Now().UTC()}
	effective, err := Resolve(def, []Config{config}, "u", "")
	if err != nil {
		t.Fatal(err)
	}
	payload[2] = 'X'
	if string(effective.Payload) != `{"key":"original","safe":true}` {
		t.Fatalf("resolver retained caller alias: %s", effective.Payload)
	}
}

func TestAccessDerivesOnlyTrustedUserScope(t *testing.T) {
	authority, err := authz.NewUserAuthority("user", false)
	if err != nil {
		t.Fatal(err)
	}
	access := &Access{service: &Service{}, authority: authority}
	userID, agentID, err := access.owner(t.Context(), ScopeUser, "")
	if err != nil || userID != "user" || agentID != "" {
		t.Fatalf("owner = %q/%q, err=%v", userID, agentID, err)
	}
	if _, _, err := access.owner(t.Context(), ScopeUser, "attacker"); err == nil {
		t.Fatal("user scope accepted an agent owner")
	}
	if _, _, err := access.owner(t.Context(), ScopeUserAgent, "agent"); err == nil {
		t.Fatal("agent scope bypassed the central Agent PEP")
	}
}

func testDefinition() Definition {
	return Definition{ID: "builtin/test", Namespace: "test", DisplayName: "Test", Backend: BackendGo, Source: SourceBuiltin, ImplementationKey: "test", Spec: json.RawMessage(`{}`), Revision: 1}
}

func boolPtr(value bool) *bool { return &value }

func TestExportedToolNameAllowsSeparatorInsideLocalName(t *testing.T) {
	got, err := ExportedToolName("safe", "part__operation")
	if err != nil || got != "safe__part__operation" {
		t.Fatalf("valid local name = %q, %v", got, err)
	}
	for _, local := range []string{"", "../operation", "part.operation", "part/operation"} {
		if _, err := ExportedToolName("safe", local); err == nil {
			t.Errorf("invalid local %q accepted", local)
		}
	}
	if _, err := ExportedToolName("unsafe__namespace", "operation"); err == nil {
		t.Fatal("ambiguous namespace accepted")
	}
}
