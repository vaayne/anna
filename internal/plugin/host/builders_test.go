package host

import (
	"context"
	"errors"
	"testing"

	"github.com/CherryHQ/stella/pkg/hooks"

	"github.com/CherryHQ/stella/internal/platform/config"
	pkgplugins "github.com/CherryHQ/stella/pkg/plugins"
	"github.com/CherryHQ/stella/pkg/sandbox"
	"github.com/CherryHQ/stella/pkg/tools"
)

type testTool struct{ name string }

func (t *testTool) Definition() tools.Definition { return tools.Definition{Name: t.name} }
func (t *testTool) Execute(context.Context, map[string]any) (string, error) {
	return "ok", nil
}

func TestBuildEnabledToolsBuildsOptionalAndRequiredToolsWithRuntimeContext(t *testing.T) {
	store := &stubStore{plugins: map[string]config.Plugin{
		"tool/a": {ID: "tool/a", Enabled: true},
		"tool/b": {ID: "tool/b", Enabled: true},
	}}
	host := New(store)
	host.RegisterPluginID("tool/a")
	host.RegisterPluginID("tool/b")
	host.RegisterPluginID("tool/c")

	var runtimeSeen int
	fakeRuntime := sandbox.NopSession()
	host.AddTool(pkgplugins.ToolSpec{
		PluginID: "tool/a",
		Name:     "a",
		Build: func(ctx pkgplugins.ToolContext) (tools.Tool, error) {
			if ctx.Runtime != nil {
				runtimeSeen++
			}
			return &testTool{name: "a"}, nil
		},
	})
	host.AddTool(pkgplugins.ToolSpec{
		PluginID: "tool/b",
		Name:     "b",
		Required: true,
		Build: func(ctx pkgplugins.ToolContext) (tools.Tool, error) {
			if ctx.Runtime != nil {
				runtimeSeen++
			}
			return &testTool{name: "b"}, nil
		},
	})
	host.AddTool(pkgplugins.ToolSpec{
		PluginID: "tool/c",
		Name:     "c",
		Build: func(ctx pkgplugins.ToolContext) (tools.Tool, error) {
			t.Fatal("disabled optional tool should not be built")
			return nil, nil
		},
	})

	build := pkgplugins.ToolBuildContext{Runtime: fakeRuntime}
	got, err := host.BuildEnabledTools(context.Background(), build, testHostSnapshot(t, map[string]bool{"tool/a": true, "tool/b": true, "tool/c": false}))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("BuildEnabledTools() len = %d, want 2", len(got))
	}
	if got[0].Definition().Name != "a" || got[1].Definition().Name != "b" {
		t.Fatalf("unexpected tools: %q, %q", got[0].Definition().Name, got[1].Definition().Name)
	}
	if runtimeSeen != 2 {
		t.Fatalf("runtime seen = %d, want 2", runtimeSeen)
	}
}

type closableHostTool struct {
	testTool
	closed int
}

func (t *closableHostTool) Close() error { t.closed++; return nil }

type closableHostHook struct{ closed int }

func (h *closableHostHook) Name() string  { return "a" }
func (h *closableHostHook) Priority() int { return 0 }
func (h *closableHostHook) Close() error  { h.closed++; return nil }
func TestBuildFailureClosesPriorResources(t *testing.T) {
	snapshot := testHostSnapshot(t, map[string]bool{"tool/a": true})
	want := errors.New("build failed")
	t.Run("tools", func(t *testing.T) {
		h := New(nil)
		h.RegisterPluginID("tool/a")
		first := &closableHostTool{testTool: testTool{name: "a"}}
		partial := &closableHostTool{testTool: testTool{name: "b"}}
		h.AddTool(pkgplugins.ToolSpec{PluginID: "tool/a", Name: "a", Build: func(pkgplugins.ToolContext) (tools.Tool, error) { return first, nil }})
		h.AddTool(pkgplugins.ToolSpec{PluginID: "tool/a", Name: "b", Build: func(pkgplugins.ToolContext) (tools.Tool, error) { return partial, want }})
		got, err := h.BuildEnabledTools(t.Context(), pkgplugins.ToolBuildContext{}, snapshot)
		if !errors.Is(err, want) || len(got) != 0 || first.closed != 1 || partial.closed != 1 {
			t.Fatalf("result=%v error=%v closed=%d/%d", got, err, first.closed, partial.closed)
		}
	})
	t.Run("hooks", func(t *testing.T) {
		h := New(nil)
		h.RegisterPluginID("tool/a")
		first := &closableHostHook{}
		h.AddHook(pkgplugins.HookSpec{PluginID: "tool/a", Name: "a", Build: func(pkgplugins.HookContext) (hooks.HookPlugin, error) { return first, nil }})
		h.AddHook(pkgplugins.HookSpec{PluginID: "tool/a", Name: "b", Build: func(pkgplugins.HookContext) (hooks.HookPlugin, error) { return nil, want }})
		got, err := h.BuildEnabledHooks(t.Context(), "", snapshot)
		if !errors.Is(err, want) || len(got) != 0 || first.closed != 1 {
			t.Fatalf("result=%v error=%v closed=%d", got, err, first.closed)
		}
	})
}
