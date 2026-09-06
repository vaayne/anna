package agent

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/CherryHQ/stella/internal/agent/sandbox"
	"github.com/CherryHQ/stella/internal/authz"
	"github.com/CherryHQ/stella/internal/platform/config"
	"github.com/CherryHQ/stella/internal/plugin"
	coreagent "github.com/CherryHQ/stella/pkg/agent"
	"github.com/CherryHQ/stella/pkg/ai"
	"github.com/CherryHQ/stella/pkg/hooks"
	pkgplugins "github.com/CherryHQ/stella/pkg/plugins"
	"github.com/CherryHQ/stella/pkg/providers"
	"github.com/CherryHQ/stella/pkg/toolmeta"
	pkgtools "github.com/CherryHQ/stella/pkg/tools"
)

type cleanupTool struct {
	name   string
	closed atomic.Int32
}

type cleanupHook struct {
	closed atomic.Int32
}

func (*cleanupHook) Name() string  { return "cleanup-hook" }
func (*cleanupHook) Priority() int { return 0 }
func (h *cleanupHook) Close() error {
	h.closed.Add(1)
	return nil
}

func (t *cleanupTool) Definition() pkgtools.Definition {
	return pkgtools.Definition{Name: t.name}
}

func (*cleanupTool) Execute(context.Context, map[string]any) (string, error) { return "", nil }

func (t *cleanupTool) Close() error {
	t.closed.Add(1)
	return nil
}

func cleanupProviderBuilder(string, string, string) (providers.StreamFunc, error) {
	return func(context.Context, ai.Model, ai.Context, ai.StreamOptions) (providers.AssistantEventStream, error) {
		return nil, errors.New("unused")
	}, nil
}

func TestBuildToolRegistryClosesCandidatesAfterCollision(t *testing.T) {
	first := &cleanupTool{name: "same"}
	second := &cleanupTool{name: "same"}
	cfg := failClosedConfig(t)
	cfg.BuiltinTools = []BuiltinTool{{Tool: first}}
	cfg.PerRunTools = []pkgtools.Tool{second}

	_, _, _, err := buildToolRegistry(context.Background(), cfg, &fakeSession{alive: true}, nil, ai.Model{}, "")
	if err == nil {
		t.Fatal("expected duplicate tool error")
	}
	if got := first.closed.Load(); got != 1 {
		t.Fatalf("first tool close count = %d, want 1", got)
	}
	if got := second.closed.Load(); got != 1 {
		t.Fatalf("second tool close count = %d, want 1", got)
	}
}

func TestBuildToolRegistryClosesFilteredCandidateAndLaterError(t *testing.T) {
	filtered := &cleanupTool{name: "hidden__run"}
	invalid := &cleanupTool{name: "broken"}
	cfg := failClosedConfig(t)
	cfg.PluginContext = PluginContext{}
	cfg.PluginTools = func(context.Context, pkgplugins.ToolBuildContext, plugin.Snapshot) ([]pkgtools.Tool, error) {
		return []pkgtools.Tool{filtered, invalid}, nil
	}
	cfg.ToolMetaRegistry = toolmeta.NewRegistry(
		toolmeta.ActionTool{Name: "hidden__run", PluginID: "hidden-plugin", Namespace: "hidden", LocalName: "run"},
		toolmeta.ActionTool{Name: "broken", PluginID: "broken-plugin", Namespace: "broken"},
	)

	_, _, _, err := buildToolRegistry(context.Background(), cfg, &fakeSession{alive: true}, nil, ai.Model{}, "")
	if err == nil {
		t.Fatal("expected malformed plugin metadata error")
	}
	if got := filtered.closed.Load(); got != 1 {
		t.Fatalf("filtered tool close count = %d, want 1", got)
	}
	if got := invalid.closed.Load(); got != 1 {
		t.Fatalf("invalid tool close count = %d, want 1", got)
	}
}

func TestBuildToolRegistryClosesAllPluginToolsWhenIdentityFails(t *testing.T) {
	first := &cleanupTool{name: "broken-one"}
	second := &cleanupTool{name: "broken-two"}
	cfg := failClosedConfig(t)
	cfg.PluginTools = func(context.Context, pkgplugins.ToolBuildContext, plugin.Snapshot) ([]pkgtools.Tool, error) {
		return []pkgtools.Tool{first, second}, nil
	}
	cfg.ToolMetaRegistry = toolmeta.NewRegistry(
		toolmeta.ActionTool{Name: "broken-one", PluginID: "plugin", Namespace: "plugin"},
		toolmeta.ActionTool{Name: "broken-two", PluginID: "plugin", Namespace: "plugin"},
	)

	_, _, _, err := buildToolRegistry(context.Background(), cfg, &fakeSession{alive: true}, nil, ai.Model{}, "")
	if err == nil {
		t.Fatal("expected malformed plugin metadata error")
	}
	if got := first.closed.Load(); got != 1 {
		t.Fatalf("first plugin tool close count = %d, want 1", got)
	}
	if got := second.closed.Load(); got != 1 {
		t.Fatalf("second plugin tool close count = %d, want 1", got)
	}
}

