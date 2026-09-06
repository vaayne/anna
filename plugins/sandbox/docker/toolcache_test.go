package docker

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	pkgplugins "github.com/CherryHQ/stella/pkg/plugins"
	"github.com/CherryHQ/stella/plugins/core"
)

func TestSelectionMiseTOMLRegistryTool(t *testing.T) {
	got, err := selectionMiseTOML([]ToolBinary{{Name: "uv", Tool: "uv"}})
	if err != nil {
		t.Fatalf("selectionMiseTOML: %v", err)
	}
	want := `uv = 'latest'`
	if !strings.Contains(got, want) {
		t.Fatalf("expected registry tool form %q in:\n%s", want, got)
	}
}

func TestSelectionMiseTOMLRejectsConflictingScopes(t *testing.T) {
	_, err := selectionMiseTOML([]ToolBinary{
		{PluginID: "tool/uv", ConfigID: "system", Scope: "system", Name: "uv", Tool: "uv", Version: "1"},
		{PluginID: "tool/uv", ConfigID: "user", Scope: "user", Name: "uv", Tool: "uv", Version: "2"},
	})
	if err == nil || !strings.Contains(err.Error(), `disagree on mise tool "uv"`) {
		t.Fatalf("selectionMiseTOML error = %v, want conflicting tool error", err)
	}
}

func TestSelectionToolCacheScriptPublishesCoreAlias(t *testing.T) {
	coreRuntimes := []core.RuntimeResource{{Name: "xberg", Version: "core-1", Embedded: true}}
	script := selectionToolInstallScript("hash", nil, coreRuntimes)
	if !strings.Contains(script, "test -x \"$ROOT/core/xberg\"") || !strings.Contains(script, "/opt/stella/selection-tools/core/xberg") {
		t.Fatalf("selection helper did not publish trusted xberg alias:\n%s", script)
	}
	if strings.Contains(script, "ln -s /opt/stella/bin/mise") {
		t.Fatalf("selection helper exposed unselected mise:\n%s", script)
	}
}

func TestSelectionToolInstallScriptCoreOnlyDoesNotExposeMise(t *testing.T) {
	coreRuntimes := []core.RuntimeResource{
		{Name: "fd", Version: "core-1"},
		{Name: "mise", Version: "core-1", Embedded: true},
		{Name: "rg", Version: "core-1"},
		{Name: "xberg", Version: "core-1", Embedded: true},
	}
	script := selectionToolInstallScript("hash", nil, coreRuntimes)
	for _, name := range []string{"fd", "mise", "rg", "xberg"} {
		if !strings.Contains(script, "test -x \"$ROOT/core/"+name+"\"") || !strings.Contains(script, "/opt/stella/selection-tools/core/"+name) {
			t.Fatalf("core-only selection must publish %s from the image:\n%s", name, script)
		}
	}
	if strings.Contains(script, "/opt/stella/bin/mise install") || strings.Contains(script, "STELLA_SELECTION_MISE_TOML") {
		t.Fatal("core-only selection must not create or run a private mise install")
	}
}

func TestSelectionToolInstallScriptRejectsOptionalCoreCollision(t *testing.T) {
	script := selectionToolInstallScript("hash", []ToolBinary{{Name: "rg", Tool: "github:BurntSushi/ripgrep"}}, core.RuntimeResources())
	if !strings.Contains(script, "selection binary conflicts with mandatory core runtime rg") {
		t.Fatalf("optional core collision must fail closed:\n%s", script)
	}
}

