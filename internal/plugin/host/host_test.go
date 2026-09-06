package host

import (
	"context"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/CherryHQ/stella/internal/platform/config"
	internalplugin "github.com/CherryHQ/stella/internal/plugin"
	"github.com/CherryHQ/stella/internal/plugin/manifest"
	pkgplugins "github.com/CherryHQ/stella/pkg/plugins"
)

type stubStore struct {
	plugins       map[string]config.Plugin
	channels      map[string][]config.Channel
	channelsError error
}

func (s *stubStore) ListProviders(context.Context) ([]config.Provider, error) { return nil, nil }
func (s *stubStore) GetProvider(context.Context, string) (config.Provider, error) {
	return config.Provider{}, nil
}
func (s *stubStore) CreateProvider(context.Context, config.Provider) error { return nil }
func (s *stubStore) UpdateProvider(context.Context, config.Provider) error { return nil }
func (s *stubStore) DeleteProvider(context.Context, string) error          { return nil }
func (s *stubStore) ListCachedModels(context.Context) ([]config.CachedModel, error) {
	return nil, nil
}
func (s *stubStore) ReplaceCachedModels(context.Context, string, []string) error { return nil }
func (s *stubStore) Seed(context.Context) error                                  { return nil }
func (s *stubStore) ListAgents(context.Context) ([]config.Agent, error)          { return nil, nil }
func (s *stubStore) ListEnabledAgents(context.Context) ([]config.Agent, error)   { return nil, nil }
func (s *stubStore) GetAgent(context.Context, string) (config.Agent, error) {
	return config.Agent{}, nil
}
func (s *stubStore) CreateAgent(context.Context, config.Agent) error        { return nil }
func (s *stubStore) UpdateAgent(context.Context, config.Agent) error        { return nil }
func (s *stubStore) DeleteAgent(context.Context, string) error              { return nil }
func (s *stubStore) ListChannels(context.Context) ([]config.Channel, error) { return nil, nil }
func (s *stubStore) ListChannelsByType(_ context.Context, channelType string) ([]config.Channel, error) {
	if s.channelsError != nil {
		return nil, s.channelsError
	}
	return slices.Clone(s.channels[channelType]), nil
}

func (s *stubStore) GetChannel(_ context.Context, id string) (config.Channel, error) {
	for _, channels := range s.channels {
		for _, channel := range channels {
			if channel.ID == id {
				return channel, nil
			}
		}
	}
	return config.Channel{}, config.ErrChannelNotFound
}
func (s *stubStore) CreateChannel(context.Context, config.Channel) error { return nil }
func (s *stubStore) UpdateChannel(context.Context, config.Channel) error { return nil }
func (s *stubStore) DeleteChannel(context.Context, string) error         { return nil }
func (s *stubStore) ListPlugins(context.Context) ([]config.Plugin, error) {
	plugins := make([]config.Plugin, 0, len(s.plugins))
	for _, plugin := range s.plugins {
		plugins = append(plugins, plugin)
	}
	sort.Slice(plugins, func(i, j int) bool {
		if plugins[i].Kind != plugins[j].Kind {
			return plugins[i].Kind < plugins[j].Kind
		}
		return plugins[i].Name < plugins[j].Name
	})
	return plugins, nil
}

func (s *stubStore) ListPluginOverrides(ctx context.Context) ([]config.Plugin, error) {
	return s.ListPlugins(ctx)
}

func (s *stubStore) ListEnabledPlugins(context.Context) ([]config.Plugin, error) { return nil, nil }
func (s *stubStore) GetPlugin(_ context.Context, id string) (config.Plugin, error) {
	return s.plugins[id], nil
}
func (s *stubStore) UpsertPlugin(context.Context, config.Plugin) error { return nil }
func (s *stubStore) SetPluginEnabled(_ context.Context, id string, enabled bool) error {
	p := s.plugins[id]
	p.Enabled = enabled
	s.plugins[id] = p
	return nil
}

func (s *stubStore) SetPluginConfig(_ context.Context, id string, cfg map[string]any) error {
	p := s.plugins[id]
	p.Config = cfg
	s.plugins[id] = p
	return nil
}

// The real store writes only the config column here; the stub mirrors that by
// leaving Enabled alone, and creating a missing row enabled.
func (s *stubStore) SetChannelPluginConfig(_ context.Context, id, kind, name string, cfg map[string]any) error {
	p, ok := s.plugins[id]
	if !ok {
		p = config.Plugin{ID: id, Kind: kind, Name: name, Enabled: true}
	}
	p.Config = cfg
	s.plugins[id] = p
	return nil
}

