package agent

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"

	delegatetool "github.com/CherryHQ/stella/internal/agent/delegate"
	"github.com/CherryHQ/stella/internal/authz"
	"github.com/CherryHQ/stella/internal/memory"
	"github.com/CherryHQ/stella/internal/platform/config"
	"github.com/CherryHQ/stella/internal/platform/home"
	"github.com/CherryHQ/stella/internal/plugin"
	"github.com/CherryHQ/stella/internal/sessionmedia"
	"github.com/CherryHQ/stella/pkg/ai"
	"github.com/CherryHQ/stella/pkg/plugins"
	"github.com/CherryHQ/stella/pkg/providers"
	"github.com/CherryHQ/stella/resources/binaries"
)

func TestRunnerPluginAuthorityUsesOnlyNamedSessionIdentity(t *testing.T) {
	for _, tt := range []struct {
		name   string
		params RunnerParams
		kind   authz.ActorKind
		valid  bool
	}{
		{name: "direct worker", params: RunnerParams{UserID: "user", AgentID: "agent"}, kind: authz.ActorAgent, valid: true},
		{name: "group worker", params: RunnerParams{UserID: "group", GroupID: "group", AgentID: "agent"}, kind: authz.ActorGroupAgent, valid: true},
		{name: "user without agent", params: RunnerParams{UserID: "user"}, valid: false},
		{name: "userless", params: RunnerParams{AgentID: "agent"}, valid: false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			authority, err := runnerPluginAuthority(tt.params)
			if err != nil {
				t.Fatal(err)
			}
			if authority.Valid() != tt.valid {
				t.Fatalf("authority valid = %t, want %t", authority.Valid(), tt.valid)
			}
			if tt.valid && authority.Kind() != tt.kind {
				t.Fatalf("authority kind = %s, want %s", authority.Kind(), tt.kind)
			}
		})
	}
}

type panicSnapshotOpener struct {
	root string
	gate chan struct{}
}

func (o *panicSnapshotOpener) OpenRoot(context.Context, home.WorkspaceRequest, home.RootScope, home.RootAccess) (home.RootOperations, error) {
	<-o.gate
	r, err := os.OpenRoot(o.root)
	if err != nil {
		o.gate <- struct{}{}
		return nil, err
	}
	return &panicSnapshotRoot{runnerTestRoot: runnerTestRoot{Root: r}, gate: o.gate}, nil
}

type panicSnapshotRoot struct {
	runnerTestRoot
	gate chan struct{}
}

func (*panicSnapshotRoot) Stat(context.Context, string) (fs.FileInfo, error) {
	panic("snapshot read panic")
}

func (r *panicSnapshotRoot) Close() error {
	err := r.runnerTestRoot.Close()
	r.gate <- struct{}{}
	return err
}

type fakeStreamProvider struct{}

func (fakeStreamProvider) API() string { return "anthropic" }
func (fakeStreamProvider) Stream(context.Context, ai.Model, ai.Context, ai.StreamOptions) (providers.AssistantEventStream, error) {
	return nil, errors.New("not implemented")
}

type rebuildingDelegateRunner struct {
	build    NewRunnerFunc
	requests []delegatetool.SessionRunRequest
}

func (r *rebuildingDelegateRunner) RunDelegateSession(ctx context.Context, req delegatetool.SessionRunRequest) (delegatetool.SessionRunResult, error) {
	r.requests = append(r.requests, req)
	if req.SessionID == "" {
		req.SessionID = "managed-session"
	}
	child, err := r.build(ctx, RunnerParams{
		Model:          req.Model,
		UserID:         "user-1",
		AgentID:        "test-agent",
		SessionID:      req.SessionID,
		DelegateRunner: r,
	})
	if err != nil {
		return delegatetool.SessionRunResult{SessionID: req.SessionID}, err
	}
	if err := child.Close(); err != nil {
		return delegatetool.SessionRunResult{SessionID: req.SessionID}, err
	}
	return delegatetool.SessionRunResult{SessionID: req.SessionID, Output: "done", Complete: true}, nil
}

