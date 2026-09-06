package resources

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestGenerateBuiltinManifestNestedRootAndExecutable(t *testing.T) {
	root := t.TempDir()
	writeTestBuiltin(t, root, "core/demo/SKILL.md", "---\nname: demo\ndescription: Demo\ntags: [one, two]\ndisable_model_invocation: true\nmetadata:\n  owner_plugin: tool/demo\n---\nbody\n", 0o644)
	writeTestBuiltin(t, root, "core/demo/scripts/run.sh", "#!/bin/sh\necho demo\n", 0o755)

	manifest, err := GenerateBuiltinManifest(root)
	if err != nil {
		t.Fatalf("GenerateBuiltinManifest: %v", err)
	}
	if len(manifest.Skills) != 1 || manifest.Skills[0].Root != "core/demo" {
		t.Fatalf("skills = %#v, want nested core/demo root", manifest.Skills)
	}
	skill := manifest.Skills[0]
	if skill.Ref != "builtin:demo" || skill.APIID != "builtin-demo" || skill.Digest == "" || manifest.Revision == "" {
		t.Fatalf("incomplete descriptor: %#v", skill)
	}
	if owner := skill.Metadata["metadata"].(map[string]any)["owner_plugin"]; owner != "tool/demo" {
		t.Fatalf("owner_plugin = %#v, want tool/demo", owner)
	}
	if got := skill.Tags; len(got) != 2 || got[0] != "one" || got[1] != "two" {
		t.Fatalf("tags = %#v, want [one two]", got)
	}
	if !skill.DisableModelInvocation {
		t.Fatal("disable_model_invocation = false, want true")
	}
	if got := findBuiltinFile(t, skill.Files, "scripts/run.sh").Mode; got != 0o755 {
		t.Fatalf("script mode = %04o, want 0755", got)
	}
}

func TestGenerateBuiltinManifestDerivesOwnerFromTrustedPath(t *testing.T) {
	root := t.TempDir()
	writeTestBuiltin(t, root, "plugins/tool/demo/demo/SKILL.md", "---\nname: demo\nmetadata:\n  owner_plugin: tool/attacker\n---\nbody\n", 0o644)

	manifest, err := GenerateBuiltinManifest(root)
	if err != nil {
		t.Fatalf("GenerateBuiltinManifest: %v", err)
	}
	if got := manifest.Skills[0].OwnerPluginID; got != "tool/demo" {
		t.Fatalf("owner = %q, want tool/demo", got)
	}
	metadata := manifest.Skills[0].Metadata["metadata"].(map[string]any)
	if got := metadata["owner_plugin"]; got != "tool/attacker" {
		t.Fatalf("frontmatter owner = %#v, want preserved metadata only", got)
	}
}

func TestGenerateBuiltinManifestRejectsLegacySystemSkillRoot(t *testing.T) {
	root := t.TempDir()
	writeTestBuiltin(t, root, "system/demo/SKILL.md", "---\nname: demo\n---\nbody\n", 0o644)
	if _, err := GenerateBuiltinManifest(root); err == nil || !strings.Contains(err.Error(), "core/<skill>") {
		t.Fatalf("GenerateBuiltinManifest() error = %v, want structural owner rejection", err)
	}
}

