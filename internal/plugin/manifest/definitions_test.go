package manifest

import (
	"encoding/json"
	"testing"

	"github.com/CherryHQ/stella/internal/plugin"
)

func TestBuiltinDefinitionsPreserveReleasePayload(t *testing.T) {
	authored, err := LoadBuiltin()
	if err != nil {
		t.Fatal(err)
	}
	definitions, err := BuiltinDefinitions()
	if err != nil {
		t.Fatal(err)
	}
	if len(definitions) != len(authored.Plugins) {
		t.Fatalf("definitions = %d, release plugins = %d", len(definitions), len(authored.Plugins))
	}
	catalog := plugin.NewCatalog()
	for i, definition := range definitions {
		original := authored.Plugins[i]
		if err := catalog.Register(definition); err != nil {
			t.Fatal(err)
		}
		if definition.ID != original.ID || definition.Namespace != original.Name || definition.Backend != plugin.BackendCLI || definition.Source != plugin.SourceBuiltin || definition.DefaultEnabled != original.Enabled {
			t.Fatalf("release identity or default changed: %#v", definition)
		}
		var decoded ManifestPluginDefinition
		if err := json.Unmarshal(definition.Spec, &decoded); err != nil {
			t.Fatal(err)
		}
		decoded.Name, decoded.DisplayName = original.Name, original.DisplayName
		got, err := json.Marshal(decoded)
		if err != nil {
			t.Fatal(err)
		}
		want, err := json.Marshal(original.ManifestPluginDefinition)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != string(want) {
			t.Fatalf("plugin %s lost release configuration", definition.ID)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(definition.Spec, &fields); err != nil {
			t.Fatal(err)
		}
		for _, authority := range []string{"name", "display_name", "enabled", "essential", "builtin", "overridden_fields"} {
			if _, exists := fields[authority]; exists {
				t.Errorf("plugin %s retains separate %s authority", definition.ID, authority)
			}
		}
	}
}
