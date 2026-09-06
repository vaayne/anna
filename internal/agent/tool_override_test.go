package agent

import (
	"strings"
	"testing"
)

func TestToolIdentityValidate(t *testing.T) {
	for name, tc := range map[string]struct {
		identity ToolIdentity
		wantErr  bool
	}{
		"core":           {identity: ToolIdentity{CoreToolName: "memory"}},
		"plugin":         {identity: ToolIdentity{PluginID: "system/email", LocalToolName: "send"}},
		"empty":          {wantErr: true},
		"both":           {identity: ToolIdentity{CoreToolName: "memory", PluginID: "system/email", LocalToolName: "send"}, wantErr: true},
		"missing local":  {identity: ToolIdentity{PluginID: "system/email"}, wantErr: true},
		"missing plugin": {identity: ToolIdentity{LocalToolName: "send"}, wantErr: true},
	} {
		t.Run(name, func(t *testing.T) {
			err := tc.identity.Validate()
			if (err != nil) != tc.wantErr {
				t.Fatalf("Validate() error = %v, wantErr %t", err, tc.wantErr)
			}
		})
	}
}

func TestResolveToolOverridePrecedence(t *testing.T) {
	tests := []struct {
		name        string
		defaultOn   bool
		rows        []ToolOverride
		wantEnabled bool
		wantOrigin  string
	}{
		{name: "default on", defaultOn: true, wantEnabled: true, wantOrigin: ToolOverrideOriginDefault},
		{name: "default off", defaultOn: false, wantEnabled: false, wantOrigin: ToolOverrideOriginDefault},
		{name: "system enables", rows: []ToolOverride{{Identity: ToolIdentity{CoreToolName: "x"}, Scope: ToolOverrideScopeSystem, Enabled: true}}, wantEnabled: true, wantOrigin: ToolOverrideScopeSystem},
		{name: "system disable caps system agent enable", rows: []ToolOverride{{Identity: ToolIdentity{CoreToolName: "x"}, Scope: ToolOverrideScopeSystem, Enabled: false}, {Identity: ToolIdentity{CoreToolName: "x"}, Scope: ToolOverrideScopeSystemAgent, Enabled: true}}, wantEnabled: false, wantOrigin: ToolOverrideScopeSystem},
		{name: "user disables after admin enable", rows: []ToolOverride{{Identity: ToolIdentity{CoreToolName: "x"}, Scope: ToolOverrideScopeSystem, Enabled: true}, {Identity: ToolIdentity{CoreToolName: "x"}, Scope: ToolOverrideScopeUser, Enabled: false}}, wantEnabled: false, wantOrigin: ToolOverrideScopeUser},
		{name: "user agent beats user", rows: []ToolOverride{{Identity: ToolIdentity{CoreToolName: "x"}, Scope: ToolOverrideScopeUser, Enabled: false}, {Identity: ToolIdentity{CoreToolName: "x"}, Scope: ToolOverrideScopeUserAgent, Enabled: true}}, wantEnabled: true, wantOrigin: ToolOverrideScopeUserAgent},
		{name: "admin disable beats user enable", rows: []ToolOverride{{Identity: ToolIdentity{CoreToolName: "x"}, Scope: ToolOverrideScopeSystemAgent, Enabled: false}, {Identity: ToolIdentity{CoreToolName: "x"}, Scope: ToolOverrideScopeUserAgent, Enabled: true}}, wantEnabled: false, wantOrigin: ToolOverrideScopeSystemAgent},
		{name: "system agent disable caps user agent enable", rows: []ToolOverride{{Identity: ToolIdentity{CoreToolName: "x"}, Scope: ToolOverrideScopeSystem, Enabled: true}, {Identity: ToolIdentity{CoreToolName: "x"}, Scope: ToolOverrideScopeSystemAgent, Enabled: false}, {Identity: ToolIdentity{CoreToolName: "x"}, Scope: ToolOverrideScopeUserAgent, Enabled: true}}, wantEnabled: false, wantOrigin: ToolOverrideScopeSystemAgent},
		{name: "both admin enable leaves user agent origin", rows: []ToolOverride{{Identity: ToolIdentity{CoreToolName: "x"}, Scope: ToolOverrideScopeSystem, Enabled: true}, {Identity: ToolIdentity{CoreToolName: "x"}, Scope: ToolOverrideScopeSystemAgent, Enabled: true}, {Identity: ToolIdentity{CoreToolName: "x"}, Scope: ToolOverrideScopeUserAgent, Enabled: true}}, wantEnabled: true, wantOrigin: ToolOverrideScopeUserAgent},
		{name: "other tool ignored", defaultOn: true, rows: []ToolOverride{{Identity: ToolIdentity{CoreToolName: "y"}, Scope: ToolOverrideScopeUserAgent, Enabled: false}}, wantEnabled: true, wantOrigin: ToolOverrideOriginDefault},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ResolveToolOverride(tt.defaultOn, ToolIdentity{CoreToolName: "x"}, tt.rows)
			if got.Enabled != tt.wantEnabled || got.Origin != tt.wantOrigin {
				t.Fatalf("ResolveToolOverride() = (%v, %q), want (%v, %q)", got.Enabled, got.Origin, tt.wantEnabled, tt.wantOrigin)
			}
		})
	}
}

