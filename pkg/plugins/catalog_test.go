package plugins

import (
	"reflect"
	"testing"
)

type stubHost struct{}

func (h stubHost) SetInfo(PluginInfo)                     {}
func (h stubHost) AddAdmin(AdminSpec)                     {}
func (h stubHost) AddTool(ToolSpec)                       {}
func (h stubHost) AddChannel(ChannelSpec)                 {}
func (h stubHost) AddHook(HookSpec)                       {}
func (h stubHost) AddRuntime(RuntimeSpec)                 {}
func (h stubHost) AddPromptInventory(PromptInventorySpec) {}
func (h stubHost) AddSystemPrompt(SystemPromptSpec)       {}
func (h stubHost) AddBeforeRun(BeforeRunSpec)             {}
func (h stubHost) AddBeforeToolCall(BeforeToolCallSpec)   {}
func (h stubHost) AddAfterToolResult(AfterToolResultSpec) {}
func (h stubHost) AddSessionEnv(SessionEnvSpec)           {}

func TestPluginFuncRegister(t *testing.T) {
	called := false
	plugin := PluginFunc(func(host Host) {
		called = true
		if host == nil {
			t.Fatal("expected host")
		}
	})

	plugin.Register(stubHost{})
	if !called {
		t.Fatal("expected plugin func to be called")
	}
}

func TestCatalogRegisterGetAndNames(t *testing.T) {
	catalog := NewCatalog()
	pluginA := PluginFunc(func(Host) {})
	pluginB := PluginFunc(func(Host) {})

	catalog.Register("zeta", pluginA)
	catalog.Register("alpha", pluginB)

	got, ok := catalog.Get("alpha")
	if !ok {
		t.Fatal("expected alpha plugin to exist")
	}
	if reflect.ValueOf(got).Pointer() != reflect.ValueOf(pluginB).Pointer() {
		t.Fatal("expected alpha plugin to round-trip")
	}

	if names := catalog.Names(); !reflect.DeepEqual(names, []string{"alpha", "zeta"}) {
		t.Fatalf("unexpected names: %#v", names)
	}
}

func TestCatalogRegisterPanicsOnDuplicate(t *testing.T) {
	catalog := NewCatalog()
	catalog.Register("dup", PluginFunc(func(Host) {}))

	defer func() {
		if recover() == nil {
			t.Fatal("expected duplicate register to panic")
		}
	}()

	catalog.Register("dup", PluginFunc(func(Host) {}))
}

func TestCatalogRegisterPanicsOnInvalidInput(t *testing.T) {
	t.Run("empty id", func(t *testing.T) {
		catalog := NewCatalog()
		defer func() {
			if recover() == nil {
				t.Fatal("expected empty id to panic")
			}
		}()
		catalog.Register("", PluginFunc(func(Host) {}))
	})

	t.Run("nil plugin", func(t *testing.T) {
		catalog := NewCatalog()
		defer func() {
			if recover() == nil {
				t.Fatal("expected nil plugin to panic")
			}
		}()
		catalog.Register("nil", nil)
	})
}