func (s *stubStore) DeletePlugin(context.Context, string) error { return nil }
func (s *stubStore) GetManifestPluginOverride(context.Context, string) (config.ManifestPluginOverride, bool, error) {
	return config.ManifestPluginOverride{}, false, nil
}

func (s *stubStore) ListManifestPluginOverrides(context.Context) ([]config.ManifestPluginOverride, error) {
	return nil, nil
}

func (s *stubStore) UpsertManifestPluginOverride(context.Context, config.ManifestPluginOverride) error {
	return nil
}
func (s *stubStore) DeleteManifestPluginOverride(context.Context, string) error { return nil }
func (s *stubStore) GetChatAgent(context.Context, string, string, string) (string, error) {
	return "", nil
}
func (s *stubStore) SetChatAgent(context.Context, string, string, string, string) error { return nil }
func (s *stubStore) DeleteChatAgent(context.Context, string, string, string) error      { return nil }
func (s *stubStore) GetSetting(context.Context, string) (string, error)                 { return "", nil }
func (s *stubStore) SetSetting(context.Context, string, string) error                   { return nil }
func (s *stubStore) Snapshot(context.Context, string) (*config.Snapshot, error)         { return nil, nil }

func TestConfigServiceUsesPluginIDDirectly(t *testing.T) {
	store := &stubStore{plugins: map[string]config.Plugin{"tool/test": {ID: "tool/test", Enabled: true, Config: map[string]any{"x": 1}}}}
	host := New(store)
	state, err := host.DesiredState(context.Background(), "tool/test")
	if err != nil {
		t.Fatal(err)
	}
	if state.ID != "tool/test" || !state.Enabled || state.Config["x"] != 1 {
		t.Fatalf("bad state: %#v", state)
	}
}

func TestPromptToolsUsesPluginIDDirectly(t *testing.T) {
	store := &stubStore{plugins: map[string]config.Plugin{}}
	host := New(store)
	host.RegisterPluginID("tool/test")
	host.AddPromptInventory(pkgplugins.PromptInventorySpec{PluginID: "tool/test", Name: "tools", GetTools: func(context.Context, pkgplugins.PromptInventoryContext) ([]pkgplugins.PromptToolInfo, error) {
		return []pkgplugins.PromptToolInfo{{Name: "test__docs__search"}}, nil
	}})
	tools, err := host.PromptTools(context.Background(), "tool/test", testHostSnapshot(t, map[string]bool{"tool/test": true}))
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 1 || tools[0].Name != "test__docs__search" {
		t.Fatalf("unexpected tools: %#v", tools)
	}
}

func TestSystemPromptSectionsUsePluginIDDirectly(t *testing.T) {
	store := &stubStore{plugins: map[string]config.Plugin{}}
	host := New(store)
	host.RegisterPluginID("tool/skills")
	host.AddSystemPrompt(pkgplugins.SystemPromptSpec{
		PluginID: "tool/skills",
		Name:     "skills",
		Required: true,
		Build: func(context.Context, pkgplugins.SystemPromptContext) (pkgplugins.SystemPromptSection, error) {
			return pkgplugins.SystemPromptSection{
				Title:   "Skills",
				Content: "<available_skills></available_skills>",
			}, nil
		},
	})
	sections, err := host.SystemPromptSections(context.Background(), pkgplugins.SystemPromptContext{}, testHostSnapshot(t, map[string]bool{"tool/skills": true}))
	if err != nil {
		t.Fatal(err)
	}
	if len(sections) != 1 || sections[0].Title != "Skills" {
		t.Fatalf("unexpected prompt sections: %#v", sections)
	}
}

func TestBeforeRunUsesPluginIDDirectly(t *testing.T) {
	store := &stubStore{plugins: map[string]config.Plugin{}}
	host := New(store)
	host.RegisterPluginID("tool/skills")
	host.AddBeforeRun(pkgplugins.BeforeRunSpec{
		PluginID: "tool/skills",
		Name:     "skills",
		Required: true,
		Run: func(context.Context, pkgplugins.BeforeRunContext) (pkgplugins.BeforeRunResult, error) {
			return pkgplugins.BeforeRunResult{SystemPrompt: "override"}, nil
		},
	})

	result, err := host.BeforeRun(context.Background(), pkgplugins.BeforeRunContext{SystemPrompt: "base"}, testHostSnapshot(t, map[string]bool{"tool/skills": true}))
	if err != nil {
		t.Fatal(err)
	}
	if result.SystemPrompt != "override" {
		t.Fatalf("unexpected before-run result: %#v", result)
	}
}

