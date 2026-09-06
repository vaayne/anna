//go:build !windows

package resources

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
)

func TestBuiltinBundleDirectoriesIgnoreRestrictiveUmaskAndRepair(t *testing.T) {
	const childEnv = "STELLA_TEST_BUNDLE_UMASK"
	if os.Getenv(childEnv) == "1" {
		oldUmask := syscall.Umask(0o077)
		defer syscall.Umask(oldUmask)

		registry := testBuiltinRegistry(t)
		home := t.TempDir()
		bundle, err := registry.InstallBuiltinBundle(home)
		if err != nil {
			t.Fatalf("InstallBuiltinBundle: %v", err)
		}
		if err := filepath.WalkDir(bundle, func(filename string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if !entry.IsDir() {
				return nil
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			if info.Mode().Perm() != 0o755 {
				t.Fatalf("bundle directory %q mode = %04o, want 0755", filename, info.Mode().Perm())
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}

		nested := filepath.Join(bundle, "core", "demo", "scripts")
		if err := os.Chmod(nested, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := registry.VerifyBuiltinBundle(home); err == nil {
			t.Fatal("VerifyBuiltinBundle accepted a non-traversable nested directory")
		}
		if _, err := registry.InstallBuiltinBundle(home); err != nil {
			t.Fatalf("repair InstallBuiltinBundle: %v", err)
		}
		info, err := os.Stat(nested)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o755 {
			t.Fatalf("repaired nested directory mode = %04o, want 0755", info.Mode().Perm())
		}
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestBuiltinBundleDirectoriesIgnoreRestrictiveUmaskAndRepair$")
	cmd.Env = append(os.Environ(), childEnv+"=1")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("restrictive-umask child: %v\n%s", err, output)
	}
}
