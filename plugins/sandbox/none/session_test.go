package none

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	sandboxpkg "github.com/CherryHQ/stella/pkg/sandbox"
	"github.com/CherryHQ/stella/plugins/sandbox/internal/sessionfs"
)

func TestFactory_basics(t *testing.T) {
	f := NewFactoryWithMountSources(nil, Config{})
	if f.Name() != "none" {
		t.Errorf("expected name 'none', got %q", f.Name())
	}
	if !f.Available() {
		t.Error("expected Available to return true on Unix")
	}
	// Supported should accept any policy for none backend
	policy := sandboxpkg.Policy{
		Filesystem: sandboxpkg.FilesystemPolicy{WorkingDir: t.TempDir()},
	}
	if err := f.Supported(policy); err != nil {
		t.Errorf("Supported: unexpected error: %v", err)
	}
}

func TestFactory_createSession(t *testing.T) {
	policy := sandboxpkg.Policy{
		Filesystem: sandboxpkg.FilesystemPolicy{
			WorkingDir: t.TempDir(),
		},
		Network:    sandboxpkg.NetworkPolicy{Mode: sandboxpkg.NetworkAllowAll},
		InheritEnv: true,
	}
	f := NewFactoryWithMountSources(nil, Config{})
	sess, err := f.CreateSession(context.Background(), policy)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	defer sess.Close() //nolint:errcheck

	if sess.Policy().Filesystem.WorkingDir != policy.Filesystem.WorkingDir {
		t.Error("Policy not preserved")
	}
	if !sess.Alive() {
		t.Error("expected Alive=true before close")
	}
}

func TestFactoryCreateSession_setsHostXDGPaths(t *testing.T) {
	workspace := t.TempDir()
	userData := t.TempDir()
	policy := sandboxpkg.Policy{
		Env: map[string]string{"XDG_RUNTIME_DIR": "/run/user/1000"},
		Filesystem: sandboxpkg.FilesystemPolicy{
			WorkingDir: sandboxpkg.MountWorkspace,
			Mounts: []sandboxpkg.Mount{
				{SandboxPath: sandboxpkg.MountWorkspace, Access: sandboxpkg.MountReadWrite},
				{SandboxPath: sandboxpkg.MountUserData, Access: sandboxpkg.MountReadWrite},
			},
		},
	}
	sess, err := NewFactoryWithMountSources(map[string]string{sandboxpkg.MountWorkspace: workspace, sandboxpkg.MountUserData: userData}, Config{}).CreateSession(context.Background(), policy)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	defer sess.Close() //nolint:errcheck

	env := sess.Policy().Env
	for key, want := range map[string]string{
		"HOME":              workspace,
		"STELLA_ASSETS_DIR": filepath.Join(userData, "assets"),
		"XDG_CONFIG_HOME":   filepath.Join(userData, ".config"),
		"XDG_DATA_HOME":     filepath.Join(userData, ".local", "share"),
		"XDG_STATE_HOME":    filepath.Join(userData, ".local", "state"),
		"XDG_CACHE_HOME":    filepath.Join(userData, ".cache"),
	} {
		if got := env[key]; got != want {
			t.Errorf("%s = %q, want real host path %q", key, got, want)
		}
	}
	if _, ok := env["XDG_RUNTIME_DIR"]; ok {
		t.Error("XDG_RUNTIME_DIR must not be set")
	}
	if tmpDir := env[sandboxpkg.EnvTempDir]; tmpDir == "" {
		t.Error("TMPDIR must be set")
	} else if _, err := os.Stat(tmpDir); err != nil {
		t.Errorf("TMPDIR %q is unavailable: %v", tmpDir, err)
	}
	wantMounts := map[string]sandboxpkg.MountAccess{
		workspace:                  sandboxpkg.MountReadWrite,
		userData:                   sandboxpkg.MountReadWrite,
		env[sandboxpkg.EnvTempDir]: sandboxpkg.MountReadWrite,
	}
	for _, mount := range sess.Policy().Filesystem.Mounts {
		if mount.SandboxPath == sandboxpkg.MountWorkspace || mount.SandboxPath == sandboxpkg.MountUserData {
			t.Fatalf("identity backend retained virtual mount coordinate %+v", mount)
		}
		if access, ok := wantMounts[mount.SandboxPath]; ok && access == mount.Access {
			delete(wantMounts, mount.SandboxPath)
		}
	}
	if len(wantMounts) != 0 {
		t.Fatalf("identity policy omitted active data mounts: %v", wantMounts)
	}
}