func TestBeforeToolCallUsesPluginIDDirectly(t *testing.T) {
	store := &stubStore{plugins: map[string]config.Plugin{}}
	host := New(store)
	host.RegisterPluginID("tool/filter")
	host.AddBeforeToolCall(pkgplugins.BeforeToolCallSpec{
		PluginID: "tool/filter",
		Name:     "filter",
		Required: true,
		Run: func(context.Context, pkgplugins.BeforeToolCallContext) (pkgplugins.BeforeToolCallResult, error) {
			return pkgplugins.BeforeToolCallResult{
				Arguments:    map[string]any{"q": "rewritten"},
				Block:        true,
				BlockMessage: "blocked",
			}, nil
		},
	})

	result, err := host.BeforeToolCall(context.Background(), pkgplugins.BeforeToolCallContext{
		ToolName:  "web_fetch",
		Arguments: map[string]any{"q": "original"},
	}, testHostSnapshot(t, map[string]bool{"tool/filter": true}))
	if err != nil {
		t.Fatal(err)
	}
	if !result.Block || result.BlockMessage != "blocked" {
		t.Fatalf("unexpected before-tool result: %#v", result)
	}
	if got := result.Arguments["q"]; got != "rewritten" {
		t.Fatalf("arguments not rewritten: %#v", result.Arguments)
	}
}

func TestAfterToolResultUsesPluginIDDirectly(t *testing.T) {
	store := &stubStore{plugins: map[string]config.Plugin{}}
	host := New(store)
	host.RegisterPluginID("tool/filter")
	host.AddAfterToolResult(pkgplugins.AfterToolResultSpec{
		PluginID: "tool/filter",
		Name:     "filter",
		Required: true,
		Run: func(context.Context, pkgplugins.AfterToolResultContext) (pkgplugins.AfterToolResult, error) {
			text := "rewritten"
			isError := false
			return pkgplugins.AfterToolResult{Result: &text, IsError: &isError}, nil
		},
	})

	result, err := host.AfterToolResult(context.Background(), pkgplugins.AfterToolResultContext{
		ToolName: "web_fetch",
		Result:   "original",
		IsError:  true,
	}, testHostSnapshot(t, map[string]bool{"tool/filter": true}))
	if err != nil {
		t.Fatal(err)
	}
	if result.Result == nil || *result.Result != "rewritten" {
		t.Fatalf("unexpected after-tool result: %#v", result)
	}
	if result.IsError == nil || *result.IsError {
		t.Fatalf("unexpected after-tool error flag: %#v", result)
	}
}

func TestAfterToolResultNoopDoesNotReturnTextMutation(t *testing.T) {
	store := &stubStore{plugins: map[string]config.Plugin{}}
	host := New(store)
	host.RegisterPluginID("tool/noop")
	host.AddAfterToolResult(pkgplugins.AfterToolResultSpec{
		PluginID: "tool/noop",
		Name:     "noop",
		Required: true,
		Run: func(context.Context, pkgplugins.AfterToolResultContext) (pkgplugins.AfterToolResult, error) {
			return pkgplugins.AfterToolResult{}, nil
		},
	})

	result, err := host.AfterToolResult(context.Background(), pkgplugins.AfterToolResultContext{
		ToolName: "read",
		Result:   "Read image file [image/jpeg]",
		IsError:  false,
	}, testHostSnapshot(t, map[string]bool{"tool/noop": true}))
	if err != nil {
		t.Fatal(err)
	}
	if result.Result != nil {
		t.Fatalf("noop lifecycle must not return text mutation: %#v", result)
	}
	if result.IsError != nil {
		t.Fatalf("noop lifecycle must not return error mutation: %#v", result)
	}
}