func TestGenerateBuiltinManifestRejectsInvalidSource(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, root string)
		want  string
	}{
		{
			name: "duplicate names",
			setup: func(t *testing.T, root string) {
				writeTestBuiltin(t, root, "core/demo/SKILL.md", "---\nname: demo\n---\n", 0o644)
				writeTestBuiltin(t, root, "plugins/tool/other/demo/SKILL.md", "---\nname: demo\n---\n", 0o644)
			},
			want: "duplicate",
		},
		{
			name: "name mismatch",
			setup: func(t *testing.T, root string) {
				writeTestBuiltin(t, root, "core/demo/SKILL.md", "---\nname: other\n---\n", 0o644)
			},
			want: "does not match",
		},
		{
			name: "unsupported mode",
			setup: func(t *testing.T, root string) {
				writeTestBuiltin(t, root, "core/demo/SKILL.md", "---\nname: demo\n---\n", 0o600)
			},
			want: "unsupported mode",
		},
	}
	if runtime.GOOS != "windows" {
		tests = append(tests, struct {
			name  string
			setup func(t *testing.T, root string)
			want  string
		}{
			name: "symlink",
			setup: func(t *testing.T, root string) {
				writeTestBuiltin(t, root, "core/demo/SKILL.md", "---\nname: demo\n---\n", 0o644)
				if err := os.Symlink("SKILL.md", filepath.Join(root, "core", "demo", "linked.md")); err != nil {
					t.Fatalf("Symlink: %v", err)
				}
			},
			want: "symlink",
		})
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			test.setup(t, root)
			_, err := GenerateBuiltinManifest(root)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("GenerateBuiltinManifest() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestBuiltinManifestRejectsTraversal(t *testing.T) {
	files := []BuiltinSkillFile{{Path: "../escape", Digest: strings.Repeat("0", 64), Mode: 0o644}}
	manifest := BuiltinManifest{
		Revision: strings.Repeat("0", 64),
		Skills: []BuiltinSkillDescriptor{{
			Ref:    "builtin:demo",
			APIID:  "builtin-demo",
			Name:   "demo",
			Root:   "core/demo",
			Digest: builtinSkillDigest(files),
			Files:  files,
		}},
	}
	if err := validateBuiltinManifest(manifest); err == nil || !strings.Contains(err.Error(), "invalid builtin file descriptor") {
		t.Fatalf("validateBuiltinManifest() error = %v, want traversal rejection", err)
	}
}

func TestBuiltinManifestGenerationIsDeterministic(t *testing.T) {
	root := t.TempDir()
	writeTestBuiltin(t, root, "core/demo/SKILL.md", "---\nname: demo\n---\n", 0o644)
	writeTestBuiltin(t, root, "plugins/tool/other/other/SKILL.md", "---\nname: other\n---\n", 0o644)
	first, err := GenerateBuiltinManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	second, err := GenerateBuiltinManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	firstRendered, err := renderBuiltinManifest(first)
	if err != nil {
		t.Fatal(err)
	}
	secondRendered, err := renderBuiltinManifest(second)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(firstRendered, secondRendered) {
		t.Fatal("manifest generation is not deterministic")
	}
}

func TestWritableBundleModeProjection(t *testing.T) {
	for _, test := range []struct {
		name     string
		actual   fs.FileMode
		expected fs.FileMode
		want     bool
	}{
		{name: "writable source", actual: 0o666, expected: 0o644, want: true},
		{name: "executable source", actual: 0o666, expected: 0o755, want: true},
		{name: "read-only marker", actual: 0o444, expected: 0o444, want: true},
		{name: "writable marker", actual: 0o666, expected: 0o444, want: false},
		{name: "read-only source", actual: 0o444, expected: 0o644, want: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := writableBundleModeMatches(test.actual, test.expected); got != test.want {
				t.Fatalf("writableBundleModeMatches(%04o, %04o) = %t, want %t", test.actual, test.expected, got, test.want)
			}
		})
	}
}

