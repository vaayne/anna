package manifest

import (
	"slices"
	"testing"
)

func TestLoadBuiltin(t *testing.T) {
	m, err := LoadBuiltin()
	if err != nil {
		t.Fatalf("LoadBuiltin() error: %v", err)
	}
	if len(m.Plugins) == 0 {
		t.Fatal("LoadBuiltin() returned empty plugins")
	}
}

func TestLoadBuiltinExcludesCoreRuntimePlugins(t *testing.T) {
	m, err := LoadBuiltin()
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range m.Plugins {
		if slices.Contains([]string{"tool/mise", "tool/xberg", "tool/fd", "tool/rg"}, p.ID) {
			t.Fatalf("mandatory core runtime leaked into configurable plugins: %s", p.ID)
		}
	}
}

func TestLoadBuiltinLarkCLIUsesManagedFeishuOAuth(t *testing.T) {
	m, err := LoadBuiltin()
	if err != nil {
		t.Fatalf("LoadBuiltin() error: %v", err)
	}
	for _, p := range m.Plugins {
		if p.ID != "tool/lark-cli" {
			continue
		}
		if p.OAuthProvider != "feishu" {
			t.Fatalf("OAuthProvider = %q, want feishu", p.OAuthProvider)
		}
		if len(p.SessionEnvs) != 3 {
			t.Fatalf("SessionEnvs = %#v, want token, app ID, and brand injection", p.SessionEnvs)
		}
		if len(p.Binaries) != 1 || p.Binaries[0].Version != "1.0.87" {
			t.Fatalf("Binaries = %#v, want pinned lark-cli 1.0.87", p.Binaries)
		}
		return
	}
	t.Fatal("tool/lark-cli not found")
}

func TestLoadBuiltinLarkProvidersRecommendFullCLIScopes(t *testing.T) {
	m, err := LoadBuiltin()
	if err != nil {
		t.Fatalf("LoadBuiltin() error: %v", err)
	}
	found := map[string]bool{}
	for _, provider := range m.OAuthProviders {
		if provider.ID != "lark" && provider.ID != "feishu" {
			continue
		}
		found[provider.ID] = true
		// The builtin default is the recommended lark-cli capability surface, so
		// one authorization covers every documented command. Admins trim it; it
		// is a floor users can still grow incrementally.
		if len(provider.Scopes) < 100 {
			t.Fatalf("%s defaults = %d scopes, want the full lark-cli capability set", provider.ID, len(provider.Scopes))
		}
		for _, want := range []string{"offline_access", "contact:user.basic_profile:readonly"} {
			if !slices.Contains(provider.Scopes, want) {
				t.Fatalf("%s defaults missing %q", provider.ID, want)
			}
		}
	}
	if !found["lark"] || !found["feishu"] {
		t.Fatalf("providers found = %v, want lark and feishu", found)
	}
}
