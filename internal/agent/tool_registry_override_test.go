package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/CherryHQ/stella/internal/agent/sandbox"
	coreagent "github.com/CherryHQ/stella/pkg/agent"
	"github.com/CherryHQ/stella/pkg/ai"
	pkgplugins "github.com/CherryHQ/stella/pkg/plugins"
	"github.com/CherryHQ/stella/pkg/providers"
	pkgtools "github.com/CherryHQ/stella/pkg/tools"
)

type staticTool struct{ name string }

func (t staticTool) Definition() pkgtools.Definition {
	return pkgtools.Definition{Name: t.name, Description: t.name}
}

func (t staticTool) Execute(context.Context, map[string]any) (string, error) { return "", nil }
func (t staticTool) ExecuteContent(context.Context, map[string]any) ([]ai.ContentBlock, error) {
	return nil, nil
}

func TestBuildToolRegistryBuildsRuntimeBoundBuiltin(t *testing.T) {
	home := t.TempDir()
	runtime := &fakeSession{alive: true}
	built := false
	reg, _, _, err := buildToolRegistry(context.Background(), runnerConfig{
		Sandbox: sandbox.Config{Paths: sandbox.Paths{
			StellaHome: home,
			AgentRoot:  filepath.Join(home, "agents", "agent-1"),
			UserRoot:   filepath.Join(home, "users", "user-1"),
		}},
		BuiltinParams: RunnerParams{UserID: "user-1", AgentID: "agent-1"},
		BuiltinTools: []BuiltinTool{{Build: func(build pkgplugins.ToolBuildContext) (pkgtools.Tool, error) {
			if build.Runtime != runtime {
				t.Fatalf("runtime = %p, want %p", build.Runtime, runtime)
			}
			built = true
			return staticTool{name: "recally"}, nil
		}, Spec: staticTool{name: "recally"}.Definition()}},
		SkillRevisionReader: emptySkillRuntime{},
		SkillReadAuthorizer: allowSkillReads{},
	}, runtime, nil, ai.Model{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if !built || !reg.Has("recally") {
		t.Fatalf("runtime builtin built=%v registered=%v", built, reg.Has("recally"))
	}
}

func TestBuildToolRegistryAppliesToolOverrides(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	reg, _, _, err := buildToolRegistry(ctx, runnerConfig{
		Sandbox: sandbox.Config{Paths: sandbox.Paths{
			StellaHome: home,
			AgentRoot:  filepath.Join(home, "agents", "agent-1"),
			UserRoot:   filepath.Join(home, "users", "user-1"),
		}},
		BuiltinParams:       RunnerParams{UserID: "user-1", AgentID: "agent-1"},
		BuiltinTools:        []BuiltinTool{{Tool: staticTool{name: "memory"}}},
		SkillRevisionReader: emptySkillRuntime{},
		SkillReadAuthorizer: allowSkillReads{},
		ToolOverrideFetcher: func(context.Context, string, string) ([]ToolOverride, error) {
			return []ToolOverride{{Identity: ToolIdentity{CoreToolName: "memory"}, Scope: ToolOverrideScopeUserAgent, Enabled: false}}, nil
		},
	}, &fakeSession{alive: true}, nil, ai.Model{}, "")
	if err != nil {
		t.Fatalf("buildToolRegistry: %v", err)
	}
	if reg.Has("memory") {
		t.Fatal("memory tool is registered, want filtered by override")
	}
	if !reg.Has("bash") {
		t.Fatal("core tools should remain registered")
	}
}

func TestBuildToolRegistryRejectsEveryReservedCoreName(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })

	for _, name := range []string{"vllm", "view_image", "bash"} {
		t.Run(name, func(t *testing.T) {
			logs.Reset()
			home := t.TempDir()
			reg, _, _, err := buildToolRegistry(context.Background(), runnerConfig{
				Sandbox: sandbox.Config{Paths: sandbox.Paths{
					StellaHome: home,
					AgentRoot:  filepath.Join(home, "agents", "agent-1"),
					UserRoot:   filepath.Join(home, "users", "user-1"),
				}},
				BuiltinParams:       RunnerParams{UserID: "user-1", AgentID: "agent-1"},
				BuiltinTools:        []BuiltinTool{{Tool: staticTool{name: name}}},
				SkillRevisionReader: emptySkillRuntime{},
				SkillReadAuthorizer: allowSkillReads{},
				// Vision intentionally remains nil: vllm is reserved even when its
				// core implementation is unavailable in this runner.
			}, &fakeSession{alive: true}, nil, ai.Model{}, "")
			if err != nil {
				t.Fatalf("buildToolRegistry: %v", err)
			}

			if name == "vllm" && reg.Has(name) {
				t.Fatal("non-core vllm registered while the core implementation was unavailable")
			}
			if name == "view_image" {
				for _, definition := range reg.Definitions() {
					if definition.Name == name && definition.Description == name {
						t.Fatal("builtin view_image replaced the sandbox core implementation")
					}
				}
			}
			if name == "bash" {
				for _, definition := range reg.Definitions() {
					if definition.Name == name && definition.Description == name {
						t.Fatal("builtin bash replaced the sandbox core implementation")
					}
				}
			}
			logText := logs.String()
			for _, want := range []string{"skipping non-core tool with reserved core name", "tool=" + name, "reserved core tool name"} {
				if !strings.Contains(logText, want) {
					t.Fatalf("debug log = %q, missing %q", logText, want)
				}
			}
		})
	}
}