func TestSelectionToolCacheHashIncludesIdentityAndRevision(t *testing.T) {
	base := []ToolBinary{{PluginID: "tool/one", ConfigID: "cfg", Scope: "system", Revision: 1, Name: "one", Tool: "github:owner/one", Version: "1"}}
	other := []ToolBinary{{PluginID: "tool/one", ConfigID: "cfg", Scope: "system", Revision: 2, Name: "one", Tool: "github:owner/one", Version: "1"}}
	if selectionToolCacheHash("sha256:image-a", base, nil) == selectionToolCacheHash("sha256:image-a", other, nil) {
		t.Fatal("selection cache identity must include config revision")
	}
	if selectionToolCacheHash("sha256:image-a", base, nil) == selectionToolCacheHash("sha256:image-b", base, nil) {
		t.Fatal("selection cache identity must include resolved image ID")
	}
	second := ToolBinary{PluginID: "tool/two", ConfigID: "cfg", Scope: "system", Revision: 1, Name: "two", Tool: "uv", Version: "2"}
	if selectionToolCacheHash("sha256:image-a", []ToolBinary{base[0], second}, nil) != selectionToolCacheHash("sha256:image-a", []ToolBinary{second, base[0]}, nil) {
		t.Fatal("selection cache hash must be independent of input order")
	}
	coreRuntimes := []core.RuntimeResource{{Name: "mise", Version: "core-1", Embedded: true}}
	if selectionToolCacheHash("sha256:image-a", nil, coreRuntimes) == selectionToolCacheHash("sha256:image-a", nil, []core.RuntimeResource{{Name: "mise", Version: "core-2", Embedded: true}}) {
		t.Fatal("selection cache identity must include core runtime revision")
	}
}

func TestSelectionToolInstallScriptRemovesPrivateInstallerState(t *testing.T) {
	script := selectionToolInstallScript("hash", []ToolBinary{{Name: "uv", Tool: "uv"}}, []core.RuntimeResource{{Name: "xberg", Embedded: true}})
	for _, required := range []string{"PRIVATE=/tmp/stella-selection-private", "trap 'rm -rf \"$PRIVATE\"'", "cp -R \"$install_dir/.\"", "cp -R /opt/stella/core-runtime/. \"$ROOT/core/\""} {
		if !strings.Contains(script, required) {
			t.Fatalf("selection script missing %q:\n%s", required, script)
		}
	}
	for _, required := range []string{"MISE_CACHE_DIR=\"$PRIVATE/mise-cache\"", "MISE_STATE_DIR=\"$PRIVATE/mise-state\"", "MISE_CONFIG_DIR=\"$PRIVATE/mise-config\"", "MISE_SYSTEM_CONFIG_FILE=\"$PRIVATE/mise.toml\"", "XDG_CACHE_HOME=\"$PRIVATE/xdg-cache\"", "XDG_CONFIG_HOME=\"$PRIVATE/xdg-config\"", "XDG_DATA_HOME=\"$PRIVATE/xdg-data\"", "XDG_STATE_HOME=\"$PRIVATE/xdg-state\""} {
		if !strings.Contains(script, required) {
			t.Fatalf("selection script does not isolate mise path %q:\n%s", required, script)
		}
	}
	if strings.Contains(script, "MISE_SYSTEM_CONFIG_FILE=/opt/stella") || strings.Contains(script, "ln -s /opt/stella/bin/xberg") {
		t.Fatalf("selection script leaks shared installer state or image alias:\n%s", script)
	}
}