func TestBuiltinBundleInstallerVerifiesTamperingAndDoesNotRewrite(t *testing.T) {
	registry := testBuiltinRegistry(t)
	home := t.TempDir()
	bundle, err := registry.InstallBuiltinBundle(home)
	if err != nil {
		t.Fatalf("InstallBuiltinBundle: %v", err)
	}
	script := filepath.Join(bundle, "core", "demo", "scripts", "run.sh")
	info, err := os.Stat(script)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("installed script mode = %04o, want 0755", info.Mode().Perm())
	}
	rootInfo, err := os.Stat(bundle)
	if err != nil {
		t.Fatal(err)
	}
	if !bundleDirectoryModeMatches(rootInfo.Mode()) {
		t.Fatalf("installed bundle root mode = %04o, want traversable projection", rootInfo.Mode().Perm())
	}
	firstModTime := info.ModTime()
	time.Sleep(20 * time.Millisecond)
	if _, err := registry.InstallBuiltinBundle(home); err != nil {
		t.Fatalf("second InstallBuiltinBundle: %v", err)
	}
	info, err = os.Stat(script)
	if err != nil {
		t.Fatal(err)
	}
	if !info.ModTime().Equal(firstModTime) {
		t.Fatal("verified reinstall rewrote a bundle file")
	}
	if err := os.WriteFile(script, []byte("tampered"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := registry.VerifyBuiltinBundle(home); err == nil {
		t.Fatal("VerifyBuiltinBundle accepted tampered content")
	}
	if _, err := registry.InstallBuiltinBundle(home); err != nil {
		t.Fatalf("repair InstallBuiltinBundle: %v", err)
	}
	if err := registry.VerifyBuiltinBundle(home); err != nil {
		t.Fatalf("VerifyBuiltinBundle after repair: %v", err)
	}
	marker := filepath.Join(bundle, bundleCompleteMarker)
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(marker, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := registry.VerifyBuiltinBundle(home); err == nil {
		t.Fatal("VerifyBuiltinBundle accepted a completion-marker directory")
	}
}

func TestBuiltinBundleInstallerRepairsNonTraversableRoot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows directory traversal is controlled by ACLs")
	}
	registry := testBuiltinRegistry(t)
	home := t.TempDir()
	bundle, err := registry.InstallBuiltinBundle(home)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(bundle, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := registry.VerifyBuiltinBundle(home); err == nil {
		t.Fatal("VerifyBuiltinBundle accepted a non-traversable bundle root")
	}
	if _, err := registry.InstallBuiltinBundle(home); err != nil {
		t.Fatalf("repair InstallBuiltinBundle: %v", err)
	}
	info, err := os.Stat(bundle)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("repaired bundle root mode = %04o, want 0755", info.Mode().Perm())
	}
}

func TestBuiltinBundleInstallerQuarantinesBeforePublishingRepair(t *testing.T) {
	registry := testBuiltinRegistry(t)
	home := t.TempDir()
	bundle, err := registry.InstallBuiltinBundle(home)
	if err != nil {
		t.Fatal(err)
	}
	tampered := filepath.Join(bundle, "core", "demo", "SKILL.md")
	if err := os.WriteFile(tampered, []byte("tampered"), 0o644); err != nil {
		t.Fatal(err)
	}

	var quarantine string
	renames := 0
	rename := func(oldpath, newpath string) error {
		renames++
		switch renames {
		case 1:
			if oldpath != bundle || !strings.Contains(filepath.Base(newpath), ".invalid-") {
				t.Fatalf("quarantine rename = %q -> %q", oldpath, newpath)
			}
			quarantine = newpath
			if err := os.Rename(oldpath, newpath); err != nil {
				return err
			}
			if _, err := os.Lstat(bundle); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("published pathname after quarantine = %v, want absent", err)
			}
			if got, err := os.ReadFile(filepath.Join(quarantine, "core", "demo", "SKILL.md")); err != nil || string(got) != "tampered" {
				t.Fatalf("quarantined tree = %q, %v; want intact invalid tree", got, err)
			}
			if _, err := os.Stat(filepath.Join(quarantine, bundleCompleteMarker)); err != nil {
				t.Fatalf("quarantined completion marker: %v", err)
			}
			return nil
		case 2:
			if newpath != bundle || !strings.Contains(filepath.Base(oldpath), ".tmp-") {
				t.Fatalf("publication rename = %q -> %q", oldpath, newpath)
			}
			if _, err := os.Lstat(bundle); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("published pathname before publication = %v, want absent", err)
			}
			return os.Rename(oldpath, newpath)
		default:
			t.Fatalf("unexpected rename %q -> %q", oldpath, newpath)
			return nil
		}
	}

	if got, err := registry.installBuiltinBundle(home, rename); err != nil || got != bundle {
		t.Fatalf("installBuiltinBundle() = %q, %v; want %q", got, err, bundle)
	}
	if err := registry.VerifyBuiltinBundle(home); err != nil {
		t.Fatalf("VerifyBuiltinBundle: %v", err)
	}
	if _, err := os.Lstat(quarantine); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("quarantine cleanup = %v, want absent", err)
	}
}

