package manifest

import (
	"context"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"

	pkgsandbox "github.com/CherryHQ/stella/pkg/sandbox"
)

// miseToolsDir returns the MISE_DATA_DIR path for isolated mise installs. The
// layout is owned by pkg/sandbox so the install side and the sandbox PATH stay
// in lockstep.
func miseToolsDir(stellaHome string) string {
	return pkgsandbox.MiseToolsDir(stellaHome)
}

// findMiseBin returns the path to the mise binary. It prefers the Stella-managed
// binary in $STELLA_HOME/bin, then falls back to mise on PATH.
func findMiseBin(stellaHome string) (string, error) {
	local := filepath.Join(stellaHome, "bin", runtimeBinaryName("mise"))
	if _, err := os.Stat(local); err == nil {
		return local, nil
	}
	if path, err := exec.LookPath("mise"); err == nil {
		return path, nil
	}
	return "", fmt.Errorf("mise not found at %s or on PATH", local)
}

func bootstrapMise(_ context.Context, stellaHome string) error {
	_, err := findMiseBin(stellaHome)
	return err
}

var misePassthroughEnv = []string{
	"PATH",
	"SystemRoot",
	"WINDIR",
	"ComSpec",
	"PATHEXT",
	"TMPDIR",
	"TMP",
	"TEMP",
	"LANG",
	"LC_ALL",
	"SSL_CERT_FILE",
	"SSL_CERT_DIR",
	"HTTP_PROXY",
	"HTTPS_PROXY",
	"ALL_PROXY",
	"NO_PROXY",
	"http_proxy",
	"https_proxy",
	"all_proxy",
	"no_proxy",
	"GITHUB_TOKEN",
	"GH_TOKEN",
}

func isolatedMiseEnv(stellaHome string) ([]string, error) {
	return isolatedMiseEnvAt(stellaHome, miseToolsDir(stellaHome), filepath.Join(miseToolsDir(stellaHome), "shims"))
}

func isolatedMiseEnvAt(stellaHome, dataDir, shimsDir string) ([]string, error) {
	if dataDir == "" || shimsDir == "" {
		return nil, fmt.Errorf("empty mise data or shims directory")
	}
	// Directories the install needs on disk; the shared base (miseBaseEnv)
	// already covers DATA/CONFIG/CACHE/STATE, install adds shims + an isolated
	// HOME/XDG so nothing leaks into the host user's profile.
	installDirs := map[string]string{
		"MISE_SHIMS_DIR":  shimsDir,
		"HOME":            filepath.Join(dataDir, "home"),
		"XDG_CONFIG_HOME": filepath.Join(dataDir, "xdg", "config"),
		"XDG_CACHE_HOME":  filepath.Join(dataDir, "xdg", "cache"),
		"XDG_STATE_HOME":  filepath.Join(dataDir, "xdg", "state"),
	}
	if runtime.GOOS == "windows" {
		installDirs["USERPROFILE"] = installDirs["HOME"]
	}

	base := miseBaseEnv(stellaHome)
	base["MISE_DATA_DIR"] = dataDir
	base["MISE_CONFIG_DIR"] = filepath.Join(dataDir, "config")
	base["MISE_CACHE_DIR"] = filepath.Join(dataDir, "cache")
	base["MISE_STATE_DIR"] = filepath.Join(dataDir, "state")
	for _, dir := range []string{
		base["MISE_DATA_DIR"], base["MISE_CONFIG_DIR"], base["MISE_CACHE_DIR"], base["MISE_STATE_DIR"],
		installDirs["MISE_SHIMS_DIR"], installDirs["HOME"],
		installDirs["XDG_CONFIG_HOME"], installDirs["XDG_CACHE_HOME"], installDirs["XDG_STATE_HOME"],
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create isolated mise dir %s: %w", dir, err)
		}
	}

	env := make(map[string]string, len(misePassthroughEnv)+len(base)+len(installDirs))
	for _, key := range misePassthroughEnv {
		if value, ok := os.LookupEnv(key); ok {
			env[key] = value
		}
	}
	maps.Copy(env, base)
	maps.Copy(env, installDirs)

	keys := make([]string, 0, len(env))
	for key := range env {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	out := make([]string, 0, len(keys))
	for _, key := range keys {
		out = append(out, key+"="+env[key])
	}
	return out, nil
}

func runtimeBinaryName(name string) string {
	if runtime.GOOS == "windows" {
		return name + ".exe"
	}
	return name
}

// relinkShims rewrites the system-tree mise shims to relative paths so they
// resolve inside bwrap sandboxes where STELLA_HOME is remapped. The relink logic
// lives in pkg/sandbox alongside the per-user variant; this delegates so the
// install/reconcile path and the session path stay in lockstep. The second
// parameter is retained for the existing call sites and tests.
func relinkShims(stellaHome, _ string) error {
	return pkgsandbox.RelinkSystemMiseShims(stellaHome)
}

// stringOption returns a non-empty string tool option value.
func stringOption(options map[string]any, key string) (string, bool) {
	value, ok := options[key]
	if !ok {
		return "", false
	}
	s, ok := value.(string)
	return s, ok && s != ""
}
