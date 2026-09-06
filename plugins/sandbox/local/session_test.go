package local

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/CherryHQ/stella/internal/agent/prompt"
	sandboxpkg "github.com/CherryHQ/stella/pkg/sandbox"
	"github.com/CherryHQ/stella/plugins/sandbox/internal/sessionfs"
)

func newWorkspaceFactory(workspace string) sandboxpkg.Factory {
	return NewFactoryWithMountSources(map[string]string{sandboxpkg.MountWorkspace: workspace}, Config{})
}

func TestFactory_basics(t *testing.T) {
	f := NewFactoryWithMountSources(nil, Config{})
	if f.(*Factory).Name() != "local" {
		t.Error("expected name 'local'")
	}
	if !f.(*Factory).Available() {
		t.Error("expected Available to return true")
	}
	skipIfBwrapNotFunctional(t)
	policy := sandboxpkg.Policy{
		Filesystem: sandboxpkg.FilesystemPolicy{WorkingDir: sandboxpkg.MountWorkspace},
	}
	if err := f.(*Factory).Supported(policy); err != nil {
		t.Errorf("Supported: unexpected error: %v", err)
	}
}

func TestFactory_createSession(t *testing.T) {
	skipIfBwrapNotFunctional(t)
	root := t.TempDir()
	policy := sandboxpkg.Policy{
		Filesystem: sandboxpkg.FilesystemPolicy{
			WorkingDir: sandboxpkg.MountWorkspace,
			Mounts:     []sandboxpkg.Mount{{SandboxPath: sandboxpkg.MountWorkspace, Access: sandboxpkg.MountReadWrite}},
		},
		Network:    sandboxpkg.NetworkPolicy{Mode: sandboxpkg.NetworkAllowAll},
		InheritEnv: true,
	}
	f := newWorkspaceFactory(root)
	sess, err := f.(*Factory).CreateSession(context.Background(), policy)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	defer sess.(*localSession).Close() //nolint:errcheck

	if sess.WorkingDir() == "" {
		t.Error("expected non-empty WorkingDir")
	}
	if !sess.(*localSession).Alive() {
		t.Error("expected Alive=true before close")
	}
	wantMounts := map[string]bool{
		sess.WorkingDir():                        true,
		sess.Policy().Env[sandboxpkg.EnvTempDir]: true,
	}
	for _, mount := range sess.Policy().Filesystem.Mounts {
		delete(wantMounts, mount.SandboxPath)
	}
	if len(wantMounts) != 0 {
		t.Fatalf("local policy omitted active data mounts: %v", wantMounts)
	}
}

func TestFactory_sessionsOwnDistinctTempDirs(t *testing.T) {
	skipIfBwrapNotFunctional(t)
	root := t.TempDir()
	policy := sandboxpkg.Policy{Filesystem: sandboxpkg.FilesystemPolicy{
		WorkingDir: sandboxpkg.MountWorkspace,
		Mounts:     []sandboxpkg.Mount{{SandboxPath: sandboxpkg.MountWorkspace, Access: sandboxpkg.MountReadWrite}},
	}}
	firstSession, err := newWorkspaceFactory(root).CreateSession(context.Background(), policy)
	if err != nil {
		t.Fatalf("CreateSession(first): %v", err)
	}
	secondSession, err := newWorkspaceFactory(root).CreateSession(context.Background(), policy)
	if err != nil {
		firstSession.Close() //nolint:errcheck
		t.Fatalf("CreateSession(second): %v", err)
	}
	firstTmp := firstSession.(*localSession).tmpMounts[0].realPath
	secondTmp := secondSession.(*localSession).tmpMounts[0].realPath
	if firstTmp == "" || secondTmp == "" || firstTmp == secondTmp {
		t.Fatalf("session temp backings = %q and %q, want distinct non-empty paths", firstTmp, secondTmp)
	}
	toolPath := filepath.Join(firstSession.Policy().Env[sandboxpkg.EnvTempDir], "from-tool")
	if err := firstSession.Files().WriteFile(toolPath, []byte("tool"), 0o600); err != nil {
		t.Fatalf("write temp through file-tool path: %v", err)
	}
	result, err := firstSession.Exec(context.Background(), `cat "$TMPDIR/from-tool"; printf exec > "$TMPDIR/from-exec"`, sandboxpkg.ExecOptions{})
	if err != nil || result.ExitCode != 0 || result.Stdout != "tool" {
		t.Fatalf("temp exec round trip = %+v, %v", result, err)
	}
	execPath := filepath.Join(firstSession.Policy().Env[sandboxpkg.EnvTempDir], "from-exec")
	if data, err := firstSession.Files().ReadFile(execPath); err != nil || string(data) != "exec" {
		t.Fatalf("read exec temp through file-tool path = %q, %v", data, err)
	}
	if err := firstSession.Close(); err != nil {
		t.Fatalf("Close(first): %v", err)
	}
	if _, err := os.Stat(firstTmp); !os.IsNotExist(err) {
		t.Errorf("first TMPDIR survives close: %v", err)
	}
	if _, err := os.Stat(secondTmp); err != nil {
		t.Errorf("closing first session affected second TMPDIR: %v", err)
	}
	if err := secondSession.Close(); err != nil {
		t.Fatalf("Close(second): %v", err)
	}
	if _, err := os.Stat(secondTmp); !os.IsNotExist(err) {
		t.Errorf("second TMPDIR survives close: %v", err)
	}
}

func TestCleanupOwnedTmpMountsLeavesBorrowedMounts(t *testing.T) {
	owned := t.TempDir()
	borrowed := t.TempDir()
	cleanupOwnedTmpMounts([]tmpMount{{realPath: owned, owned: true}, {realPath: borrowed}})
	if _, err := os.Stat(owned); !os.IsNotExist(err) {
		t.Fatalf("owned temp remains after cleanup: %v", err)
	}
	if _, err := os.Stat(borrowed); err != nil {
		t.Fatalf("borrowed temp was removed: %v", err)
	}
}