func TestAdjustPolicySnapshotsRunnerPath(t *testing.T) {
	stellaHome := t.TempDir()
	workspace := t.TempDir()
	adjusted, err := (&Factory{cfg: Config{StellaHome: stellaHome}}).adjustPolicy(
		sandboxpkg.Policy{}, workspace, "", t.TempDir(),
	)
	if err != nil {
		t.Fatalf("adjustPolicy: %v", err)
	}
	if got, want := adjusted.Env["PATH"], sandboxpkg.HostEnvBuildPath(stellaHome, "", ""); got != want {
		t.Fatalf("PATH = %q, want %q", got, want)
	}
	if got, want := adjusted.Env[sandboxpkg.EnvRunnerPath], adjusted.Env["PATH"]; got != want {
		t.Fatalf("%s = %q, want final PATH %q", sandboxpkg.EnvRunnerPath, got, want)
	}
}

func TestAdjustPolicyUsesSelectionLocalShims(t *testing.T) {
	stellaHome := t.TempDir()
	selectionShims := filepath.Join(stellaHome, ".mise-tools", "contexts", "system-a", "shims")
	adjusted, err := (&Factory{cfg: Config{StellaHome: stellaHome}}).adjustPolicy(
		sandboxpkg.Policy{Env: map[string]string{"MISE_SHIMS_DIR": selectionShims}},
		t.TempDir(), "", t.TempDir(),
	)
	if err != nil {
		t.Fatalf("adjustPolicy: %v", err)
	}
	if !strings.HasPrefix(adjusted.Env["PATH"], selectionShims+string(filepath.ListSeparator)) {
		t.Fatalf("selection-local shims must lead PATH, got %q", adjusted.Env["PATH"])
	}
	if strings.Contains(adjusted.Env["PATH"], filepath.Join(stellaHome, "bin")) || strings.Contains(adjusted.Env["PATH"], filepath.Join(stellaHome, ".mise-tools", "shims")) {
		t.Fatalf("PATH leaked shared Stella paths: %q", adjusted.Env["PATH"])
	}
}

func TestAdjustPolicyPreservesCoreAndOptionalSelectionMarkers(t *testing.T) {
	stellaHome := t.TempDir()
	optional := filepath.Join(stellaHome, ".mise-tools", "public", "optional")
	core := filepath.Join(stellaHome, "core-runtime")
	base := map[string]string{
		"PATH":                           "/usr/bin",
		"MISE_DATA_DIR":                  filepath.Join(stellaHome, ".mise-tools"),
		"MISE_CONFIG_DIR":                filepath.Join(stellaHome, ".mise-tools", "config"),
		"MISE_YES":                       "1",
		"MISE_NOT_FOUND_AUTO_INSTALL":    "false",
		sandboxpkg.EnvNativeSelectionDir: optional,
		sandboxpkg.EnvCoreRuntimeDir:     core,
	}
	adjusted, err := (&Factory{cfg: Config{StellaHome: stellaHome}}).adjustPolicy(
		sandboxpkg.Policy{Env: base}, t.TempDir(), "", t.TempDir(),
	)
	if err != nil {
		t.Fatalf("adjustPolicy: %v", err)
	}
	if adjusted.Env[sandboxpkg.EnvNativeSelectionDir] != optional {
		t.Fatalf("optional selection marker = %q, want %q", adjusted.Env[sandboxpkg.EnvNativeSelectionDir], optional)
	}
	if adjusted.Env[sandboxpkg.EnvCoreRuntimeDir] != core {
		t.Fatalf("core selection marker = %q, want %q", adjusted.Env[sandboxpkg.EnvCoreRuntimeDir], core)
	}
	if adjusted.Env["MISE_DATA_DIR"] != filepath.Join(stellaHome, ".mise-tools") {
		t.Fatalf("MISE_DATA_DIR was not preserved: %q", adjusted.Env["MISE_DATA_DIR"])
	}
	if adjusted.Env["MISE_CONFIG_DIR"] != filepath.Join(stellaHome, ".mise-tools", "config") || adjusted.Env["MISE_YES"] != "1" {
		t.Fatalf("MISE_* overlay was not preserved: config=%q yes=%q", adjusted.Env["MISE_CONFIG_DIR"], adjusted.Env["MISE_YES"])
	}
	path := adjusted.Env["PATH"]
	if !strings.HasPrefix(path, optional+string(filepath.ListSeparator)) || !strings.Contains(path, core) {
		t.Fatalf("PATH lost optional/core selections: %q", path)
	}
}

