package agent

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/CherryHQ/stella/internal/authz"
	"github.com/CherryHQ/stella/internal/platform/config"
	"github.com/CherryHQ/stella/internal/platform/home"
	"github.com/CherryHQ/stella/internal/plugin"
	skillstool "github.com/CherryHQ/stella/internal/skill"
	"github.com/CherryHQ/stella/pkg/plugins"
	"github.com/CherryHQ/stella/pkg/providers"
	pkgsandbox "github.com/CherryHQ/stella/pkg/sandbox"
)

type testWorkspaceViewer struct{ root string }

type failingWorkspaceViewer struct{ err error }

type emptySkillRuntime struct{}

func (emptySkillRuntime) GetIdentity(context.Context, string) (*skillstool.Skill, error) {
	return nil, nil
}

func (emptySkillRuntime) ListIdentityVisible(context.Context, skillstool.ViewContext) ([]skillstool.Skill, error) {
	return nil, nil
}

func (emptySkillRuntime) ListIdentityByScope(context.Context, string, string, string) ([]skillstool.Skill, error) {
	return nil, nil
}

func (emptySkillRuntime) ListIdentityCandidate(context.Context, string, skillstool.ViewContext) ([]skillstool.Skill, error) {
	return nil, nil
}

func (emptySkillRuntime) LoadCurrentRevision(context.Context, skillstool.Skill) (skillstool.ManagedRevision, error) {
	return skillstool.ManagedRevision{}, fs.ErrNotExist
}

func (emptySkillRuntime) LoadExactRevision(context.Context, skillstool.Skill, string) (skillstool.ManagedRevision, error) {
	return skillstool.ManagedRevision{}, fs.ErrNotExist
}

func (emptySkillRuntime) TouchReflectSkillRuntimeUseDigest(context.Context, string, string, string, string) error {
	return nil
}

type allowSkillReads struct{}

func (allowSkillReads) BeginRead(context.Context) (skillstool.SkillReadDecision, error) {
	return allowSkillReads{}, nil
}

func (allowSkillReads) AllowRead(context.Context, string, string, string, string) (bool, error) {
	return true, nil
}

func withTestSkillDependencies(cfg runnerBuilderConfig) runnerBuilderConfig {
	cfg.SkillRevisionReader = emptySkillRuntime{}
	cfg.SkillReadAuthorizer = allowSkillReads{}
	return cfg
}

func (w failingWorkspaceViewer) WorkspaceView(context.Context, home.WorkspaceRequest) (home.WorkspaceView, error) {
	return home.WorkspaceView{}, w.err
}

func (w failingWorkspaceViewer) OpenRoot(context.Context, home.WorkspaceRequest, home.RootScope, home.RootAccess) (home.RootOperations, error) {
	return nil, w.err
}

func (w testWorkspaceViewer) WorkspaceView(_ context.Context, req home.WorkspaceRequest) (home.WorkspaceView, error) {
	shared := home.WorkspaceView{}
	if req.GroupID != "" {
		return principalView(filepath.Join(w.root, "users", "group-"+req.GroupID), req.AgentID)
	}
	if req.UserID != "" {
		return principalView(filepath.Join(w.root, "users", req.UserID), req.AgentID)
	}
	return shared, nil
}

// principalView mirrors what the real Home viewer materializes: both the agent
// dir and the shared data root exist before a runner is built. Creating the data
// root is not cosmetic — ResolvePaths compares resolved paths, and an absent dir
// cannot be resolved, so on macOS the caller's /var/... would never match the
// authorized /private/var/... root.
func principalView(principal, agentID string) (home.WorkspaceView, error) {
	agent := filepath.Join(principal, "agents", agentID)
	data := filepath.Join(principal, "data")
	for _, dir := range []string{agent, data} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return home.WorkspaceView{}, err
		}
	}
	return home.WorkspaceView{PrincipalRoot: principal, DataRoot: data, AgentRoot: agent}, nil
}

