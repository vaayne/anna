package host

import (
	"testing"

	"github.com/CherryHQ/stella/internal/plugin"
	pkgplugins "github.com/CherryHQ/stella/pkg/plugins"
)

func TestBuiltinDefinitionsUseCodeRegistrations(t *testing.T) {
	code := pkgplugins.NewCatalog()
	code.Register("channel/test", pkgplugins.PluginFunc(func(h pkgplugins.Host) {
		h.SetInfo(pkgplugins.PluginInfo{ID: "channel/test", Kind: "channel", Name: "test", DisplayName: "Test"})
	}))
	host := New(nil)
	if _, err := host.BuiltinDefinitions(code); err == nil {
		t.Fatal("unregistered implementation accepted")
	}
	if err := host.LoadCatalog(code); err != nil {
		t.Fatal(err)
	}
	definitions, err := host.BuiltinDefinitions(code)
	if err != nil {
		t.Fatal(err)
	}
	if len(definitions) != 1 {
		t.Fatalf("definitions = %d", len(definitions))
	}
	got := definitions[0]
	if got.ID != "channel/test" || got.Namespace != "test" || got.Backend != plugin.BackendGo || got.Source != plugin.SourceBuiltin || !got.DefaultEnabled || string(got.Spec) != `{}` {
		t.Fatalf("unexpected code definition: %#v", got)
	}
	catalog := plugin.NewCatalog()
	if err := catalog.Register(got); err != nil {
		t.Fatal(err)
	}
}

func TestChannelDefinitionsDoNotInheritInstanceConfiguration(t *testing.T) {
	code := defaultCatalog()
	host := New(nil)
	if err := host.LoadCatalog(code); err != nil {
		t.Fatal(err)
	}
	definitions, err := host.BuiltinDefinitions(code)
	if err != nil {
		t.Fatal(err)
	}
	seen := make(map[string]bool)
	for _, definition := range definitions {
		seen[definition.ID] = true
		if string(definition.Spec) != `{}` || !definition.DefaultEnabled {
			t.Errorf("platform %s inherited instance config or disabled default: %#v", definition.ID, definition)
		}
	}
	for _, name := range []string{"telegram", "discord", "qq", "feishu", "dingtalk", "weixin"} {
		if !seen["channel/"+name] {
			t.Errorf("missing channel definition %s", name)
		}
	}
}