func TestSelectionToolInstallScriptPublishesRunnableArtifactsAndNoPrivateState(t *testing.T) {
	imageBin := filepath.Join(t.TempDir(), "image-bin")
	selectionRoot := filepath.Join(t.TempDir(), "selection")
	privateRoot := filepath.Join(t.TempDir(), "private")
	if err := os.MkdirAll(imageBin, 0o755); err != nil {
		t.Fatal(err)
	}
	mise := `#!/bin/sh
set -eu
case "$1" in
trust) exit 0 ;;
install) mkdir -p "$MISE_DATA_DIR/installs/uv/1/bin"; printf '#!/bin/sh\nprintf "uv-ok\\n"\n' > "$MISE_DATA_DIR/installs/uv/1/bin/uv"; chmod 755 "$MISE_DATA_DIR/installs/uv/1/bin/uv" ;;
where) printf '%s/installs/uv/1\n' "$MISE_DATA_DIR" ;;
esac
`
	if err := os.WriteFile(filepath.Join(imageBin, "mise"), []byte(mise), 0o755); err != nil {
		t.Fatal(err)
	}
	bundleDir := filepath.Join(imageBin, "xberg-v1")
	if err := os.MkdirAll(bundleDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundleDir, "xberg"), []byte("#!/bin/sh\nprintf 'xberg-ok\\n'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundleDir, "libxberg.so"), []byte("sidecar"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("xberg-v1", "xberg"), filepath.Join(imageBin, "xberg")); err != nil {
		t.Fatal(err)
	}

	script := selectionToolInstallScript("hash", []ToolBinary{{Name: "uv", Tool: "uv", Version: "1"}}, []core.RuntimeResource{{Name: "xberg", Embedded: true}})
	script = strings.ReplaceAll(script, "/opt/stella/selection-tools", selectionRoot)
	script = strings.ReplaceAll(script, "/opt/stella/bin", imageBin)
	script = strings.ReplaceAll(script, "/opt/stella/core-runtime", imageBin)
	script = strings.ReplaceAll(script, "/tmp/stella-selection-private", privateRoot)
	cmd := exec.Command("/bin/sh", "-c", script)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("selection installer: %v\n%s\nscript:\n%s", err, output, script)
	}
	for _, private := range []string{"mise.toml", "mise-data"} {
		if _, err := os.Stat(filepath.Join(privateRoot, private)); !os.IsNotExist(err) {
			t.Fatalf("private installer state %s remains, err=%v", private, err)
		}
	}
	identity, err := pkgplugins.BinaryArtifactIdentity(pkgplugins.PluginBinarySpec{Name: "uv", Tool: "uv", Version: "1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(selectionRoot, "artifacts", identity, "bin", "uv")); err != nil {
		t.Fatalf("selected runtime artifact missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(selectionRoot, "core", "xberg-v1", "libxberg.so")); err != nil {
		t.Fatalf("core sidecar missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(selectionRoot, "bin", "mise")); !os.IsNotExist(err) {
		t.Fatalf("core mise is absent from this test image, err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(selectionRoot, "bin", "unselected")); !os.IsNotExist(err) {
		t.Fatalf("unselected image binary was published, err=%v", err)
	}
	pathEnv := filepath.Join(selectionRoot, "bin")
	result := exec.Command("/bin/sh", "-c", "uv && xberg")
	result.Env = append(os.Environ(), "PATH="+pathEnv)
	output, err := result.CombinedOutput()
	if err != nil || string(output) != "uv-ok\nxberg-ok\n" {
		t.Fatalf("selected aliases output=%q err=%v", output, err)
	}
}

func TestSelectionToolInstallScriptUsesBuiltinArtifactWithoutMise(t *testing.T) {
	imageRoot := t.TempDir()
	selectionRoot := filepath.Join(t.TempDir(), "selection")
	artifactRoot := filepath.Join(t.TempDir(), "builtin-artifacts")
	privateRoot := filepath.Join(t.TempDir(), "private")
	if err := os.MkdirAll(imageRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	binary := ToolBinary{Name: "uv", Tool: "uv", Version: "1"}
	identity, err := binaryArtifactIdentity(binary)
	if err != nil {
		t.Fatal(err)
	}
	artifactDir := filepath.Join(artifactRoot, identity)
	if err := os.MkdirAll(filepath.Join(artifactDir, "installs", "uv-1", "lib"), 0o755); err != nil {
		t.Fatal(err)
	}
	uv := filepath.Join(artifactDir, "installs", "uv-1", "uv")
	if err := os.WriteFile(uv, []byte("#!/bin/sh\nprintf 'builtin-uv-ok\\n'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(artifactDir, "installs", "uv-1", "lib", "sidecar.so"), []byte("sidecar"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("installs", "uv-1", "uv"), filepath.Join(artifactDir, "uv")); err != nil {
		t.Fatal(err)
	}
	// A different artifact proves the helper only copies the exact fingerprint.
	otherIdentity, err := binaryArtifactIdentity(ToolBinary{Name: "uv", Tool: "uv", Version: "2"})
	if err != nil {
		t.Fatal(err)
	}
	wrong := filepath.Join(artifactRoot, otherIdentity)
	if err := os.MkdirAll(wrong, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wrong, "other"), []byte("other"), 0o755); err != nil {
		t.Fatal(err)
	}

	miseMarker := filepath.Join(t.TempDir(), "mise-called")
	mise := "#!/bin/sh\nprintf called > \"$MISE_MARKER\"\nexit 99\n"
	if err := os.WriteFile(filepath.Join(imageRoot, "mise"), []byte(mise), 0o755); err != nil {
		t.Fatal(err)
	}
	script := selectionToolInstallScript("hash", []ToolBinary{binary}, nil)
	script = strings.ReplaceAll(script, "/opt/stella/selection-tools", selectionRoot)
	script = strings.ReplaceAll(script, "/opt/stella/.mise-tools/builtin-artifacts", artifactRoot)
	script = strings.ReplaceAll(script, "/opt/stella/core-runtime", imageRoot)
	script = strings.ReplaceAll(script, "/tmp/stella-selection-private", privateRoot)
	cmd := exec.Command("/bin/sh", "-c", script)
	cmd.Env = append(os.Environ(), "MISE_MARKER="+miseMarker)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("offline builtin artifact selection: %v\n%s\nscript:\n%s", err, output, script)
	}
	if _, err := os.Stat(miseMarker); !os.IsNotExist(err) {
		t.Fatalf("builtin artifact hit invoked mise, err=%v", err)
	}
	selected := filepath.Join(selectionRoot, "bin", "uv")
	result := exec.Command(selected)
	output, err := result.CombinedOutput()
	if err != nil || string(output) != "builtin-uv-ok\n" {
		t.Fatalf("selected builtin alias output=%q err=%v", output, err)
	}
	if _, err := os.Stat(filepath.Join(selectionRoot, "artifacts", identity, "installs", "uv-1", "lib", "sidecar.so")); err != nil {
		t.Fatalf("builtin artifact sidecar missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(selectionRoot, "artifacts", otherIdentity)); !os.IsNotExist(err) {
		t.Fatalf("unselected artifact became reachable, err=%v", err)
	}
}

func TestSelectionToolInstallScriptRejectsBuiltinArtifactMismatch(t *testing.T) {
	imageRoot := t.TempDir()
	selectionRoot := filepath.Join(t.TempDir(), "selection")
	artifactRoot := filepath.Join(t.TempDir(), "builtin-artifacts")
	privateRoot := filepath.Join(t.TempDir(), "private")
	if err := os.MkdirAll(imageRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	binary := ToolBinary{Name: "uv", Tool: "uv", Version: "1"}
	otherIdentity, err := binaryArtifactIdentity(ToolBinary{Name: "uv", Tool: "uv", Version: "2"})
	if err != nil {
		t.Fatal(err)
	}
	wrong := filepath.Join(artifactRoot, otherIdentity)
	if err := os.MkdirAll(wrong, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wrong, "uv"), []byte("#!/bin/sh\nprintf wrong\\n\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	mise := `#!/bin/sh
set -eu
case "$1" in
trust) exit 0 ;;
install)
  mkdir -p "$MISE_DATA_DIR/installs/uv/1/bin"
  cat > "$MISE_DATA_DIR/installs/uv/1/bin/uv" <<'EOF'
#!/bin/sh
printf 'mise-uv-ok\n'
EOF
  chmod 755 "$MISE_DATA_DIR/installs/uv/1/bin/uv"
  ;;
where) printf '%s/installs/uv/1\n' "$MISE_DATA_DIR" ;;
esac
`
	if err := os.WriteFile(filepath.Join(imageRoot, "mise"), []byte(mise), 0o755); err != nil {
		t.Fatal(err)
	}
	script := selectionToolInstallScript("hash", []ToolBinary{binary}, nil)
	script = strings.ReplaceAll(script, "/opt/stella/selection-tools", selectionRoot)
	script = strings.ReplaceAll(script, "/opt/stella/.mise-tools/builtin-artifacts", artifactRoot)
	script = strings.ReplaceAll(script, "/opt/stella/core-runtime", imageRoot)
	script = strings.ReplaceAll(script, "/tmp/stella-selection-private", privateRoot)
	cmd := exec.Command("/bin/sh", "-c", script)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("mismatched builtin artifact fallback: %v\n%s\nscript:\n%s", err, output, script)
	}
	result := exec.Command(filepath.Join(selectionRoot, "bin", "uv"))
	output, err := result.CombinedOutput()
	if err != nil || string(output) != "mise-uv-ok\n" {
		t.Fatalf("isolated fallback output=%q err=%v", output, err)
	}
	if _, err := os.Stat(filepath.Join(selectionRoot, "artifacts", otherIdentity)); !os.IsNotExist(err) {
		t.Fatalf("mismatched artifact was copied, err=%v", err)
	}
}

func TestSelectionToolInstallScriptMixedHitInstallsOnlyMiss(t *testing.T) {
	imageRoot := t.TempDir()
	selectionRoot := filepath.Join(t.TempDir(), "selection")
	artifactRoot := filepath.Join(t.TempDir(), "builtin-artifacts")
	privateRoot := filepath.Join(t.TempDir(), "private")
	if err := os.MkdirAll(imageRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	hit := ToolBinary{Name: "uv", Tool: "uv", Version: "1"}
	miss := ToolBinary{Name: "bun", Tool: "bun", Version: "1"}
	hitIdentity, err := binaryArtifactIdentity(hit)
	if err != nil {
		t.Fatal(err)
	}
	hitDir := filepath.Join(artifactRoot, hitIdentity)
	if err := os.MkdirAll(filepath.Join(hitDir, "installs", "uv-1"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hitDir, "installs", "uv-1", "uv"), []byte("#!/bin/sh\nprintf 'hit-uv\\n'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("installs", "uv-1", "uv"), filepath.Join(hitDir, "uv")); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(t.TempDir(), "mise.log")
	mise := `#!/bin/sh
set -eu
printf '%s %s\n' "$1" "${2-}" >> "$MISE_LOG"
case "$1" in
trust) exit 0 ;;
install) mkdir -p "$MISE_DATA_DIR/installs/bun/1/bin"; cat > "$MISE_DATA_DIR/installs/bun/1/bin/bun" <<'EOF2'
#!/bin/sh
printf 'miss-bun\n'
EOF2
chmod 755 "$MISE_DATA_DIR/installs/bun/1/bin/bun" ;;
where)
  case "$2" in
  bun) printf '%s/installs/bun/1\n' "$MISE_DATA_DIR" ;;
  uv) printf '%s/installs/uv/1\n' "$MISE_DATA_DIR" ;;
  esac ;;
