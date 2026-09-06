package docker

import (
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"testing"

	sandboxpkg "github.com/CherryHQ/stella/pkg/sandbox"
	"github.com/CherryHQ/stella/plugins/sandbox/docker/dockerclient"
	"github.com/CherryHQ/stella/plugins/sandbox/internal/sessionfs"
)

func TestMergeEnv(t *testing.T) {
	t.Run("nil inputs", func(t *testing.T) {
		got := mergeEnv(nil, nil)
		if len(got) != 0 {
			t.Fatalf("expected empty map, got %v", got)
		}
	})
	t.Run("policy only", func(t *testing.T) {
		got := mergeEnv(map[string]string{"A": "1"}, nil)
		if got["A"] != "1" {
			t.Fatalf("unexpected: %v", got)
		}
	})
	t.Run("opts override policy", func(t *testing.T) {
		got := mergeEnv(map[string]string{"A": "policy"}, map[string]string{"A": "opts"})
		if got["A"] != "opts" {
			t.Fatalf("expected opts to win, got %q", got["A"])
		}
	})
	t.Run("merge both", func(t *testing.T) {
		got := mergeEnv(map[string]string{"A": "1"}, map[string]string{"B": "2"})
		if got["A"] != "1" || got["B"] != "2" {
			t.Fatalf("unexpected: %v", got)
		}
	})
}

func TestWithServerURL(t *testing.T) {
	t.Run("blank url leaves env untouched", func(t *testing.T) {
		in := map[string]string{"A": "1"}
		got := withServerURL(in, "")
		if _, ok := got["STELLA_SERVER_URL"]; ok {
			t.Fatalf("did not expect STELLA_SERVER_URL: %v", got)
		}
	})
	t.Run("sets url and does not mutate input", func(t *testing.T) {
		in := map[string]string{"A": "1"}
		got := withServerURL(in, "http://stella:25678")
		if got["STELLA_SERVER_URL"] != "http://stella:25678" || got["A"] != "1" {
			t.Fatalf("unexpected: %v", got)
		}
		if _, ok := in["STELLA_SERVER_URL"]; ok {
			t.Fatalf("input map was mutated: %v", in)
		}
	})
	t.Run("nil env", func(t *testing.T) {
		got := withServerURL(nil, "http://stella:25678")
		if got["STELLA_SERVER_URL"] != "http://stella:25678" {
			t.Fatalf("unexpected: %v", got)
		}
	})
}

func TestBuildMountTable(t *testing.T) {
	table := buildMountTable(mountTableOptions{
		WorkspaceHost:  "/host/ws",
		WorkspaceMount: "/container/ws",
		Mounts: []sessionfs.Mount{
			{HostPath: "/host/ws", SandboxPath: "/container/ws"},
			{HostPath: "/host/data", SandboxPath: "/user"},
			{HostPath: "/extra/path", SandboxPath: "/extra/path", ReadOnly: true},
		},
		TempHost: "/tmp/user-1",
	})
	if len(table) != 4 {
		t.Fatalf("expected 4 entries, got %d: %+v", len(table), table)
	}
	if table[0].HostPath != "/host/ws" || table[0].ContainerPath != "/container/ws" || table[0].ReadOnly {
		t.Fatalf("unexpected workspace mount: %+v", table[0])
	}
	if table[1].HostPath != "/host/data" || table[1].ContainerPath != "/user" || table[1].ReadOnly {
		t.Fatalf("unexpected user-data mount: %+v", table[1])
	}
	if table[2].HostPath != "/extra/path" || table[2].ContainerPath != "/extra/path" || !table[2].ReadOnly {
		t.Fatalf("unexpected extra mount: %+v", table[2])
	}
	if table[3].HostPath != "/tmp/user-1" || table[3].ContainerPath != "/tmp" || table[3].ReadOnly {
		t.Fatalf("unexpected tmp mount: %+v", table[3])
	}
}