func TestBuiltinBundleInstallerRestoresQuarantineAfterPublicationFailure(t *testing.T) {
	registry := testBuiltinRegistry(t)
	home := t.TempDir()
	bundle, err := registry.InstallBuiltinBundle(home)
	if err != nil {
		t.Fatal(err)
	}
	tampered := filepath.Join(bundle, "core", "demo", "SKILL.md")
	if err := os.WriteFile(tampered, []byte("tampered"), 0o644); err != nil {
		t.Fatal(err)
	}

	publishErr := errors.New("injected publication failure")
	var quarantine string
	renames := 0
	rename := func(oldpath, newpath string) error {
		renames++
		switch renames {
		case 1:
			quarantine = newpath
			return os.Rename(oldpath, newpath)
		case 2:
			return publishErr
		case 3:
			if oldpath != quarantine || newpath != bundle {
				t.Fatalf("restore rename = %q -> %q", oldpath, newpath)
			}
			return os.Rename(oldpath, newpath)
		default:
			t.Fatalf("unexpected rename %q -> %q", oldpath, newpath)
			return nil
		}
	}

	if _, err := registry.installBuiltinBundle(home, rename); !errors.Is(err, publishErr) || !strings.Contains(err.Error(), "restored previous invalid bundle") {
		t.Fatalf("installBuiltinBundle() error = %v, want publication failure with restoration", err)
	}
	if got, err := os.ReadFile(tampered); err != nil || string(got) != "tampered" {
		t.Fatalf("restored final = %q, %v; want intact previous tree", got, err)
	}
	if _, err := os.Lstat(quarantine); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("restored quarantine = %v, want absent", err)
	}
}

func TestBuiltinBundleInstallerPreservesQuarantineWhenRestorationFails(t *testing.T) {
	registry := testBuiltinRegistry(t)
	home := t.TempDir()
	bundle, err := registry.InstallBuiltinBundle(home)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundle, "core", "demo", "SKILL.md"), []byte("tampered"), 0o644); err != nil {
		t.Fatal(err)
	}

	publishErr := errors.New("injected publication failure")
	restoreErr := errors.New("injected restoration failure")
	var quarantine string
	renames := 0
	rename := func(oldpath, newpath string) error {
		renames++
		switch renames {
		case 1:
			quarantine = newpath
			return os.Rename(oldpath, newpath)
		case 2:
			return publishErr
		case 3:
			return restoreErr
		default:
			t.Fatalf("unexpected rename %q -> %q", oldpath, newpath)
			return nil
		}
	}

	_, err = registry.installBuiltinBundle(home, rename)
	if !errors.Is(err, publishErr) || !errors.Is(err, restoreErr) {
		t.Fatalf("installBuiltinBundle() error = %v, want joined publication and restoration failures", err)
	}
	if _, err := os.Lstat(bundle); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("published pathname = %v, want absent rather than partial", err)
	}
	if got, err := os.ReadFile(filepath.Join(quarantine, "core", "demo", "SKILL.md")); err != nil || string(got) != "tampered" {
		t.Fatalf("preserved quarantine = %q, %v; want intact previous tree", got, err)
	}
	if _, err := os.Stat(filepath.Join(quarantine, bundleCompleteMarker)); err != nil {
		t.Fatalf("preserved quarantine completion marker: %v", err)
	}
}

