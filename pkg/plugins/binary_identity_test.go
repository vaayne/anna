package plugins

import "testing"

func TestBinaryArtifactIdentityNormalizesVersionAndOptions(t *testing.T) {
	base := PluginBinarySpec{Name: "gh", Tool: "github:cli/cli"}
	latest := base
	latest.Version = "latest"
	if got, err := BinaryArtifactIdentity(base); err != nil {
		t.Fatal(err)
	} else if want, err := BinaryArtifactIdentity(latest); err != nil || got != want {
		t.Fatalf("empty version identity = %q, latest identity = %q, err=%v", got, want, err)
	}
	empty := base
	empty.Options = map[string]any{}
	if got, err := BinaryArtifactIdentity(base); err != nil {
		t.Fatal(err)
	} else if want, err := BinaryArtifactIdentity(empty); err != nil || got != want {
		t.Fatalf("nil options identity = %q, empty options identity = %q, err=%v", got, want, err)
	}
}

func TestBinaryArtifactIdentityCanonicalizesNestedOptions(t *testing.T) {
	first := PluginBinarySpec{
		Name: "demo", Tool: "github:owner/demo", Version: "1",
		Options: map[string]any{"z": "last", "nested": map[string]any{"b": 2, "a": 1}},
	}
	second := PluginBinarySpec{
		Name: "demo", Tool: "github:owner/demo", Version: "1",
		Options: map[string]any{"nested": map[string]any{"a": 1, "b": 2}, "z": "last"},
	}
	one, err := BinaryArtifactIdentity(first)
	if err != nil {
		t.Fatal(err)
	}
	two, err := BinaryArtifactIdentity(second)
	if err != nil {
		t.Fatal(err)
	}
	if one != two {
		t.Fatalf("equivalent nested options produced different identities: %q != %q", one, two)
	}
	if one == "" {
		t.Fatal("identity must not be empty")
	}
}

func TestBinaryArtifactIdentityExcludesResourceOwnership(t *testing.T) {
	first := PluginBinarySpec{Name: "uv", Tool: "uv", Version: "1", PluginResourceIdentity: PluginResourceIdentity{PluginID: "plugin/one", ConfigID: "config/one", Scope: "system", Revision: 1}}
	second := first
	second.PluginResourceIdentity = PluginResourceIdentity{PluginID: "plugin/two", ConfigID: "config/two", Scope: "user_agent", Revision: 9}
	one, err := BinaryArtifactIdentity(first)
	if err != nil {
		t.Fatal(err)
	}
	two, err := BinaryArtifactIdentity(second)
	if err != nil {
		t.Fatal(err)
	}
	if one != two {
		t.Fatalf("resource ownership changed artifact identity: %q != %q", one, two)
	}
}