func TestValidateRegistrationsChecksPromptAndLifecycleCapabilities(t *testing.T) {
	store := &stubStore{plugins: map[string]config.Plugin{}}
	host := New(store)
	host.RegisterPluginID("tool/skills")
	host.SetInfo(pkgplugins.PluginInfo{
		ID:           "tool/skills",
		Kind:         "tool",
		Name:         "skills",
		DisplayName:  "Skills",
		Capabilities: []string{pkgplugins.CapabilityPrompt, pkgplugins.CapabilityLifecycle},
	})
	host.AddBeforeRun(pkgplugins.BeforeRunSpec{
		PluginID: "tool/skills",
		Name:     "skills",
		Required: true,
		Run: func(context.Context, pkgplugins.BeforeRunContext) (pkgplugins.BeforeRunResult, error) {
			return pkgplugins.BeforeRunResult{}, nil
		},
	})
	host.AddSystemPrompt(pkgplugins.SystemPromptSpec{
		PluginID: "tool/skills",
		Name:     "skills",
		Required: true,
		Build: func(context.Context, pkgplugins.SystemPromptContext) (pkgplugins.SystemPromptSection, error) {
			return pkgplugins.SystemPromptSection{
				Title:   "Skills",
				Content: "content",
			}, nil
		},
	})

	if err := host.ValidateRegistrations(); err != nil {
		t.Fatalf("ValidateRegistrations: %v", err)
	}
}

func TestValidateRegistrationsAcceptsToolLifecycleOnly(t *testing.T) {
	store := &stubStore{plugins: map[string]config.Plugin{}}
	host := New(store)
	host.RegisterPluginID("tool/filter")
	host.SetInfo(pkgplugins.PluginInfo{
		ID:           "tool/filter",
		Kind:         "tool",
		Name:         "filter",
		DisplayName:  "Filter",
		Capabilities: []string{pkgplugins.CapabilityLifecycle},
	})
	host.AddBeforeToolCall(pkgplugins.BeforeToolCallSpec{
		PluginID: "tool/filter",
		Name:     "filter",
		Required: true,
		Run: func(context.Context, pkgplugins.BeforeToolCallContext) (pkgplugins.BeforeToolCallResult, error) {
			return pkgplugins.BeforeToolCallResult{}, nil
		},
	})

	if err := host.ValidateRegistrations(); err != nil {
		t.Fatalf("ValidateRegistrations: %v", err)
	}
}

func TestSessionPluginViewUsesOnlySnapshot(t *testing.T) {
	host := New(&stubStore{plugins: map[string]config.Plugin{}})
	host.RegisterManifestPlugins(&manifest.Manifest{Plugins: []manifest.ManifestPlugin{
		{ID: "tool/xberg", Kind: "tool", Enabled: false, ManifestPluginDefinition: manifest.ManifestPluginDefinition{Name: "xberg", Binaries: []manifest.ManifestBinary{{Name: "xberg", Tool: "github:xberg-io/xberg"}}}},
		{ID: "tool/enabled", Kind: "tool", Enabled: true, ManifestPluginDefinition: manifest.ManifestPluginDefinition{Name: "enabled", Prompt: "enabled"}},
	}})

	view, err := host.SessionPluginView(internalplugin.Snapshot{})
	if err != nil {
		t.Fatalf("SessionPluginView: %v", err)
	}
	if len(view.RegisteredPluginIDs) != 0 || len(view.ExposedPluginIDs) != 0 {
		t.Fatalf("SessionPluginView = %+v, manifest registrations must not enter snapshot view", view)
	}
}

func TestValidateRegistrationsAcceptsCLIBackedPromptOnlyTool(t *testing.T) {
	store := &stubStore{plugins: map[string]config.Plugin{}}
	host := New(store)

	enabled := true
	host.RegisterManifestPlugins(&manifest.Manifest{
		Plugins: []manifest.ManifestPlugin{
			{
				ID:      "tool/mise",
				Kind:    "tool",
				Enabled: enabled,
				ManifestPluginDefinition: manifest.ManifestPluginDefinition{
					Name:        "mise",
					DisplayName: "mise",
					Prompt:      "Use mise to manage runtimes and tools.",
				},
			},
		},
	})

	if err := host.ValidateRegistrations(); err != nil {
		t.Fatalf("ValidateRegistrations: %v", err)
	}
}