func TestBuiltinBundleInstallerFailsClosedWhenQuarantineRenameFails(t *testing.T) {
	registry := testBuiltinRegistry(t)
	home := t.TempDir()
	bundle, err := registry.InstallBuiltinBundle(home)
	if err != nil {
		t.Fatal(err)
	}
	tampered := filepath.Join(bundle, "core", "demo", "SKILL.md")
	if err := os.WriteFile(tampered, []byte("tampered"), 0o644); err != nil {
		t.Fatal(err)
	}

	renameErr := errors.New("injected open-handle rename failure")
	if _, err := registry.installBuiltinBundle(home, func(string, string) error { return renameErr }); !errors.Is(err, renameErr) {
		t.Fatalf("installBuiltinBundle() error = %v, want quarantine rename failure", err)
	}
	if got, err := os.ReadFile(tampered); err != nil || string(got) != "tampered" {
		t.Fatalf("final after failed quarantine = %q, %v; want untouched invalid tree", got, err)
	}
	if _, err := os.Stat(filepath.Join(bundle, bundleCompleteMarker)); err != nil {
		t.Fatalf("final completion marker after failed quarantine: %v", err)
	}
}

func TestBuiltinBundleInstallerConcurrentRepairersAreVerified(t *testing.T) {
	registry := testBuiltinRegistry(t)
	home := t.TempDir()
	bundle, err := registry.InstallBuiltinBundle(home)
	if err != nil {
		t.Fatalf("initial InstallBuiltinBundle: %v", err)
	}
	if err := os.WriteFile(filepath.Join(bundle, "core", "demo", "SKILL.md"), []byte("tampered"), 0o644); err != nil {
		t.Fatalf("tamper bundle: %v", err)
	}
	const installers = 12
	errs := make(chan error, installers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for range installers {
		wg.Go(func() {
			<-start
			_, err := registry.InstallBuiltinBundle(home)
			errs <- err
		})
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent install: %v", err)
		}
	}
	if err := registry.VerifyBuiltinBundle(home); err != nil {
		t.Fatalf("VerifyBuiltinBundle: %v", err)
	}
}

