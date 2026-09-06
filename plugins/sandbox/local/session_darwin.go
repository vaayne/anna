//go:build darwin

// macOS Seatbelt (sandbox-exec) isolation for the local backend.
// sandbox-exec is deprecated since macOS 10.15 but still ships and works;
// if Apple ever removes it, checkSandboxRequirements will catch the absence
// and the factory will fall through to the next available backend.
//
// Profile strategy: start with (allow default) so that dynamic linking,
// mach IPC, and getcwd all work without enumeration, then deny writes to the
// whole filesystem and re-allow writes only to the workspace root, temp dirs,
// and /dev. Network access is denied unless the policy requests allow_all.
// SBPL uses last-match-wins semantics, so ordering matters.
package local

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	sandboxpkg "github.com/CherryHQ/stella/pkg/sandbox"
	"github.com/CherryHQ/stella/plugins/sandbox/internal/sessionfs"
)

const seatbeltExecPath = "/usr/bin/sandbox-exec"

var (
	seatbeltOnce      sync.Once
	seatbeltAvailable bool
)

// seatbeltFunctional returns true when sandbox-exec is present and can run.
// Result is cached after the first call.
func seatbeltFunctional() bool {
	seatbeltOnce.Do(func() {
		if _, err := exec.LookPath(seatbeltExecPath); err != nil {
			return
		}
		// Probe with an allow-all profile to verify the binary works.
		if exec.Command(seatbeltExecPath, "-p", "(version 1)(allow default)", "/usr/bin/true").Run() == nil {
			seatbeltAvailable = true
		}
	})
	return seatbeltAvailable
}

func processVisiblePath(_ string, hostPath string) string { return hostPath }

// createSessionTmpMounts returns session-private identity mounts for the macOS
// temporary roots. Seatbelt has no bind mounts, so commands and FileAccess must
// both address each real backing directory directly.
func createSessionTmpMounts() ([]tmpMount, error) {
	tmp, err := os.MkdirTemp("", "stella-session-tmp-*")
	if err != nil {
		return nil, err
	}
	varTmp, err := os.MkdirTemp("", "stella-session-vartmp-*")
	if err != nil {
		os.RemoveAll(tmp) //nolint:errcheck
		return nil, err
	}
	return []tmpMount{
		{sandboxPath: tmp, realPath: tmp, owned: true, environment: true},
		{sandboxPath: varTmp, realPath: varTmp, owned: true},
	}, nil
}

// filesystemTempDir returns the macOS process view: Seatbelt has no path
// remapping, so TMPDIR names the designated session-private identity mount.
func filesystemTempDir(mounts []tmpMount) string {
	for _, mount := range mounts {
		if mount.environment && mount.sandboxPath != "" && mount.sandboxPath == mount.realPath {
			return mount.sandboxPath
		}
	}
	return os.TempDir()
}

// adjustStellaHome returns the sandbox-view STELLA_HOME directory.
// On macOS (Seatbelt), no path remapping; uses the real host path.
func adjustStellaHome(stellaHome string) string { return stellaHome }

// checkSandboxRequirements verifies that sandbox-exec is available and functional.
func checkSandboxRequirements() error {
	if !seatbeltFunctional() {
		return fmt.Errorf(
			"local sandbox: sandbox-exec (macOS Seatbelt) is required but not available; " +
				"this binary ships with macOS at /usr/bin/sandbox-exec",
		)
	}
	return nil
}