func TestBuildToolRegistryClosesBuiltinReturnedWithError(t *testing.T) {
	built := &cleanupTool{name: "partial"}
	buildErr := errors.New("partial build")
	cfg := failClosedConfig(t)
	cfg.BuiltinTools = []BuiltinTool{{
		Build: func(pkgplugins.ToolBuildContext) (pkgtools.Tool, error) {
			return built, buildErr
		},
		Spec: built.Definition(),
	}}

	_, _, _, err := buildToolRegistry(context.Background(), cfg, &fakeSession{alive: true}, nil, ai.Model{}, "")
	if !errors.Is(err, buildErr) {
		t.Fatalf("build error = %v, want build error", err)
	}
	if got := built.closed.Load(); got != 1 {
		t.Fatalf("partially built tool close count = %d, want 1", got)
	}
}

func TestNewRunnerClosesRegistryWhenCoreRunnerBuildFails(t *testing.T) {
	built := &cleanupTool{name: "built"}
	cfg := withTestRunnerPaths(t, runnerConfig{
		Provider: providerConfig{
			API:     "anthropic",
			Model:   "model",
			APIKey:  "key",
			Builder: cleanupProviderBuilder,
		},
		BuiltinTools:        []BuiltinTool{{Tool: built}},
		SkillRevisionReader: emptySkillRuntime{},
		SkillReadAuthorizer: allowSkillReads{},
		Sandbox: sandbox.Config{
			SandboxBackendFn: func(context.Context) string { return config.SandboxBackendNone },
			Backends:         testSandboxBackends(t),
		},
		CodeToolSurface: coreagent.CodeToolSurface("invalid"),
	})
	cfg.Sandbox.CoreRuntimePlan = fixtureRunnerCoreRuntimePlan(t, cfg.Sandbox.Paths.StellaHome)

	if _, err := newRunner(context.Background(), cfg); err == nil {
		t.Fatal("expected invalid code tool surface error")
	}
	if got := built.closed.Load(); got != 1 {
		t.Fatalf("built tool close count = %d, want 1", got)
	}
}

func TestNewRunnerClosesHooksWhenSandboxBuildFails(t *testing.T) {
	hook := &cleanupHook{}
	cfg := withTestRunnerPaths(t, runnerConfig{
		Provider: providerConfig{
			API:     "anthropic",
			Model:   "model",
			APIKey:  "key",
			Builder: cleanupProviderBuilder,
		},
		PluginHookPlugins: []hooks.HookPlugin{hook},
	})

	if _, err := newRunner(context.Background(), cfg); err == nil {
		t.Fatal("expected missing sandbox backend error")
	}
	if got := hook.closed.Load(); got != 1 {
		t.Fatalf("plugin hook close count = %d, want 1", got)
	}
}

func TestNewRunnerFuncClosesHooksWhenLifecycleBuildFails(t *testing.T) {
	lifeErr := errors.New("lifecycle unavailable")
	hook := &cleanupHook{}
	stellaHome := t.TempDir()
	snap := &config.Snapshot{
		Provider:     "anthropic",
		Model:        "model",
		APIKey:       "key",
		Workspace:    t.TempDir(),
		SystemPrompt: "system",
	}
	build := newRunnerFunc(withTestSkillDependencies(runnerBuilderConfig{
		Snap:                  snap,
		Home:                  testWorkspaceViewer{root: stellaHome},
		ProviderStreamBuilder: cleanupProviderBuilder,
		SandboxBackendFn:      func(context.Context) string { return config.SandboxBackendNone },
		SandboxBackends:       testSandboxBackends(t),
		PluginContextBuilder: func(context.Context, authz.Authority, string) (PluginContext, error) {
			return PluginContext{}, nil
		},
		PluginHooksBuilder: func(context.Context, plugin.Snapshot) ([]hooks.HookPlugin, error) {
			return []hooks.HookPlugin{hook}, nil
		},
		ToolLifecycleBuilder: func(context.Context, plugin.Snapshot) (*coreagent.ToolLifecycle, error) {
			return nil, lifeErr
		},
	}))

	_, err := build(context.Background(), RunnerParams{UserID: "user", AgentID: "agent"})
	if !errors.Is(err, lifeErr) {
		t.Fatalf("build error = %v, want lifecycle error", err)
	}
	if got := hook.closed.Load(); got != 1 {
		t.Fatalf("plugin hook close count = %d, want 1", got)
	}
}