func TestBuildToolRegistryKeepsDelegateInternalOnly(t *testing.T) {
	home := t.TempDir()
	reg, _, delegateTool, err := buildToolRegistry(context.Background(), runnerConfig{
		Sandbox: sandbox.Config{Paths: sandbox.Paths{
			StellaHome: home,
			AgentRoot:  filepath.Join(home, "agents", "agent-1"),
			UserRoot:   filepath.Join(home, "users", "user-1"),
		}},
		BuiltinParams:       RunnerParams{UserID: "user-1", AgentID: "agent-1"},
		BuiltinTools:        []BuiltinTool{{Tool: staticTool{name: "session"}}},
		SkillRevisionReader: emptySkillRuntime{},
		SkillReadAuthorizer: allowSkillReads{},
	}, &fakeSession{alive: true}, nil, ai.Model{}, "")
	if err != nil {
		t.Fatalf("buildToolRegistry: %v", err)
	}
	if delegateTool == nil {
		t.Fatal("internal delegate adapter is unavailable to session.create/send")
	}
	if reg.Has("delegate") {
		t.Fatal("delegate is still registered on the model-facing tool surface")
	}
	if !reg.Has("session") {
		t.Fatal("session replacement is absent from the model-facing tool surface")
	}
}

// A tool_override row is matched by exact name. That is why the split ships a
// migration: an operator's `goal` row keeps its own name after the split and
// stops matching anything, which would hand the whole family back. These two
// cases are the before and after of that migration.
func TestBuildToolRegistryOverridesAreExactNamesNotFamilies(t *testing.T) {
	goalTools := []string{"goal_cancel", "goal_create", "goal_get", "goal_list"}

	build := func(t *testing.T, overrides []ToolOverride) *pkgtools.Registry {
		t.Helper()
		home := t.TempDir()
		builtins := make([]BuiltinTool, 0, len(goalTools)+1)
		for _, name := range append(append([]string(nil), goalTools...), "workflow_run") {
			builtins = append(builtins, BuiltinTool{Tool: staticTool{name: name}})
		}
		reg, _, _, err := buildToolRegistry(context.Background(), runnerConfig{
			Sandbox: sandbox.Config{Paths: sandbox.Paths{
				StellaHome: home,
				AgentRoot:  filepath.Join(home, "agents", "agent-1"),
				UserRoot:   filepath.Join(home, "users", "user-1"),
			}},
			BuiltinParams:       RunnerParams{UserID: "user-1", AgentID: "agent-1"},
			BuiltinTools:        builtins,
			SkillRevisionReader: emptySkillRuntime{},
			SkillReadAuthorizer: allowSkillReads{},
			ToolOverrideFetcher: func(context.Context, string, string) ([]ToolOverride, error) {
				return overrides, nil
			},
		}, &fakeSession{alive: true}, nil, ai.Model{}, "")
		if err != nil {
			t.Fatalf("buildToolRegistry: %v", err)
		}
		return reg
	}

	t.Run("unmigrated union row hides nothing", func(t *testing.T) {
		reg := build(t, []ToolOverride{{Identity: ToolIdentity{CoreToolName: "goal"}, Scope: ToolOverrideScopeUserAgent, Enabled: false}})
		for _, name := range goalTools {
			if !reg.Has(name) {
				t.Fatalf("%s was hidden by an unmigrated union row", name)
			}
		}
	})

	t.Run("migrated action rows hide the whole family", func(t *testing.T) {
		var overrides []ToolOverride
		for _, name := range goalTools {
			overrides = append(overrides, ToolOverride{Identity: ToolIdentity{CoreToolName: name}, Scope: ToolOverrideScopeUserAgent, Enabled: false})
		}
		reg := build(t, overrides)
		for _, name := range goalTools {
			if reg.Has(name) {
				t.Errorf("%s is registered, want hidden by its migrated override", name)
			}
		}
		if !reg.Has("workflow_run") {
			t.Error("a sibling family lost a tool to goal's overrides")
		}
	})
}