func TestManifestSessionEnvPropagatesOAuthProvider(t *testing.T) {
	store := &stubStore{plugins: map[string]config.Plugin{}}
	host := New(store)
	manifest := &manifest.Manifest{
		Plugins: []manifest.ManifestPlugin{{
			ID:      "tool/acme-cli",
			Kind:    "tool",
			Enabled: true,
			ManifestPluginDefinition: manifest.ManifestPluginDefinition{
				Name: "acme-cli",
				SessionEnvs: []manifest.ManifestSessionEnv{{
					EnvVar: "ACME_ACCESS_TOKEN",
					Source: "oauth.access_token",
				}},
				OAuthProvider: "acme",
			},
		}},
	}
	host.RegisterManifestPlugins(manifest)

	specs := host.AllSessionEnvSpecs()
	var found bool
	for _, spec := range specs {
		if spec.PluginID == "tool/acme-cli" && spec.EnvVar == "ACME_ACCESS_TOKEN" {
			found = true
			if spec.OAuthProviderID != "acme" {
				t.Errorf("OAuthProviderID = %q, want acme", spec.OAuthProviderID)
			}
			break
		}
	}
	if !found {
		t.Error("acme-cli session env spec not found")
	}
}

func TestValidateRegistrationsRejectsDuplicateSessionEnvs(t *testing.T) {
	store := &stubStore{plugins: map[string]config.Plugin{}}
	host := New(store)
	host.RegisterPluginID("tool/gh")
	host.RegisterPluginID("tool/acme")
	host.AddSessionEnv(pkgplugins.SessionEnvSpec{PluginID: "tool/gh", EnvVar: "GH_TOKEN", Source: pkgplugins.SessionEnvSource("oauth.access_token")})
	host.AddSessionEnv(pkgplugins.SessionEnvSpec{PluginID: "tool/acme", EnvVar: "GH_TOKEN", Source: pkgplugins.SessionEnvSourceStatic, Value: "x"})
	if err := host.ValidateRegistrations(); err == nil || !strings.Contains(err.Error(), `session env "GH_TOKEN"`) {
		t.Fatalf("ValidateRegistrations error = %v, want duplicate env", err)
	}
}

func TestConfigSchemaUsesPluginIDDirectly(t *testing.T) {
	store := &stubStore{plugins: map[string]config.Plugin{}}
	host := New(store)
	host.RegisterPluginID("tool/test")
	host.AddAdmin(pkgplugins.AdminSpec{
		PluginID: "tool/test",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"servers": map[string]any{"type": "array"},
			},
		},
	})

	schema := host.ConfigSchema("tool/test")
	props := schema["properties"].(map[string]any)
	props["servers"].(map[string]any)["type"] = "object"

	original := host.ConfigSchema("tool/test")
	if got := original["properties"].(map[string]any)["servers"].(map[string]any)["type"]; got != "array" {
		t.Fatalf("expected schema clone, got %#v", got)
	}
}

func TestRuntimeApplyCreatesAndApplies(t *testing.T) {
	store := &stubStore{plugins: map[string]config.Plugin{"tool/test": {ID: "tool/test", Enabled: true, Config: map[string]any{"x": 1}}}}
	host := New(store)
	host.RegisterPluginID("tool/test")
	called := 0
	host.AddRuntime(pkgplugins.RuntimeSpec{PluginID: "tool/test", Name: "main", Build: func(ctx pkgplugins.RuntimeContext) (pkgplugins.Runtime, error) {
		return runtimeStub{apply: func(_ context.Context, desired pkgplugins.PluginState) error {
			called++
			if desired.ID != "tool/test" {
				t.Fatal(desired.ID)
			}
			return nil
		}}, nil
	}})
	if err := host.ApplyPlugin(context.Background(), "tool/test"); err != nil {
		t.Fatal(err)
	}
	if called != 1 {
		t.Fatalf("expected 1 apply, got %d", called)
	}
}

type runtimeStub struct {
	apply func(context.Context, pkgplugins.PluginState) error
}

func (r runtimeStub) Apply(ctx context.Context, desired pkgplugins.PluginState) error {
	return r.apply(ctx, desired)
}

func (r runtimeStub) Start(ctx context.Context, desired pkgplugins.PluginState) error {
	return r.Apply(ctx, desired)
}

func (r runtimeStub) Reconcile(ctx context.Context, desired pkgplugins.PluginState) error {
	return r.Apply(ctx, desired)
}
func (r runtimeStub) Stop(context.Context) error { return nil }
func (r runtimeStub) Snapshot(context.Context) (pkgplugins.RuntimeStatus, error) {
	return pkgplugins.RuntimeStatus{State: pkgplugins.RuntimeStateRunning}, nil
}

func (r runtimeStub) Status(ctx context.Context) (pkgplugins.RuntimeStatus, error) {
	return r.Snapshot(ctx)
}