func TestLocalSessionExecRejectsRemovedManagedTempDirectory(t *testing.T) {
	skipIfBwrapNotFunctional(t)
	root := t.TempDir()
	policy := sandboxpkg.Policy{
		Filesystem: sandboxpkg.FilesystemPolicy{
			WorkingDir: sandboxpkg.MountWorkspace,
			Mounts:     []sandboxpkg.Mount{{SandboxPath: sandboxpkg.MountWorkspace, Access: sandboxpkg.MountReadWrite}},
		},
		Network: sandboxpkg.NetworkPolicy{Mode: sandboxpkg.NetworkAllowAll},
	}
	session, err := newWorkspaceFactory(root).CreateSession(context.Background(), policy)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	s := session.(*localSession)
	defer s.Close() //nolint:errcheck

	tmpDir := s.tmpMounts[0].realPath
	if err := os.RemoveAll(tmpDir); err != nil {
		t.Fatalf("remove managed temp directory: %v", err)
	}
	if _, err := s.Exec(context.Background(), `printf unsafe > "$TMPDIR/unsafe"`, sandboxpkg.ExecOptions{}); err == nil {
		t.Fatal("Exec accepted a removed managed temp directory")
	}
	if s.Alive() {
		t.Fatal("session with an invalid backing plan remained alive")
	}
	select {
	case <-s.Done():
	default:
		t.Fatal("invalid backing plan did not close the session generation")
	}
	if _, err := os.Stat(tmpDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("removed managed temp directory was rebound: %v", err)
	}
}

