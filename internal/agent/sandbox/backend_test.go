package sandbox

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	oauth "github.com/CherryHQ/stella/internal/connections/oauth"
	"github.com/CherryHQ/stella/internal/platform/config"
	"github.com/CherryHQ/stella/internal/plugin"
	"github.com/CherryHQ/stella/internal/plugin/manifest"
	"github.com/CherryHQ/stella/internal/vault"
	pkgplugins "github.com/CherryHQ/stella/pkg/plugins"
	pkgsandbox "github.com/CherryHQ/stella/pkg/sandbox"
	"github.com/CherryHQ/stella/plugins/core"
	localbackend "github.com/CherryHQ/stella/plugins/sandbox/local"
)

// stubVaultLoader is a test-only VaultEnvLoader that returns a fixed map.
type stubVaultLoader struct {
	env map[string]string
	err error
}

func (s *stubVaultLoader) LoadEnvForAgent(_ context.Context, _ string, _ string) (map[string]string, error) {
	return s.env, s.err
}

func (s *stubVaultLoader) ListAmbientSecretMetas(_ context.Context, _ string, _ string) ([]vault.AmbientSecretMeta, error) {
	return nil, s.err
}

type stubOAuthVaultStore struct {
	data map[string]string
}

func newStubOAuthVaultStore() *stubOAuthVaultStore {
	return &stubOAuthVaultStore{data: make(map[string]string)}
}

func (s *stubOAuthVaultStore) key(userID string, name string) string {
	return userID + ":" + name
}

func (s *stubOAuthVaultStore) Set(_ context.Context, userID string, name string, plaintext string) error {
	s.data[s.key(userID, name)] = plaintext
	return nil
}

func (s *stubOAuthVaultStore) Delete(_ context.Context, userID string, name string) error {
	delete(s.data, s.key(userID, name))
	return nil
}

func (s *stubOAuthVaultStore) Lookup(_ context.Context, userID string, name string) (string, bool, error) {
	value, ok := s.data[s.key(userID, name)]
	return value, ok, nil
}

func (s *stubOAuthVaultStore) LoadEnvForAgent(_ context.Context, userID string, _ string) (map[string]string, error) {
	out := make(map[string]string)
	prefix := userID + ":"
	for k, v := range s.data {
		if len(k) <= len(prefix) || k[:len(prefix)] != prefix {
			continue
		}
		out[k[len(prefix):]] = v
	}
	return out, nil
}

func (s *stubOAuthVaultStore) ListAmbientSecretMetas(_ context.Context, _ string, _ string) ([]vault.AmbientSecretMeta, error) {
	return nil, nil
}

// TestResolveSessionRequiresUserRoot tests that ResolveSession fails without a UserRoot.
func TestResolveSessionRequiresUserRoot(t *testing.T) {
	_, err := ResolveSession(context.Background(), Config{
		Paths: Paths{
			AgentRoot: "/workspace/agent",
			// UserRoot intentionally omitted
		},
	})
	if err == nil {
		t.Fatal("expected error when UserRoot is missing")
	}
}

func TestSandboxProcessEnvIsRunnerOnly(t *testing.T) {
	paths := Paths{StellaHome: "/stella", WorkspaceRoot: "/workspace/job", UserDataDir: "/user/data"}
	env := ProcessEnv(paths)
	if got, want := env["STELLA_HOME"], paths.StellaHome; got != want {
		t.Errorf("STELLA_HOME = %q, want %q", got, want)
	}
	for _, key := range []string{"HOME", "STELLA_USER_DIR", "STELLA_ASSETS_DIR", "TMPDIR", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME", "XDG_CACHE_HOME"} {
		if got, ok := env[key]; ok {
			t.Errorf("ProcessEnv must not set backend filesystem root %s=%q", key, got)
		}
	}
}

func TestSandboxProcessEnvWithoutStellaHomeIsEmpty(t *testing.T) {
	if env := ProcessEnv(Paths{}); len(env) != 0 {
		t.Errorf("ProcessEnv = %#v, want empty", env)
	}
}