// buildSeatbeltProfile returns an SBPL profile string for the given policy.
//
// The profile uses (allow default) as the base so that system operations
// (dynamic linking, mach IPC, getcwd) work without enumerating every required
// path. All filesystem writes are then denied globally and re-allowed only for
// the workspace, temp directories, and device nodes. Network is denied when
// the policy requests NetworkDisabled.
//
// SBPL evaluates rules in order and the last match wins.
//
// stellaHomeHost is the host STELLA_HOME, used to recognize whether the policy
// carries a writable per-user mise tree (see the cache/state fallback below).
func buildSeatbeltProfile(policy sandboxpkg.Policy, mounts []sessionfs.Mount, workspace, stellaHomeHost string) string {
	networkMode := policy.NetworkModeOrDefault()

	var sb strings.Builder
	sb.WriteString("(version 1)\n")

	// Base: allow everything so the toolchain and shell work out of the box.
	sb.WriteString("(allow default)\n")
	if stellaHomeHost != "" {
		// Shared runtime roots contain stale versions and per-agent installer
		// state. A native session may read only the exact selection mounts below.
		for _, root := range []string{
			filepath.Join(stellaHomeHost, "bin"),
			filepath.Join(stellaHomeHost, ".mise-tools"),
			filepath.Join(stellaHomeHost, ".mise-managed"),
			filepath.Join(stellaHomeHost, ".mise-private"),
		} {
			if canonical, err := filepath.EvalSymlinks(root); err == nil {
				fmt.Fprintf(&sb, "(deny file-read* (subpath %q))\n", canonical)
			}
		}
		if policy.Env["STELLA_NATIVE_PREP"] == "true" {
			engine := filepath.Join(stellaHomeHost, "bin", "mise")
			if canonical, err := filepath.EvalSymlinks(engine); err == nil {
				// The preparation session is internal. Reopen only the mise engine,
				// never the other shared runtime aliases such as xberg.
				fmt.Fprintf(&sb, "(allow file-read* (literal %q))\n", canonical)
			}
			if canonical, err := filepath.EvalSymlinks(filepath.Join(stellaHomeHost, ".mise-managed")); err == nil {
				fmt.Fprintf(&sb, "(allow file-read* (subpath %q))\n", canonical)
			}
		}
		// Reopen only the immutable, content-addressed selections mounted for
		// this session. In particular, never reopen the shared bin or mise root.
		publicRoot := filepath.Join(stellaHomeHost, ".mise-tools", "public")
		managedPublicRoot := filepath.Join(stellaHomeHost, ".mise-managed")
		for _, mount := range mounts {
			if !mount.ReadOnly || (!pathWithin(publicRoot, mount.HostPath) && !pathWithin(managedPublicRoot, mount.HostPath)) {
				continue
			}
			if canonical, err := filepath.EvalSymlinks(mount.HostPath); err == nil {
				fmt.Fprintf(&sb, "(allow file-read* (subpath %q))\n", canonical)
			}
		}
	}

	// Deny all filesystem writes. Re-allows below (last-match-wins) carve out
	// the locations that the sandbox legitimately needs to write to.
	sb.WriteString("(deny file-write* (subpath \"/\"))\n")

	// Temp directories: process-local scratch space, macOS per-user temp, and
	// persistent scratch (mirrors Linux /var/tmp).
	sb.WriteString("(allow file-write* (subpath \"/private/tmp\"))\n")
	sb.WriteString("(allow file-write* (subpath \"/private/var/folders\"))\n")
	sb.WriteString("(allow file-write* (subpath \"/private/var/tmp\"))\n")

	// Dev nodes: required for stdout/stderr, pseudo-terminals, /dev/null, etc.
	sb.WriteString("(allow file-write* (subpath \"/dev\"))\n")

	// Writable mounts (e.g. the per-user mise home): carve out each subtree so the
	// agent can write through it — for mise that's installs/cache/state.
	for _, m := range mounts {
		if !m.ReadOnly {
			fmt.Fprintf(&sb, "(allow file-write* (subpath %q))\n", filepath.Clean(m.HostPath))
		}
	}
	// With no writable per-user mise tree, the shared system installs stay
	// read-only; mise still needs its cache/state metadata holes open. Key off the
	// per-user mise data dir recovered from the env — not the mount count — so an
	// unrelated writable mount cannot suppress these holes. This mirrors how the
	// none/linux backends recover the per-user mise home from the env.
	if sandboxpkg.PerUserMiseDataDir(policy.Env, stellaHomeHost) == "" {
		appendSeatbeltWritableEnvDirs(&sb, policy.Env)
	}

	// Workspace root: the agent's fully writable working tree.
	fmt.Fprintf(&sb, "(allow file-write* (subpath %q))\n", workspace)

	// Network: deny unless the policy explicitly requests unrestricted access.
	if networkMode != sandboxpkg.NetworkAllowAll {
		sb.WriteString("(deny network*)\n")
	}

	return sb.String()
}

func pathWithin(root, candidate string) bool {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(candidate))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

func appendSeatbeltWritableEnvDirs(sb *strings.Builder, env map[string]string) {
	for _, key := range []string{"MISE_CACHE_DIR", "MISE_STATE_DIR"} {
		dir := filepath.Clean(env[key])
		if dir == "." || dir == string(filepath.Separator) || !filepath.IsAbs(dir) {
			continue
		}
		fmt.Fprintf(sb, "(allow file-write* (subpath %q))\n", dir)
	}
}

// wrapCommand wraps name+args with sandbox-exec for macOS Seatbelt isolation.
func (s *localSession) wrapCommand(policy sandboxpkg.Policy, _, name string, args []string) (execPath string, execArgs []string, err error) {
	if !seatbeltFunctional() {
		return "", nil, fmt.Errorf(
			"local sandbox: sandbox-exec (macOS Seatbelt) is required but not available",
		)
	}

	resolved, lookErr := exec.LookPath(name)
	if lookErr != nil {
		return "", nil, fmt.Errorf("local exec: look up %q: %w", name, lookErr)
	}

	profile := buildSeatbeltProfile(policy, s.providerMounts, s.realRoot, s.stellaHomeHost)
	seatbeltArgs := []string{"-p", profile, resolved}
	seatbeltArgs = append(seatbeltArgs, args...)
	return seatbeltExecPath, seatbeltArgs, nil
}