func TestNewRunnerFuncPassesProjectRootToSystemPrompt(t *testing.T) {
	stellaHome := t.TempDir()
	t.Setenv("STELLA_HOME", stellaHome)
	config.ResetStellaHome()
	t.Cleanup(config.ResetStellaHome)

	snap := &config.Snapshot{
		AgentID:      "test-agent",
		Provider:     "anthropic",
		Model:        "test-model",
		APIKey:       "test-key",
		SystemPrompt: "You are Stella.",
	}
	snap.Workspace = t.TempDir()

	// A project is owned by the agent, so it lives under the agent's private subdir
	// of the user home (#442).
	userAgentDir := filepath.Join(stellaHome, "users", "user-1", "agents", snap.AgentID)
	projectRoot := filepath.Join(userAgentDir, "projects", "app")
	if err := os.MkdirAll(filepath.Join(stellaHome, "users", "user-1", "data"), 0o700); err != nil {
		t.Fatalf("MkdirAll user data: %v", err)
	}
	if err := os.MkdirAll(projectRoot, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(userAgentDir, "AGENTS.md"), []byte("root instructions from runner builder"), 0o644); err != nil {
		t.Fatalf("WriteFile root AGENTS.md: %v", err)
	}
	if err := os.WriteFile(filepath.Join(projectRoot, "AGENTS.md"), []byte("project instructions from runner builder"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	corePlan := fixtureRunnerCoreRuntimePlan(t, stellaHome)

	var promptBuild plugins.SystemPromptContext
	resolveCalls := 0
	pluginContextCalls := 0
	build := newRunnerFunc(withTestSkillDependencies(runnerBuilderConfig{
		Snap:            snap,
		Home:            testWorkspaceViewer{root: stellaHome},
		CoreRuntimePlan: corePlan,
		PluginContextBuilder: func(_ context.Context, authority authz.Authority, agentID string) (PluginContext, error) {
			pluginContextCalls++
			if authority.Kind() != authz.ActorAgent || string(authority.UserID()) != "user-1" || string(authority.AgentID()) != agentID {
				t.Fatalf("plugin context authority = %#v, agentID = %q", authority, agentID)
			}
			return PluginContext{}, nil
		},
		PromptSectionsBuilder: func(_ context.Context, build plugins.SystemPromptContext, _ plugin.Snapshot) ([]plugins.SystemPromptSection, error) {
			promptBuild = build
			return nil, nil
		},
		ProviderStreamBuilder: func(api, apiKey, baseURL string) (providers.StreamFunc, error) {
			return providers.AdapterStreamFunc(fakeStreamProvider{}), nil
		},
		SandboxBackendFn: func(context.Context) string { return config.SandboxBackendNone },
		SandboxBackends:  testSandboxBackends(t),
		ProjectResolver: func(ctx context.Context, projectID, userID, agentID string) (ProjectDescriptor, error) {
			resolveCalls++
			if projectID != "project-1" || userID != "user-1" || agentID != snap.AgentID {
				t.Fatalf("ProjectResolver called with projectID=%q userID=%q", projectID, userID)
			}
			if resolveCalls > 1 {
				return ProjectDescriptor{ID: projectID, UserID: userID, AgentID: agentID, Path: "changed/generation"}, nil
			}
			return ProjectDescriptor{ID: projectID, UserID: userID, AgentID: agentID, Path: "projects/app"}, nil
		},
	}))

	r, err := build(context.Background(), RunnerParams{UserID: "user-1", AgentID: snap.AgentID, ProjectID: "project-1"})
	if err != nil {
		t.Fatalf("build runner: %v", err)
	}
	t.Cleanup(func() {
		if err := r.Close(); err != nil {
			t.Fatalf("Close runner: %v", err)
		}
	})

	if got := r.SystemPrompt(); !strings.Contains(got, "root instructions from runner builder") || !strings.Contains(got, "project instructions from runner builder") || strings.Contains(got, stellaHome) {
		t.Fatalf("expected logical root-to-leaf project context without host path, got:\n%s", got)
	}
	if promptBuild.UserID != "user-1" || promptBuild.AgentID != snap.AgentID {
		t.Errorf("prompt identity = (%q, %q), want (%q, %q)", promptBuild.UserID, promptBuild.AgentID, "user-1", snap.AgentID)
	}
	if resolveCalls != 1 {
		t.Fatalf("project resolved %d times, want exactly once", resolveCalls)
	}
	if pluginContextCalls != 1 {
		t.Fatalf("plugin context builder called %d times, want exactly once", pluginContextCalls)
	}
}

func TestSnapshotAuthorizedProjectPanicReleasesOwnerGate(t *testing.T) {
	root := t.TempDir()
	opener := &panicSnapshotOpener{root: root, gate: make(chan struct{}, 1)}
	opener.gate <- struct{}{}
	resolve := func(_ context.Context, projectID, userID, agentID string) (ProjectDescriptor, error) {
		return ProjectDescriptor{ID: projectID, UserID: userID, AgentID: agentID, Path: "."}, nil
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("SnapshotAuthorizedProject did not propagate read panic")
			}
		}()
		_, _ = SnapshotAuthorizedProject(context.Background(), resolve, opener, "p", "u", "a")
	}()
	if len(opener.gate) != 1 {
		t.Fatal("owner gate remained held after panic")
	}
	reopened, err := opener.OpenRoot(context.Background(), home.WorkspaceRequest{}, home.RootAgentWorkspace, home.RootReadOnly)
	if err != nil {
		t.Fatalf("owner gate remained held after panic: %v", err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestNewRunnerFuncGuestHasMinimalPromptAndNoTools(t *testing.T) {
	stellaHome := t.TempDir()
	t.Setenv("STELLA_HOME", stellaHome)
	config.ResetStellaHome()
	t.Cleanup(config.ResetStellaHome)
	snap := &config.Snapshot{AgentID: "agent-1", Provider: "anthropic", Model: "test-model", APIKey: "test-key", SystemPrompt: "Operator base prompt", Workspace: t.TempDir()}
	build := newRunnerFunc(withTestSkillDependencies(runnerBuilderConfig{
		Snap: snap,
		ProviderStreamBuilder: func(string, string, string) (providers.StreamFunc, error) {
			return providers.AdapterStreamFunc(fakeStreamProvider{}), nil
		},
		SandboxBackendFn: func(context.Context) string { return config.SandboxBackendNone },
		SandboxBackends:  testSandboxBackends(t),
		PromptSectionsBuilder: func(context.Context, plugins.SystemPromptContext, plugin.Snapshot) ([]plugins.SystemPromptSection, error) {
			t.Fatal("guest must not build prompt sections")
			return nil, nil
		},
	}))
	r, err := build(context.Background(), RunnerParams{UserID: "11111111-1111-4111-8111-111111111111", GuestID: "11111111-1111-4111-8111-111111111111", AgentID: "agent-1", SessionID: "guest-session"})
	if err != nil {
		t.Fatalf("build guest runner: %v", err)
	}
	t.Cleanup(func() { _ = r.Close() })
	impl := r.(*runner)
	if got := impl.tools.Definitions(); len(got) != 0 {
		t.Fatalf("guest tool definitions = %#v, want none", got)
	}
	if impl.SandboxSession() != nil {
		t.Fatal("guest runner created a sandbox session")
	}
	if impl.hookSet != nil || impl.toolLifecycle != nil {
		t.Fatal("guest runner initialized tool hooks or lifecycle")
	}
	system := r.SystemPrompt()
	for _, forbidden := range []string{"# Tools", "# Filesystem", "# Memories", "# User Profile", "# Agent Soul", "# Plugins", "# Project Context"} {
		if strings.Contains(system, forbidden) {
			t.Fatalf("guest prompt contains forbidden section %q:\n%s", forbidden, system)
		}
	}
	if !strings.Contains(system, "Operator base prompt") || !strings.Contains(system, "# Guest limitations") {
		t.Fatalf("unexpected guest prompt:\n%s", system)
	}
}

func TestNewRunnerFuncCarriesDeclaredModelInput(t *testing.T) {
	stellaHome := t.TempDir()
	t.Setenv("STELLA_HOME", stellaHome)
	config.ResetStellaHome()
	t.Cleanup(config.ResetStellaHome)

	snap := &config.Snapshot{
		AgentID:   "test-agent",
		Provider:  "anthropic",
		Model:     "anthropic/text-only-model",
		APIKey:    "test-key",
		Providers: map[string]config.ProviderCreds{"anthropic": {Type: "anthropic", APIKey: "test-key"}},
		ModelInputs: map[config.ModelKey][]string{
			{Provider: "anthropic", Model: "text-only-model"}: {"text"},
		},
	}
	snap.Workspace = t.TempDir()
	if err := os.MkdirAll(filepath.Join(stellaHome, "users", "user-1", "data"), 0o700); err != nil {
		t.Fatalf("MkdirAll user data: %v", err)
	}
	corePlan := fixtureRunnerCoreRuntimePlan(t, stellaHome)

	build := newRunnerFunc(withTestSkillDependencies(runnerBuilderConfig{
		Snap:            snap,
		Home:            testWorkspaceViewer{root: stellaHome},
		CoreRuntimePlan: corePlan,
		ProviderStreamBuilder: func(api, apiKey, baseURL string) (providers.StreamFunc, error) {
			return providers.AdapterStreamFunc(fakeStreamProvider{}), nil
		},
		SandboxBackendFn: func(context.Context) string { return config.SandboxBackendNone },
		SandboxBackends:  testSandboxBackends(t),
	}))

	r, err := build(context.Background(), RunnerParams{UserID: "user-1", AgentID: snap.AgentID})
	if err != nil {
		t.Fatalf("build runner: %v", err)
	}
	t.Cleanup(func() { _ = r.Close() })

	impl, ok := r.(*runner)
	if !ok {
		t.Fatalf("runner type = %T, want *runner", r)
	}
	if got := impl.model.ImageCapability(); got != ai.ImageUnsupported {
		t.Fatalf("model.ImageCapability() = %v, want ImageUnsupported (Input=%v)", got, impl.model.Input)
	}
}

func TestNewRunnerFuncManagedSessionsPreserveQualifiedModelRef(t *testing.T) {
	stellaHome := t.TempDir()
	t.Setenv("STELLA_HOME", stellaHome)
	config.ResetStellaHome()
	t.Cleanup(config.ResetStellaHome)

	const (
		providerID    = "openrouter-production"
		providerAlias = "openai"
		providerAPI   = "openai"
		modelID       = "anthropic/claude-sonnet-4-6"
		modelRef      = providerID + "/" + modelID
	)
	snap := &config.Snapshot{
		AgentID:  "test-agent",
		Provider: providerAlias,
		Model:    providerAlias + "/" + modelID,
		Providers: map[string]config.ProviderCreds{
			providerAlias: {Type: providerAPI, APIKey: "test-key", ProviderID: providerID},
		},
		Workspace: t.TempDir(),
	}
	if err := os.MkdirAll(filepath.Join(stellaHome, "users", "user-1", "data"), 0o700); err != nil {
		t.Fatalf("MkdirAll user data: %v", err)
	}
	corePlan := fixtureRunnerCoreRuntimePlan(t, stellaHome)

	var adapterBuilds int
	bridge := &rebuildingDelegateRunner{}
	build := newRunnerFunc(withTestSkillDependencies(runnerBuilderConfig{
		Snap:            snap,
		Home:            testWorkspaceViewer{root: stellaHome},
		CoreRuntimePlan: corePlan,
		ProviderStreamBuilder: func(api, apiKey, baseURL string) (providers.StreamFunc, error) {
			if api != providerAPI {
				return nil, providers.ErrProviderNotFound
			}
			adapterBuilds++
			return providers.AdapterStreamFunc(fakeStreamProvider{}), nil
		},
		SandboxBackendFn: func(context.Context) string { return config.SandboxBackendNone },
		SandboxBackends:  testSandboxBackends(t),
	}))
	bridge.build = build

	source, err := build(context.Background(), RunnerParams{
		UserID:         "user-1",
		AgentID:        snap.AgentID,
		SessionID:      "source-session",
		DelegateRunner: bridge,
	})
	if err != nil {
		t.Fatalf("build source runner: %v", err)
	}
	t.Cleanup(func() { _ = source.Close() })
	impl := source.(*runner)
	if got := impl.model.Provider; got != providerID {
		t.Fatalf("source model provider = %q, want canonical ID %q", got, providerID)
	}

	ctx := memory.WithSessionID(context.Background(), "source-session")
	created, err := impl.delegateTool.RunManagedSession(ctx, delegatetool.ManagedSessionRequest{Message: "create"})
	if err != nil {
		t.Fatalf("managed create: %v", err)
	}
	if _, err := impl.delegateTool.RunManagedSession(ctx, delegatetool.ManagedSessionRequest{SessionID: created.SessionID, Message: "send"}); err != nil {
		t.Fatalf("managed send: %v", err)
	}

	if len(bridge.requests) != 2 {
		t.Fatalf("managed requests = %d, want 2", len(bridge.requests))
	}
	for i, req := range bridge.requests {
		if req.Model != modelRef {
			t.Errorf("managed request %d model = %q, want %q", i, req.Model, modelRef)
		}
	}
	if adapterBuilds != 3 {
		t.Fatalf("provider adapter builds = %d, want source + create + send = 3", adapterBuilds)
	}
}

func TestNewRunnerFunc(t *testing.T) {
	stellaHome := t.TempDir()
	if err := os.MkdirAll(filepath.Join(stellaHome, "bin"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	_ = binaries.EnsureTools(stellaHome)
	t.Setenv("STELLA_HOME", stellaHome)
	config.ResetStellaHome()
	t.Cleanup(config.ResetStellaHome)

	snap := &config.Snapshot{
		AgentID:  "test-agent",
		Provider: "anthropic",
		Model:    "test-model",
		APIKey:   "test-key",
	}
	snap.Workspace = t.TempDir()

	build := newRunnerFunc(withTestSkillDependencies(runnerBuilderConfig{
		Snap: snap,
		Home: testWorkspaceViewer{root: stellaHome},
		ProviderStreamBuilder: func(api, apiKey, baseURL string) (providers.StreamFunc, error) {
			return providers.AdapterStreamFunc(fakeStreamProvider{}), nil
		},
	}))

	r, err := build(context.Background(), RunnerParams{UserID: "1"})
	if err != nil {
		t.Skipf("build runner: docker not available: %v", err)
	}
	if r == nil {
		t.Fatal("expected non-nil runner")
	}
}

type fakeSessionImages struct {
	enrichOwner sessionmedia.Owner
	loadOwner   sessionmedia.Owner
}

func (f *fakeSessionImages) Enrich(_ context.Context, owner sessionmedia.Owner, _ string, blocks []ai.ContentBlock) ([]ai.ContentBlock, error) {
	f.enrichOwner = owner
	return []ai.ContentBlock{ai.ImageRefContent{MediaID: "media-1"}}, nil
}

func (f *fakeSessionImages) Load(_ context.Context, owner sessionmedia.Owner, _ string) (ai.ImageContent, error) {
	f.loadOwner = owner
	return ai.ImageContent{Data: "aGk=", MimeType: "image/png"}, nil
}

// A group session carries the same canonical image policy a direct session
// does; only the owner differs. Leaving a group runner without one is what used
// to push group images onto the legacy inline path.
func TestCanonicalImageConfigOwnerFollowsSession(t *testing.T) {
	groupID := uuid.NewString()
	userID := uuid.NewString()
	raw := ai.ToolResultMessage{Content: []ai.ContentBlock{ai.ImageContent{Data: "aGk=", MimeType: "image/png"}}}

	for _, tc := range []struct {
		name   string
		params RunnerParams
		want   sessionmedia.Owner
	}{
		{"group", RunnerParams{UserID: groupID, GroupID: groupID, AgentID: "agent-1"}, sessionmedia.GroupOwner(uuid.MustParse(groupID))},
		{"direct", RunnerParams{UserID: userID, AgentID: "agent-1"}, sessionmedia.UserOwner(uuid.MustParse(userID))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			images := &fakeSessionImages{}
			cfg := canonicalImageConfig(images, tc.params)
			if cfg == nil {
				t.Fatal("session has no canonical image policy")
			}
			if _, err := cfg.CanonicalizeToolResult(context.Background(), raw); err != nil {
				t.Fatalf("canonicalize: %v", err)
			}
			if images.enrichOwner != tc.want {
				t.Fatalf("enrich owner = %#v, want %#v", images.enrichOwner, tc.want)
			}
			if _, err := cfg.Load(context.Background(), "media-1"); err != nil {
				t.Fatalf("load: %v", err)
			}
			if images.loadOwner != tc.want {
				t.Fatalf("load owner = %#v, want %#v", images.loadOwner, tc.want)
			}
		})
	}
}

// A guest session has no UUID principal. It must fail at the image, not mint
// media under a channel identity that no owner column can hold.
func TestCanonicalImageConfigRejectsNonUUIDPrincipal(t *testing.T) {
	images := &fakeSessionImages{}
	cfg := canonicalImageConfig(images, RunnerParams{UserID: "guest-42", AgentID: "agent-1"})
	raw := ai.ToolResultMessage{Content: []ai.ContentBlock{ai.ImageContent{Data: "aGk=", MimeType: "image/png"}}}
	if _, err := cfg.CanonicalizeToolResult(context.Background(), raw); err == nil {
		t.Fatal("guest principal produced canonical media")
	}
	if _, err := cfg.Load(context.Background(), "media-1"); err == nil {
		t.Fatal("guest principal loaded media")
	}
	// A text-only result still passes through untouched.
	if _, err := cfg.CanonicalizeToolResult(context.Background(), ai.ToolResultMessage{
		Content: []ai.ContentBlock{ai.TextContent{Text: "ok"}},
	}); err != nil {
		t.Fatalf("text-only result: %v", err)
	}
}