func TestCopyLocalHostEnvAllowlist(t *testing.T) {
	t.Setenv("STELLA_TEST_SECRET", "must-not-leak")
	t.Setenv("LANG", "C.UTF-8")
	t.Setenv("HTTPS_PROXY", "http://proxy.example:8080")

	env := map[string]string{}
	pkgsandbox.HostEnvCopy(env)

	if _, ok := env["STELLA_TEST_SECRET"]; ok {
		t.Fatal("local sandbox env copied non-allowlisted host variable")
	}
	if got := env["LANG"]; got != "C.UTF-8" {
		t.Fatalf("LANG = %q, want allowlisted host value", got)
	}
	if got := env["HTTPS_PROXY"]; got != "http://proxy.example:8080" {
		t.Fatalf("HTTPS_PROXY = %q, want allowlisted proxy value", got)
	}
}

func TestLocalSandboxPathAllowed(t *testing.T) {
	stellaBin := "/home/me/.stella/bin"
	for _, entry := range []string{
		"/usr/bin",
		"/usr/local/bin",
		"/bin",
		"/sbin",
		"/nix/store/abc/bin",
		"/run/current-system/sw/bin",
	} {
		if !pkgsandbox.HostEnvPathAllowed(entry, stellaBin) {
			t.Fatalf("expected %q to be allowed", entry)
		}
	}
	for _, entry := range []string{"", stellaBin, "/home/me/bin", "/tmp/bin", "/binary"} {
		if pkgsandbox.HostEnvPathAllowed(entry, stellaBin) {
			t.Fatalf("expected %q to be rejected", entry)
		}
	}
}

// TestSyncSessionNoop verifies that SyncSession is a no-op for nil and
// for sessions that don't implement Sync.
func TestSyncSessionNoop(t *testing.T) {
	if err := SyncSession(nil); err != nil {
		t.Errorf("SyncSession(nil): %v", err)
	}

	nop := pkgsandbox.NopSession()
	if err := SyncSession(nop); err != nil {
		t.Errorf("SyncSession(nop): %v", err)
	}
}