func TestBundleInstallLockSerializesProcesses(t *testing.T) {
	const lockEnv = "STELLA_TEST_BUNDLE_LOCK"
	if lockPath := os.Getenv(lockEnv); lockPath != "" {
		fmt.Println("ready")
		unlock, err := lockBundleInstall(lockPath)
		if err != nil {
			t.Fatalf("child lockBundleInstall: %v", err)
		}
		fmt.Println("acquired")
		if err := unlock(); err != nil {
			t.Fatalf("child unlock: %v", err)
		}
		return
	}

	lockPath := filepath.Join(t.TempDir(), "bundle.lock")
	unlock, err := lockBundleInstall(lockPath)
	if err != nil {
		t.Fatalf("parent lockBundleInstall: %v", err)
	}
	locked := true
	defer func() {
		if locked {
			_ = unlock()
		}
	}()

	cmd := exec.Command(os.Args[0], "-test.run=^TestBundleInstallLockSerializesProcesses$")
	cmd.Env = append(os.Environ(), lockEnv+"="+lockPath)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("child stdout pipe: %v", err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start child: %v", err)
	}
	reader := bufio.NewReader(stdout)
	if line, err := reader.ReadString('\n'); err != nil || line != "ready\n" {
		t.Fatalf("child ready = %q, %v; stderr: %s", line, err, stderr.String())
	}
	acquired := make(chan string, 1)
	go func() {
		line, _ := reader.ReadString('\n')
		acquired <- line
	}()
	select {
	case line := <-acquired:
		t.Fatalf("child acquired parent-held lock early: %q", line)
	case <-time.After(100 * time.Millisecond):
	}
	if err := unlock(); err != nil {
		t.Fatalf("parent unlock: %v", err)
	}
	locked = false
	select {
	case line := <-acquired:
		if line != "acquired\n" {
			t.Fatalf("child acquisition = %q, want acquired", line)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("child did not acquire released lock")
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("child wait: %v; stderr: %s", err, stderr.String())
	}
}

func TestBuiltinBundleRejectsInvalidProjectionDirectory(t *testing.T) {
	registry := testBuiltinRegistry(t)

	t.Run("non-directory", func(t *testing.T) {
		home := t.TempDir()
		if err := os.WriteFile(filepath.Join(home, "bundles"), []byte("not a directory"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := registry.InstallBuiltinBundle(home); err == nil || !strings.Contains(err.Error(), "non-symlink directory") {
			t.Fatalf("InstallBuiltinBundle() error = %v, want projection directory rejection", err)
		}
		if err := registry.VerifyBuiltinBundle(home); err == nil || !strings.Contains(err.Error(), "non-symlink directory") {
			t.Fatalf("VerifyBuiltinBundle() error = %v, want projection directory rejection", err)
		}
	})

	if runtime.GOOS == "windows" {
		return
	}
	t.Run("symlink", func(t *testing.T) {
		home := t.TempDir()
		if err := os.Symlink(t.TempDir(), filepath.Join(home, "bundles")); err != nil {
			t.Skipf("create bundles symlink: %v", err)
		}
		if _, err := registry.InstallBuiltinBundle(home); err == nil || !strings.Contains(err.Error(), "non-symlink directory") {
			t.Fatalf("InstallBuiltinBundle() error = %v, want projection symlink rejection", err)
		}
		if err := registry.VerifyBuiltinBundle(home); err == nil || !strings.Contains(err.Error(), "non-symlink directory") {
			t.Fatalf("VerifyBuiltinBundle() error = %v, want projection symlink rejection", err)
		}
	})

	t.Run("configured home symlink is allowed", func(t *testing.T) {
		target := t.TempDir()
		link := filepath.Join(t.TempDir(), "stella-home")
		if err := os.Symlink(target, link); err != nil {
			t.Skipf("create configured home symlink: %v", err)
		}
		if _, err := registry.InstallBuiltinBundle(link); err != nil {
			t.Fatalf("InstallBuiltinBundle through configured home symlink: %v", err)
		}
	})
}

func TestVerifiedPublishedBundleFailsClosed(t *testing.T) {
	registry := testBuiltinRegistry(t)
	home := t.TempDir()
	bundle, err := registry.InstallBuiltinBundle(home)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(bundle, bundleCompleteMarker), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundle, bundleCompleteMarker), []byte("tampered"), 0o444); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.verifiedPublishedBundle(home, bundle); err == nil {
		t.Fatal("verifiedPublishedBundle accepted a tampered final tree")
	}
}

func testBuiltinRegistry(t *testing.T) *Registry {
	t.Helper()
	source := t.TempDir()
	writeTestBuiltin(t, source, "skills/core/demo/SKILL.md", "---\nname: demo\ndescription: Demo\n---\nbody\n", 0o644)
	writeTestBuiltin(t, source, "skills/core/demo/scripts/run.sh", "#!/bin/sh\necho demo\n", 0o755)
	manifest, err := GenerateBuiltinManifest(filepath.Join(source, "skills"))
	if err != nil {
		t.Fatal(err)
	}
	registry, err := LoadBuiltin(os.DirFS(source), manifest)
	if err != nil {
		t.Fatal(err)
	}
	return registry
}

func writeTestBuiltin(t *testing.T, root, name, contents string, mode fs.FileMode) {
	t.Helper()
	filename := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filename, []byte(contents), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filename, mode); err != nil {
		t.Fatal(err)
	}
}

func findBuiltinFile(t *testing.T, files []BuiltinSkillFile, name string) BuiltinSkillFile {
	t.Helper()
	for _, file := range files {
		if file.Path == name {
			return file
		}
	}
	t.Fatalf("file %q not found in %v", name, files)
	return BuiltinSkillFile{}
}

func ExampleRegistry_BundleRevision() {
	registry, err := Default()
	if err != nil {
		panic(err)
	}
	fmt.Println(len(registry.BundleRevision()))
	// Output: 64
}
