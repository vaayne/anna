package sandbox

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

// EnvRunnerPath snapshots the backend-rendered PATH so login profiles cannot
// discard or reorder the runner's executable search contract. Runtime owns the
// value; user configuration must not supply it.
const EnvRunnerPath = "STELLA_RUNNER_PATH"

// EnvNativeSelectionDir carries the exact native selection directory through
// backend policy rendering. It is runtime-owned; backends must prefer it over
// the legacy mise shims field when rebuilding PATH.
const EnvNativeSelectionDir = "STELLA_NATIVE_SELECTION_DIR"

// EnvUserNativeSelectionDir carries the exact user/user-agent selection. It
// stays separate from the system selection because Linux maps system binaries
// onto /opt/stella/bin while user selections remain secondary mounts.
const EnvUserNativeSelectionDir = "STELLA_USER_NATIVE_SELECTION_DIR"

// EnvCoreRuntimeDir carries the mandatory release command selection.
// It is runtime-owned and never copied from Vault.
const EnvCoreRuntimeDir = "STELLA_CORE_RUNTIME_DIR"

// StellaHomeSandboxDirs returns the core runtime directories (relative to
// STELLA_HOME) that sandbox backends may expose. Selection-owned mise contexts
// and artifacts are mounted separately by the runner; keeping the shared
// .mise-tools root out of this list prevents one runner from reading another's
// selection config.
func StellaHomeSandboxDirs() []string {
	return []string{
		"bin",
	}
}

// MiseToolsDir returns the root MISE_DATA_DIR for Stella-managed mise installs.
// This is the single source of truth for the on-disk layout: the manifest/org
// reconcilers install into it and the sandbox PATH is built from it, so both
// sides must derive their paths from here to stay in lockstep.
func MiseToolsDir(stellaHome string) string {
	return filepath.Join(stellaHome, ".mise-tools")
}

// MiseShimsDir returns the mise shims directory for host-execution sandbox
// backends. Tools installed by the manifest/org reconcilers are exposed here as
// shims (not copied into bin), so it must be on PATH for them to resolve.
func MiseShimsDir(stellaHome string) string {
	return filepath.Join(MiseToolsDir(stellaHome), "shims")
}

// ShellEnvPath returns the read-only startup file that restores Stella-managed
// mise paths after a nested login shell replaces PATH. The file is installed by
// resources/binaries.EnsureTools alongside the embedded mise binary.
func ShellEnvPath(stellaHome string) string {
	return filepath.Join(stellaHome, "bin", ".stella-shell-env")
}

// miseUserKeyPattern restricts a per-user mise directory name to a single safe
// path component. Anything else falls back to the shared (read-only) system tree.
var miseUserKeyPattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// MiseUserToolsDir returns the per-principal writable MISE_DATA_DIR. It mirrors a
// real machine's per-user mise home: each principal gets one tree shared by all
// their agents, layered above the shared read-only system installs. principalDir
// is the home subtree — always "users", the only top-level isolation boundary
// (#442) — and id is the principal's key: a raw user ID, or a channel group's ID
// under a "group-" prefix so equal raw IDs across users and groups can never
// collide. Keying as {stellaHome}/{principalDir}/{id}/.mise-tools never depends on
// the agent, so all of a principal's agents share one tree. An empty or unsafe
// argument yields "" so callers fall back to the system tree.
func MiseUserToolsDir(stellaHome, principalDir, id string) string {
	if principalDir != "users" {
		return ""
	}
	if !miseUserKeyPattern.MatchString(id) {
		return ""
	}
	return filepath.Join(stellaHome, principalDir, id, ".mise-tools")
}

// MiseUserShimsDir returns the per-user mise shims directory. It is prepended to
// PATH ahead of the system shims so a user's own tool versions win.
func MiseUserShimsDir(userToolsDir string) string {
	return filepath.Join(userToolsDir, "shims")
}