// TestResolveSessionDockerUnreachableDaemonReturnsError verifies that ResolveSession
// routes to createDockerSession and fails with a docker-related error when the daemon
// is unreachable.
func TestResolveSessionDockerUnreachableDaemonReturnsError(t *testing.T) {
	t.Setenv("DOCKER_HOST", "unix:///nonexistent/stella-test-docker.sock")
	t.Setenv("DOCKER_TLS_VERIFY", "")
	t.Setenv("DOCKER_CERT_PATH", "")

	workspace := t.TempDir()
	userRoot := workspace + "/users/1"
	backends, err := NewBackendRegistry(BackendDefinition{
		Name: config.SandboxBackendDocker,
		Create: func(context.Context, BackendRequest) (pkgsandbox.Session, error) {
			return nil, errors.New("docker daemon unreachable")
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = ResolveSession(context.Background(), Config{
		Paths: Paths{
			AgentRoot: workspace,
			UserRoot:  userRoot,
		},
		SandboxConfig:    config.SandboxConfig{},
		SandboxBackendFn: func(_ context.Context) string { return config.SandboxBackendDocker },
		Backends:         backends,
	})
	if err == nil {
		t.Fatal("expected error for docker backend with unreachable daemon")
	}
	if !strings.Contains(err.Error(), "docker") {
		t.Fatalf("expected error to mention 'docker', got: %v", err)
	}
}

func TestResolveSessionNativeSelectionSurvivesPrepClose(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("requires the real Darwin Seatbelt backend")
	}
	if err := exec.Command("/usr/bin/sandbox-exec", "-p", "(version 1)(allow default)", "/usr/bin/true").Run(); err != nil {
		t.Skip("macOS Seatbelt is unavailable in this environment")
	}
	// Keep a host-installed mise from masking the disabled shared engine. The
	// session must prove discovery comes only from the authorized selections.
	t.Setenv("PATH", "/usr/bin:/bin")
	stellaHome := canonicalTempDir(t)
	corePlan := fixtureCoreRuntimePlan(t, stellaHome)
	userRoot := canonicalTempDir(t)
	workspace := filepath.Join(userRoot, "agents", "agent")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(userRoot, "data"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(stellaHome, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	fakeMise := `#!/bin/sh
set -eu
case "$1" in
trust|reshim) exit 0 ;;
install)
  case "$MISE_DATA_DIR" in
    */.mise-tools) root="$MISE_DATA_DIR/installs/sys-tool/1"; tool=sys-tool ;;
    *)
      for tool in user-tool user-two; do
        root="$MISE_DATA_DIR/installs/$tool/1"
        mkdir -p "$root/bin"
        printf '#!/bin/sh\nprintf "%s\n" selected\n' "$tool" > "$root/bin/$tool"
        chmod 755 "$root/bin/$tool"
      done
      exit 0
      ;;
  esac
  mkdir -p "$root/bin"
  printf '#!/bin/sh\nprintf "%s\n" selected\n' "$tool" > "$root/bin/$tool"
  chmod 755 "$root/bin/$tool"
  ;;
where)
  case "$2" in
    *sys-tool*) printf '%s\n' "$MISE_DATA_DIR/installs/sys-tool/1" ;;
    *user-tool*) printf '%s\n' "$MISE_DATA_DIR/installs/user-tool/1" ;;
    *user-two*) printf '%s\n' "$MISE_DATA_DIR/installs/user-two/1" ;;
    *) exit 2 ;;
  esac
  ;;
which)
  case "$2" in
    sys-tool) printf '%s\n' "$MISE_DATA_DIR/installs/sys-tool/1/bin/sys-tool" ;;
    user-tool) printf '%s\n' "$MISE_DATA_DIR/installs/user-tool/1/bin/user-tool" ;;
    user-two) printf '%s\n' "$MISE_DATA_DIR/installs/user-two/1/bin/user-two" ;;
    *) exit 2 ;;
  esac
  ;;
*) exit 2 ;;
esac
`
	if err := os.WriteFile(filepath.Join(stellaHome, "bin", "mise"), []byte(fakeMise), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stellaHome, "bin", ".stella-shell-env"), []byte("# test\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	backends, err := NewBackendRegistry(BackendDefinition{
		Name: config.SandboxBackendLocal,
		Create: func(ctx context.Context, request BackendRequest) (pkgsandbox.Session, error) {
			factory := localbackend.NewFactoryWithMountSources(request.MountSources, localbackend.Config{StellaHome: request.Paths.StellaHome})
			return factory.CreateSession(ctx, request.Policy)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	system := pkgplugins.PluginBinarySpec{
		PluginResourceIdentity: pkgplugins.PluginResourceIdentity{
			PluginID: "tool/system", ConfigID: "system-config", Scope: string(plugin.ScopeSystemAgent), Revision: 1,
		},
		Name: "sys-tool", Tool: "github:owner/sys-tool", Version: "1.0.0",
	}
	user := pkgplugins.PluginBinarySpec{
		PluginResourceIdentity: pkgplugins.PluginResourceIdentity{
			PluginID: "tool/user", ConfigID: "user-config", Scope: string(plugin.ScopeUserAgent), Revision: 1,
		},
		Name: "user-tool", Tool: "github:owner/user-tool", Version: "1.0.0",
	}
	userTwo := user
	userTwo.PluginID = "tool/user-two"
	userTwo.ConfigID = "user-two-config"
	userTwo.Name = "user-two"
	userTwo.Tool = "github:owner/user-two"
	type resolvedSession struct {
		pkgsandbox.Session
		userName string
	}
	var sessions []resolvedSession
	type sessionResult struct {
		session pkgsandbox.Session
		err     error
		name    string
	}
	results := make(chan sessionResult, 2)
	for _, userSpec := range []pkgplugins.PluginBinarySpec{user, userTwo} {
		go func(userSpec pkgplugins.PluginBinarySpec) {
			session, err := ResolveSession(context.Background(), Config{
				SandboxBackendFn: func(context.Context) string { return config.SandboxBackendLocal },
				Backends:         backends,
				Paths:            Paths{StellaHome: stellaHome, UserRoot: userRoot, AgentRoot: workspace},
				UserID:           "user",
				AgentID:          "agent",
				CoreRuntimePlan:  corePlan,
				BinarySpecs:      []pkgplugins.PluginBinarySpec{system, userSpec},
			})
			results <- sessionResult{session: session, err: err, name: userSpec.Name}
		}(userSpec)
	}
	for range 2 {
		result := <-results
		if result.err != nil {
			t.Fatalf("ResolveSession failed in concurrent native setup: %v", result.err)
		}
		sessions = append(sessions, resolvedSession{Session: result.session, userName: result.name})
	}
	for _, resolved := range sessions {
		session := resolved.Session
		defer session.Close() //nolint:errcheck
		// The channel completion order is intentionally nondeterministic. The
		// selection identity, rather than arrival order, owns this assertion.
		userName := resolved.userName
		for _, name := range []string{"sys-tool", userName} {
			result, err := session.Exec(context.Background(), "command -v "+name, pkgsandbox.ExecOptions{})
			if err != nil || result.ExitCode != 0 || (name != "sys-tool" && !strings.Contains(strings.TrimSpace(result.Stdout), ".mise-managed")) {
				t.Fatalf("command -v %s = %q, exit=%d, err=%v", name, result.Stdout, result.ExitCode, err)
			}
			if name == "sys-tool" && !strings.Contains(strings.TrimSpace(result.Stdout), ".mise-tools/public") {
				t.Fatalf("system selection escaped public tree: %q", result.Stdout)
			}
		}
		result, err := session.Exec(context.Background(), "command -v mise", pkgsandbox.ExecOptions{})
		if err != nil || result.ExitCode != 0 {
			t.Fatalf("core mise is not discoverable: %q, exit=%d, err=%v", result.Stdout, result.ExitCode, err)
		}
		var coreMisePath string
		for _, runtime := range corePlan.Runtimes {
			if runtime.Name == "mise" {
				coreMisePath = runtime.Path
				break
			}
		}
		if coreMisePath == "" {
			t.Fatal("core fixture has no mise runtime")
		}
		gotMisePath, err := filepath.EvalSymlinks(strings.TrimSpace(result.Stdout))
		if err != nil {
			t.Fatalf("resolve discovered core mise path %q: %v", result.Stdout, err)
		}
		wantMisePath, err := filepath.EvalSymlinks(coreMisePath)
		if err != nil {
			t.Fatalf("resolve core fixture mise path %q: %v", coreMisePath, err)
		}
		if gotMisePath != wantMisePath {
			t.Fatalf("command -v mise = %q, want core runtime %q", gotMisePath, wantMisePath)
		}
		oldInstall := filepath.Join(stellaHome, ".mise-tools", "installs", "sys-tool", "1", "bin", "sys-tool")
		if result, err := session.Exec(context.Background(), "cat '"+oldInstall+"'", pkgsandbox.ExecOptions{}); err == nil && result.ExitCode == 0 {
			t.Fatalf("final selection exposed shared install: %q", result.Stdout)
		}
	}
	var privateFiles []string
	if err := filepath.WalkDir(filepath.Join(stellaHome, ".mise-managed"), func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() && filepath.Base(path) == "config.toml" {
			privateFiles = append(privateFiles, path)
		}
		return nil
	}); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if len(privateFiles) != 0 {
		t.Fatalf("prep config survived final publication: %v", privateFiles)
	}
}

// fixtureCoreRuntimePlan creates the same complete shape startup publishes,
// while using temporary executable files so tests never invoke the installer
// or download a release runtime.
func fixtureCoreRuntimePlan(t *testing.T, root string) *core.RuntimePlan {
	t.Helper()
	identity, err := core.RuntimeIdentity()
	if err != nil {
		t.Fatalf("core.RuntimeIdentity: %v", err)
	}
	publicDir := filepath.Join(root, "core-runtime")
	if err := os.MkdirAll(publicDir, 0o755); err != nil {
		t.Fatalf("create core fixture directory: %v", err)
	}
	plan := &core.RuntimePlan{
		Identity:     identity,
		PublicDir:    publicDir,
		PublicBinDir: publicDir,
		Runtimes:     make([]core.Runtime, 0, len(core.RuntimeResources())),
	}
	for _, resource := range core.RuntimeResources() {
		name := resource.Name
		if runtime.GOOS == "windows" {
			name += ".exe"
		}
		path := filepath.Join(publicDir, name)
		if err := os.WriteFile(path, []byte("fixture runtime\n"), 0o755); err != nil {
			t.Fatalf("write core fixture %s: %v", resource.Name, err)
		}
		plan.Runtimes = append(plan.Runtimes, core.Runtime{
			Name: resource.Name, Version: resource.Version, Path: path, Available: true,
		})
	}
	if err := core.Verify(*plan); err != nil {
		t.Fatalf("core.Verify fixture: %v", err)
	}
	return plan
}

func TestRunnerFilesystemPolicyKeepsCoreAndOptionalSelectionsSeparate(t *testing.T) {
	stellaHome := t.TempDir()
	corePlan := fixtureCoreRuntimePlan(t, stellaHome)
	optionalDir := filepath.Join(stellaHome, "optional-selection")
	if err := os.MkdirAll(optionalDir, 0o755); err != nil {
		t.Fatal(err)
	}
	paths := Paths{
		StellaHome: stellaHome, WorkspaceRoot: filepath.Join(stellaHome, "workspace"),
		WorkDir: filepath.Join(stellaHome, "workspace"),
	}
	cfg := Config{
		CoreRuntimePlan: corePlan,
		ContextBinaryPlan: &manifest.BinaryInstallPlan{
			PublicDir: optionalDir, PublicBinDir: optionalDir,
		},
	}
	_, sources := runnerFilesystemPolicy(paths, cfg)
	coreSource, ok := sources[pkgsandbox.MountStellaHome+"/bin"]
	if !ok || coreSource != corePlan.PublicDir {
		t.Fatalf("core /bin source = %q, want %q", coreSource, corePlan.PublicDir)
	}
	optionalSource, ok := sources[pkgsandbox.MountStellaHome+"/optional-selection"]
	if !ok || optionalSource != optionalDir {
		t.Fatalf("optional selection source = %q, want %q", optionalSource, optionalDir)
	}
	if coreSource == optionalSource {
		t.Fatal("core and optional selections must remain separate directories")
	}
}

func TestCreateSessionForBackendOverlaysCoreWithoutClobberingOptionalState(t *testing.T) {
	stellaHome := canonicalTempDir(t)
	corePlan := fixtureCoreRuntimePlan(t, stellaHome)
	optionalDir := filepath.Join(stellaHome, "optional-selection")
	userDir := filepath.Join(stellaHome, "user-selection")
	for _, dir := range []string{optionalDir, userDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	contextPlan := &manifest.BinaryInstallPlan{
		Identity: "context", ConfigPath: filepath.Join(optionalDir, "system.toml"),
		ShimsDir: filepath.Join(optionalDir, "shims"), PublicDir: optionalDir, PublicBinDir: optionalDir,
	}
	userPlan := &manifest.BinaryInstallPlan{
		Identity: "user", ConfigPath: filepath.Join(userDir, "global.toml"),
		ShimsDir: filepath.Join(userDir, "shims"), PublicDir: userDir, PublicBinDir: userDir,
	}
	workspace := t.TempDir()
	userRoot := canonicalTempDir(t)
	var captured BackendRequest
	backends, err := NewBackendRegistry(BackendDefinition{
		Name: "capture",
		Create: func(_ context.Context, request BackendRequest) (pkgsandbox.Session, error) {
			captured = request
			return pkgsandbox.NopSession(), nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = createSessionForBackend(context.Background(), Config{
		SandboxBackendFn:  func(context.Context) string { return "capture" },
		Backends:          backends,
		Paths:             Paths{StellaHome: stellaHome, AgentRoot: workspace, UserRoot: userRoot},
		UserID:            "user-1",
		AgentID:           "agent-1",
		ContextBinaryPlan: contextPlan,
		UserBinaryPlan:    userPlan,
		CoreRuntimePlan:   corePlan,
	}, "capture")
	if err != nil {
		t.Fatalf("createSessionForBackend: %v", err)
	}
	env := captured.Policy.Env
	dataDir := filepath.Join(userRoot, "data", ".config", "mise")
	miseDataDir := filepath.Join(stellaHome, "users", "user-1", ".mise-tools")
	if got := env["MISE_DATA_DIR"]; got != miseDataDir {
		t.Fatalf("MISE_DATA_DIR = %q, want %q", got, miseDataDir)
	}
	if got, want := env["MISE_CONFIG_DIR"], dataDir; got != want {
		t.Fatalf("MISE_CONFIG_DIR = %q, want %q", got, want)
	}
	if got := env["MISE_YES"]; got != "1" {
		t.Fatalf("MISE_YES = %q, want 1", got)
	}
	if got, want := env["MISE_GLOBAL_CONFIG_FILE"], userPlan.ConfigPath; got != want {
		t.Fatalf("MISE_GLOBAL_CONFIG_FILE = %q, want %q", got, want)
	}
	trusted := env["MISE_TRUSTED_CONFIG_PATHS"]
	if !strings.Contains(trusted, userPlan.ConfigPath) {
		t.Fatalf("MISE_TRUSTED_CONFIG_PATHS lost user config: %q", trusted)
	}
	if got, want := env[pkgsandbox.EnvUserNativeSelectionDir], userPlan.PublicBinDir; got != want {
		t.Fatalf("user optional marker = %q, want %q", got, want)
	}
	if got, want := env[pkgsandbox.EnvCoreRuntimeDir], corePlan.PublicBinDir; got != want {
		t.Fatalf("core marker = %q, want %q", got, want)
	}
	if got, want := env[pkgsandbox.EnvNativeSelectionDir], contextPlan.PublicBinDir; got != want {
		t.Fatalf("system optional marker = %q, want %q", got, want)
	}
	pathEntries := strings.Split(env["PATH"], string(os.PathListSeparator))
	for i, entry := range pathEntries {
		if entry == "" {
			t.Fatalf("PATH contains empty element at index %d: %q", i, env["PATH"])
		}
	}
	if !strings.Contains(env["PATH"], userPlan.PublicBinDir) || !strings.Contains(env["PATH"], corePlan.PublicBinDir) {
		t.Fatalf("PATH lost user/core selections: %q", env["PATH"])
	}
}

func canonicalTempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	canonical, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("canonicalize fixture directory %q: %v", dir, err)
	}
	return canonical
}

// TestBuildSandboxEnv_vaultSecretsInjected verifies that vault secrets appear
// in the sandbox env and that runner-set vars (STELLA_HOME) take precedence over
// any same-named vault entry.
// A group session carries both a GroupID and a synthetic UserID equal to it. It
// must key off the group under the "group-" prefix in the users tree, so a real
// user whose ID equals the group ID can never share the group's writable mise
// tree (#442). Temporary storage is backend-owned and session-private.
func TestRunnerFilesystemPolicyGroupUsesGroupSubtree(t *testing.T) {
	userData := filepath.Join("/stella", "users", "group-g7", "data")
	paths := Paths{
		StellaHome:    "/stella",
		UserRoot:      "/stella/users/group-g7",
		UserDataDir:   userData,
		WorkspaceRoot: "/stella/users/group-g7/agents/a1",
		WorkDir:       "/stella/users/group-g7/agents/a1",
	}
	cfg := Config{GroupID: "g7", UserID: "g7", AgentID: "a1"}

	// The mise tree lives under the group's home in the STELLA_HOME frame (a
	// sibling of data, not inside it), so it shares the system tree's sandbox root
	// once STELLA_HOME is remapped. The group-keying still gates validity (a real
	// user with the same raw ID can't share it).
	if want := filepath.Join("/stella", "users", "group-g7", ".mise-tools"); miseUserDirHost(paths, cfg) != want {
		t.Fatalf("miseUserDirHost = %q, want %q", miseUserDirHost(paths, cfg), want)
	}
}

func TestBuildSandboxEnv_vaultSecretsInjected(t *testing.T) {
	cfg := Config{
		Paths: Paths{
			StellaHome: "/stella",
			AgentRoot:  "/workspace/agent",
			UserRoot:   "/workspace/users/1",
		},
		UserID:  "42",
		AgentID: "a1",
		VaultEnvLoader: &stubVaultLoader{
			env: map[string]string{
				"MY_SECRET":   "s3cr3t",
				"STELLA_HOME": "should-be-overridden", // runner var must win
			},
		},
	}

	paths, err := ResolvePaths(cfg)
	if err != nil {
		t.Fatalf("ResolvePaths: %v", err)
	}

	env, err := buildSandboxEnv(context.Background(), cfg, paths)
	if err != nil {
		t.Fatalf("buildSandboxEnv: %v", err)
	}

	// Vault secret must be present.
	if got := env["MY_SECRET"]; got != "s3cr3t" {
		t.Errorf("MY_SECRET = %q, want %q", got, "s3cr3t")
	}

	// Runner var (STELLA_HOME) must override any same-named vault entry.
	if got := env["STELLA_HOME"]; got != cfg.Paths.StellaHome {
		t.Errorf("STELLA_HOME = %q, want %q (runner var must take precedence)", got, cfg.Paths.StellaHome)
	}
}

// TestBuildSandboxEnv_noVaultLoader verifies that buildSandboxEnv behaves
// correctly (returns runner env vars) when no vault loader is configured.
func TestBuildSandboxEnv_noVaultLoader(t *testing.T) {
	cfg := Config{
		Paths: Paths{
			StellaHome: "/stella",
			AgentRoot:  "/workspace/agent",
			UserRoot:   "/workspace/users/1",
		},
	}

	paths, err := ResolvePaths(cfg)
	if err != nil {
		t.Fatalf("ResolvePaths: %v", err)
	}

	env, err := buildSandboxEnv(context.Background(), cfg, paths)
	if err != nil {
		t.Fatalf("buildSandboxEnv: %v", err)
	}

	if got := env["STELLA_HOME"]; got != cfg.Paths.StellaHome {
		t.Errorf("STELLA_HOME = %q, want %q", got, cfg.Paths.StellaHome)
	}
}

// TestBuildSandboxEnv_OAuthBundleKeysStripped verifies that vault entries for
// OAuth bundle keys are not forwarded into the sandbox environment, even when
// present in the vault.
func TestBuildSandboxEnv_OAuthBundleKeysStripped(t *testing.T) {
	cfg := Config{
		Paths: Paths{
			StellaHome: "/stella",
			AgentRoot:  "/workspace/agent",
			UserRoot:   "/workspace/users/1",
		},
		UserID:  "1",
		AgentID: "a1",
		VaultEnvLoader: &stubVaultLoader{
			env: map[string]string{
				"GH_OAUTH":     `{"version":1,"access_token":"ghp_secret"}`,
				"OTHER_SECRET": "should-pass-through",
			},
		},
	}

	paths, err := ResolvePaths(cfg)
	if err != nil {
		t.Fatalf("ResolvePaths: %v", err)
	}

	env, err := buildSandboxEnv(context.Background(), cfg, paths)
	if err != nil {
		t.Fatalf("buildSandboxEnv: %v", err)
	}

	if _, ok := env["GH_OAUTH"]; ok {
		t.Error("GH_OAUTH must not appear in sandbox env")
	}

	// Unrelated vault entries must still pass through.
	if got := env["OTHER_SECRET"]; got != "should-pass-through" {
		t.Errorf("OTHER_SECRET = %q, want %q", got, "should-pass-through")
	}
}

func TestBuildSandboxEnv_RuntimeOAuthEnvInjected(t *testing.T) {
	ctx := context.Background()
	store := newStubOAuthVaultStore()
	userID := "7"
	now := time.Now().UTC().Truncate(time.Second)

	registry := oauth.NewProviderRegistry()
	registry.Register(oauth.ProviderConfig{ID: "github", VaultKey: oauth.VaultKeyGitHub})
	registry.Register(oauth.ProviderConfig{ID: "acme", VaultKey: "ACME_OAUTH"})
	tm := oauth.NewTokenManager(store)
	tm.SetRegistry(registry)

	if err := oauth.SaveOAuthBundle(ctx, store, userID, oauth.VaultKeyGitHub, oauth.OAuthBundle{
		Version:     1,
		AccessToken: "ghp_runtime_token",
	}); err != nil {
		t.Fatalf("SaveOAuthBundle: %v", err)
	}
	if err := oauth.SaveOAuthBundle(ctx, store, userID, "ACME_OAUTH", oauth.OAuthBundle{
		Version:          1,
		ClientID:         "acme_client_id",
		ClientSecret:     "acme_client_secret",
		AccessToken:      "acme_access_token",
		RefreshToken:     "acme_refresh_token",
		AccessExpiresAt:  now.Add(2 * time.Hour),
		RefreshExpiresAt: now.Add(24 * time.Hour),
	}); err != nil {
		t.Fatalf("SaveOAuthBundle: %v", err)
	}
	secretValues := NewSessionSecretValues()
	cfg := Config{
		Paths: Paths{
			StellaHome: "/stella",
			AgentRoot:  "/workspace/agent",
			UserRoot:   "/workspace/users/1",
		},
		UserID:              userID,
		AgentID:             "a1",
		VaultEnvLoader:      &stubVaultLoader{env: map[string]string{"OTHER_SECRET": "still-present"}},
		SessionSecretValues: secretValues,
		TokenManager:        tm,
		SessionEnvSpecs: []pkgplugins.SessionEnvSpec{
			{EnvVar: "GH_TOKEN", Source: pkgplugins.SessionEnvSource("oauth.access_token"), OAuthProviderID: "github"},
			{EnvVar: "ACME_ACCESS_TOKEN", Source: pkgplugins.SessionEnvSource("oauth.access_token"), OAuthProviderID: "acme"},
			{EnvVar: "ACME_REFRESH_TOKEN", Source: pkgplugins.SessionEnvSource("oauth.refresh_token"), OAuthProviderID: "acme"},
			{EnvVar: "ACME_CLIENT_ID", Source: pkgplugins.SessionEnvSource("oauth.client_id"), OAuthProviderID: "acme"},
		},
	}

	paths, err := ResolvePaths(cfg)
	if err != nil {
		t.Fatalf("ResolvePaths: %v", err)
	}

	env, err := buildSandboxEnv(ctx, cfg, paths)
	if err != nil {
		t.Fatalf("buildSandboxEnv: %v", err)
	}
	if _, ok := env[oauth.VaultKeyGitHub]; ok {
		t.Fatalf("%s must not appear in sandbox env", oauth.VaultKeyGitHub)
	}
	if got := env["GH_TOKEN"]; got != "ghp_runtime_token" {
		t.Fatalf("GH_TOKEN = %q, want %q", got, "ghp_runtime_token")
	}
	if got := env["ACME_ACCESS_TOKEN"]; got != "acme_access_token" {
		t.Fatalf("ACME_ACCESS_TOKEN = %q, want %q", got, "acme_access_token")
	}
	if got := env["ACME_REFRESH_TOKEN"]; got != "acme_refresh_token" {
		t.Fatalf("ACME_REFRESH_TOKEN = %q, want %q", got, "acme_refresh_token")
	}
	if got := env["ACME_CLIENT_ID"]; got != "acme_client_id" {
		t.Fatalf("ACME_CLIENT_ID = %q, want %q", got, "acme_client_id")
	}
	if got := env["OTHER_SECRET"]; got != "still-present" {
		t.Fatalf("OTHER_SECRET = %q, want %q", got, "still-present")
	}
	requireSessionSecretValues(t, secretValues.Values(),
		[]string{"still-present", "ghp_runtime_token", "acme_access_token", "acme_refresh_token", "acme_client_id"},
		[]string{"/stella"},
	)
}

func TestBuildSandboxEnv_TokenInjectionErrorsAreSkipped(t *testing.T) {
	ctx := context.Background()
	store := newStubOAuthVaultStore()
	userID := "9"

	registry := oauth.NewProviderRegistry()
	registry.Register(oauth.ProviderConfig{ID: "github", VaultKey: oauth.VaultKeyGitHub})
	registry.Register(oauth.ProviderConfig{ID: "acme", VaultKey: "ACME_OAUTH"})
	tm := oauth.NewTokenManager(store)
	tm.SetRegistry(registry)

	if err := store.Set(ctx, userID, oauth.VaultKeyGitHub, `{"version":1,"client_id":"","client_secret":"","access_token":""}`); err != nil {
		t.Fatalf("Set GH_OAUTH: %v", err)
	}
	if err := store.Set(ctx, userID, "ACME_OAUTH", `{"version":1,"client_id":"app","client_secret":"secret","access_token":"token","refresh_token":"refresh","access_expires_at":"2000-01-01T00:00:00Z","refresh_expires_at":"2000-01-01T00:00:00Z"}`); err != nil {
		t.Fatalf("Set ACME_OAUTH: %v", err)
	}

	cfg := Config{
		Paths: Paths{
			StellaHome: "/stella",
			AgentRoot:  "/workspace/agent",
			UserRoot:   "/workspace/users/1",
		},
		UserID:         userID,
		AgentID:        "a1",
		VaultEnvLoader: &stubVaultLoader{},
		TokenManager:   tm,
		SessionEnvSpecs: []pkgplugins.SessionEnvSpec{
			{EnvVar: "GH_TOKEN", Source: pkgplugins.SessionEnvSource("oauth.access_token"), OAuthProviderID: "github"},
			{EnvVar: "ACME_ACCESS_TOKEN", Source: pkgplugins.SessionEnvSource("oauth.access_token"), OAuthProviderID: "acme"},
		},
	}

	paths, err := ResolvePaths(cfg)
	if err != nil {
		t.Fatalf("ResolvePaths: %v", err)
	}

	env, err := buildSandboxEnv(ctx, cfg, paths)
	if err != nil {
		t.Fatalf("buildSandboxEnv: %v", err)
	}
	if _, ok := env["GH_TOKEN"]; ok {
		t.Fatal("GH_TOKEN should be skipped when access token is empty")
	}
	// The acme bundle is expired with an expired refresh token, so it cannot be
	// renewed; it must be skipped rather than injected as a dead credential (#722).
	if got, ok := env["ACME_ACCESS_TOKEN"]; ok {
		t.Fatalf("expired acme token must be skipped, got ACME_ACCESS_TOKEN = %q", got)
	}
	if _, ok := env[oauth.VaultKeyGitHub]; ok {
		t.Fatal("GH_OAUTH must still be stripped when token injection fails")
	}
}