func TestProviderFilesystemRejectsOverlappingPhysicalMounts(t *testing.T) {
	workspace := t.TempDir()
	nested := filepath.Join(workspace, "nested")
	if err := os.Mkdir(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	policy := sandboxpkg.Policy{Filesystem: sandboxpkg.FilesystemPolicy{
		WorkingDir: "/workspace",
		Mounts: []sandboxpkg.Mount{
			{SandboxPath: "/workspace", Access: sandboxpkg.MountReadWrite},
			{SandboxPath: "/user", Access: sandboxpkg.MountReadWrite},
		},
	}}
	_, _, _, _, _, _, err := providerFilesystem(policy, map[string]string{
		"/workspace": workspace,
		"/user":      nested,
	})
	if err == nil {
		t.Fatal("provider accepted overlapping physical mount sources")
	}
}

func TestProviderFilesystemDoesNotTreatWorkingDirAsPhysicalSource(t *testing.T) {
	_, _, _, _, _, _, err := providerFilesystem(sandboxpkg.Policy{Filesystem: sandboxpkg.FilesystemPolicy{
		WorkingDir: "/host/workspace",
	}}, nil)
	if err == nil || !strings.Contains(err.Error(), "physical source for mount /workspace is required") {
		t.Fatalf("provider inferred a physical source from WorkingDir: %v", err)
	}
}

func TestLocalSession_closeAndAlive(t *testing.T) {
	s, _ := newTestSession(t)
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

func TestLocalSession_doneChanClosed(t *testing.T) {
	s, _ := newTestSession(t)
	done := s.Done()
	_ = s.Close()
	select {
	case <-done:
	default:
		t.Error("done channel should be closed after Close()")
	}
}

func TestLocalSession_workspaceAndWorkingDir(t *testing.T) {
	root := t.TempDir()
	resolved, _ := filepath.EvalSymlinks(root)
	policy := sandboxpkg.Policy{
		Filesystem: sandboxpkg.FilesystemPolicy{
			WorkingDir: resolved,
		},
	}
	s := &localSession{
		id:          "test",
		policy:      policy,
		realRoot:    resolved,
		sandboxRoot: resolved,
		done:        make(chan struct{}),
	}
	if s.WorkingDir() != resolved {
		t.Errorf("WorkingDir = %q, want %q", s.WorkingDir(), resolved)
	}
	if s.Policy().Filesystem.WorkingDir != resolved {
		t.Error("Policy not preserved")
	}
}

// newTestSession returns a localSession with a temporary workspace root.
// The root is resolved through EvalSymlinks so that macOS /var → /private/var
// symlinks do not cause false path-escape rejections.
// sandboxRoot and realRoot are both set to root (no remapping in tests).
func newTestSession(t *testing.T) (*localSession, string) {
	t.Helper()
	rawRoot := t.TempDir()
	root, err := filepath.EvalSymlinks(rawRoot)
	if err != nil {
		t.Fatalf("EvalSymlinks(%q): %v", rawRoot, err)
	}
	policy := sandboxpkg.Policy{
		Filesystem: sandboxpkg.FilesystemPolicy{
			WorkingDir: root,
		},
		Network:    sandboxpkg.NetworkPolicy{Mode: sandboxpkg.NetworkAllowAll},
		InheritEnv: true,
	}
	s := &localSession{
		id:          "test",
		policy:      policy,
		realRoot:    root,
		sandboxRoot: root,
		done:        make(chan struct{}),
	}
	s.resolver = s.pathResolver()
	s.files = sessionfs.NewAccess(s.resolver)
	return s, root
}

// Test-only physical mapping helpers keep provider internals out of the public
// Session contract while preserving direct coverage of the local mount plan.
func (s *localSession) pathResolver() *sessionfs.Resolver {
	if s.resolver != nil {
		return s.resolver
	}
	mounts := append([]sessionfs.Mount(nil), s.providerMounts...)
	if _, ok := mountBySandboxPath(mounts, s.sandboxRoot); !ok && s.realRoot != "" && s.sandboxRoot != "" {
		mounts = append(mounts, sessionfs.Mount{HostPath: s.realRoot, SandboxPath: s.sandboxRoot})
	}
	if _, ok := mountBySandboxPath(mounts, s.userDataSandbox); !ok && s.userDataReal != "" && s.userDataSandbox != "" {
		mounts = append(mounts, sessionfs.Mount{HostPath: s.userDataReal, SandboxPath: s.userDataSandbox})
	}
	for _, mount := range s.policy.Filesystem.Mounts {
		if _, ok := mountBySandboxPath(mounts, mount.SandboxPath); !ok {
			mounts = append(mounts, sessionfs.Mount{
				HostPath: mount.SandboxPath, SandboxPath: mount.SandboxPath,
				ReadOnly: mount.Access == sandboxpkg.MountReadOnly,
			})
		}
	}
	for _, mount := range s.tmpMounts {
		mounts = append(mounts, sessionfs.Mount{HostPath: mount.realPath, SandboxPath: mount.sandboxPath})
	}
	resolver, err := sessionfs.NewResolver(s.WorkingDir(), mounts)
	if err != nil {
		panic(err)
	}
	s.resolver = resolver
	return resolver
}

func (s *localSession) resolveReadPath(name string) (string, error) {
	resolved, err := s.pathResolver().Resolve(name, false)
	return resolved.HostPath(), err
}

func (s *localSession) resolveWritePath(name string) (string, error) {
	resolved, err := s.pathResolver().Resolve(name, true)
	return resolved.HostPath(), err
}

func (s *localSession) resolveTestPath(name string) (string, string, error) {
	resolved, err := s.pathResolver().Resolve(name, false)
	return resolved.HostPath(), resolved.SandboxPath, err
}

// newExecTestSession returns a session built by the factory, so the sandbox root
// carries the platform's real mapping (/workspace under bwrap). Tests that run a
// command must not use newTestSession: its hand-built session claims the host
// path is also the sandbox path, which on Linux collides with the session's
// private /tmp bind and leaves the working directory unreachable.
func newExecTestSession(t *testing.T) *localSession {
	t.Helper()
	root := t.TempDir()
	policy := sandboxpkg.Policy{
		Filesystem: sandboxpkg.FilesystemPolicy{
			WorkingDir: sandboxpkg.MountWorkspace,
			Mounts:     []sandboxpkg.Mount{{SandboxPath: sandboxpkg.MountWorkspace, Access: sandboxpkg.MountReadWrite}},
		},
		Network:    sandboxpkg.NetworkPolicy{Mode: sandboxpkg.NetworkAllowAll},
		InheritEnv: true,
	}
	sess, err := newWorkspaceFactory(root).CreateSession(context.Background(), policy)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	t.Cleanup(func() { sess.Close() }) //nolint:errcheck
	return sess.(*localSession)
}

// TestResolvePath_rejectsOutsideRoot verifies that ResolvePath returns an error
// when the resolved path is outside the workspace root.
func TestResolvePath_rejectsOutsideRoot(t *testing.T) {
	s, root := newTestSession(t)

	// A path that traverses above the root.
	outside := filepath.Join(root, "..", "escape")
	_, err := s.resolveReadPath(outside)
	if err == nil {
		t.Fatalf("expected error for path outside workspace root, got nil")
	}
}

// TestResolvePath_acceptsInsideRoot verifies that ResolvePath accepts paths
// that are within the workspace root.
func TestResolvePath_acceptsInsideRoot(t *testing.T) {
	s, root := newTestSession(t)

	// Create a file inside the root.
	f := filepath.Join(root, "file.txt")
	if err := os.WriteFile(f, []byte("hi"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	got, err := s.resolveReadPath(f)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// The resolved path should equal f (root is already EvalSymlinks-resolved).
	if got != f {
		t.Errorf("expected %q, got %q", f, got)
	}
}

// TestResolvePath_realRootSymlink verifies that resolvePath works when realRoot
// contains symlink components (e.g. /home/user → /Users/user on macOS autofs,
// or any symlinked home directory). CreateSession resolves realRoot through
// symlinks so the pathWithinRoot comparison succeeds.
func TestResolvePath_realRootSymlink(t *testing.T) {
	// Create the actual workspace directory.
	actualDir := t.TempDir()
	actualDir, _ = filepath.EvalSymlinks(actualDir)
	if err := os.WriteFile(filepath.Join(actualDir, "main.go"), []byte("package main"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// Create a symlink that points to the actual directory.
	symlinkParent := t.TempDir()
	symlinkPath := filepath.Join(symlinkParent, "link")
	if err := os.Symlink(actualDir, symlinkPath); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	// Simulate CreateSession's EvalSymlinks on realRoot: the session stores
	// the resolved root, not the symlinked one.
	resolvedRoot := actualDir // as if EvalSymlinks(symlinkPath) ran

	s := &localSession{
		id:          "test",
		policy:      sandboxpkg.Policy{Filesystem: sandboxpkg.FilesystemPolicy{WorkingDir: "/workspace"}},
		realRoot:    resolvedRoot,
		sandboxRoot: "/workspace",
		done:        make(chan struct{}),
	}

	got, err := s.resolveReadPath("/workspace/main.go")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := filepath.Join(actualDir, "main.go")
	if got != want {
		t.Errorf("ResolvePath = %q, want %q", got, want)
	}
}

// TestResolvePath_remapped verifies that ResolvePath translates a sandbox-space
// path to the real host path when sandboxRoot != realRoot.
func TestResolvePath_remapped(t *testing.T) {
	rawRoot := t.TempDir()
	root, err := filepath.EvalSymlinks(rawRoot)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}

	// Create a file in the real root.
	f := filepath.Join(root, "main.go")
	if err := os.WriteFile(f, []byte("package main"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	policy := sandboxpkg.Policy{
		Filesystem: sandboxpkg.FilesystemPolicy{
			WorkingDir: "/workspace",
		},
	}
	s := &localSession{
		id:          "test",
		policy:      policy,
		realRoot:    root,
		sandboxRoot: "/workspace",
		done:        make(chan struct{}),
	}

	// Agent passes sandbox-space path.
	got, err := s.resolveReadPath("/workspace/main.go")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != f {
		t.Errorf("ResolvePath(/workspace/main.go) = %q, want %q", got, f)
	}
}

func TestPromptProjectContextUsesCanonicalWorkingDir(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte("canonical local instructions"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := &localSession{
		id:          "prompt-canonical",
		policy:      sandboxpkg.Policy{Filesystem: sandboxpkg.FilesystemPolicy{WorkingDir: "/workspace"}},
		realRoot:    root,
		sandboxRoot: "/workspace",
		done:        make(chan struct{}),
	}
	s.files = sessionfs.NewAccess(s.pathResolver())

	got := prompt.BuildSystemPromptFromDB(context.Background(), prompt.DBPromptParams{
		SystemPrompt: "You are Stella.",
		Session:      s,
	})
	if !strings.Contains(got, "canonical local instructions") {
		t.Fatalf("prompt did not discover AGENTS.md through canonical project view: %s", got)
	}
	if _, err := s.resolveReadPath(filepath.Join(root, "AGENTS.md")); err == nil {
		t.Fatal("resolver accepted the physical ProjectRoot coordinate")
	}
}

// TestResolvePath_extraMountAllowed verifies that ResolvePath accepts paths
// within an ExtraReadOnlyMount even when they are outside the workspace root.
func TestResolvePath_extraMountAllowed(t *testing.T) {
	root := t.TempDir()
	root, _ = filepath.EvalSymlinks(root)
	mountDir := t.TempDir()
	mountDir, _ = filepath.EvalSymlinks(mountDir)

	skillFile := filepath.Join(mountDir, "skill.py")
	if err := os.WriteFile(skillFile, []byte("# skill"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	policy := sandboxpkg.Policy{
		Filesystem: sandboxpkg.FilesystemPolicy{
			WorkingDir: root,
			Mounts:     []sandboxpkg.Mount{{SandboxPath: mountDir, Access: sandboxpkg.MountReadOnly}},
		},
	}
	s := &localSession{
		id:          "test",
		policy:      policy,
		realRoot:    root,
		sandboxRoot: root,
		providerMounts: []sessionfs.Mount{
			{HostPath: root, SandboxPath: root},
			{HostPath: mountDir, SandboxPath: mountDir, ReadOnly: true},
		},
		done: make(chan struct{}),
	}

	got, err := s.resolveReadPath(skillFile)
	if err != nil {
		t.Fatalf("unexpected error for extra mount path: %v", err)
	}
	if got != skillFile {
		t.Errorf("ResolvePath = %q, want %q", got, skillFile)
	}
}

// TestResolvePath_rejectsAdjacentToExtraMount verifies that a path adjacent to
// (but not within) an ExtraReadOnlyMount is still rejected.
// TestResolveWritePath_rejectsExtraMount verifies that ResolveWritePath rejects
// paths within ExtraReadOnlyMounts even though ResolvePath accepts them.
func TestResolveWritePath_rejectsExtraMount(t *testing.T) {
	root := t.TempDir()
	root, _ = filepath.EvalSymlinks(root)
	mountDir := t.TempDir()
	mountDir, _ = filepath.EvalSymlinks(mountDir)

	skillFile := filepath.Join(mountDir, "skill.py")
	if err := os.WriteFile(skillFile, []byte("# skill"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	policy := sandboxpkg.Policy{
		Filesystem: sandboxpkg.FilesystemPolicy{
			WorkingDir: root,
			Mounts:     []sandboxpkg.Mount{{SandboxPath: mountDir, Access: sandboxpkg.MountReadOnly}},
		},
	}
	s := &localSession{
		id:          "test",
		policy:      policy,
		realRoot:    root,
		sandboxRoot: root,
		providerMounts: []sessionfs.Mount{
			{HostPath: root, SandboxPath: root},
			{HostPath: mountDir, SandboxPath: mountDir, ReadOnly: true},
		},
		done: make(chan struct{}),
	}

	// ResolvePath should accept it.
	if _, err := s.resolveReadPath(skillFile); err != nil {
		t.Fatalf("ResolvePath unexpectedly rejected extra mount path: %v", err)
	}

	// ResolveWritePath must reject it.
	_, err := s.resolveWritePath(skillFile)
	if err == nil {
		t.Fatal("expected ResolveWritePath to reject read-only mount path, got nil")
	}
}

// TestSystemTree_readableButNotWritable verifies that on an isolating backend
// (sandbox STELLA_HOME differs from host), the read-only system install tree
// addressed as /opt/stella is resolvable for reads and rejected for writes.
func TestBuiltinBundleReadableButNotWritable(t *testing.T) {
	root := t.TempDir()
	root, _ = filepath.EvalSymlinks(root)
	hostSH := t.TempDir()
	hostSH, _ = filepath.EvalSymlinks(hostSH)

	skillDir := filepath.Join(hostSH, "bundles", "revision", "system", "demo")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "refs.md"), []byte("# refs"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	s := &localSession{
		id:             "test",
		policy:         sandboxpkg.Policy{Filesystem: sandboxpkg.FilesystemPolicy{WorkingDir: root}},
		realRoot:       root,
		sandboxRoot:    root,
		providerMounts: []sessionfs.Mount{{HostPath: root, SandboxPath: root}, {HostPath: filepath.Join(hostSH, "bundles", "revision"), SandboxPath: sandboxpkg.MountBuiltinSkills, ReadOnly: true}},
		stellaHomeHost: hostSH,
		done:           make(chan struct{}),
	}

	// The agent addresses the exact release bundle through its fixed sandbox view.
	sandboxPath := sandboxpkg.MountBuiltinSkills + "/system/demo/refs.md"
	real, _, err := s.resolveTestPath(sandboxPath)
	if err != nil {
		t.Fatalf("resolvePath rejected system-tree read: %v", err)
	}
	if want := filepath.Join(skillDir, "refs.md"); real != want {
		t.Errorf("resolvePath real = %q, want %q", real, want)
	}

	// Writes into the bundle must be rejected.
	if _, err := s.resolveWritePath(sandboxPath); err == nil {
		t.Fatal("expected ResolveWritePath to reject system-tree path, got nil")
	}

	// The old STELLA_HOME projection is not a fallback mount.
	if err := os.MkdirAll(filepath.Join(hostSH, "users", "u1"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if _, _, err := s.resolveTestPath("/opt/stella/.agents/skills/demo/refs.md"); err == nil {
		t.Fatal("expected resolvePath to reject the retired extracted builtin path, got nil")
	}
}

// TestResolveWritePath_acceptsWorkspace verifies that ResolveWritePath allows
// paths within the writable workspace root.
func TestResolveWritePath_acceptsWorkspace(t *testing.T) {
	s, root := newTestSession(t)

	f := filepath.Join(root, "output.txt")
	if err := os.WriteFile(f, []byte("data"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	got, err := s.resolveWritePath(f)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != f {
		t.Errorf("ResolveWritePath = %q, want %q", got, f)
	}
}

func TestResolvePath_rejectsAdjacentToExtraMount(t *testing.T) {
	root := t.TempDir()
	root, _ = filepath.EvalSymlinks(root)
	mountDir := t.TempDir()
	mountDir, _ = filepath.EvalSymlinks(mountDir)

	policy := sandboxpkg.Policy{
		Filesystem: sandboxpkg.FilesystemPolicy{
			WorkingDir: root,
			Mounts:     []sandboxpkg.Mount{{SandboxPath: mountDir, Access: sandboxpkg.MountReadOnly}},
		},
	}
	s := &localSession{
		id:          "test",
		policy:      policy,
		realRoot:    root,
		sandboxRoot: root,
		providerMounts: []sessionfs.Mount{
			{HostPath: root, SandboxPath: root},
			{HostPath: mountDir, SandboxPath: mountDir, ReadOnly: true},
		},
		done: make(chan struct{}),
	}

	// A sibling directory of mountDir — not inside any mount.
	adjacent := filepath.Join(filepath.Dir(mountDir), "adjacent")
	_, err := s.resolveReadPath(adjacent)
	if err == nil {
		t.Fatal("expected error for path adjacent to extra mount, got nil")
	}
}

func TestResolvePath_rejectsSymlinkParentForMissingPath(t *testing.T) {
	s, root := newTestSession(t)
	outside := t.TempDir()
	link := filepath.Join(root, "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	if _, err := s.Files().ReadFile(filepath.Join(link, "new.txt")); err == nil {
		t.Fatal("expected symlink parent to be rejected")
	}
}

func TestResolveCwd_rejectsOutsideRoot(t *testing.T) {
	s, root := newTestSession(t)
	outside := filepath.Join(root, "..")

	_, _, err := s.resolveCwd(outside)
	if err == nil {
		t.Fatal("expected cwd outside workspace to be rejected")
	}
	if !strings.Contains(err.Error(), "outside process mount plan") {
		t.Fatalf("expected outside-root error, got: %v", err)
	}
}

// TestAdjustPolicy_rewritesMiseEnvPaths verifies that adjustPolicy rewrites
// MISE_* path-valued env vars from host STELLA_HOME to the sandbox-adjusted
// path so that mise shims resolve correctly inside bwrap.
func TestAdjustPolicy_rewritesMiseEnvPaths(t *testing.T) {
	hostSH := "/home/user/.stella"
	sandboxSH := adjustStellaHome(hostSH)

	policy := sandboxpkg.Policy{
		Env: map[string]string{
			"BASH_ENV":                    hostSH + "/bin/.stella-shell-env",
			"MISE_DATA_DIR":               hostSH + "/.mise-tools",
			"MISE_CONFIG_DIR":             hostSH + "/.mise-tools/config",
			"MISE_CACHE_DIR":              hostSH + "/.mise-tools/cache",
			"MISE_STATE_DIR":              hostSH + "/.mise-tools/state",
			"MISE_SYSTEM_CONFIG_FILE":     hostSH + "/.mise-tools/configs/_builtin.toml",
			"MISE_TRUSTED_CONFIG_PATHS":   hostSH + "/.mise-tools/configs/_builtin.toml",
			"MISE_YES":                    "1",
			"MISE_NOT_FOUND_AUTO_INSTALL": "false",
			"OTHER_VAR":                   "keep-as-is",
		},
	}

	f := &Factory{cfg: Config{StellaHome: hostSH}}
	sandboxRoot, realRoot := processVisiblePath(sandboxpkg.MountWorkspace, sandboxpkg.MountWorkspace), sandboxpkg.MountWorkspace
	adjusted := f.adjustPolicy(policy, sandboxRoot, realRoot, "", "")

	if hostSH == sandboxSH {
		t.Skip("no path remapping on this platform")
	}

	if got, want := adjusted.Env[sandboxpkg.EnvRunnerPath], adjusted.Env["PATH"]; got != want {
		t.Errorf("%s = %q, want final PATH %q", sandboxpkg.EnvRunnerPath, got, want)
	}
	for _, tc := range []struct {
		key  string
		want string
	}{
		{"BASH_ENV", sandboxSH + "/bin/.stella-shell-env"},
		{"MISE_DATA_DIR", sandboxSH + "/.mise-tools"},
		{"MISE_CONFIG_DIR", sandboxSH + "/.mise-tools/config"},
		{"MISE_CACHE_DIR", sandboxSH + "/.mise-tools/cache"},
		{"MISE_STATE_DIR", sandboxSH + "/.mise-tools/state"},
		{"MISE_SYSTEM_CONFIG_FILE", sandboxSH + "/.mise-tools/configs/_builtin.toml"},
		{"MISE_TRUSTED_CONFIG_PATHS", sandboxSH + "/.mise-tools/configs/_builtin.toml"},
		{"MISE_YES", "1"},
		{"MISE_NOT_FOUND_AUTO_INSTALL", "false"},
		{"STELLA_HOME", sandboxSH},
	} {
		got := adjusted.Env[tc.key]
		if got != tc.want {
			t.Errorf("env[%s] = %q, want %q", tc.key, got, tc.want)
		}
	}

	if adjusted.Env["OTHER_VAR"] != "keep-as-is" {
		t.Errorf("OTHER_VAR was unexpectedly modified: %q", adjusted.Env["OTHER_VAR"])
	}
}

func TestAdjustPolicyUsesSelectionLocalShims(t *testing.T) {
	hostSH := "/home/user/.stella"
	sandboxSH := adjustStellaHome(hostSH)
	selectionShims := hostSH + "/.mise-tools/contexts/system-a/shims"
	policy := sandboxpkg.Policy{Env: map[string]string{"MISE_SHIMS_DIR": selectionShims}}

	adjusted := (&Factory{cfg: Config{StellaHome: hostSH}}).adjustPolicy(
		policy, "/workspace", "/workspace", "", "",
	)
	wantSelection := sandboxSH + "/.mise-tools/contexts/system-a/shims"
	if !strings.HasPrefix(adjusted.Env["PATH"], wantSelection+string(filepath.ListSeparator)) {
		t.Fatalf("selection-local shims must lead PATH, got %q", adjusted.Env["PATH"])
	}
	if strings.Contains(adjusted.Env["PATH"], sandboxSH+"/bin") || strings.Contains(adjusted.Env["PATH"], sandboxSH+"/.mise-tools/shims") {
		t.Fatalf("PATH leaked shared Stella paths: %q", adjusted.Env["PATH"])
	}
}

func TestAdjustPolicyUsesMountedNativeSelectionPath(t *testing.T) {
	hostSH := "/home/user/.stella"
	sandboxSH := adjustStellaHome(hostSH)
	selection := hostSH + "/.mise-tools/public/selection"
	core := hostSH + "/core-runtime"
	adjusted := (&Factory{cfg: Config{StellaHome: hostSH}}).adjustPolicy(
		sandboxpkg.Policy{Env: map[string]string{
			sandboxpkg.EnvNativeSelectionDir: selection,
			sandboxpkg.EnvCoreRuntimeDir:     core,
		}},
		"/workspace", "/workspace", "", "",
	)
	path := adjusted.Env["PATH"]
	if !strings.HasPrefix(path, sandboxSH+"/.mise-tools/public/selection"+string(filepath.ListSeparator)) {
		t.Fatalf("native selection PATH lost optional selection: %q", path)
	}
	wantCore := sandboxSH + "/bin"
	if runtime.GOOS == "darwin" {
		wantCore = core
	}
	if !slices.Contains(filepath.SplitList(path), wantCore) {
		t.Fatalf("native selection PATH lost core runtime: %q", path)
	}
}

func TestAdjustPolicyLinuxMapsCoreToBinAndKeepsOptionalSelection(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux core runtime mapping only")
	}
	hostSH := "/home/user/.stella"
	sandboxSH := adjustStellaHome(hostSH)
	optional := hostSH + "/.mise-tools/public/optional"
	core := hostSH + "/core-runtime"
	adjusted := (&Factory{cfg: Config{StellaHome: hostSH}}).adjustPolicy(
		sandboxpkg.Policy{Env: map[string]string{
			"PATH":                           "/usr/bin",
			"MISE_DATA_DIR":                  hostSH + "/.mise-tools",
			"MISE_CONFIG_DIR":                hostSH + "/.mise-tools/config",
			"MISE_YES":                       "1",
			"MISE_NOT_FOUND_AUTO_INSTALL":    "false",
			sandboxpkg.EnvNativeSelectionDir: optional,
			sandboxpkg.EnvCoreRuntimeDir:     core,
		}},
		"/workspace", "/workspace", "", "",
	)
	if got, want := adjusted.Env[sandboxpkg.EnvNativeSelectionDir], sandboxSH+"/.mise-tools/public/optional"; got != want {
		t.Fatalf("optional selection marker = %q, want %q", got, want)
	}
	if got := adjusted.Env[sandboxpkg.EnvCoreRuntimeDir]; got != core {
		t.Fatalf("core marker = %q, want original marker %q", got, core)
	}
	if got := adjusted.Env["MISE_NOT_FOUND_AUTO_INSTALL"]; got != "false" {
		t.Fatalf("MISE_NOT_FOUND_AUTO_INSTALL = %q, want false", got)
	}
	if got, want := adjusted.Env["MISE_CONFIG_DIR"], sandboxSH+"/.mise-tools/config"; got != want {
		t.Fatalf("MISE_CONFIG_DIR = %q, want %q", got, want)
	}
	if got := adjusted.Env["MISE_YES"]; got != "1" {
		t.Fatalf("MISE_YES = %q, want 1", got)
	}
	path := adjusted.Env["PATH"]
	if !strings.HasPrefix(path, sandboxSH+"/.mise-tools/public/optional"+string(filepath.ListSeparator)) {
		t.Fatalf("Linux PATH must lead with optional selection, got %q", path)
	}
	if !slices.Contains(filepath.SplitList(path), sandboxSH+"/bin") {
		t.Fatalf("Linux PATH lost mapped core /bin, got %q", path)
	}
	if !strings.Contains(path, sandboxSH+"/.mise-tools/public/optional") {
		t.Fatalf("Linux PATH lost optional selection, got %q", path)
	}
}

// TestAdjustPolicy_perUserMiseShimsOnPath verifies that when the runtime env
// points MISE_DATA_DIR at a per-user tree, adjustPolicy prepends that tree's
// shims (remapped into the sandbox) onto PATH. The per-user shims are derived
// from the env, not a mise-specific policy field — exercising PerUserMiseDataDir.
func TestAdjustPolicy_perUserMiseShimsOnPath(t *testing.T) {
	hostSH := "/home/user/.stella"
	sandboxSH := adjustStellaHome(hostSH)
	if hostSH == sandboxSH {
		t.Skip("no path remapping on this platform")
	}

	policy := sandboxpkg.Policy{
		Env: map[string]string{"MISE_DATA_DIR": hostSH + "/users/u1/.mise-tools"},
	}
	f := &Factory{cfg: Config{StellaHome: hostSH}}
	sandboxRoot, realRoot := processVisiblePath(sandboxpkg.MountWorkspace, sandboxpkg.MountWorkspace), sandboxpkg.MountWorkspace
	adjusted := f.adjustPolicy(policy, sandboxRoot, realRoot, "", "")

	wantShims := sandboxSH + "/users/u1/.mise-tools/shims"
	if !strings.Contains(adjusted.Env["PATH"], wantShims) {
		t.Fatalf("PATH must include per-user shims %q, got %q", wantShims, adjusted.Env["PATH"])
	}
}

// TestAdjustPolicy_perUserMiseInStellaHomeFrame verifies that the per-user mise
// tree — under the STELLA_HOME frame ($STELLA_HOME/users/{id}/.mise-tools, a
// sibling of the user-data root) — is expressed under the sandbox STELLA_HOME
// (/opt/stella/users/{id}/.mise-tools), sharing the system tree's root so the
// relative seed/shim symlinks resolve. The system config path also stays under
// the sandbox STELLA_HOME, and the host workspace trusted entry collapses onto
// /workspace. This is the production layout (#505): the tree must NOT land in the
// /user frame, which would split it from the system tree across sandbox roots.
func TestAdjustPolicy_perUserMiseInStellaHomeFrame(t *testing.T) {
	hostSH := "/home/user/.stella"
	userHome := hostSH + "/users/u1"
	agentDir := userHome + "/agents/a1"
	userData := userHome + "/data"
	miseHome := userHome + "/.mise-tools" // sibling of data, under STELLA_HOME
	policy := sandboxpkg.Policy{
		Filesystem: sandboxpkg.FilesystemPolicy{WorkingDir: "/workspace", Mounts: []sandboxpkg.Mount{{SandboxPath: "/workspace", Access: sandboxpkg.MountReadWrite}, {SandboxPath: "/user", Access: sandboxpkg.MountReadWrite}}},
		Env: map[string]string{
			"MISE_DATA_DIR":             miseHome,
			"MISE_CACHE_DIR":            miseHome + "/cache",
			"MISE_CONFIG_DIR":           userData + "/.config/mise",
			"MISE_SYSTEM_CONFIG_FILE":   hostSH + "/.mise-tools/configs/_builtin.toml",
			"MISE_GLOBAL_CONFIG_FILE":   userData + "/.config/mise/config.toml",
			"MISE_TRUSTED_CONFIG_PATHS": hostSH + "/.mise-tools/configs/_builtin.toml:" + userData + "/.config/mise:/workspace:" + agentDir,
		},
	}
	f := &Factory{cfg: Config{StellaHome: hostSH}}
	// Drive adjustPolicy with explicit remapping roots so the two-root composition
	// is exercised on every platform, not only where resolve*Root remaps (Linux).
	sandboxRoot, realRoot := "/workspace", agentDir
	userDataSandbox, userDataReal := "/user", userData
	sandboxSH := adjustStellaHome(hostSH)
	adjusted := f.adjustPolicy(policy, sandboxRoot, realRoot, userDataSandbox, userDataReal)

	for _, tc := range []struct{ key, want string }{
		{"MISE_DATA_DIR", sandboxSH + "/users/u1/.mise-tools"},
		{"MISE_CACHE_DIR", sandboxSH + "/users/u1/.mise-tools/cache"},
		{"MISE_CONFIG_DIR", "/user/.config/mise"},
		{"MISE_SYSTEM_CONFIG_FILE", sandboxSH + "/.mise-tools/configs/_builtin.toml"},
		{"MISE_GLOBAL_CONFIG_FILE", "/user/.config/mise/config.toml"},
		{"MISE_TRUSTED_CONFIG_PATHS", sandboxSH + "/.mise-tools/configs/_builtin.toml:/user/.config/mise:/workspace"},
	} {
		if got := adjusted.Env[tc.key]; got != tc.want {
			t.Errorf("env[%s] = %q, want %q", tc.key, got, tc.want)
		}
	}
	if wantShims := sandboxSH + "/users/u1/.mise-tools/shims"; !strings.Contains(adjusted.Env["PATH"], wantShims) {
		t.Errorf("PATH must include STELLA_HOME-frame shims %q, got %q", wantShims, adjusted.Env["PATH"])
	}
	if strings.Contains(adjusted.Env["PATH"], userData+"/") {
		t.Errorf("PATH must not leak the host user-data path %q: %q", userData, adjusted.Env["PATH"])
	}
}

// TestAdjustPolicy_homeAndXDG verifies HOME remains the agent workspace while
// every persistent XDG directory uses the shared per-principal /user root.
func TestAdjustPolicy_homeAndXDG(t *testing.T) {
	root := t.TempDir()
	userData := filepath.Join(root, "data")
	policy := sandboxpkg.Policy{
		Filesystem: sandboxpkg.FilesystemPolicy{
			WorkingDir: filepath.Join(root, "projects", "p"),
			Mounts:     []sandboxpkg.Mount{{SandboxPath: sandboxpkg.MountWorkspace, Access: sandboxpkg.MountReadWrite}, {SandboxPath: sandboxpkg.MountUserData, Access: sandboxpkg.MountReadWrite}},
		},
	}
	f := &Factory{cfg: Config{StellaHome: t.TempDir()}}
	sandboxRoot, realRoot := processVisiblePath(sandboxpkg.MountWorkspace, root), root
	userDataSandbox, userDataReal := processVisiblePath(sandboxpkg.MountUserData, userData), userData
	adjusted := f.adjustPolicy(policy, sandboxRoot, realRoot, userDataSandbox, userDataReal)
	if err := applyFilesystemEnv(&adjusted, sandboxRoot, userDataSandbox, nil); err != nil {
		t.Fatalf("applyFilesystemEnv: %v", err)
	}
	env := adjusted.Env

	for _, tc := range []struct{ key, want string }{
		{"HOME", sandboxRoot},
		{"STELLA_ASSETS_DIR", filepath.Join(userDataSandbox, "assets")},
		{"TMPDIR", filesystemTempDir(nil)},
		{"XDG_CACHE_HOME", filepath.Join(userDataSandbox, ".cache")},
		{"XDG_CONFIG_HOME", filepath.Join(userDataSandbox, ".config")},
		{"XDG_DATA_HOME", filepath.Join(userDataSandbox, ".local", "share")},
		{"XDG_STATE_HOME", filepath.Join(userDataSandbox, ".local", "state")},
	} {
		if env[tc.key] != tc.want {
			t.Errorf("env[%s] = %q, want %q", tc.key, env[tc.key], tc.want)
		}
	}
	if _, ok := env["XDG_RUNTIME_DIR"]; ok {
		t.Error("XDG_RUNTIME_DIR must not be set")
	}
}

// TestAdjustPolicy_noUserDataFallsBackToWorkspace verifies a user-less session
// (no shared user-data root) keeps every XDG directory under HOME, so nothing is
// shared with other agents.
func TestAdjustPolicy_noUserDataFallsBackToWorkspace(t *testing.T) {
	root := t.TempDir()
	policy := sandboxpkg.Policy{
		Filesystem: sandboxpkg.FilesystemPolicy{WorkingDir: root},
	}
	f := &Factory{cfg: Config{StellaHome: t.TempDir()}}
	sandboxRoot, realRoot := processVisiblePath(sandboxpkg.MountWorkspace, root), root
	userDataSandbox, userDataReal := "", ""
	adjusted := f.adjustPolicy(policy, sandboxRoot, realRoot, userDataSandbox, userDataReal)
	if err := applyFilesystemEnv(&adjusted, sandboxRoot, userDataSandbox, nil); err != nil {
		t.Fatalf("applyFilesystemEnv: %v", err)
	}
	env := adjusted.Env

	for _, tc := range []struct{ key, want string }{
		{"HOME", sandboxRoot},
		{"TMPDIR", filesystemTempDir(nil)},
		{"XDG_CACHE_HOME", filepath.Join(sandboxRoot, ".cache")},
		{"XDG_CONFIG_HOME", filepath.Join(sandboxRoot, ".config")},
		{"XDG_DATA_HOME", filepath.Join(sandboxRoot, ".local", "share")},
		{"XDG_STATE_HOME", filepath.Join(sandboxRoot, ".local", "state")},
	} {
		if env[tc.key] != tc.want {
			t.Errorf("env[%s] = %q, want %q", tc.key, env[tc.key], tc.want)
		}
	}
}

// TestResolvePath_twoRoots verifies the host-side path resolver recognizes both
// top-level roots: /workspace maps to the agent dir and /user to the shared
// user-data dir, while an escape to a sibling and a symlink component under /user
// are rejected. Without the /user arm the file tools would refuse /user even
// though bash inside the sandbox can reach it (the critical gap this guards).
func TestResolvePath_twoRoots(t *testing.T) {
	agentReal := t.TempDir()
	userReal := t.TempDir()
	s := &localSession{
		realRoot:        agentReal,
		sandboxRoot:     "/workspace",
		userDataReal:    userReal,
		userDataSandbox: "/user",
		providerMounts: []sessionfs.Mount{
			{HostPath: agentReal, SandboxPath: "/workspace"},
			{HostPath: userReal, SandboxPath: "/user"},
		},
		policy: sandboxpkg.Policy{
			Filesystem: sandboxpkg.FilesystemPolicy{WorkingDir: "/workspace"},
		},
	}

	got, err := s.resolveWritePath("/user/assets/x.txt")
	if err != nil {
		t.Fatalf("ResolveWritePath(/user/...): %v", err)
	}
	if want := filepath.Join(userReal, "assets", "x.txt"); got != want {
		t.Errorf("/user write resolved to %q, want %q", got, want)
	}

	got, err = s.resolveWritePath("/workspace/main.go")
	if err != nil {
		t.Fatalf("ResolveWritePath(/workspace/...): %v", err)
	}
	if want := filepath.Join(agentReal, "main.go"); got != want {
		t.Errorf("/workspace write resolved to %q, want %q", got, want)
	}

	if _, err := s.resolveReadPath("/workspace/../other/secret"); err == nil {
		t.Error("escape from /workspace to a sibling must be rejected")
	}

	// A symlink component under /user must be rejected (no traversal escape).
	if err := os.Symlink(userReal, filepath.Join(userReal, "loop")); err != nil {
		t.Fatal(err)
	}
	s.files = sessionfs.NewAccess(s.pathResolver())
	if _, err := s.Files().ReadFile("/user/loop/x"); err == nil {
		t.Error("symlink component under /user must be rejected")
	}
}

// TestBuildEnv_denyListFiltersVaultKey verifies that STELLA_VAULT_KEY is never
// copied from the host environment into the sandbox env, even when InheritEnv
// is true, while other env vars (e.g. PATH) remain present.
func TestBuildEnv_denyListFiltersVaultKey(t *testing.T) {
	t.Setenv("STELLA_VAULT_KEY", "age-secret-key-1AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA")
	t.Setenv("STELLA_USER_DIR", "/host/stale-user")

	policy := sandboxpkg.Policy{
		Filesystem: sandboxpkg.FilesystemPolicy{
			WorkingDir: t.TempDir(),
		},
		InheritEnv: true,
	}

	env := buildEnv(policy, nil)

	// STELLA_VAULT_KEY must not appear in the sandbox env.
	for _, kv := range env {
		key, _, _ := strings.Cut(kv, "=")
		if key == "STELLA_VAULT_KEY" || key == "STELLA_USER_DIR" {
			t.Fatalf("removed or denied variable %s must not be present in sandbox env, but got: %q", key, kv)
		}
	}

	// At least one other var (PATH) should be present since InheritEnv is true.
	found := false
	for _, kv := range env {
		key, _, _ := strings.Cut(kv, "=")
		if key == "PATH" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected PATH to be present in sandbox env when InheritEnv is true")
	}
}

// TestExec_nonzeroExitCode verifies that Exec returns a non-zero ExitCode for
// failing commands and does not surface it as a Go error.
func TestExec_nonzeroExitCode(t *testing.T) {
	skipIfBwrapNotFunctional(t)
	s := newExecTestSession(t)
	result, err := s.Exec(context.Background(), "exit 42", sandboxpkg.ExecOptions{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.ExitCode != 42 {
		t.Fatalf("expected exit code 42, got %d", result.ExitCode)
	}
}

// TestExec_success verifies that a successful command returns exit code 0 and
// captures stdout correctly.
func TestExec_success(t *testing.T) {
	skipIfBwrapNotFunctional(t)
	s := newExecTestSession(t)
	result, err := s.Exec(context.Background(), "echo hello", sandboxpkg.ExecOptions{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("expected exit code 0, got %d", result.ExitCode)
	}
	if result.Stdout != "hello\n" {
		t.Fatalf("unexpected stdout: %q", result.Stdout)
	}
}

// TestResolvePath_tmpMountAllowed verifies that ResolvePath accepts paths within
// a tmpMount (e.g. /tmp) and translates them to the real host path.
func TestResolvePath_tmpMountAllowed(t *testing.T) {
	realTmpDir := t.TempDir()
	realTmpDir, _ = filepath.EvalSymlinks(realTmpDir)
	root := t.TempDir()
	root, _ = filepath.EvalSymlinks(root)

	f := filepath.Join(realTmpDir, "work", "out.json")
	if err := os.MkdirAll(filepath.Dir(f), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(f, []byte("{}"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	s := &localSession{
		id:          "test",
		policy:      sandboxpkg.Policy{Filesystem: sandboxpkg.FilesystemPolicy{WorkingDir: "/workspace"}},
		realRoot:    root,
		sandboxRoot: "/workspace",
		tmpMounts:   []tmpMount{{sandboxPath: "/tmp", realPath: realTmpDir}},
		done:        make(chan struct{}),
	}

	// Agent path in sandbox space.
	got, err := s.resolveReadPath("/tmp/work/out.json")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != f {
		t.Errorf("ResolvePath = %q, want %q", got, f)
	}
}

// TestResolvePath_varTmpMountAllowed verifies that /var/tmp paths are accepted
// when a tmpMount covers /var/tmp.
func TestResolvePath_varTmpMountAllowed(t *testing.T) {
	realVarTmp := t.TempDir()
	realVarTmp, _ = filepath.EvalSymlinks(realVarTmp)
	root := t.TempDir()
	root, _ = filepath.EvalSymlinks(root)

	f := filepath.Join(realVarTmp, "cache.bin")
	if err := os.WriteFile(f, []byte("data"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	s := &localSession{
		id:          "test",
		policy:      sandboxpkg.Policy{Filesystem: sandboxpkg.FilesystemPolicy{WorkingDir: "/workspace"}},
		realRoot:    root,
		sandboxRoot: "/workspace",
		tmpMounts:   []tmpMount{{sandboxPath: "/var/tmp", realPath: realVarTmp}},
		done:        make(chan struct{}),
	}

	got, err := s.resolveReadPath("/var/tmp/cache.bin")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != f {
		t.Errorf("ResolvePath = %q, want %q", got, f)
	}
}

// TestResolveWritePath_allowsTmp verifies that ResolveWritePath permits paths
// inside tmpMounts (they are writable, not read-only).
func TestResolveWritePath_allowsTmp(t *testing.T) {
	realTmpDir := t.TempDir()
	realTmpDir, _ = filepath.EvalSymlinks(realTmpDir)
	root := t.TempDir()
	root, _ = filepath.EvalSymlinks(root)

	f := filepath.Join(realTmpDir, "out.txt")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	s := &localSession{
		id:          "test",
		policy:      sandboxpkg.Policy{Filesystem: sandboxpkg.FilesystemPolicy{WorkingDir: "/workspace"}},
		realRoot:    root,
		sandboxRoot: "/workspace",
		tmpMounts:   []tmpMount{{sandboxPath: "/tmp", realPath: realTmpDir}},
		done:        make(chan struct{}),
	}

	got, err := s.resolveWritePath("/tmp/out.txt")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != f {
		t.Errorf("ResolveWritePath = %q, want %q", got, f)
	}
}