func TestContainerPathNormalizationWithWindowsStylePolicyPaths(t *testing.T) {
	if got, want := cleanContainerPath(`\opt\stella\users\u1\.mise-tools`), "/opt/stella/users/u1/.mise-tools"; got != want {
		t.Fatalf("cleanContainerPath = %q, want %q", got, want)
	}

	policyMounts := normalizeDockerPolicyMounts([]sandboxpkg.Mount{
		{SandboxPath: `\workspace\`, Access: sandboxpkg.MountReadWrite},
		{SandboxPath: `\opt\stella\users\u1\.mise-tools`, Access: sandboxpkg.MountReadWrite},
	})
	providerMounts, err := dockerProviderMounts(policyMounts, map[string]string{
		"/workspace":                       `C:\stella\users\u1`,
		"/opt/stella/users/u1/.mise-tools": `C:\stella\users\u1\.mise-tools`,
	})
	if err != nil {
		t.Fatal(err)
	}
	table := buildMountTable(mountTableOptions{Mounts: providerMounts})
	if got, want := table[0].ContainerPath, "/workspace"; got != want {
		t.Errorf("workspace ContainerPath = %q, want %q", got, want)
	}
	if got, want := table[1].ContainerPath, "/opt/stella/users/u1/.mise-tools"; got != want {
		t.Errorf("mise ContainerPath = %q, want %q", got, want)
	}

	policyMounts = normalizeDockerPolicyMounts([]sandboxpkg.Mount{
		{SandboxPath: `\workspace`, Access: sandboxpkg.MountReadWrite},
		{SandboxPath: `\opt\stella\bin`, Access: sandboxpkg.MountReadOnly},
		{SandboxPath: `\user`, Access: sandboxpkg.MountReadWrite},
	})
	providerMounts, err = dockerProviderMounts(policyMounts, map[string]string{
		"/workspace":      `C:\workspace`,
		"/opt/stella/bin": `C:\stella\bin`,
		"/user":           `C:\user`,
	})
	if err != nil {
		t.Fatal(err)
	}
	mounts := nonWorkspacePolicyMounts(providerMounts)
	if len(mounts) != 1 || mounts[0].SandboxPath != "/user" {
		t.Fatalf("nonWorkspacePolicyMounts = %+v, want only /user", mounts)
	}

	providerMounts, err = dockerProviderMounts(normalizeDockerPolicyMounts([]sandboxpkg.Mount{{
		SandboxPath: `\opt\stella\users\u1\.mise-tools`,
		Access:      sandboxpkg.MountReadWrite,
	}}), map[string]string{"/opt/stella/users/u1/.mise-tools": `C:\stella\users\u1\.mise-tools`})
	if err != nil {
		t.Fatal(err)
	}
	tools := writableToolTrees(providerMounts)
	if len(tools) != 1 || tools[0].Container != "/opt/stella/users/u1/.mise-tools" {
		t.Fatalf("writableToolTrees = %+v, want normalized mise tree", tools)
	}
	if got, want := path.Base(tools[0].Container), ".mise-tools"; got != want {
		t.Errorf("container base = %q, want %q", got, want)
	}
}

func TestMapNetworkMode(t *testing.T) {
	cases := []struct {
		mode sandboxpkg.NetworkMode
		want dockerclient.NetworkMode
	}{
		{sandboxpkg.NetworkDisabled, dockerclient.NetworkDisabled},
		{sandboxpkg.NetworkAllowAll, dockerclient.NetworkAllowAll},
	}
	for _, c := range cases {
		policy := sandboxpkg.Policy{Network: sandboxpkg.NetworkPolicy{Mode: c.mode}}
		got := mapNetworkMode(policy)
		if got != c.want {
			t.Fatalf("mode %v: got %v, want %v", c.mode, got, c.want)
		}
	}
}

func TestInjectToolPaths_PrependedWhenSet(t *testing.T) {
	env := map[string]string{"PATH": "/usr/bin:/bin"}
	paths := []string{"/opt/stella/users/u1/.mise-tools/shims", "/opt/stella/user-tools/bin"}
	got := injectToolPaths(env, paths)
	want := strings.Join(append(append([]string(nil), paths...), "/usr/bin", "/bin"), ":")
	if got["PATH"] != want {
		t.Errorf("PATH = %q, want %q", got["PATH"], want)
	}
	if got[sandboxpkg.EnvRunnerPath] != want {
		t.Errorf("%s = %q, want final PATH %q", sandboxpkg.EnvRunnerPath, got[sandboxpkg.EnvRunnerPath], want)
	}
}

func TestInjectToolPaths_UsesDefaultPathWhenPATHAbsent(t *testing.T) {
	got := injectToolPaths(map[string]string{}, []string{"/opt/stella/user-tools/bin"})
	if got["PATH"] == "" {
		t.Fatal("PATH should not be empty when tool paths are set")
	}
	if got["PATH"][:len("/opt/stella/user-tools/bin:")] != "/opt/stella/user-tools/bin:" {
		t.Errorf("PATH does not start with user tool bin: %q", got["PATH"])
	}
	if len(got["PATH"]) <= len("/opt/stella/user-tools/bin:") {
		t.Error("PATH should include containerDefaultPATH after user tool bin")
	}
}

func TestInjectToolPaths_UsesSelectionLocalShims(t *testing.T) {
	selection := "/opt/stella/.mise-tools/contexts/system-a/shims"
	got := injectToolPaths(map[string]string{}, []string{selection})

	if want := selection + ":" + containerDefaultPATH; got["PATH"] != want {
		t.Fatalf("PATH = %q, want selection-local path followed by system PATH %q", got["PATH"], want)
	}
	if strings.Contains(got["PATH"], "/opt/stella/bin") || strings.Contains(got["PATH"], "/opt/stella/.mise-tools/shims") {
		t.Fatalf("PATH leaked shared Stella paths: %q", got["PATH"])
	}
}

func TestInjectToolPathsExecutesUserSelectionBeforeBundled(t *testing.T) {
	userDir := t.TempDir()
	bundledDir := t.TempDir()
	systemDir := t.TempDir()
	for dir, output := range map[string]string{userDir: "user", bundledDir: "bundled", systemDir: "system"} {
		if err := os.WriteFile(filepath.Join(dir, "same-tool"), []byte("#!/bin/sh\nprintf '%s\\n' "+output+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	env := injectToolPaths(map[string]string{}, []string{userDir, bundledDir, systemDir})
	command := exec.Command("/bin/sh", "-c", "same-tool")
	command.Env = append(os.Environ(), "PATH="+env["PATH"])
	output, err := command.Output()
	if err != nil {
		t.Fatalf("execute selected tool: %v", err)
	}
	if string(output) != "user\n" {
		t.Fatalf("selected tool output = %q, want user precedence", output)
	}
}

func TestInjectToolPaths_SnapshotsPathWhenToolPathsEmpty(t *testing.T) {
	env := map[string]string{"PATH": "/usr/bin:/bin", sandboxpkg.EnvRunnerPath: "/untrusted/bin"}
	got := injectToolPaths(env, nil)
	if got["PATH"] != "/usr/bin:/bin" {
		t.Errorf("PATH changed when tool paths absent: %q", got["PATH"])
	}
	if got[sandboxpkg.EnvRunnerPath] != got["PATH"] {
		t.Errorf("%s = %q, want final PATH %q", sandboxpkg.EnvRunnerPath, got[sandboxpkg.EnvRunnerPath], got["PATH"])
	}
}

func TestDockerExecEnvironmentFiltersAndTranslatesPerCallOverrides(t *testing.T) {
	mountTable := []dockerclient.Mount{
		{HostPath: "/host/workspace", ContainerPath: "/workspace"},
		{HostPath: "/host/tmp", ContainerPath: "/tmp"},
	}
	envMaps := []envPathMap{{HostPrefix: "/host/stella", ContainerPrefix: "/opt/stella"}}
	got := dockerExecEnvironment(
		map[string]string{"HOME": "/workspace", "STELLA_HOME": "/opt/stella", "POLICY": "kept"},
		map[string]string{
			"PATH":        "/host/bin:/usr/bin",
			"TMPDIR":      "/host/tmp/nested",
			"STELLA_HOME": "/host/stella/revision",
			"LITERAL":     "/host/workspace/not-a-declared-path",
		},
		mountTable,
		envMaps,
		nil,
	)
	for key, want := range map[string]string{
		"HOME":        "/workspace",
		"TMPDIR":      "/tmp/nested",
		"STELLA_HOME": "/opt/stella/revision",
		"LITERAL":     "/host/workspace/not-a-declared-path",
		"POLICY":      "kept",
	} {
		if got[key] != want {
			t.Errorf("%s = %q, want %q", key, got[key], want)
		}
	}
	if got["PATH"] != containerDefaultPATH {
		t.Fatalf("PATH = %q, want container default %q", got["PATH"], containerDefaultPATH)
	}
	if got[sandboxpkg.EnvRunnerPath] != got["PATH"] {
		t.Fatalf("%s = %q, want final PATH %q", sandboxpkg.EnvRunnerPath, got[sandboxpkg.EnvRunnerPath], got["PATH"])
	}
}

func TestPrepareSessionTempDir(t *testing.T) {
	stellaHome := t.TempDir()
	for _, tt := range []struct {
		name string
		cfg  Config
		want string
	}{
		{name: "host", cfg: Config{RuntimeMode: DockerSandboxModeHost, StellaHome: stellaHome}, want: filepath.Join(stellaHome, "cache", "sandbox-tmp", "sandbox-test-host")},
		{name: "bind", cfg: Config{RuntimeMode: DockerSandboxModeBind, StellaHome: stellaHome, ContainerPathPrefix: stellaHome, HostPathPrefix: "/daemon/stella"}, want: filepath.Join(stellaHome, "cache", "sandbox-tmp", "sandbox-test-bind")},
		{name: "volume", cfg: Config{RuntimeMode: DockerSandboxModeVolume, StellaHome: stellaHome, StellaHomeVolume: "stella-data"}, want: filepath.Join(stellaHome, "cache", "sandbox-tmp", "sandbox-test-volume")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := &dockerFactory{cfg: tt.cfg}
			tempDir, err := f.prepareSessionTempDir("sandbox-test-" + tt.name)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(tempDir) })
			if tempDir != tt.want {
				t.Fatalf("temp dir = %q, want %q", tempDir, tt.want)
			}
			info, err := os.Stat(tempDir)
			if err != nil {
				t.Fatal(err)
			}
			if got := info.Mode().Perm(); got != 0o777 || info.Mode()&os.ModeSticky == 0 {
				t.Errorf("temp mode = %v, want sticky 0777", info.Mode())
			}
			parent, err := os.Stat(filepath.Dir(tempDir))
			if err != nil {
				t.Fatal(err)
			}
			if got := parent.Mode().Perm(); got != 0o700 {
				t.Errorf("temp parent mode = %#o, want 0700", got)
			}
		})
	}
}

func TestConfigureSessionMounts_HostMode(t *testing.T) {
	stellaHome, workspace, extra, tmp := dockerModeTestDirs(t)
	f := &dockerFactory{cfg: Config{RuntimeMode: DockerSandboxModeHost, StellaHome: stellaHome}}
	opts := dockerModeCreateOptions(workspace)
	mountedExtra, mountedTmp, _, err := f.configureSessionMounts(&opts, dockerModePolicy(stellaHome, workspace, extra, tmp), workspace, "", tmp)
	if err != nil {
		t.Fatalf("configureSessionMounts: %v", err)
	}
	if opts.WorkspaceHost != workspace {
		t.Fatalf("WorkspaceHost = %q, want %q", opts.WorkspaceHost, workspace)
	}
	if mountedTmp != tmp {
		t.Fatalf("mounted tmp = %q, want %q", mountedTmp, tmp)
	}
	if len(mountedExtra) < 2 || mountedExtra[1].HostPath != extra {
		t.Fatalf("mounted extra = %v, want [%q]", mountedExtra, extra)
	}
	assertNoMountTo(t, opts.ExtraMounts, filepath.Join(stellaHomeMount, "bin"))
	assertMount(t, opts.ExtraMounts, tmp, "/tmp", false, dockerclient.MountType(""), "")
	assertMount(t, opts.ExtraMounts, extra, "/readonly/skills", true, dockerclient.MountType(""), "")
}

func TestConfigureSessionMounts_BindModeTranslatesSources(t *testing.T) {
	stellaHome, workspace, extra, tmp := dockerModeTestDirs(t)
	f := &dockerFactory{cfg: Config{
		RuntimeMode:         DockerSandboxModeBind,
		StellaHome:          stellaHome,
		ContainerPathPrefix: stellaHome,
		HostPathPrefix:      "/daemon/stella",
	}}
	opts := dockerModeCreateOptions(workspace)
	policy := dockerModePolicy(stellaHome, workspace, extra, tmp)
	tempDir := preparedDockerTemp(t, f)
	mountedExtra, mountedTmp, _, err := f.configureSessionMounts(&opts, policy, workspace, "", tempDir)
	if err != nil {
		t.Fatalf("configureSessionMounts: %v", err)
	}
	if opts.WorkspaceHost != "/daemon/stella/users/user" {
		t.Fatalf("WorkspaceHost = %q", opts.WorkspaceHost)
	}
	if mountedTmp != tempDir {
		t.Fatalf("mounted tmp = %q, want session temp %q", mountedTmp, tempDir)
	}
	assertMount(t, opts.ExtraMounts, "/daemon/stella/cache/sandbox-tmp/sandbox-test", "/tmp", false, dockerclient.MountType(""), "")
	if len(mountedExtra) < 2 || mountedExtra[1].HostPath != extra {
		t.Fatalf("mounted extra = %v, want [%q]", mountedExtra, extra)
	}
	assertNoMountTo(t, opts.ExtraMounts, filepath.Join(stellaHomeMount, "bin"))
	assertMount(t, opts.ExtraMounts, "/daemon/stella/shared/skills", "/readonly/skills", true, dockerclient.MountType(""), "")
}

func TestConfigureSessionMounts_VolumeModeUsesSubpaths(t *testing.T) {
	stellaHome, workspace, extra, tmp := dockerModeTestDirs(t)
	outsideExtra := t.TempDir()
	f := &dockerFactory{cfg: Config{RuntimeMode: DockerSandboxModeVolume, StellaHome: stellaHome, StellaHomeVolume: "stella-data"}}
	opts := dockerModeCreateOptions(workspace)
	mounts := dockerModePolicy(stellaHome, workspace, extra, tmp)
	mounts = append(mounts, sessionfs.Mount{HostPath: outsideExtra, SandboxPath: outsideExtra, ReadOnly: true})
	tempDir := preparedDockerTemp(t, f)
	mountedExtra, mountedTmp, _, err := f.configureSessionMounts(&opts, mounts, workspace, "", tempDir)
	if err != nil {
		t.Fatalf("configureSessionMounts: %v", err)
	}
	if opts.WorkspaceHost != "" {
		t.Fatalf("WorkspaceHost = %q, want empty in volume mode", opts.WorkspaceHost)
	}
	if mountedTmp != tempDir {
		t.Fatalf("mounted tmp = %q, want session temp %q", mountedTmp, tempDir)
	}
	assertMount(t, opts.ExtraMounts, "stella-data", "/tmp", false, dockerclient.MountTypeVolume, "cache/sandbox-tmp/sandbox-test")
	if len(mountedExtra) < 2 || mountedExtra[1].HostPath != extra {
		t.Fatalf("mounted extra = %v, want only [%q]", mountedExtra, extra)
	}
	assertMount(t, opts.ExtraMounts, "stella-data", workspaceMount, false, dockerclient.MountTypeVolume, "users/user")
	assertNoMountTo(t, opts.ExtraMounts, filepath.Join(stellaHomeMount, "bin"))
	assertMount(t, opts.ExtraMounts, "stella-data", "/readonly/skills", true, dockerclient.MountTypeVolume, "shared/skills")
}

func TestConfigureSessionMounts_VolumeModeRejectsStellaHomeAsWorkspace(t *testing.T) {
	stellaHome, _, extra, tmp := dockerModeTestDirs(t)
	f := &dockerFactory{cfg: Config{RuntimeMode: DockerSandboxModeVolume, StellaHome: stellaHome, StellaHomeVolume: "stella-data"}}
	opts := dockerModeCreateOptions(stellaHome)
	_, _, _, err := f.configureSessionMounts(&opts, dockerModePolicy(stellaHome, stellaHome, extra, tmp), stellaHome, "", preparedDockerTemp(t, f))
	if err == nil {
		t.Fatal("expected error when volume workspace is STELLA_HOME itself")
	}
	if !strings.Contains(err.Error(), "not STELLA_HOME itself") {
		t.Fatalf("error = %v", err)
	}
}

func TestConfigureSessionMounts_RunnerScratchAcceptedByDockerModes(t *testing.T) {
	stellaHome := t.TempDir()
	workspace := filepath.Join(stellaHome, "runner-scratch", "runner-123")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	mounts := []sessionfs.Mount{{HostPath: workspace, SandboxPath: workspaceMount}}

	t.Run("bind", func(t *testing.T) {
		f := &dockerFactory{cfg: Config{RuntimeMode: DockerSandboxModeBind, StellaHome: stellaHome, ContainerPathPrefix: stellaHome, HostPathPrefix: "/daemon/stella"}}
		opts := dockerModeCreateOptions(workspace)
		if _, _, _, err := f.configureSessionMounts(&opts, mounts, workspace, "", ""); err != nil {
			t.Fatalf("runner scratch bind planning: %v", err)
		}
		if opts.WorkspaceHost != "/daemon/stella/runner-scratch/runner-123" {
			t.Fatalf("WorkspaceHost = %q", opts.WorkspaceHost)
		}
	})

	t.Run("volume", func(t *testing.T) {
		f := &dockerFactory{cfg: Config{RuntimeMode: DockerSandboxModeVolume, StellaHome: stellaHome, StellaHomeVolume: "stella-data"}}
		opts := dockerModeCreateOptions(workspace)
		if _, _, _, err := f.configureSessionMounts(&opts, mounts, workspace, "", ""); err != nil {
			t.Fatalf("runner scratch volume planning: %v", err)
		}
		assertMount(t, opts.ExtraMounts, "stella-data", workspaceMount, false, dockerclient.MountTypeVolume, "runner-scratch/runner-123")
	})
}

func TestConfigureSessionMounts_DoesNotMountHostBuiltinBundle(t *testing.T) {
	stellaHome, workspace, extra, tmp := dockerModeTestDirs(t)
	builtin := filepath.Join(stellaHome, "bundles", "revision")
	if err := os.MkdirAll(builtin, 0o755); err != nil {
		t.Fatal(err)
	}
	f := &dockerFactory{cfg: Config{RuntimeMode: DockerSandboxModeHost, StellaHome: stellaHome}}
	opts := dockerModeCreateOptions(workspace)
	mounts := dockerModePolicy(stellaHome, workspace, extra, tmp)
	mounts = append(mounts, sessionfs.Mount{HostPath: builtin, SandboxPath: sandboxpkg.MountBuiltinSkills, ReadOnly: true})
	_, _, _, err := f.configureSessionMounts(&opts, mounts, workspace, "", tmp)
	if err != nil {
		t.Fatalf("configureSessionMounts: %v", err)
	}
	assertNoMountTo(t, opts.ExtraMounts, filepath.Join(stellaHomeMount, "bin"))
	assertNoMountTo(t, opts.ExtraMounts, sandboxpkg.MountBuiltinSkills)
}

// TestConfigureSessionMounts_UserDataRoot verifies the shared user-data root is
// mounted RW at /user — bind mode translates the daemon source, volume mode uses
// the STELLA_HOME-relative subpath.
func TestConfigureSessionMounts_UserDataRoot(t *testing.T) {
	stellaHome, workspace, extra, tmp := dockerModeTestDirs(t)
	userData := filepath.Join(stellaHome, "users", "user", "data")
	if err := os.MkdirAll(userData, 0o755); err != nil {
		t.Fatal(err)
	}

	t.Run("bind", func(t *testing.T) {
		f := &dockerFactory{cfg: Config{
			RuntimeMode:         DockerSandboxModeBind,
			StellaHome:          stellaHome,
			ContainerPathPrefix: stellaHome,
			HostPathPrefix:      "/daemon/stella",
		}}
		opts := dockerModeCreateOptions(workspace)
		if _, _, _, err := f.configureSessionMounts(&opts, dockerModePolicy(stellaHome, workspace, extra, tmp), workspace, userData, tmp); err != nil {
			t.Fatalf("configureSessionMounts: %v", err)
		}
		assertMount(t, opts.ExtraMounts, "/daemon/stella/users/user/data", userDataMount, false, dockerclient.MountType(""), "")
	})

	t.Run("volume", func(t *testing.T) {
		f := &dockerFactory{cfg: Config{RuntimeMode: DockerSandboxModeVolume, StellaHome: stellaHome, StellaHomeVolume: "stella-data"}}
		opts := dockerModeCreateOptions(workspace)
		policy := dockerModePolicy(stellaHome, workspace, extra, tmp)
		if _, _, _, err := f.configureSessionMounts(&opts, policy, workspace, userData, preparedDockerTemp(t, f)); err != nil {
			t.Fatalf("configureSessionMounts: %v", err)
		}
		assertMount(t, opts.ExtraMounts, "stella-data", userDataMount, false, dockerclient.MountTypeVolume, "users/user/data")
	})
}

func TestConfigureSessionMounts_WritableMiseTree(t *testing.T) {
	stellaHome, workspace, extra, tmp := dockerModeTestDirs(t)
	miseDir := filepath.Join(stellaHome, "users", "user", ".mise-tools")
	if err := os.MkdirAll(miseDir, 0o700); err != nil {
		t.Fatal(err)
	}
	containerPath := filepath.Join(stellaHomeMount, "users", "user", ".mise-tools")
	for _, tc := range []struct {
		name       string
		factory    *dockerFactory
		wantSource string
		wantType   dockerclient.MountType
		wantSub    string
	}{
		{name: "bind", factory: &dockerFactory{cfg: Config{RuntimeMode: DockerSandboxModeBind, StellaHome: stellaHome, ContainerPathPrefix: stellaHome, HostPathPrefix: "/daemon/stella"}}, wantSource: "/daemon/stella/users/user/.mise-tools"},
		{name: "volume", factory: &dockerFactory{cfg: Config{RuntimeMode: DockerSandboxModeVolume, StellaHome: stellaHome, StellaHomeVolume: "stella-data"}}, wantSource: "stella-data", wantType: dockerclient.MountTypeVolume, wantSub: "users/user/.mise-tools"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mounts := dockerModePolicy(stellaHome, workspace, extra, tmp)
			mounts = append(mounts, sessionfs.Mount{HostPath: miseDir, SandboxPath: containerPath})
			tempDir := tmp
			if tc.factory.cfg.RuntimeMode == DockerSandboxModeVolume {
				tempDir = preparedDockerTemp(t, tc.factory)
			}
			opts := dockerModeCreateOptions(workspace)
			if _, _, _, err := tc.factory.configureSessionMounts(&opts, mounts, workspace, "", tempDir); err != nil {
				t.Fatal(err)
			}
			assertMount(t, opts.ExtraMounts, tc.wantSource, containerPath, false, tc.wantType, tc.wantSub)
		})
	}
}

// TestTranslateEnvPaths_Mise verifies system, principal-global, and workspace
// mise paths are translated into their distinct container roots and trusted-path
// aliases are deduped.
func TestTranslateEnvPaths_Mise(t *testing.T) {
	stellaHome := "/host/.stella"
	userConfigDir := "/host/data/.config/mise"
	sep := string(filepath.ListSeparator)
	mountTable := []dockerclient.Mount{
		{HostPath: "/host/ws", ContainerPath: "/workspace"},
		{HostPath: "/host/data", ContainerPath: "/user"},
	}
	envMaps := []envPathMap{{HostPrefix: stellaHome, ContainerPrefix: stellaHomeMount}}
	env := map[string]string{
		"BASH_ENV":                  stellaHome + "/bin/.stella-shell-env",
		"MISE_DATA_DIR":             stellaHome + "/users/u1/.mise-tools",
		"MISE_CONFIG_DIR":           userConfigDir,
		"MISE_SYSTEM_CONFIG_FILE":   stellaHome + "/.mise-tools/configs/_builtin.toml",
		"MISE_GLOBAL_CONFIG_FILE":   userConfigDir + "/config.toml",
		"MISE_TRUSTED_CONFIG_PATHS": strings.Join([]string{stellaHome + "/.mise-tools/configs/_builtin.toml", userConfigDir, "/workspace", "/host/ws"}, sep),
		"PATH":                      "/host/leak",
	}
	out := translateEnvPaths(env, mountTable, envMaps)

	if got, want := out["BASH_ENV"], stellaHomeMount+"/bin/.stella-shell-env"; got != want {
		t.Errorf("BASH_ENV = %q, want %q", got, want)
	}
	if got, want := out["MISE_DATA_DIR"], stellaHomeMount+"/users/u1/.mise-tools"; got != want {
		t.Errorf("MISE_DATA_DIR = %q, want %q", got, want)
	}
	if got, want := out["MISE_SYSTEM_CONFIG_FILE"], stellaHomeMount+"/.mise-tools/configs/_builtin.toml"; got != want {
		t.Errorf("MISE_SYSTEM_CONFIG_FILE = %q, want %q", got, want)
	}
	if got, want := out["MISE_GLOBAL_CONFIG_FILE"], "/user/.config/mise/config.toml"; got != want {
		t.Errorf("MISE_GLOBAL_CONFIG_FILE = %q, want %q", got, want)
	}
	wantTrusted := strings.Join([]string{stellaHomeMount + "/.mise-tools/configs/_builtin.toml", "/user/.config/mise", "/workspace"}, ":")
	if got := out["MISE_TRUSTED_CONFIG_PATHS"]; got != wantTrusted {
		t.Errorf("MISE_TRUSTED_CONFIG_PATHS = %q, want %q", got, wantTrusted)
	}
	if _, ok := out["PATH"]; ok {
		t.Error("PATH must be dropped (image-baked PATH wins)")
	}
}

func TestTranslateEnvPathsRejectsAmbiguousHostAndContainerCoordinate(t *testing.T) {
	mountTable := []dockerclient.Mount{
		{HostPath: "/workspace", ContainerPath: "/other"},
		{HostPath: "/host/data", ContainerPath: "/workspace"},
	}

	out := translateEnvPaths(map[string]string{
		sandboxpkg.EnvHome:          "/workspace",
		sandboxpkg.EnvXDGConfigHome: "/host/data/.config",
	}, mountTable, nil)
	if _, ok := out[sandboxpkg.EnvHome]; ok {
		t.Fatalf("ambiguous HOME was translated to %q", out[sandboxpkg.EnvHome])
	}
	if got, want := out[sandboxpkg.EnvXDGConfigHome], "/workspace/.config"; got != want {
		t.Fatalf("unambiguous XDG_CONFIG_HOME = %q, want %q", got, want)
	}
}

func TestApplyFilesystemEnvUsesMountedUserDataOrWorkspace(t *testing.T) {
	for _, tc := range []struct {
		name     string
		userData string
		root     string
		tmpDir   string
	}{
		{name: "mounted principal data", userData: "/host/data", root: "/host/data", tmpDir: "/host/tmp/principal"},
		{name: "mounted group data", userData: "/host/data/group-g1", root: "/host/data/group-g1", tmpDir: "/host/tmp/group-g1"},
		{name: "no user-data mount", root: "/host/workspace", tmpDir: "/tmp"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := map[string]string{sandboxpkg.EnvXDGRuntimeDir: "/run/user/1000"}
			view := sandboxpkg.FilesystemView{Home: "/host/workspace", SharedDataDir: tc.userData, TempDir: tc.tmpDir}
			if err := sandboxpkg.ApplyFilesystemEnv(env, view); err != nil {
				t.Fatalf("ApplyFilesystemEnv: %v", err)
			}
			for key, want := range map[string]string{
				sandboxpkg.EnvHome:          "/host/workspace",
				sandboxpkg.EnvTempDir:       tc.tmpDir,
				sandboxpkg.EnvXDGConfigHome: filepath.Join(tc.root, ".config"),
				sandboxpkg.EnvXDGDataHome:   filepath.Join(tc.root, ".local", "share"),
				sandboxpkg.EnvXDGStateHome:  filepath.Join(tc.root, ".local", "state"),
				sandboxpkg.EnvXDGCacheHome:  filepath.Join(tc.root, ".cache"),
			} {
				if got := env[key]; got != want {
					t.Errorf("%s = %q, want %q", key, got, want)
				}
			}
			if tc.userData == "" {
				if _, ok := env[sandboxpkg.EnvStellaAssetsDir]; ok {
					t.Errorf("%s must not be set", sandboxpkg.EnvStellaAssetsDir)
				}
			} else if got, want := env[sandboxpkg.EnvStellaAssetsDir], filepath.Join(tc.userData, "assets"); got != want {
				t.Errorf("STELLA_ASSETS_DIR = %q, want %q", got, want)
			}
			if _, ok := env[sandboxpkg.EnvXDGRuntimeDir]; ok {
				t.Error("XDG_RUNTIME_DIR must not be set")
			}
		})
	}
}

func TestApplyDockerFilesystemEnvRequiresMountedTempDir(t *testing.T) {
	env := map[string]string{sandboxpkg.EnvTempDir: "/stale/tmp"}
	if err := applyDockerFilesystemEnv(env, false, false); err == nil {
		t.Fatal("applyDockerFilesystemEnv accepted an unmounted TMPDIR")
	}
}

func TestApplyDockerFilesystemEnvWithoutUserDataUsesMountedFallbackTemp(t *testing.T) {
	env := map[string]string{
		"STELLA_USER_DIR":             "/stale/user",
		sandboxpkg.EnvStellaAssetsDir: "/stale/user/assets",
	}
	if err := applyDockerFilesystemEnv(env, false, true); err != nil {
		t.Fatalf("applyDockerFilesystemEnv: %v", err)
	}
	for _, key := range []string{"STELLA_USER_DIR", sandboxpkg.EnvStellaAssetsDir} {
		if _, ok := env[key]; ok {
			t.Errorf("missing user mount must clear %s", key)
		}
	}
	if got, want := env[sandboxpkg.EnvHome], workspaceMount; got != want {
		t.Errorf("HOME = %q, want %q", got, want)
	}
	if got, want := env[sandboxpkg.EnvTempDir], "/tmp"; got != want {
		t.Errorf("container TMPDIR = %q, want %q", got, want)
	}
}

func TestDockerFilesystemEnvCreateAndExecCoordinatesMatch(t *testing.T) {
	policyEnv := map[string]string{"PERSISTENT_VALUE": "policy"}
	if err := applyDockerFilesystemEnv(policyEnv, true, true); err != nil {
		t.Fatalf("applyDockerFilesystemEnv: %v", err)
	}

	// Container creation has no per-call overrides or injected tool path.
	createEnv := mergeEnv(policyEnv, nil)

	// Exec independently merges request-scoped env, then adds its container-native
	// tool path. This must not alter filesystem coordinates.
	execEnv := injectToolPaths(
		mergeEnv(policyEnv, map[string]string{"REQUEST_VALUE": "exec"}),
		[]string{"/tools/request/bin"},
	)

	for _, key := range []string{
		sandboxpkg.EnvHome,
		sandboxpkg.EnvStellaAssetsDir,
		sandboxpkg.EnvTempDir,
		sandboxpkg.EnvXDGConfigHome,
		sandboxpkg.EnvXDGDataHome,
		sandboxpkg.EnvXDGStateHome,
		sandboxpkg.EnvXDGCacheHome,
	} {
		if got, want := execEnv[key], createEnv[key]; got != want {
			t.Errorf("%s differs between create and exec: got %q, want %q", key, got, want)
		}
	}
	if _, ok := createEnv[sandboxpkg.EnvXDGRuntimeDir]; ok {
		t.Error("create environment must not set XDG_RUNTIME_DIR")
	}
	if _, ok := execEnv[sandboxpkg.EnvXDGRuntimeDir]; ok {
		t.Error("exec environment must not set XDG_RUNTIME_DIR")
	}
	for key, want := range map[string]string{
		sandboxpkg.EnvHome:            workspaceMount,
		sandboxpkg.EnvStellaAssetsDir: userDataMount + "/assets",
		sandboxpkg.EnvTempDir:         "/tmp",
		sandboxpkg.EnvXDGConfigHome:   userDataMount + "/.config",
		sandboxpkg.EnvXDGDataHome:     userDataMount + "/.local/share",
		sandboxpkg.EnvXDGStateHome:    userDataMount + "/.local/state",
		sandboxpkg.EnvXDGCacheHome:    userDataMount + "/.cache",
	} {
		if got := createEnv[key]; got != want {
			t.Errorf("create %s = %q, want %q", key, got, want)
		}
	}
	if got := createEnv["PERSISTENT_VALUE"]; got != "policy" {
		t.Errorf("create PERSISTENT_VALUE = %q, want policy", got)
	}
	if _, ok := createEnv["REQUEST_VALUE"]; ok {
		t.Error("create environment must not include request override")
	}
	if got := execEnv["REQUEST_VALUE"]; got != "exec" {
		t.Errorf("exec REQUEST_VALUE = %q, want exec", got)
	}
	if got, want := execEnv["PATH"], "/tools/request/bin:"+containerDefaultPATH; got != want {
		t.Errorf("exec PATH = %q, want %q", got, want)
	}
	if _, ok := createEnv["PATH"]; ok {
		t.Error("create environment must not inject exec tool PATH")
	}
}

func dockerModeTestDirs(t *testing.T) (stellaHome, workspace, extra, tmp string) {
	t.Helper()
	stellaHome = t.TempDir()
	workspace = filepath.Join(stellaHome, "users", "user")
	extra = filepath.Join(stellaHome, "shared", "skills")
	tmp = t.TempDir()
	for _, dir := range []string{filepath.Join(stellaHome, "bin"), workspace, extra} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return stellaHome, workspace, extra, tmp
}

func dockerModeCreateOptions(workspace string) dockerclient.CreateOptions {
	return dockerclient.CreateOptions{WorkspaceHost: workspace, WorkspaceMount: workspaceMount}
}

func preparedDockerTemp(t *testing.T, factory *dockerFactory) string {
	t.Helper()
	tempDir, err := factory.prepareSessionTempDir("sandbox-test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(tempDir) })
	return tempDir
}

func dockerModePolicy(stellaHome, workspace, extra, _ string) []sessionfs.Mount {
	return []sessionfs.Mount{{HostPath: workspace, SandboxPath: workspaceMount}, {HostPath: extra, SandboxPath: "/readonly/skills", ReadOnly: true}, {HostPath: filepath.Join(stellaHome, ".agents", "skills"), SandboxPath: filepath.Join(stellaHomeMount, ".agents", "skills"), ReadOnly: true}}
}

func assertNoMountTo(t *testing.T, mounts []dockerclient.Mount, target string) {
	t.Helper()
	for _, m := range mounts {
		if m.ContainerPath == target {
			t.Fatalf("expected no mount to %q, found %+v", target, m)
		}
	}
}

func assertMount(t *testing.T, mounts []dockerclient.Mount, source, target string, readOnly bool, mountType dockerclient.MountType, volumeSubpath string) {
	t.Helper()
	for _, m := range mounts {
		if m.HostPath == source && m.ContainerPath == target {
			if m.ReadOnly != readOnly || m.Type != mountType || m.VolumeSubpath != volumeSubpath {
				t.Fatalf("mount %+v flags mismatch; want readOnly=%v type=%q subpath=%q", m, readOnly, mountType, volumeSubpath)
			}
			return
		}
	}
	t.Fatalf("mount %q -> %q not found in %+v", source, target, mounts)
}