// PerUserMiseDataDir returns the per-user MISE_DATA_DIR carried in a session's
// env, or "" when it points at the shared read-only system tree (or is unset).
// It lets a host backend recover the per-user mise home — to derive the shims
// dir for PATH — from the env it already remaps, so the FilesystemPolicy needs no
// mise-specific field. stellaHome is the host STELLA_HOME used to recognize the
// system tree; the returned path is whatever scope the env holds (host path here,
// the backend remaps it as needed).
//
// Precondition: env's MISE_DATA_DIR and stellaHome must be in the same scope
// (both host paths, or both sandbox paths). Callers that remap MISE_* env vars
// must read this before remapping (see adjustPolicy), since after remap the
// data dir no longer matches the host system tree this compares against.
func PerUserMiseDataDir(env map[string]string, stellaHome string) string {
	dir := env["MISE_DATA_DIR"]
	if dir == "" || dir == MiseToolsDir(stellaHome) {
		return ""
	}
	return dir
}

// HostEnvBuildPath returns a sanitized PATH suitable for host-execution sandbox
// backends (local, none). Only selection-local shims are prepended: the
// per-user tree wins over the system/system-agent selection, while the shared
// mise shims and the whole STELLA_HOME/bin directory stay out of the public
// command search path. The mise engine itself is invoked by its explicit path.
func HostEnvBuildPath(stellaHome, userShimsDir string, selectionShimsDirs ...string) string {
	stellaBin := filepath.Join(stellaHome, "bin")
	sharedShims := MiseShimsDir(stellaHome)
	entries := []string{userShimsDir}
	entries = append(entries, selectionShimsDirs...)
	if runtime.GOOS != "linux" {
		for entry := range strings.SplitSeq(os.Getenv("PATH"), string(os.PathListSeparator)) {
			if entry == stellaBin || entry == sharedShims {
				continue
			}
			entries = append(entries, entry)
		}
		return strings.Join(hostEnvDedupeEntries(entries), string(os.PathListSeparator))
	}

	for entry := range strings.SplitSeq(os.Getenv("PATH"), string(os.PathListSeparator)) {
		if hostEnvPathAllowed(entry, stellaBin, sharedShims) {
			entries = append(entries, entry)
		}
	}
	entries = append(entries,
		"/run/current-system/sw/bin",
		"/usr/local/sbin",
		"/usr/local/bin",
		"/usr/sbin",
		"/usr/bin",
		"/sbin",
		"/bin",
	)
	return strings.Join(hostEnvDedupeEntries(entries), string(os.PathListSeparator))
}

// HostEnvCopy copies a fixed allowlist of host environment variables into env.
// Only locale, terminal, and proxy variables are included.
func HostEnvCopy(env map[string]string) {
	for _, key := range []string{
		"TERM", "COLORTERM", "LANG", "LC_ALL", "LC_CTYPE", "TZ",
		"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY",
		"http_proxy", "https_proxy", "all_proxy", "no_proxy",
	} {
		if value := os.Getenv(key); value != "" {
			env[key] = value
		}
	}
}

// HostEnvPathAllowed reports whether a PATH entry is in the safe allowlist.
func HostEnvPathAllowed(entry, stellaBin string) bool {
	return hostEnvPathAllowed(entry, stellaBin)
}

func hostEnvPathAllowed(entry string, excluded ...string) bool {
	if entry == "" {
		return false
	}
	for _, path := range excluded {
		if path != "" && entry == path {
			return false
		}
	}
	for _, root := range []string{"/usr", "/bin", "/sbin", "/nix", "/run/current-system/sw"} {
		if entry == root || strings.HasPrefix(entry, root+"/") {
			return true
		}
	}
	return false
}

func hostEnvDedupeEntries(entries []string) []string {
	seen := make(map[string]struct{}, len(entries))
	out := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry == "" {
			continue
		}
		if _, ok := seen[entry]; ok {
			continue
		}
		seen[entry] = struct{}{}
		out = append(out, entry)
	}
	return out
}