// A tool_override is only worth migrating if it reaches the model: registry
// membership is one hop short of what the model can actually call. Under Code
// Mode that reach has two halves, and the test asserts both: the hot tools the
// provider is offered, and the cold catalog behind the `code` tool, which is
// where a disabled tool would otherwise still be callable.
func TestMigratedOverridesNeverReachTheProviderRequest(t *testing.T) {
	goalTools := []string{"goal_cancel", "goal_create", "goal_get", "goal_list"}
	home := t.TempDir()

	builtins := make([]BuiltinTool, 0, len(goalTools)+1)
	for _, name := range append(append([]string(nil), goalTools...), "workflow_run") {
		builtins = append(builtins, BuiltinTool{Tool: staticTool{name: name}})
	}
	overrides := make([]ToolOverride, 0, len(goalTools))
	for _, name := range goalTools {
		overrides = append(overrides, ToolOverride{Identity: ToolIdentity{CoreToolName: name}, Scope: ToolOverrideScopeUserAgent, Enabled: false})
	}

	reg, _, _, err := buildToolRegistry(context.Background(), runnerConfig{
		Sandbox: sandbox.Config{Paths: sandbox.Paths{
			StellaHome: home,
			AgentRoot:  filepath.Join(home, "agents", "agent-1"),
			UserRoot:   filepath.Join(home, "users", "user-1"),
		}},
		BuiltinParams:       RunnerParams{UserID: "user-1", AgentID: "agent-1"},
		BuiltinTools:        builtins,
		SkillRevisionReader: emptySkillRuntime{},
		SkillReadAuthorizer: allowSkillReads{},
		ToolOverrideFetcher: func(context.Context, string, string) ([]ToolOverride, error) {
			return overrides, nil
		},
	}, &fakeSession{alive: true}, nil, ai.Model{}, "")
	if err != nil {
		t.Fatalf("buildToolRegistry: %v", err)
	}

	// The catalog has no Go-side accessor, by design: it exists for the model.
	// The first turn calls `code` and pages tools.search back out, which is the
	// same view a real turn gets; the second turn reads that result and stops.
	const listCatalog = `{"code":"const names = []; for (let offset = 0; ; ) { const page = tools.search(\"\", offset); for (const tool of page) { names.push(tool.name); } if (!page.hasMore) { break; } offset = page.nextOffset; } return names;"}`

	var offered []string
	var catalog []string
	turns := 0
	stream := func(_ context.Context, _ ai.Model, aiCtx ai.Context, _ ai.StreamOptions) (providers.AssistantEventStream, error) {
		turns++
		events := providers.NewChannelEventStream(2)
		if turns == 1 {
			for _, definition := range aiCtx.Tools {
				offered = append(offered, definition.Name)
			}
			go func() {
				events.Emit(ai.EventToolCallDelta{ID: "catalog", Name: coreagent.CodeToolName, Arguments: listCatalog})
				events.Emit(ai.EventStop{Reason: ai.StopReasonToolUse})
				events.Finish(nil)
			}()
			return events, nil
		}
		for _, message := range aiCtx.Messages {
			result, ok := message.(ai.ToolResultMessage)
			if !ok || result.ToolCallID != "catalog" {
				continue
			}
			if result.IsError {
				t.Errorf("listing the code catalog failed: %s", ai.FlattenText(result.Content))
				break
			}
			if err := json.Unmarshal([]byte(ai.FlattenText(result.Content)), &catalog); err != nil {
				t.Errorf("decode code catalog %q: %v", ai.FlattenText(result.Content), err)
			}
		}
		go func() {
			events.Emit(ai.EventStop{Reason: ai.StopReasonStop})
			events.Finish(nil)
		}()
		return events, nil
	}
	model := ai.Model{ID: "test", API: "test", Name: "test"}
	coreRunner, err := newAgentRunner(stream, reg, model, ai.StreamOptions{}, "system", nil, nil, nil, coreagent.CodeToolSurfaceHot)
	if err != nil {
		t.Fatalf("newAgentRunner: %v", err)
	}
	r := &runner{
		runner:       coreRunner,
		stream:       stream,
		tools:        reg,
		model:        model,
		system:       "system",
		chatTimeout:  30 * time.Second,
		lastActivity: time.Now(),
	}
	for event := range r.Chat(context.Background(), nil, "work") {
		if event.Err != nil {
			t.Fatalf("Chat: %v", event.Err)
		}
	}

	for _, name := range goalTools {
		if slices.Contains(offered, name) {
			t.Errorf("%s was offered to the provider despite a disabling override", name)
		}
		if slices.Contains(catalog, name) {
			t.Errorf("%s was in the code catalog despite a disabling override (catalog: %v)", name, catalog)
		}
	}
	if !slices.Contains(catalog, "workflow_run") {
		t.Errorf("workflow_run was not in the code catalog; goal's overrides took a sibling family with them (catalog: %v, offered: %v)", catalog, offered)
	}
}
