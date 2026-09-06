package sandbox

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sync"
)

// EnsureUserMiseHome creates the per-user mise tree at userToolsDir and seeds it
// with the shared system installs. Pointing a session's MISE_DATA_DIR at this
// tree gives the agent a real, writable mise home — it resolves the builtin
// tools (via the seeded symlinks) and can install its own tools/versions
// alongside, layered above the read-only system installs. Idempotent and safe to
// call before every session; a no-op when userToolsDir is empty.
//
// The tree is created 0700 (like the sibling per-user temp dir) so other host
// users can't read one tenant's installed tools, and every structural directory
// is verified to be a real directory — never a symlink. The tree is mounted
// writable into the sandbox and persists across sessions, so a prior session's
// agent could replace a structural dir with a symlink; since this runs unsandboxed
// with server privileges, following such a link would let seeding escape the tree.
//
// The "shims" dir is seeded empty on purpose: per-user shims fill in only as the
// agent installs its own tools. Any shims the agent did create are relinked to a
// relative target so a persisted tree stays usable if the same user later runs
// under a different backend.
func EnsureUserMiseHome(stellaHome, userToolsDir string) error {
	if userToolsDir == "" {
		return nil
	}
	if err := ensureRealDir(userToolsDir); err != nil {
		return err
	}
	for _, sub := range []string{"installs", "shims", "cache", "state", "config"} {
		if err := ensureRealDir(filepath.Join(userToolsDir, sub)); err != nil {
			return err
		}
	}
	if err := pruneDanglingSeedLinks(filepath.Join(userToolsDir, "installs")); err != nil {
		return err
	}
	if err := seedSystemInstalls(MiseToolsDir(stellaHome), userToolsDir); err != nil {
		return err
	}
	return relinkUserShims(stellaHome, userToolsDir)
}

// pruneDanglingSeedLinks removes per-user install symlinks whose target no longer
// resolves — e.g. a system version pruned or upgraded after it was seeded. Only
// broken symlinks are touched; real user installs (directories) and still-valid
// seed links are left alone, so the next seed pass heals the tree without
// clobbering anything the agent installed.
func pruneDanglingSeedLinks(userInstalls string) error {
	tools, err := os.ReadDir(userInstalls)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("sandbox: read per-user installs: %w", err)
	}
	for _, tool := range tools {
		toolDir := filepath.Join(userInstalls, tool.Name())
		versions, err := os.ReadDir(toolDir)
		if err != nil {
			continue
		}
		for _, v := range versions {
			if v.Type()&os.ModeSymlink == 0 {
				continue // real user install, not a seed link
			}
			link := filepath.Join(toolDir, v.Name())
			if _, err := os.Stat(link); err != nil {
				_ = os.Remove(link) // target gone: drop the dangling link
			}
		}
	}
	return nil
}

// EnsureMiseShims prepares the mise shims a sandbox session resolves against,
// before the session's PATH is used. It relinks the shared system-tree shims
// (always) and, when userToolsDir is set, seeds and relinks the per-user tree via
// EnsureUserMiseHome. Splitting the two trees keeps each concern separate while a
// single call site guarantees both resolve under the active backend's remap.
// All backends call this, including docker: the relinked relative shims and
// seeded per-user tree are mounted into the container and resolve against the
// image's in-image system tree (#436).
func EnsureMiseShims(stellaHome, userToolsDir string) error {
	if err := RelinkSystemMiseShims(stellaHome); err != nil {
		return err
	}
	if userToolsDir == "" {
		return nil
	}
	return EnsureUserMiseHome(stellaHome, userToolsDir)
}

// shimRelinkMu serializes shim relinking. The system shims dir is shared by every
// session, so concurrent session starts would otherwise race on the per-shim temp
// symlink. Relinking is fast (one readlink per shim once already relative), so a
// single lock across all trees is cheap.
var shimRelinkMu sync.Mutex

// miseBinName is the mise binary file name for the current platform.
func miseBinName() string {
	if runtime.GOOS == "windows" {
		return "mise.exe"
	}
	return "mise"
}

// RelinkSystemMiseShims rewrites the shared system-tree shims to relative targets.
// Idempotent and safe to call before every session; see relinkShimsToLocalMise.
func RelinkSystemMiseShims(stellaHome string) error {
	return relinkShimsToLocalMise(stellaHome, MiseShimsDir(stellaHome))
}

// RelinkMiseShims rewrites managed shims in an arbitrary selection-local
// directory to the embedded mise binary. Context-specific installers use this
// after reshim so a host path can never leak into a sandbox-visible shim.
func RelinkMiseShims(stellaHome, shimsDir string) error {
	if shimsDir == "" {
		return nil
	}
	return relinkShimsToLocalMise(stellaHome, shimsDir)
}

// relinkUserShims rewrites the per-user shims (those the agent created with mise
// reshim inside a sandbox) to relative targets. Mirrors RelinkSystemMiseShims for
// the per-user tree.
func relinkUserShims(stellaHome, userToolsDir string) error {
	return relinkShimsToLocalMise(stellaHome, MiseUserShimsDir(userToolsDir))
}