esac
`
	if err := os.WriteFile(filepath.Join(imageRoot, "mise"), []byte(mise), 0o755); err != nil {
		t.Fatal(err)
	}
	script := selectionToolInstallScript("hash", []ToolBinary{hit, miss}, nil)
	script = strings.ReplaceAll(script, "/opt/stella/selection-tools", selectionRoot)
	script = strings.ReplaceAll(script, "/opt/stella/.mise-tools/builtin-artifacts", artifactRoot)
	script = strings.ReplaceAll(script, "/opt/stella/core-runtime", imageRoot)
	script = strings.ReplaceAll(script, "/tmp/stella-selection-private", privateRoot)
	cmd := exec.Command("/bin/sh", "-c", script)
	cmd.Env = append(os.Environ(), "MISE_LOG="+logPath)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("mixed artifact selection: %v\n%s\nscript:\n%s", err, output, script)
	}
	logBytes, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	log := string(logBytes)
	if strings.Contains(log, "where uv") {
		t.Fatalf("artifact hit invoked mise where: %s", log)
	}
	if !strings.Contains(log, "install ") || !strings.Contains(log, "where bun") {
		t.Fatalf("artifact miss did not run isolated install and lookup: %s", log)
	}
	for _, want := range []struct {
		name string
		text string
	}{
		{"uv", "hit-uv\n"},
		{"bun", "miss-bun\n"},
	} {
		result := exec.Command(filepath.Join(selectionRoot, "bin", want.name))
		output, runErr := result.CombinedOutput()
		if runErr != nil || string(output) != want.text {
			t.Fatalf("selected %s output=%q err=%v", want.name, output, runErr)
		}
	}
}