func TestVLLMNameRemainsReservedWithoutVisionAvailability(t *testing.T) {
	if !IsCoreToolName("vllm") {
		t.Fatal("vllm core name must remain reserved when vision is unavailable")
	}
}

func TestResolveToolOverrideCoreExemption(t *testing.T) {
	rows := []ToolOverride{{Identity: ToolIdentity{CoreToolName: "bash"}, Scope: ToolOverrideScopeSystemAgent, Enabled: false}}
	got := ResolveToolOverride(false, ToolIdentity{CoreToolName: "bash"}, rows)
	if !got.Enabled || got.Origin != ToolOverrideOriginDefault {
		t.Fatalf("core tool decision = (%v, %q), want enabled default", got.Enabled, got.Origin)
	}
}

func TestResolveToolOverrideUsesPluginIdentity(t *testing.T) {
	identity := ToolIdentity{PluginID: "system/email", LocalToolName: "send"}
	rows := []ToolOverride{
		{Identity: identity, Scope: ToolOverrideScopeSystem, Enabled: false},
		{Identity: ToolIdentity{PluginID: "system/email", LocalToolName: "other"}, Scope: ToolOverrideScopeUserAgent, Enabled: true},
	}
	got := ResolveToolOverride(true, identity, rows)
	if got.Enabled || got.Origin != ToolOverrideScopeSystem {
		t.Fatalf("plugin identity decision = (%v, %q), want system disable", got.Enabled, got.Origin)
	}

	// An exported-looking string is a distinct core identity. It is never split
	// to manufacture the plugin/local pair and therefore cannot reach a plugin
	// policy row.
	got = ResolveToolOverride(false, ToolIdentity{CoreToolName: "email__send"}, rows)
	if got.Enabled || got.Origin != ToolOverrideOriginDefault {
		t.Fatalf("exported name decision = (%v, %q), want default disabled", got.Enabled, got.Origin)
	}
}

func TestResolveToolOverrideSkipsMalformedPluginRows(t *testing.T) {
	identity := ToolIdentity{PluginID: "system/email", LocalToolName: "send"}
	rows := []ToolOverride{{
		Identity: ToolIdentity{PluginID: "system/email"},
		Scope:    ToolOverrideScopeSystem,
		Enabled:  false,
	}}
	got := ResolveToolOverride(true, identity, rows)
	if !got.Enabled || got.Origin != ToolOverrideOriginDefault {
		t.Fatalf("malformed plugin row decision = (%v, %q), want default enabled", got.Enabled, got.Origin)
	}
	if err := rows[0].Identity.Validate(); err == nil || !strings.Contains(err.Error(), "local_tool_name") {
		t.Fatalf("malformed identity error = %v, want missing local_tool_name", err)
	}
}