func (w testWorkspaceViewer) OpenRoot(ctx context.Context, req home.WorkspaceRequest, scope home.RootScope, _ home.RootAccess) (home.RootOperations, error) {
	view, err := w.WorkspaceView(ctx, req)
	if err != nil {
		return nil, err
	}
	dir := view.AgentRoot
	if scope == home.RootPrincipalData {
		dir = view.DataRoot
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	return runnerTestRoot{Root: root}, nil
}

type runnerTestRoot struct{ *os.Root }

func (r runnerTestRoot) Stat(_ context.Context, name string) (fs.FileInfo, error) {
	return r.Root.Stat(name)
}

func (r runnerTestRoot) List(_ context.Context, name string, options home.ListOptions) ([]fs.DirEntry, error) {
	directory, err := r.Open(name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = directory.Close() }()
	entries, err := directory.ReadDir(options.Limit + 1)
	if err != nil {
		return nil, err
	}
	if options.Limit > 0 && len(entries) > options.Limit {
		return nil, home.ErrListLimit
	}
	return entries, nil
}

func (r runnerTestRoot) Read(_ context.Context, name string, dst io.Writer, options home.ReadOptions) error {
	file, err := r.Open(name)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	_, err = io.Copy(dst, io.LimitReader(file, options.MaxBytes))
	return err
}

func (r runnerTestRoot) Write(context.Context, string, io.Reader, home.WriteOptions) error {
	return errors.New("not implemented")
}

func (r runnerTestRoot) Upload(context.Context, string, io.Reader, home.WriteOptions) error {
	return errors.New("not implemented")
}

func (r runnerTestRoot) Mkdir(context.Context, string, fs.FileMode, home.MkdirOptions) error {
	return errors.New("not implemented")
}

func (r runnerTestRoot) Remove(context.Context, string, home.RemoveOptions) error {
	return errors.New("not implemented")
}

func (r runnerTestRoot) Rename(context.Context, string, string, home.RenameOptions) error {
	return errors.New("not implemented")
}

func TestNewRunnerFuncUsesPrincipalWorkspace(t *testing.T) {
	stellaHome := t.TempDir()
	// Sandbox mounts carry resolved host paths, and macOS hands out temp dirs
	// under the /var -> /private/var symlink; compare against the same form.
	stellaHome, _ = filepath.EvalSymlinks(stellaHome)
	t.Setenv("STELLA_HOME", stellaHome)
	config.ResetStellaHome()
	t.Cleanup(config.ResetStellaHome)

	snap := &config.Snapshot{AgentID: "a1", Provider: "anthropic", Model: "test-model", APIKey: "test-key"}
	snap.Workspace = t.TempDir()
	corePlan := fixtureRunnerCoreRuntimePlan(t, stellaHome)
	for _, tt := range []struct {
		name     string
		params   RunnerParams
		wantRoot string
		wantWork string
	}{
		{name: "personal", params: RunnerParams{UserID: "u1", AgentID: "a1"}, wantRoot: filepath.Join(stellaHome, "users", "u1"), wantWork: filepath.Join(stellaHome, "users", "u1", "agents", "a1")},
		{name: "group", params: RunnerParams{UserID: "g1", GroupID: "g1", AgentID: "a1"}, wantRoot: filepath.Join(stellaHome, "users", "group-g1"), wantWork: filepath.Join(stellaHome, "users", "group-g1", "agents", "a1")},
		{name: "user-less", params: RunnerParams{AgentID: "a1"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if tt.wantRoot != "" {
				if err := os.MkdirAll(filepath.Join(tt.wantRoot, "data"), 0o700); err != nil {
					t.Fatalf("create principal data root: %v", err)
				}
			}
			var promptBuild plugins.SystemPromptContext
			build := newRunnerFunc(withTestSkillDependencies(runnerBuilderConfig{
				Snap:            snap,
				Home:            testWorkspaceViewer{root: stellaHome},
				CoreRuntimePlan: corePlan,
				PluginContextBuilder: func(context.Context, authz.Authority, string) (PluginContext, error) {
					return PluginContext{}, nil
				},
				PromptSectionsBuilder: func(_ context.Context, build plugins.SystemPromptContext, _ plugin.Snapshot) ([]plugins.SystemPromptSection, error) {
					if tt.name == "user-less" {
						t.Fatal("user-less runner must skip plugin prompt sections")
					}
					promptBuild = build
					return nil, nil
				},
				ProviderStreamBuilder: func(_, _, _ string) (providers.StreamFunc, error) {
					return providers.AdapterStreamFunc(fakeStreamProvider{}), nil
				},
				SandboxBackendFn: func(context.Context) string { return config.SandboxBackendNone },
				SandboxBackends:  testSandboxBackends(t),
			}))
			builtRunner, err := build(context.Background(), tt.params)
			if err != nil {
				t.Fatalf("build runner: %v", err)
			}
			t.Cleanup(func() { _ = builtRunner.Close() })
			impl := builtRunner.(*runner)
			if tt.name == "user-less" {
				scratch := impl.sandboxCfg.Paths.WorkspaceRoot
				if scratch == "" || filepath.Dir(scratch) != filepath.Join(stellaHome, runnerScratchDir) {
					t.Errorf("user-less root = %q, want disposable scratch", scratch)
				}
				for _, dir := range []string{filepath.Dir(scratch), scratch} {
					info, err := os.Stat(dir)
					if err != nil || info.Mode().Perm() != 0o700 {
						t.Fatalf("scratch permissions for %q = %v, %v; want 0700", dir, info, err)
					}
				}
				if err := os.WriteFile(filepath.Join(scratch, "owned"), []byte("ok"), 0o600); err != nil {
					t.Fatalf("scratch is not writable: %v", err)
				}
				workspaceRoot, err := filepath.EvalSymlinks(impl.sandboxCfg.Paths.WorkspaceRoot)
				if err != nil || impl.sandboxCfg.Paths.AgentRoot != snap.Workspace || workspaceRoot != scratch {
					t.Fatalf("definition/scratch roots = agent %q workspace %q scratch %q", impl.sandboxCfg.Paths.AgentRoot, impl.sandboxCfg.Paths.WorkspaceRoot, scratch)
				}
				if err := builtRunner.Close(); err != nil {
					t.Fatal(err)
				}
				if _, err := os.Stat(scratch); !os.IsNotExist(err) {
					t.Fatalf("scratch remains after Close: %v", err)
				}
			} else {
				if promptBuild.UserID != tt.params.UserID || promptBuild.AgentID != tt.params.AgentID {
					t.Errorf("prompt identity = (%q, %q), want (%q, %q)", promptBuild.UserID, promptBuild.AgentID, tt.params.UserID, tt.params.AgentID)
				}
				mounts := impl.session.Policy().Filesystem.Mounts
				wantMounts := map[string]string{
					tt.wantWork:                        impl.sandboxCfg.Paths.WorkspaceRoot,
					filepath.Join(tt.wantRoot, "data"): impl.sandboxCfg.Paths.UserDataDir,
				}
				for processPath, source := range wantMounts {
					found := false
					for _, mount := range mounts {
						if mount.SandboxPath == processPath {
							found = mount.Access == pkgsandbox.MountReadWrite
						}
					}
					if !found {
						t.Errorf("mount %s = %#v, want RW", processPath, mounts)
					}
					if source != processPath {
						t.Errorf("none backend private mount source for %s = %q", processPath, source)
					}
				}
			}
		})
	}
}

func TestNewRunnerFuncPropagatesWorkspaceError(t *testing.T) {
	want := errors.New("Home unavailable")
	build := newRunnerFunc(withTestSkillDependencies(runnerBuilderConfig{
		Snap: &config.Snapshot{Provider: "anthropic", Model: "test"},
		Home: failingWorkspaceViewer{err: want},
	}))
	if _, err := build(context.Background(), RunnerParams{UserID: "u", AgentID: "a"}); !errors.Is(err, want) {
		t.Fatalf("runner error = %v, want %v", err, want)
	}
}

func TestNewRunnerFuncRejectsUserlessProject(t *testing.T) {
	build := newRunnerFunc(withTestSkillDependencies(runnerBuilderConfig{
		Snap: &config.Snapshot{Provider: "anthropic", Model: "test"},
		Home: testWorkspaceViewer{root: t.TempDir()},
	}))
	if _, err := build(context.Background(), RunnerParams{AgentID: "a", ProjectID: "p"}); err == nil {
		t.Fatal("user-less ProjectID was accepted")
	}
}

func TestNewRunnerFuncCleansUserlessScratchOnConstructionFailure(t *testing.T) {
	stellaHome := t.TempDir()
	t.Setenv("STELLA_HOME", stellaHome)
	config.ResetStellaHome()
	t.Cleanup(config.ResetStellaHome)

	build := newRunnerFunc(withTestSkillDependencies(runnerBuilderConfig{
		Snap: &config.Snapshot{AgentID: "a", Provider: "anthropic", Model: "test"},
		Home: testWorkspaceViewer{root: stellaHome},
		ProviderStreamBuilder: func(_, _, _ string) (providers.StreamFunc, error) {
			return nil, errors.New("provider unavailable")
		},
		SandboxBackendFn: func(context.Context) string { return config.SandboxBackendNone },
		SandboxBackends:  testSandboxBackends(t),
	}))
	if _, err := build(context.Background(), RunnerParams{AgentID: "a"}); err == nil {
		t.Fatal("runner construction succeeded")
	}
	entries, err := os.ReadDir(filepath.Join(stellaHome, runnerScratchDir))
	if err != nil {
		t.Fatalf("read scratch parent: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("scratch remains after construction failure: %v", entries)
	}
}

func TestNewRunnerScratchCleanupUsesOpenedRootAfterPathReplacement(t *testing.T) {
	if runtime.GOOS == "windows" || runtime.GOOS == "plan9" || runtime.GOOS == "js" {
		t.Skip("open directory handles do not remain usable across rename on this platform")
	}
	home := t.TempDir()
	dir, cleanup, err := newRunnerScratch(home)
	if err != nil {
		t.Fatal(err)
	}
	oldRoot := filepath.Join(home, "old-runner-scratch")
	if err := os.Rename(filepath.Join(home, runnerScratchDir), oldRoot); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(home, runnerScratchDir), 0o700); err != nil {
		t.Fatal(err)
	}
	replacementMarker := filepath.Join(home, runnerScratchDir, "keep")
	if err := os.WriteFile(replacementMarker, []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := cleanup(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(oldRoot, filepath.Base(dir))); !os.IsNotExist(err) {
		t.Fatalf("scratch remains in original opened root: %v", err)
	}
	if _, err := os.Stat(replacementMarker); err != nil {
		t.Fatalf("replacement root was modified: %v", err)
	}
}
