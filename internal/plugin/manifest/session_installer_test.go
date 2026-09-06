package manifest

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/CherryHQ/stella/internal/plugin"
	pkgplugins "github.com/CherryHQ/stella/pkg/plugins"
	pkgsandbox "github.com/CherryHQ/stella/pkg/sandbox"
	nonebackend "github.com/CherryHQ/stella/plugins/sandbox/none"
)

func TestContextBinaryInstallPlanIsolatedBySelectionIdentity(t *testing.T) {
	stellaHome := t.TempDir()
	base := pkgplugins.PluginBinarySpec{
		PluginResourceIdentity: pkgplugins.PluginResourceIdentity{
			PluginID: "tool/one", ConfigID: "cfg-one", Scope: string(plugin.ScopeSystem), Revision: 4,
		},
		Name: "one", Tool: "github:owner/one", Version: "1.0.0",
	}
	other := base
	other.ConfigID = "cfg-two"
	other.Scope = string(plugin.ScopeSystemAgent)
	other.Revision = 7
	other.Version = "2.0.0"

	first, err := ContextBinaryInstallPlan(stellaHome, []pkgplugins.PluginBinarySpec{base})
	if err != nil {
		t.Fatalf("ContextBinaryInstallPlan(first): %v", err)
	}
	second, err := ContextBinaryInstallPlan(stellaHome, []pkgplugins.PluginBinarySpec{other})
	if err != nil {
		t.Fatalf("ContextBinaryInstallPlan(second): %v", err)
	}
	if first.Identity == second.Identity || first.PublicDir == second.PublicDir || first.PublicBinDir == second.PublicBinDir {
		t.Fatalf("selection identities must isolate public selections: first=%+v second=%+v", first, second)
	}
	if !strings.HasPrefix(first.PublicDir, filepath.Join(stellaHome, ".mise-tools", "public")) {
		t.Fatalf("public selection escaped managed public root: %q", first.PublicDir)
	}
}