// relinkShimsToLocalMise rewrites every symlink shim in shimsDir to a relative
// path targeting $STELLA_HOME/bin/mise. mise reshim writes each shim with the
// absolute path of whichever mise ran it — a host path that does not exist once a
// backend remaps STELLA_HOME (bwrap's /opt/stella), which is the #505 failure:
// the shim resolves on the host but dangles in the sandbox. A relative target
// resolves identically on the host and under any remap. A no-op when
// $STELLA_HOME/bin/mise is absent: we never point a shim at an arbitrary host mise.
func relinkShimsToLocalMise(stellaHome, shimsDir string) error {
	miseBin := filepath.Join(stellaHome, "bin", miseBinName())
	if _, err := os.Stat(miseBin); err != nil {
		return nil
	}
	relTarget, err := filepath.Rel(shimsDir, miseBin)
	if err != nil {
		return fmt.Errorf("sandbox: relative shim target: %w", err)
	}

	shimRelinkMu.Lock()
	defer shimRelinkMu.Unlock()

	entries, err := os.ReadDir(shimsDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("sandbox: read shims dir: %w", err)
	}
	for _, e := range entries {
		if e.Type()&os.ModeSymlink == 0 {
			continue
		}
		shimPath := filepath.Join(shimsDir, e.Name())
		if target, err := os.Readlink(shimPath); err != nil || target == relTarget {
			continue
		}
		// Atomic replace: create a temp symlink, then rename over the old one.
		tmp := shimPath + ".tmp"
		_ = os.Remove(tmp)
		if err := os.Symlink(relTarget, tmp); err != nil {
			return fmt.Errorf("sandbox: relink shim %s: %w", e.Name(), err)
		}
		if err := os.Rename(tmp, shimPath); err != nil {
			_ = os.Remove(tmp)
			return fmt.Errorf("sandbox: relink shim %s: %w", e.Name(), err)
		}
	}
	return nil
}

// ensureRealDir creates dir (0700) if missing, or verifies an existing entry is a
// real directory. A symlink left in the writable, persisted per-user tree is
// removed (os.Remove targets the link, not what it points at) and replaced with a
// real dir, so later MkdirAll/Symlink can't write through it to escape the tree.
func ensureRealDir(dir string) error {
	fi, err := os.Lstat(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("sandbox: create per-user mise dir: %w", err)
		}
	case err != nil:
		return fmt.Errorf("sandbox: stat per-user mise dir: %w", err)
	case fi.Mode()&os.ModeSymlink != 0:
		if err := os.Remove(dir); err != nil {
			return fmt.Errorf("sandbox: remove symlinked per-user mise dir %q: %w", dir, err)
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("sandbox: recreate per-user mise dir: %w", err)
		}
	case !fi.IsDir():
		return fmt.Errorf("sandbox: per-user mise path %q is not a directory", dir)
	}
	return nil
}

// seedSystemInstalls mirrors each system installs/<tool>/<version> into the
// per-user installs dir as a relative symlink. Relative targets resolve
// identically on the host and inside the sandbox — both trees are remapped under
// the same root — the same trick mise shims use (see relinkShims). Existing
// entries are left untouched, so a real user install of the same tool/version
// shadows the system one and re-seeding never clobbers user state.
func seedSystemInstalls(systemToolsDir, userToolsDir string) error {
	sysInstalls := filepath.Join(systemToolsDir, "installs")
	tools, err := os.ReadDir(sysInstalls)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("sandbox: read system installs: %w", err)
	}
	userInstalls := filepath.Join(userToolsDir, "installs")
	for _, tool := range tools {
		if !tool.IsDir() {
			continue
		}
		versions, err := os.ReadDir(filepath.Join(sysInstalls, tool.Name()))
		if errors.Is(err, fs.ErrNotExist) {
			// The shared system tree is mutated concurrently by the manifest/org
			// reconciler; a tool dir can vanish between the outer and inner read.
			// A transient race must not fail an unrelated user's session start.
			continue
		}
		if err != nil {
			return fmt.Errorf("sandbox: read system installs for %s: %w", tool.Name(), err)
		}
		userToolDir := filepath.Join(userInstalls, tool.Name())
		for _, v := range versions {
			link := filepath.Join(userToolDir, v.Name())
			if _, err := os.Lstat(link); err == nil {
				continue // already seeded, or shadowed by a real user install
			}
			if err := ensureRealDir(userToolDir); err != nil {
				return err
			}
			target, err := filepath.Rel(userToolDir, filepath.Join(sysInstalls, tool.Name(), v.Name()))
			if err != nil {
				return fmt.Errorf("sandbox: relative seed target: %w", err)
			}
			if err := os.Symlink(target, link); err != nil && !errors.Is(err, fs.ErrExist) {
				return fmt.Errorf("sandbox: seed install symlink: %w", err)
			}
		}
	}
	return nil
}