func TestNativeSelectionPathRunsSelectedCommand(t *testing.T) {
	stellaHome := t.TempDir()
	selection := filepath.Join(stellaHome, ".mise-tools", "public", "selection")
	if err := os.MkdirAll(selection, 0o755); err != nil {
		t.Fatal(err)
	}
	command := filepath.Join(selection, "selected-tool")
	if err := os.WriteFile(command, []byte("#!/bin/sh\nprintf 'selected\\n'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	policy := sandboxpkg.Policy{
		Env: map[string]string{
			sandboxpkg.EnvNativeSelectionDir: selection,
			"PATH":                           "/usr/bin",
		},
		Filesystem: sandboxpkg.FilesystemPolicy{WorkingDir: t.TempDir()},
	}
	sess, err := (&Factory{cfg: Config{StellaHome: stellaHome}}).CreateSession(context.Background(), policy)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	defer sess.Close() //nolint:errcheck
	result, err := sess.Exec(context.Background(), "selected-tool", sandboxpkg.ExecOptions{})
	if err != nil || result.ExitCode != 0 || result.Stdout != "selected\n" {
		t.Fatalf("selected command result = %+v, err=%v", result, err)
	}
}

func TestFactoryCreateSession_withoutUserDataFallsBackToWorkspace(t *testing.T) {
	workspace := t.TempDir()
	sess, err := NewFactoryWithMountSources(nil, Config{}).CreateSession(context.Background(), sandboxpkg.Policy{
		Filesystem: sandboxpkg.FilesystemPolicy{WorkingDir: workspace},
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	defer sess.Close() //nolint:errcheck

	env := sess.Policy().Env
	for key, want := range map[string]string{
		"HOME":            workspace,
		"XDG_CONFIG_HOME": filepath.Join(workspace, ".config"),
		"XDG_DATA_HOME":   filepath.Join(workspace, ".local", "share"),
		"XDG_STATE_HOME":  filepath.Join(workspace, ".local", "state"),
		"XDG_CACHE_HOME":  filepath.Join(workspace, ".cache"),
	} {
		if got := env[key]; got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
	if _, ok := env["STELLA_ASSETS_DIR"]; ok {
		t.Error("STELLA_ASSETS_DIR must not be set")
	}
}

func TestFactoryCreateSession_errorRemovesOwnedTempDir(t *testing.T) {
	before, err := filepath.Glob(filepath.Join(os.TempDir(), "stella-none-session-tmp-*"))
	if err != nil {
		t.Fatal(err)
	}
	known := make(map[string]struct{}, len(before))
	for _, path := range before {
		known[path] = struct{}{}
	}
	if _, err := NewFactoryWithMountSources(nil, Config{}).CreateSession(context.Background(), sandboxpkg.Policy{}); err == nil {
		t.Fatal("CreateSession accepted policy without a workspace")
	}
	after, err := filepath.Glob(filepath.Join(os.TempDir(), "stella-none-session-tmp-*"))
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range after {
		if _, existed := known[path]; !existed {
			t.Errorf("CreateSession error leaked owned temp directory %q", path)
			_ = os.RemoveAll(path)
		}
	}
}

func TestFactoryCreateSession_ownsDistinctTempDirs(t *testing.T) {
	workspace := t.TempDir()
	policy := sandboxpkg.Policy{Filesystem: sandboxpkg.FilesystemPolicy{WorkingDir: workspace}}
	first, err := NewFactoryWithMountSources(nil, Config{}).CreateSession(context.Background(), policy)
	if err != nil {
		t.Fatalf("CreateSession(first): %v", err)
	}
	second, err := NewFactoryWithMountSources(nil, Config{}).CreateSession(context.Background(), policy)
	if err != nil {
		first.Close() //nolint:errcheck
		t.Fatalf("CreateSession(second): %v", err)
	}
	firstTmp := first.Policy().Env[sandboxpkg.EnvTempDir]
	secondTmp := second.Policy().Env[sandboxpkg.EnvTempDir]
	if firstTmp == "" || secondTmp == "" || firstTmp == secondTmp {
		t.Fatalf("session temp dirs = %q and %q, want distinct non-empty paths", firstTmp, secondTmp)
	}
	toolPath := filepath.Join(firstTmp, "from-tool")
	if err := os.WriteFile(toolPath, []byte("tool"), 0o600); err != nil {
		t.Fatalf("write temp through file-tool path: %v", err)
	}
	result, err := first.Exec(context.Background(), `cat "$TMPDIR/from-tool"; printf exec > "$TMPDIR/from-exec"`, sandboxpkg.ExecOptions{})
	if err != nil || result.ExitCode != 0 || result.Stdout != "tool" {
		t.Fatalf("temp exec round trip = %+v, %v", result, err)
	}
	if data, err := os.ReadFile(filepath.Join(firstTmp, "from-exec")); err != nil || string(data) != "exec" {
		t.Fatalf("read exec temp through file-tool path = %q, %v", data, err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close(first): %v", err)
	}
	if _, err := os.Stat(firstTmp); !os.IsNotExist(err) {
		t.Errorf("first TMPDIR survives close: %v", err)
	}
	if _, err := os.Stat(secondTmp); err != nil {
		t.Errorf("closing first session affected second TMPDIR: %v", err)
	}
	if err := second.Close(); err != nil {
		t.Fatalf("Close(second): %v", err)
	}
	if _, err := os.Stat(secondTmp); !os.IsNotExist(err) {
		t.Errorf("second TMPDIR survives close: %v", err)
	}
}

func TestNoneSession_closeAndAlive(t *testing.T) {
	s := newTestSession(t)
	if !s.Alive() {
		t.Fatal("expected Alive=true initially")
	}
	_ = s.Close()
	if s.Alive() {
		t.Error("expected Alive=false after close")
	}
	// second close must be a no-op
	if err := s.Close(); err != nil {
		t.Errorf("second Close returned error: %v", err)
	}
}

func TestNoneSession_doneChanClosed(t *testing.T) {
	s := newTestSession(t)
	done := s.Done()
	_ = s.Close()
	select {
	case <-done:
	default:
		t.Error("done channel should be closed after Close()")
	}
}

func TestNoneSession_workingDir(t *testing.T) {
	tempDir := t.TempDir()
	policy := sandboxpkg.Policy{
		Filesystem: sandboxpkg.FilesystemPolicy{
			WorkingDir: tempDir,
		},
	}
	s := &noneSession{
		id:     "test",
		policy: policy,
		done:   make(chan struct{}),
	}
	if s.WorkingDir() != tempDir {
		t.Errorf("WorkingDir = %q, want %q", s.WorkingDir(), tempDir)
	}
	if s.Policy().Filesystem.WorkingDir != tempDir {
		t.Error("Policy not preserved")
	}
}

func TestNoneSession_workingDirDefaultsToCwd(t *testing.T) {
	policy := sandboxpkg.Policy{
		Filesystem: sandboxpkg.FilesystemPolicy{},
	}
	s := &noneSession{
		id:     "test",
		policy: policy,
		done:   make(chan struct{}),
	}
	cwd, _ := os.Getwd()
	if s.WorkingDir() != cwd {
		t.Errorf("WorkingDir = %q, want %q", s.WorkingDir(), cwd)
	}
}

func TestFileAccessResolvesRelativePaths(t *testing.T) {
	s := newTestSession(t)
	if err := s.Files().WriteFile("subdir/file.txt", []byte("content"), 0o600); err != nil {
		t.Fatal(err)
	}
	if content, err := os.ReadFile(filepath.Join(s.WorkingDir(), "subdir", "file.txt")); err != nil || string(content) != "content" {
		t.Fatalf("physical file = %q, %v", content, err)
	}
}

func TestFileAccessProjectionRejectsPoisonedExactPath(t *testing.T) {
	s := newTestSession(t)
	target := filepath.Join(s.WorkingDir(), "projection", "digest")
	files := []sandboxpkg.ProjectedFile{
		{Path: "SKILL.md", Content: []byte("# Exact\n"), Mode: 0o444},
		{Path: "scripts/run.sh", Content: []byte("#!/bin/sh\nprintf exact"), Mode: 0o555},
	}
	if err := s.Files().ProjectFiles(target, files); err != nil {
		t.Fatal(err)
	}
	poisoned := filepath.Join(target, "scripts", "run.sh")
	if err := os.Chmod(poisoned, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(poisoned, []byte("poisoned"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.Files().ProjectFiles(target, files); !errors.Is(err, sandboxpkg.ErrProjectionConflict) {
		t.Fatalf("poisoned exact projection = %v, want ErrProjectionConflict", err)
	}
	if content, err := os.ReadFile(poisoned); err != nil || string(content) != "poisoned" {
		t.Fatalf("poisoned projection was replaced: %q, %v", content, err)
	}
}

func TestExec_success(t *testing.T) {
	s := newTestSession(t)
	result, err := s.Exec(context.Background(), "echo hello", sandboxpkg.ExecOptions{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.ExitCode != 0 {
		t.Errorf("expected exit code 0, got %d", result.ExitCode)
	}
	if result.Stdout != "hello\n" {
		t.Errorf("unexpected stdout: %q", result.Stdout)
	}
}

func TestExec_nonzeroExitCode(t *testing.T) {
	s := newTestSession(t)
	result, err := s.Exec(context.Background(), "exit 42", sandboxpkg.ExecOptions{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.ExitCode != 42 {
		t.Errorf("expected exit code 42, got %d", result.ExitCode)
	}
}

func TestExec_withCwd(t *testing.T) {
	s := newTestSession(t)
	rawTempDir := filepath.Join(s.WorkingDir(), "nested")
	if err := os.Mkdir(rawTempDir, 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	// Resolve symlinks for macOS /var → /private/var compatibility
	tempDir, err := filepath.EvalSymlinks(rawTempDir)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	opts := sandboxpkg.ExecOptions{Cwd: tempDir}
	result, err := s.Exec(context.Background(), "pwd", opts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.ExitCode != 0 {
		t.Errorf("expected exit code 0, got %d", result.ExitCode)
	}
	expected := tempDir + "\n"
	if result.Stdout != expected {
		t.Errorf("expected stdout %q, got %q", expected, result.Stdout)
	}
}

func TestExec_withEnv(t *testing.T) {
	s := newTestSession(t)
	opts := sandboxpkg.ExecOptions{
		Env: map[string]string{"TEST_VAR": "test_value"},
	}
	result, err := s.Exec(context.Background(), "echo $TEST_VAR", opts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.ExitCode != 0 {
		t.Errorf("expected exit code 0, got %d", result.ExitCode)
	}
	if result.Stdout != "test_value\n" {
		t.Errorf("unexpected stdout: %q", result.Stdout)
	}
}

func TestExec_closedSession(t *testing.T) {
	s := newTestSession(t)
	_ = s.Close()
	_, err := s.Exec(context.Background(), "echo hello", sandboxpkg.ExecOptions{})
	if err == nil {
		t.Fatal("expected error for closed session, got nil")
	}
}

func TestStartProcess_success(t *testing.T) {
	s := newTestSession(t)
	req := sandboxpkg.ProcessRequest{
		Path: "cat",
		Args: []string{},
	}
	proc, err := s.StartProcess(context.Background(), req)
	if err != nil {
		t.Fatalf("StartProcess: %v", err)
	}
	defer proc.Close() //nolint:errcheck

	if proc.PID() == 0 {
		t.Error("expected non-zero PID")
	}

	// Write to stdin and read from stdout
	_, err = proc.Stdin().Write([]byte("hello\n"))
	if err != nil {
		t.Fatalf("Write to stdin: %v", err)
	}
	_ = proc.Stdin().Close()

	result, err := proc.Wait(context.Background())
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if result.ExitCode != 0 {
		t.Errorf("expected exit code 0, got %d", result.ExitCode)
	}
}

func TestStartProcess_closedSession(t *testing.T) {
	s := newTestSession(t)
	_ = s.Close()
	req := sandboxpkg.ProcessRequest{Path: "echo", Args: []string{"hello"}}
	_, err := s.StartProcess(context.Background(), req)
	if err == nil {
		t.Fatal("expected error for closed session, got nil")
	}
}

func TestBuildEnv(t *testing.T) {
	// Set test env vars.
	t.Setenv("TEST_HOST_VAR", "host_value")
	t.Setenv("STELLA_USER_DIR", "/host/stale-user")

	policy := sandboxpkg.Policy{
		Env:        map[string]string{"POLICY_VAR": "policy_value"},
		InheritEnv: true,
	}
	overrides := map[string]string{"OVERRIDE_VAR": "override_value", "STELLA_USER_DIR": "/override/stale-user"}

	env := buildEnv(policy, overrides)

	// Check that all expected vars are present
	var hasHost, hasPolicy, hasOverride bool
	for _, kv := range env {
		if strings.HasPrefix(kv, "STELLA_USER_DIR=") {
			t.Fatalf("removed STELLA_USER_DIR must not be present: %q", kv)
		}
		switch kv {
		case "TEST_HOST_VAR=host_value":
			hasHost = true
		case "POLICY_VAR=policy_value":
			hasPolicy = true
		case "OVERRIDE_VAR=override_value":
			hasOverride = true
		}
	}

	if !hasHost {
		t.Error("expected TEST_HOST_VAR from host env")
	}
	if !hasPolicy {
		t.Error("expected POLICY_VAR from policy")
	}
	if !hasOverride {
		t.Error("expected OVERRIDE_VAR from overrides")
	}
}

func TestBuildEnv_noInherit(t *testing.T) {
	t.Setenv("TEST_HOST_VAR", "host_value")

	policy := sandboxpkg.Policy{
		Env:        map[string]string{"POLICY_VAR": "policy_value"},
		InheritEnv: false,
	}

	env := buildEnv(policy, nil)

	// Check that host var is NOT present
	for _, kv := range env {
		if kv == "TEST_HOST_VAR=host_value" {
			t.Error("TEST_HOST_VAR should not be present when InheritEnv is false")
		}
	}

	// But policy var should be present
	var hasPolicy bool
	for _, kv := range env {
		if kv == "POLICY_VAR=policy_value" {
			hasPolicy = true
		}
	}
	if !hasPolicy {
		t.Error("expected POLICY_VAR from policy")
	}
}

// newTestSession returns a noneSession with a temporary working directory.
func newTestSession(t *testing.T) *noneSession {
	t.Helper()
	workingDir := t.TempDir()
	session := &noneSession{
		id:     "test",
		policy: sandboxpkg.Policy{Filesystem: sandboxpkg.FilesystemPolicy{WorkingDir: workingDir}},
		done:   make(chan struct{}),
	}
	resolver, err := sessionfs.NewResolver(workingDir, []sessionfs.Mount{{
		HostPath:              workingDir,
		SandboxPath:           workingDir,
		ResolveSymlinkAliases: true,
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resolver.Close() })
	session.resolver = resolver
	session.files = sessionfs.NewAccess(resolver)
	return session
}