func TestContextBinaryInstallPlanRejectsPathTraversal(t *testing.T) {
	spec := pkgplugins.PluginBinarySpec{
		PluginResourceIdentity: pkgplugins.PluginResourceIdentity{
			PluginID: "tool/evil", ConfigID: "cfg", Scope: string(plugin.ScopeSystem), Revision: 1,
		},
		Name: "../../victim", Tool: "github:owner/tool",
	}
	victim := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(victim, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ContextBinaryInstallPlan(filepath.Dir(victim), []pkgplugins.PluginBinarySpec{spec}); err == nil {
		t.Fatal("path traversal name must be rejected before publication")
	}
	data, err := os.ReadFile(victim)
	if err != nil || string(data) != "keep" {
		t.Fatalf("path traversal touched external victim: data=%q err=%v", data, err)
	}
}

func TestContextBinaryInstallPlanRejectsNonPositiveRevision(t *testing.T) {
	base := pkgplugins.PluginBinarySpec{
		PluginResourceIdentity: pkgplugins.PluginResourceIdentity{
			PluginID: "tool/one", ConfigID: "cfg-one", Scope: string(plugin.ScopeSystem),
		},
		Name: "one", Tool: "github:owner/one", Version: "1.0.0",
	}
	for _, revision := range []int64{0, -1} {
		t.Run(fmt.Sprintf("revision_%d", revision), func(t *testing.T) {
			spec := base
			spec.Revision = revision
			if _, err := ContextBinaryInstallPlan(t.TempDir(), []pkgplugins.PluginBinarySpec{spec}); err == nil || !strings.Contains(err.Error(), "non-positive config revision") {
				t.Fatalf("ContextBinaryInstallPlan() error = %v, want non-positive revision", err)
			}
		})
	}
}

func TestOverlayBinaryInstallPlanKeepsLayersAndRunnerPathSeparate(t *testing.T) {
	plan := BinaryInstallPlan{ConfigPath: "/opt/stella/.mise-tools/contexts/a/config.toml", ShimsDir: "/opt/stella/.mise-tools/contexts/a/shims"}
	base := map[string]string{
		"PATH":                      "/opt/stella/bin:/usr/bin",
		"MISE_SYSTEM_CONFIG_FILE":   "/opt/stella/.mise-tools/_builtin.toml",
		"MISE_GLOBAL_CONFIG_FILE":   "/user/.config/mise/config.toml",
		"MISE_TRUSTED_CONFIG_PATHS": "/opt/stella/.mise-tools/_builtin.toml:/user/.config/mise",
		"SECRET":                    "preserved",
	}
	system := OverlayBinaryInstallPlan(base, plan, BinarySystemLayer)
	if system["MISE_SYSTEM_CONFIG_FILE"] != plan.ConfigPath || system["MISE_GLOBAL_CONFIG_FILE"] != base["MISE_GLOBAL_CONFIG_FILE"] {
		t.Fatalf("system layer changed wrong config layer: %#v", system)
	}
	if system["PATH"] != plan.ShimsDir+string(filepath.ListSeparator)+base["PATH"] || system[pkgsandbox.EnvRunnerPath] != system["PATH"] {
		t.Fatalf("system layer PATH = %q, runner path = %q", system["PATH"], system[pkgsandbox.EnvRunnerPath])
	}
	if system["SECRET"] != "preserved" {
		t.Fatal("overlay copied unrelated env incorrectly")
	}
	user := OverlayBinaryInstallPlan(base, plan, BinaryUserLayer)
	if user["MISE_GLOBAL_CONFIG_FILE"] != plan.ConfigPath || user["MISE_SYSTEM_CONFIG_FILE"] != base["MISE_SYSTEM_CONFIG_FILE"] {
		t.Fatalf("user layer changed wrong config layer: %#v", user)
	}
}

func TestOverlayNativeSelectionDropsSharedMisePaths(t *testing.T) {
	plan := BinaryInstallPlan{PublicDir: "/opt/stella/.mise-tools/public/selection", PublicBinDir: "/opt/stella/.mise-tools/public/selection"}
	base := map[string]string{
		"PATH":                      "/usr/bin",
		"MISE_DATA_DIR":             "/opt/stella/.mise-tools",
		"MISE_CONFIG_DIR":           "/opt/stella/.mise-tools/config",
		"MISE_CACHE_DIR":            "/opt/stella/.mise-tools/cache",
		"MISE_STATE_DIR":            "/opt/stella/.mise-tools/state",
		"MISE_GLOBAL_CONFIG_FILE":   "/opt/stella/.mise-tools/config.toml",
		"MISE_TRUSTED_CONFIG_PATHS": "/opt/stella/.mise-tools/config.toml",
	}
	env := OverlayBinaryInstallPlan(base, plan, BinarySystemLayer)
	for _, key := range []string{"MISE_DATA_DIR", "MISE_CONFIG_DIR", "MISE_CACHE_DIR", "MISE_STATE_DIR", "MISE_GLOBAL_CONFIG_FILE", "MISE_TRUSTED_CONFIG_PATHS"} {
		if _, ok := env[key]; ok {
			t.Fatalf("native selection retained shared mise path %s=%q", key, env[key])
		}
	}
	if !strings.HasPrefix(env["PATH"], plan.PublicBinDir+string(filepath.ListSeparator)) {
		t.Fatalf("native selection PATH = %q, want public selection first", env["PATH"])
	}
	if env[pkgsandbox.EnvNativeSelectionDir] != plan.PublicBinDir {
		t.Fatalf("native selection marker = %q, want %q", env[pkgsandbox.EnvNativeSelectionDir], plan.PublicBinDir)
	}
}

func TestOverlayNativeSelectionKeepsPrivateUserMiseTree(t *testing.T) {
	plan := BinaryInstallPlan{PublicDir: "/opt/stella/.mise-tools/public/selection", PublicBinDir: "/opt/stella/.mise-tools/public/selection"}
	base := map[string]string{
		"PATH":                        "/usr/bin",
		"MISE_DATA_DIR":               "/opt/stella/users/u/.mise-tools",
		"MISE_CONFIG_DIR":             "/opt/stella/users/u/.mise-tools/config",
		"MISE_CACHE_DIR":              "/opt/stella/users/u/.mise-tools/cache",
		"MISE_STATE_DIR":              "/opt/stella/users/u/.mise-tools/state",
		"MISE_GLOBAL_CONFIG_FILE":     "/opt/stella/users/u/.mise-tools/config.toml",
		"MISE_NOT_FOUND_AUTO_INSTALL": "true",
	}
	env := OverlayBinaryInstallPlan(base, plan, BinarySystemLayer)
	for _, key := range []string{"MISE_DATA_DIR", "MISE_CONFIG_DIR", "MISE_CACHE_DIR", "MISE_STATE_DIR", "MISE_GLOBAL_CONFIG_FILE"} {
		if env[key] != base[key] {
			t.Fatalf("native system overlay changed private user path %s=%q", key, env[key])
		}
	}
}

func TestContextBinaryInstallRejectsConflictingMiseToolSelections(t *testing.T) {
	baseIdentity := pkgplugins.PluginResourceIdentity{PluginID: "tool/one", ConfigID: "cfg-one", Scope: string(plugin.ScopeSystem), Revision: 1}
	specs := []pkgplugins.PluginBinarySpec{
		{PluginResourceIdentity: baseIdentity, Name: "one", Tool: "github:owner/shared", Version: "1.0.0"},
		{PluginResourceIdentity: pkgplugins.PluginResourceIdentity{PluginID: "tool/two", ConfigID: "cfg-two", Scope: string(plugin.ScopeSystemAgent), Revision: 1}, Name: "two", Tool: "github:owner/shared", Version: "2.0.0"},
	}
	if _, err := ContextBinaryInstallPlan(t.TempDir(), specs); err != nil {
		t.Fatalf("identity should accept distinct resources: %v", err)
	}
	if _, err := InstallContextBinaries(context.Background(), t.TempDir(), specs); err == nil || !strings.Contains(err.Error(), "disagree on mise tool") {
		t.Fatalf("conflicting mise selections should fail closed, got %v", err)
	}
}

func TestInstallContextBinariesUsesSelectionLocalConfigAndShims(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake mise script uses POSIX shell")
	}
	stellaHome := t.TempDir()
	binDir := filepath.Join(stellaHome, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(stellaHome, "mise.log")
	fake := "#!/bin/sh\nset -eu\nprintf '%s|data=%s|config=%s|shims=%s\\n' \"$1\" \"${MISE_DATA_DIR-}\" \"${MISE_GLOBAL_CONFIG_FILE-}\" \"${MISE_SHIMS_DIR-}\" >> " + shellQuote(logPath) + "\n"
	fake += "case \"$1\" in\n"
	fake += "trust) exit 0 ;;\n"
	fake += "install) mkdir -p \"$MISE_DATA_DIR/installs/test-one/1.0.0/bin\"; printf '#!/bin/sh\\necho one\\n' > \"$MISE_DATA_DIR/installs/test-one/1.0.0/bin/one\"; chmod 755 \"$MISE_DATA_DIR/installs/test-one/1.0.0/bin/one\"; printf sidecar > \"$MISE_DATA_DIR/installs/test-one/1.0.0/runtime.dat\"; exit 0 ;;\n"
	fake += "where) printf '%s\\n' \"$MISE_DATA_DIR/installs/test-one/1.0.0\" ;;\n"
	fake += "which) printf '%s\\n' \"$MISE_DATA_DIR/installs/test-one/1.0.0/bin/one\" ;;\n"
	fake += "*) exit 9 ;;\nesac\n"
	fakePath := filepath.Join(binDir, "mise")
	if err := os.WriteFile(fakePath, []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}

	spec := pkgplugins.PluginBinarySpec{
		PluginResourceIdentity: pkgplugins.PluginResourceIdentity{
			PluginID: "tool/one", ConfigID: "cfg-one", Scope: string(plugin.ScopeSystem), Revision: 4,
		},
		Name: "one", Tool: "github:owner/one", Version: "1.0.0", Options: map[string]any{"private": "secret"},
	}
	plan, err := InstallContextBinaries(context.Background(), stellaHome, []pkgplugins.PluginBinarySpec{spec})
	if err != nil {
		t.Fatalf("InstallContextBinaries: %v", err)
	}
	if _, err := os.Stat(filepath.Join(stellaHome, ".mise-tools", "_builtin.toml")); !os.IsNotExist(err) {
		t.Fatalf("context installer must not rewrite _builtin.toml, stat err=%v", err)
	}
	if plan.ConfigPath != "" || plan.ShimsDir != "" || plan.PublicBinDir == "" {
		t.Fatalf("native plan leaked config/shims or missed public bin: %+v", plan)
	}
	logBytes, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	log := string(logBytes)
	if !strings.Contains(log, "config=") || strings.Contains(log, "config="+plan.PublicDir) || strings.Contains(log, "config="+plan.DataDir) {
		t.Fatalf("installer did not use selection-local paths, plan=%+v log=%s", plan, log)
	}
	if strings.Count(log, "data="+plan.DataDir) != 4 {
		t.Fatalf("installer did not use shared artifact data dir for all steps, log=%s", log)
	}
	if strings.Contains(string(logBytes), "secret") {
		t.Fatalf("native private options leaked into installer log: %s", log)
	}
	privateInfo, err := os.Stat(filepath.Join(stellaHome, ".mise-private"))
	if err != nil {
		t.Fatalf("native private root missing: %v", err)
	}
	if privateInfo.Mode().Perm() != 0o700 {
		t.Fatalf("native private root mode = %o, want 700", privateInfo.Mode().Perm())
	}
	if entries, err := os.ReadDir(filepath.Join(stellaHome, ".mise-private")); err != nil || len(entries) != 0 {
		t.Fatalf("native private root retained installer state: entries=%v err=%v", entries, err)
	}
	if matches, _ := filepath.Glob(filepath.Join(plan.PublicDir, "installs", "*", "runtime.dat")); len(matches) != 1 {
		t.Fatalf("selected install sidecar missing: %v", matches)
	}
	if _, err := os.Stat(filepath.Join(plan.PublicBinDir, "one")); err != nil {
		t.Fatalf("selected direct alias missing: %v", err)
	}
	output, err := exec.Command(filepath.Join(plan.PublicBinDir, "one")).Output()
	if err != nil || string(output) != "one\n" {
		t.Fatalf("selected alias output = %q, err=%v; want selected version", output, err)
	}
	var leaked bool
	_ = filepath.WalkDir(filepath.Join(stellaHome, ".mise-tools"), func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() {
			return walkErr
		}
		data, readErr := os.ReadFile(path)
		if readErr == nil && bytes.Contains(data, []byte("secret")) {
			leaked = true
		}
		return readErr
	})
	if leaked {
		t.Fatal("native private options persisted in the shared mise tree")
	}
}

