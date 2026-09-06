package runtime

import (
	"testing"

	"github.com/CherryHQ/stella/internal/plugin"
	pkgplugins "github.com/CherryHQ/stella/pkg/plugins"
)

func TestPluginContextCopiesViewSlicesAndMaps(t *testing.T) {
	view := pkgplugins.SessionPluginView{
		RegisteredPluginIDs: []string{"plugin/a"},
		ExposedPluginIDs:    []string{"plugin/a"},
		BinarySpecs: []pkgplugins.PluginBinarySpec{{Options: map[string]any{
			"token":  "secret",
			"nested": map[string]any{"scope": "private"},
			"items":  []any{map[string]any{"value": "original"}},
		}}},
	}
	ctx := NewPluginContext(plugin.Snapshot{}, view)
	view.RegisteredPluginIDs[0] = "plugin/changed"
	view.BinarySpecs[0].Options["token"] = "changed"
	view.BinarySpecs[0].Options["nested"].(map[string]any)["scope"] = "changed"
	view.BinarySpecs[0].Options["items"].([]any)[0].(map[string]any)["value"] = "changed"

	got := ctx.SessionPluginView()
	got.RegisteredPluginIDs[0] = "plugin/mutated"
	got.BinarySpecs[0].Options["token"] = "mutated"
	got.BinarySpecs[0].Options["nested"].(map[string]any)["scope"] = "mutated"
	got.BinarySpecs[0].Options["items"].([]any)[0].(map[string]any)["value"] = "mutated"
	want := ctx.SessionPluginView()
	nested := want.BinarySpecs[0].Options["nested"].(map[string]any)
	items := want.BinarySpecs[0].Options["items"].([]any)
	item := items[0].(map[string]any)
	if want.RegisteredPluginIDs[0] != "plugin/a" || want.BinarySpecs[0].Options["token"] != "secret" || nested["scope"] != "private" || item["value"] != "original" {
		t.Fatalf("plugin context view was mutable: %#v", want)
	}
}