func TestRemoveNativeMiseConfigReportsCleanupFailure(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "leftover"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := removeNativeMiseConfig(dir); err == nil {
		t.Fatal("removing a non-empty config path must report cleanup failure")
	}
}

func TestInstallContextBinariesSkipsEmptySystemSelection(t *testing.T) {
	spec := pkgplugins.PluginBinarySpec{
		PluginResourceIdentity: pkgplugins.PluginResourceIdentity{
			PluginID: "tool/one", ConfigID: "cfg-one", Scope: string(plugin.ScopeUser), Revision: 1,
		},
		Name: "one", Tool: "github:owner/one", Version: "1.0.0",
	}
	plan, err := InstallContextBinaries(context.Background(), t.TempDir(), []pkgplugins.PluginBinarySpec{spec})
	if err != nil {
		t.Fatalf("InstallContextBinaries: %v", err)
	}
	if plan.ConfigPath != "" || plan.ShimsDir != "" {
		t.Fatalf("user-only context selection must not claim system paths: %+v", plan)
	}
	if plan.PublicDir == "" || plan.PublicBinDir == "" {
		t.Fatalf("user-only context selection must still publish a core selection: %+v", plan)
	}
}

func TestInstallContextBinariesDoesNotExposeMiseWhenDisabled(t *testing.T) {
	stellaHome := t.TempDir()
	if err := os.MkdirAll(filepath.Join(stellaHome, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"mise", ".stella-shell-env"} {
		if err := os.WriteFile(filepath.Join(stellaHome, "bin", name), []byte("internal"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	plan, err := InstallContextBinaries(context.Background(), stellaHome, nil)
	if err != nil {
		t.Fatalf("InstallContextBinaries: %v", err)
	}
	if _, err := os.Stat(filepath.Join(plan.PublicBinDir, "mise")); !os.IsNotExist(err) {
		t.Fatalf("disabled mise selection exposed mise, stat err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(plan.PublicBinDir, ".stella-shell-env")); err != nil {
		t.Fatalf("disabled mise selection lost runner PATH restoration file: %v", err)
	}
	env := OverlayBinaryInstallPlan(map[string]string{"PATH": plan.PublicBinDir}, plan, BinarySystemLayer)
	cmd := exec.Command("/bin/sh", "-c", "command -v mise")
	cmd.Env = append(os.Environ(), "PATH="+env["PATH"])
	if result, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("disabled mise remained discoverable on PATH: %q", result)
	}
}

func TestPublishNativeSelectionReusesCompleteIdentity(t *testing.T) {
	root := filepath.Join(t.TempDir(), "selection")
	builds := 0
	build := func(dir string) error {
		builds++
		return os.WriteFile(filepath.Join(dir, "tool"), []byte("old"), 0o755)
	}
	if err := publishNativeSelection(root, []string{"tool"}, build); err != nil {
		t.Fatalf("first publication: %v", err)
	}
	before, err := os.Stat(filepath.Join(root, "tool"))
	if err != nil {
		t.Fatal(err)
	}
	if err := publishNativeSelection(root, []string{"tool"}, func(dir string) error {
		builds++
		return os.WriteFile(filepath.Join(dir, "tool"), []byte("new"), 0o755)
	}); err != nil {
		t.Fatalf("repeat publication: %v", err)
	}
	after, err := os.Stat(filepath.Join(root, "tool"))
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, "tool"))
	if err != nil {
		t.Fatal(err)
	}
	if builds != 1 || !os.SameFile(before, after) || string(data) != "old" {
		t.Fatalf("complete publication was rebuilt: builds=%d sameFile=%v content=%q", builds, os.SameFile(before, after), data)
	}
}

func TestPublishNativeSelectionSerializesConcurrentIdentity(t *testing.T) {
	root := filepath.Join(t.TempDir(), "selection")
	var builds int
	build := func(dir string) error {
		builds++
		return os.WriteFile(filepath.Join(dir, "tool"), []byte("selected"), 0o755)
	}
	const callers = 8
	var wg sync.WaitGroup
	errCh := make(chan error, callers)
	for range callers {
		wg.Go(func() {
			errCh <- publishNativeSelection(root, []string{"tool"}, build)
		})
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatalf("concurrent publication: %v", err)
		}
	}
	if builds != 1 {
		t.Fatalf("concurrent publication built %d times, want one", builds)
	}
	if data, err := os.ReadFile(filepath.Join(root, "tool")); err != nil || string(data) != "selected" {
		t.Fatalf("published selection = %q, err=%v", data, err)
	}
}

func TestCopyNativeTreeRejectsEscapingSymlink(t *testing.T) {
	source := t.TempDir()
	nested := filepath.Join(source, "nested")
	if err := os.Mkdir(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret")
	if err := os.WriteFile(secret, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(nested, "escape")
	rel, err := filepath.Rel(filepath.Dir(link), secret)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(rel, link); err != nil {
		t.Fatal(err)
	}

	err = copyNativeTree(source, filepath.Join(t.TempDir(), "copy"))
	if err == nil || !strings.Contains(err.Error(), "escapes install") {
		t.Fatalf("copyNativeTree error = %v, want escaping symlink rejection", err)
	}
}

func TestCopyNativeTreePortableAbsoluteSymlinkAndChain(t *testing.T) {
	source := t.TempDir()
	target := filepath.Join(source, "installs", "uv", "bin", "uv")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("uv-ready"), 0o755); err != nil {
		t.Fatal(err)
	}
	links := filepath.Join(source, ".mise-bins")
	if err := os.MkdirAll(links, 0o755); err != nil {
		t.Fatal(err)
	}
	chain := filepath.Join(links, "uv-chain")
	if err := os.Symlink(target, chain); err != nil {
		t.Fatal(err)
	}
	canonicalTarget, err := filepath.EvalSymlinks(target)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(canonicalTarget, filepath.Join(links, "uv-canonical")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(chain, filepath.Join(links, "uv")); err != nil {
		t.Fatal(err)
	}

	destination := filepath.Join(t.TempDir(), "copy")
	if err := copyNativeTree(source, destination); err != nil {
		t.Fatalf("copyNativeTree: %v", err)
	}
	for _, name := range []string{"uv", "uv-chain", "uv-canonical"} {
		link := filepath.Join(destination, ".mise-bins", name)
		gotTarget, err := os.Readlink(link)
		if err != nil {
			t.Fatalf("read copied %s link: %v", name, err)
		}
		if filepath.IsAbs(gotTarget) {
			t.Fatalf("copied %s link retained absolute target %q", name, gotTarget)
		}
		got, err := os.ReadFile(link)
		if err != nil || string(got) != "uv-ready" {
			t.Fatalf("read copied %s target = %q, err=%v", name, got, err)
		}
	}
	if err := os.RemoveAll(source); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(destination, ".mise-bins", "uv")); err != nil || string(got) != "uv-ready" {
		t.Fatalf("copied selection depends on source after publication: %q, err=%v", got, err)
	}
}

func TestCopyNativeTreeRejectsAbsoluteSiblingAndSymlinkCycle(t *testing.T) {
	root := t.TempDir()
	sibling := root + "-sibling"
	if err := os.MkdirAll(sibling, 0o755); err != nil {
		t.Fatal(err)
	}
	external := filepath.Join(sibling, "secret")
	if err := os.WriteFile(external, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, filepath.Join(root, "absolute-escape")); err != nil {
		t.Fatal(err)
	}
	if err := copyNativeTree(root, filepath.Join(t.TempDir(), "absolute-copy")); err == nil || !strings.Contains(err.Error(), "absolute or unsafe target") {
		t.Fatalf("absolute sibling link error = %v, want rejection", err)
	}

	cycleRoot := t.TempDir()
	if err := os.Symlink("b", filepath.Join(cycleRoot, "a")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("a", filepath.Join(cycleRoot, "b")); err != nil {
		t.Fatal(err)
	}
	if err := copyNativeTree(cycleRoot, filepath.Join(t.TempDir(), "cycle-copy")); err == nil || !strings.Contains(err.Error(), "symlink cycle") {
		t.Fatalf("symlink cycle error = %v, want rejection", err)
	}
}

func TestMaterializeNativeSelectionCanonicalizesMisePaths(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake mise script uses POSIX shell")
	}
	root := t.TempDir()
	install := filepath.Join(root, "install")
	binary := filepath.Join(install, "bin", "bun")
	if err := os.MkdirAll(filepath.Dir(binary), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(binary, []byte("bun-real\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(install, "runtime.dat"), []byte("sidecar\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	installAlias := filepath.Join(root, "install-alias")
	if err := os.Symlink(install, installAlias); err != nil {
		t.Fatal(err)
	}

	mise := filepath.Join(root, "mise")
	script := "#!/bin/sh\nset -eu\ncase \"$1\" in\nwhere) printf '%s\\n' " + shellQuotePOSIX(installAlias) + ";;\nwhich) printf '%s\\n' " + shellQuotePOSIX(binary) + ";;\n*) exit 9;;\nesac\n"
	if err := os.WriteFile(mise, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	public := filepath.Join(root, "public")
	tool := miseTool{Key: "github:oven/bun", Lookup: "bun", PublicName: "bun"}
	plan := BinaryInstallPlan{Identity: "realpath", PublicDir: public, PublicBinDir: public}
	if err := materializeNativeSelection(context.Background(), filepath.Join(root, "stella"), plan, []miseTool{tool}, mise, nil, root); err != nil {
		t.Fatalf("materializeNativeSelection: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(public, "installs", nativeInstallKey(tool), "bin", "bun"))
	if err != nil {
		t.Fatalf("read selected bun: %v", err)
	}
	if string(data) != "bun-real\n" {
		t.Fatalf("selected bun = %q, want real install content", data)
	}
	sidecar, err := os.ReadFile(filepath.Join(public, "installs", nativeInstallKey(tool), "runtime.dat"))
	if err != nil {
		t.Fatalf("read selected sidecar: %v", err)
	}
	if string(sidecar) != "sidecar\n" {
		t.Fatalf("selected sidecar = %q, want source sidecar", sidecar)
	}
}

func TestInstallNativeMiseSelectionReturnsBeforeMiseWhenPublicationIsComplete(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake mise script uses POSIX shell")
	}
	stellaHome := t.TempDir()
	binDir := filepath.Join(stellaHome, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	called := filepath.Join(stellaHome, "mise-called")
	fake := "#!/bin/sh\nprintf called > " + shellQuote(called) + "\nexit 99\n"
	if err := os.WriteFile(filepath.Join(binDir, "mise"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	public := filepath.Join(stellaHome, ".mise-tools", "public", "core-ready")
	if err := os.MkdirAll(public, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"mise", "fd"} {
		if err := os.WriteFile(filepath.Join(public, name), []byte(name+"-ready\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	err := InstallNativeMiseSelection(context.Background(), stellaHome, NativeSelectionPlan{
		DataDir: filepath.Join(stellaHome, ".mise-tools"), PublicDir: public, PublicBinDir: public, EmbeddedNames: []string{"mise"},
	}, []NativeMiseTool{{Key: "github:sharkdp/fd", Lookup: "fd", PublicName: "fd", Version: "10.4.2"}})
	if err != nil {
		t.Fatalf("InstallNativeMiseSelection: %v", err)
	}
	if _, err := os.Stat(called); !os.IsNotExist(err) {
		t.Fatalf("complete publication invoked mise, stat err=%v", err)
	}
}

func TestNativeMiseInstallEnvIgnoresAmbientToolVersions(t *testing.T) {
	miseBin, err := exec.LookPath("mise")
	if err != nil {
		t.Skip("real mise is not installed")
	}
	stellaHome := t.TempDir()
	dataDir := filepath.Join(stellaHome, ".mise-tools")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stellaHome, ".tool-versions"), []byte("rust 1.80.0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stellaHome, "mise.toml"), []byte("[tools]\ntailspin = '0.1.0'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	hostHome := t.TempDir()
	hostXDG := filepath.Join(t.TempDir(), "config")
	if err := os.MkdirAll(filepath.Join(hostXDG, "mise"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		filepath.Join(hostHome, ".config", "mise", "config.toml"),
		filepath.Join(hostXDG, "mise", "config.toml"),
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("[tools]\nzellij = '0.1.0'\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	hostSystem := filepath.Join(t.TempDir(), "system.toml")
	if err := os.WriteFile(hostSystem, []byte("[tools]\nherd = '0.1.0'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", hostHome)
	t.Setenv("XDG_CONFIG_HOME", hostXDG)
	t.Setenv("MISE_GLOBAL_CONFIG_FILE", filepath.Join(hostHome, ".config", "mise", "config.toml"))
	t.Setenv("MISE_SYSTEM_CONFIG_FILE", hostSystem)
	private := filepath.Join(stellaHome, ".mise-private", "install-ambient")
	if err := os.MkdirAll(private, 0o700); err != nil {
		t.Fatal(err)
	}
	globalConfig := filepath.Join(private, "selection.toml")
	systemConfig := filepath.Join(private, "system.toml")
	if err := os.WriteFile(globalConfig, []byte("[tools]\nbun = '1.3.14'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(systemConfig, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	env, err := nativeMiseInstallEnv(stellaHome, dataDir, filepath.Join(private, "shims"), private, globalConfig, systemConfig)
	if err != nil {
		t.Fatalf("nativeMiseInstallEnv: %v", err)
	}
	output, err := runMiseOutput(context.Background(), miseBin, env, private, "ls", "--json")
	if err != nil {
		t.Fatalf("real mise ls: %v", err)
	}
	for _, ambient := range []string{"rust", "tailspin", "zellij", "herd"} {
		if strings.Contains(strings.ToLower(output), ambient) {
			t.Fatalf("ambient %s config leaked into native selection: %q", ambient, output)
		}
	}
	if !strings.Contains(strings.ToLower(output), "bun") {
		t.Fatalf("selection config was not used by real mise: %q", output)
	}
}

func TestNativeMiseErrorsHideStderr(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake mise script uses POSIX shell")
	}
	mise := filepath.Join(t.TempDir(), "mise")
	if err := os.WriteFile(mise, []byte("#!/bin/sh\necho secret-install-output >&2\nexit 23\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	err := runMise(context.Background(), mise, nil, t.TempDir(), "install")
	if err == nil || strings.Contains(err.Error(), "secret-install-output") || len(err.Error()) > 128 {
		t.Fatalf("runMise error = %v, want closed error", err)
	}
	_, err = runMiseOutput(context.Background(), mise, nil, t.TempDir(), "where", "bun")
	if err == nil || strings.Contains(err.Error(), "secret-install-output") || len(err.Error()) > 128 {
		t.Fatalf("runMiseOutput error = %v, want closed error", err)
	}

	stellaHome := t.TempDir()
	binDir := filepath.Join(stellaHome, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	fake := "#!/bin/sh\nset -eu\ncase \"$1\" in\ntrust|install) printf 'secret-install-output-%0100d\\n' 0 >&2; printf 'secret-install-stdout-%0100d\\n' 0; exit 0;;\nwhere) printf 'secret-where-output-%0100d\\n' 0 >&2; exit 23;;\n*) exit 9;;\nesac\n"
	if err := os.WriteFile(filepath.Join(binDir, "mise"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	spec := pkgplugins.PluginBinarySpec{
		PluginResourceIdentity: pkgplugins.PluginResourceIdentity{
			PluginID: "tool/secret", ConfigID: "cfg-secret", Scope: string(plugin.ScopeSystem), Revision: 1,
		},
		Name: "secret-tool", Tool: "github:owner/secret-tool", Version: "1.0.0",
	}
	_, err = InstallContextBinaries(context.Background(), stellaHome, []pkgplugins.PluginBinarySpec{spec})
	if err == nil || strings.Contains(err.Error(), "secret-install-output") || strings.Contains(err.Error(), "secret-install-stdout") || strings.Contains(err.Error(), "secret-where-output") || strings.Contains(err.Error(), spec.Tool) || len(err.Error()) > 256 {
		t.Fatalf("InstallContextBinaries error = %v, want bounded closed error", err)
	}
}

func TestInstallSandboxBinariesUsesSessionOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake mise script uses POSIX shell")
	}
	stellaHome := t.TempDir()
	workspace := t.TempDir()
	userTools := filepath.Join(stellaHome, "users", "u", ".mise-tools")
	if err := os.MkdirAll(filepath.Join(stellaHome, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	fake := "#!/bin/sh\nset -eu\ncase \"$1\" in trust|reshim) exit 0;; install) mkdir -p \"$MISE_DATA_DIR/installs/user-tool/3.0.0/bin\"; printf '#!/bin/sh\\necho user\\n' > \"$MISE_DATA_DIR/installs/user-tool/3.0.0/bin/user-tool\"; chmod 755 \"$MISE_DATA_DIR/installs/user-tool/3.0.0/bin/user-tool\";; where) printf '%s\\n' \"$MISE_DATA_DIR/installs/user-tool/3.0.0\";; which) printf '%s\\n' \"$MISE_DATA_DIR/installs/user-tool/3.0.0/bin/user-tool\";; *) exit 9;; esac\n"
	if err := os.WriteFile(filepath.Join(stellaHome, "bin", "mise"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(userTools, 0o700); err != nil {
		t.Fatal(err)
	}
	factory := nonebackend.NewFactoryWithMountSources(map[string]string{
		pkgsandbox.MountWorkspace:                           workspace,
		pkgsandbox.MountStellaHome + "/bin":                 filepath.Join(stellaHome, "bin"),
		pkgsandbox.MountStellaHome + "/.mise-tools":         filepath.Join(stellaHome, ".mise-tools"),
		pkgsandbox.MountStellaHome + "/users/u/.mise-tools": userTools,
	}, nonebackend.Config{StellaHome: stellaHome})
	policy := pkgsandbox.Policy{
		Filesystem: pkgsandbox.FilesystemPolicy{
			WorkingDir: pkgsandbox.MountWorkspace,
			Mounts: []pkgsandbox.Mount{
				{SandboxPath: pkgsandbox.MountWorkspace, Access: pkgsandbox.MountReadWrite},
				{SandboxPath: pkgsandbox.MountStellaHome + "/bin", Access: pkgsandbox.MountReadOnly},
				{SandboxPath: pkgsandbox.MountStellaHome + "/.mise-tools", Access: pkgsandbox.MountReadOnly},
				{SandboxPath: pkgsandbox.MountStellaHome + "/users/u/.mise-tools", Access: pkgsandbox.MountReadWrite},
			},
		},
		Env: map[string]string{
			"STELLA_HOME":                 stellaHome,
			"MISE_DATA_DIR":               userTools,
			"MISE_NOT_FOUND_AUTO_INSTALL": "true",
			"PATH":                        filepath.Join(stellaHome, "bin") + string(filepath.ListSeparator) + "/usr/bin",
		},
	}
	session, err := factory.CreateSession(context.Background(), policy)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	defer func() { _ = session.Close() }()
	spec := pkgplugins.PluginBinarySpec{
		PluginResourceIdentity: pkgplugins.PluginResourceIdentity{
			PluginID: "tool/user", ConfigID: "cfg-user", Scope: string(plugin.ScopeUser), Revision: 2,
		},
		Name: "user-tool", Tool: "github:owner/user-tool", Version: "3.0.0",
	}
	plan, err := InstallSandboxBinaries(context.Background(), session, []pkgplugins.PluginBinarySpec{spec})
	if err != nil {
		t.Fatalf("InstallSandboxBinaries: %v", err)
	}
	if plan.ConfigPath != "" || plan.ShimsDir != "" || plan.PublicDir == "" {
		t.Fatalf("returned plan must expose only exact public selection: %+v", plan)
	}
	if _, err := session.Files().Stat(filepath.Join(plan.PublicDir, "user-tool")); err != nil {
		t.Fatalf("sandbox public selection was not materialized: %v", err)
	}
	if got := session.Policy().Env["PATH"]; plan.ShimsDir != "" && strings.HasPrefix(got, plan.ShimsDir) {
		t.Fatalf("session policy unexpectedly mutated in place: %q", got)
	}
}

func TestSandboxMiseInstallCommandUsesExplicitEnginePath(t *testing.T) {
	command := sandboxMiseInstallCommand()
	if strings.Contains(command, "&& mise ") || strings.HasPrefix(command, "mise ") {
		t.Fatalf("sandbox installer must invoke the runner-owned mise executable explicitly: %q", command)
	}
	if !strings.Contains(command, "STELLA_HOME") {
		t.Fatalf("sandbox installer command must resolve mise from STELLA_HOME: %q", command)
	}
}
