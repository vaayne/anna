package manifest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	pathpkg "path"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"sync"

	"github.com/CherryHQ/stella/internal/plugin"
	pkgplugins "github.com/CherryHQ/stella/pkg/plugins"
	pkgsandbox "github.com/CherryHQ/stella/pkg/sandbox"
)

// BinaryInstallPlan identifies one complete CLI selection. The identity is
// derived from every selected binary, including its config scope and revision,
// so two runners cannot overwrite one another's mise config or shim set.
// DataDir is shared only for immutable mise artifacts. ConfigPath and ShimsDir
// are used by the user-sandbox installer; native selections use PublicDir.
type BinaryInstallPlan struct {
	Identity   string
	DataDir    string
	ConfigPath string
	ShimsDir   string
	// PublicDir and PublicBinDir are native-only, exact selection paths. The
	// installer copies selected complete installs here and publishes direct
	// aliases, so a sandbox never needs the shared mise tree or its config.
	PublicDir     string
	PublicBinDir  string
	embeddedNames []string
}

// NativeMiseTool is the identity-free input accepted by release-owned runtime
// installers. Plugin configuration and snapshot identities do not belong in
// this primitive.
type NativeMiseTool struct {
	Key        string
	Version    string
	Options    map[string]any
	Lookup     string
	PublicName string
}

// NativeSelectionPlan names the immutable public directory for one native
// selection. It intentionally has no plugin or configuration identity.
type NativeSelectionPlan struct {
	DataDir       string
	PublicDir     string
	PublicBinDir  string
	EmbeddedNames []string
}

// InstallNativeMiseSelection installs fixed, release-owned mise tools into an
// exact public selection. The caller must have extracted embedded runtimes
// before invoking this primitive, so core and plugin packages share the same
// lower-level install and atomic publication behavior without importing each
// other's ownership model.
func InstallNativeMiseSelection(ctx context.Context, stellaHome string, selection NativeSelectionPlan, tools []NativeMiseTool) error {
	if stellaHome == "" || selection.DataDir == "" || selection.PublicDir == "" {
		return errors.New("manifest: native selection paths are required")
	}
	publicBinDir := selection.PublicBinDir
	if publicBinDir == "" {
		publicBinDir = selection.PublicDir
	}
	plan := BinaryInstallPlan{
		DataDir:       selection.DataDir,
		PublicDir:     selection.PublicDir,
		PublicBinDir:  publicBinDir,
		embeddedNames: slices.Clone(selection.EmbeddedNames),
	}
	nativeTools := make([]miseTool, 0, len(tools))
	for _, tool := range tools {
		nativeTools = append(nativeTools, miseTool{
			Key:        tool.Key,
			Version:    tool.Version,
			Options:    maps.Clone(tool.Options),
			Lookup:     tool.Lookup,
			PublicName: tool.PublicName,
		})
	}
	if len(nativeTools) == 0 {
		return materializeNativeRuntimeSelection(stellaHome, plan)
	}
	return runContextInstallWithCore(ctx, stellaHome, plan, nativeTools, true)
}

// BinaryConfigLayer selects which mise precedence layer a plan represents.
// System selections replace MISE_SYSTEM_CONFIG_FILE; user selections replace
// MISE_GLOBAL_CONFIG_FILE. Both layers share the plan's selection-local shims.
type BinaryConfigLayer uint8

const (
	BinarySystemLayer BinaryConfigLayer = iota
	BinaryUserLayer
)

var nativePublicationMu sync.Mutex

// OverlayBinaryInstallPlan applies a completed plan to a runner environment.
// It only changes runner-owned mise fields and PATH; caller-supplied secrets or
// unrelated environment entries are copied unchanged.
func OverlayBinaryInstallPlan(base map[string]string, plan BinaryInstallPlan, layer BinaryConfigLayer) map[string]string {
	env := maps.Clone(base)
	if plan.PublicBinDir != "" {
		// The system layer removes ambient state once. The user layer is
		// applied afterwards and must retain the selected system configuration.
		if layer == BinarySystemLayer {
			clearNativeMisePaths(env)
		}
		if layer == BinaryUserLayer {
			env[pkgsandbox.EnvUserNativeSelectionDir] = plan.PublicBinDir
		} else {
			env[pkgsandbox.EnvNativeSelectionDir] = plan.PublicBinDir
		}
		env["PATH"] = prependPath(env["PATH"], plan.PublicBinDir)
		env[pkgsandbox.EnvRunnerPath] = env["PATH"]
	}
	env["MISE_SHIMS_DIR"] = plan.ShimsDir
	if plan.ConfigPath != "" {
		if layer == BinaryUserLayer {
			env["MISE_GLOBAL_CONFIG_FILE"] = plan.ConfigPath
		} else {
			env["MISE_SYSTEM_CONFIG_FILE"] = plan.ConfigPath
		}
		trusted := []string{plan.ConfigPath}
		for path := range strings.SplitSeq(env["MISE_TRUSTED_CONFIG_PATHS"], string(filepath.ListSeparator)) {
			if path == "" || path == plan.ConfigPath {
				continue
			}
			trusted = append(trusted, path)
		}
		env["MISE_TRUSTED_CONFIG_PATHS"] = strings.Join(trusted, string(filepath.ListSeparator))
	}
	env["PATH"] = prependPath(env["PATH"], plan.ShimsDir)
	env[pkgsandbox.EnvRunnerPath] = env["PATH"]
	return env
}

// ContextBinaryInstallPlan returns the host-visible paths for one selected
// binary set. It performs no filesystem or network operation. Callers should
// pass all enabled namespace-winner binaries, across all scopes, in one call.
func ContextBinaryInstallPlan(stellaHome string, specs []pkgplugins.PluginBinarySpec) (BinaryInstallPlan, error) {
	if stellaHome == "" {
		return BinaryInstallPlan{}, errors.New("manifest: stella home is required")
	}
	identity, err := binarySelectionIdentity(specs)
	if err != nil {
		return BinaryInstallPlan{}, err
	}
	dataDir := miseToolsDir(stellaHome)
	return BinaryInstallPlan{
		Identity:     identity,
		DataDir:      dataDir,
		PublicDir:    filepath.Join(dataDir, "public", identity),
		PublicBinDir: filepath.Join(dataDir, "public", identity),
	}, nil
}

// InstallContextBinaries installs trusted system and system-agent selections
// into the shared immutable mise artifact cache, then copies the selected full
// installs into a public selection with direct aliases. The temporary config is
// deleted before returning, so options never become cross-agent state.
func InstallContextBinaries(ctx context.Context, stellaHome string, specs []pkgplugins.PluginBinarySpec) (BinaryInstallPlan, error) {
	plan, err := ContextBinaryInstallPlan(stellaHome, specs)
	if err != nil {
		return BinaryInstallPlan{}, err
	}
	tools, err := miseToolsFromSpecs(specs, func(spec pkgplugins.PluginBinarySpec) bool {
		return spec.Scope == string(plugin.ScopeSystem) || spec.Scope == string(plugin.ScopeSystemAgent)
	})
	if err != nil {
		return BinaryInstallPlan{}, err
	}
	if len(tools) == 0 {
		// A user-only selection does not own the system config layer. Keep the
		// system layer unset, but still publish the core-only selection. This
		// prevents an empty snapshot from falling back to the entire host bin.
		plan.ConfigPath = ""
		plan.ShimsDir = ""
		if err := materializeNativeCoreSelection(stellaHome, plan); err != nil {
			return BinaryInstallPlan{}, err
		}
		return plan, nil
	}
	if err := runContextInstall(ctx, stellaHome, plan, tools); err != nil {
		return BinaryInstallPlan{}, err
	}
	return plan, nil
}

// InstallSandboxBinaries installs user and user-agent selections through the
// already-created sandbox session. The host only supplies validated specs;
// mise, its config write, and its cache writes all happen through the session
// capability. The selected system config must have been prepared with the
// same complete spec list first.
func InstallSandboxBinaries(ctx context.Context, session pkgsandbox.Session, specs []pkgplugins.PluginBinarySpec) (BinaryInstallPlan, error) {
	if session == nil {
		return BinaryInstallPlan{}, errors.New("manifest: sandbox session is required")
	}
	nativePublicationMu.Lock()
	defer nativePublicationMu.Unlock()
	identity, err := binarySelectionIdentity(specs)
	if err != nil {
		return BinaryInstallPlan{}, err
	}
	baseEnv := session.Policy().Env
	dataDir := baseEnv["MISE_DATA_DIR"]
	if dataDir == "" || baseEnv["MISE_NOT_FOUND_AUTO_INSTALL"] != "true" {
		return BinaryInstallPlan{}, errors.New("manifest: user CLI install requires a writable sandbox mise home")
	}
	root := filepath.Join(dataDir, "contexts", identity)
	plan := BinaryInstallPlan{
		Identity:     identity,
		DataDir:      dataDir,
		ConfigPath:   filepath.Join(root, "config.toml"),
		ShimsDir:     filepath.Join(root, "shims"),
		PublicDir:    filepath.Join(dataDir, "public", identity),
		PublicBinDir: filepath.Join(dataDir, "public", identity),
	}
	tools, err := miseToolsFromSpecs(specs, func(spec pkgplugins.PluginBinarySpec) bool {
		return spec.Scope == string(plugin.ScopeUser) || spec.Scope == string(plugin.ScopeUserAgent)
	})
	if err != nil {
		return BinaryInstallPlan{}, err
	}
	if len(tools) == 0 {
		return plan, nil
	}

	env := sandboxMiseEnv(baseEnv, plan)
	if _, err := session.Exec(ctx, sandboxMisePrepareCommand(), pkgsandbox.ExecOptions{Env: env}); err != nil {
		return BinaryInstallPlan{}, fmt.Errorf("manifest: prepare sandbox mise dirs: %w", err)
	}
	content, err := renderMiseTOML(tools)
	if err != nil {
		return BinaryInstallPlan{}, err
	}
	if err := session.Files().WriteFile(plan.ConfigPath, []byte(content), 0o600); err != nil {
		return BinaryInstallPlan{}, fmt.Errorf("manifest: write sandbox mise config: %w", err)
	}
	result, err := session.Exec(ctx, sandboxMiseInstallCommand(), pkgsandbox.ExecOptions{Env: env})
	if err != nil {
		return BinaryInstallPlan{}, fmt.Errorf("manifest: install sandbox CLI binaries: %w", err)
	}
	if result.ExitCode != 0 {
		return BinaryInstallPlan{}, fmt.Errorf("manifest: install sandbox CLI binaries exited with code %d: %s", result.ExitCode, strings.TrimSpace(result.Stderr))
	}
	publicEnv := maps.Clone(env)
	publicEnv["STELLA_NATIVE_PUBLIC_DIR"] = plan.PublicDir
	if result, err := session.Exec(ctx, sandboxMiseMaterializeCommand(tools), pkgsandbox.ExecOptions{Env: publicEnv}); err != nil {
		return BinaryInstallPlan{}, fmt.Errorf("manifest: publish sandbox CLI selection: %w", err)
	} else if result.ExitCode != 0 {
		return BinaryInstallPlan{}, fmt.Errorf("manifest: publish sandbox CLI selection exited with code %d: %s", result.ExitCode, strings.TrimSpace(result.Stderr))
	}

	// ConfigPath and ShimsDir describe the private preparation session only. The
	// returned plan is consumed by the final session, which mounts PublicDir and
	// must never inherit the managed config or shim tree.
	plan.ConfigPath = ""
	plan.ShimsDir = ""
	return plan, nil
}

func sandboxMiseMaterializeCommand(tools []miseTool) string {
	if runtime.GOOS == "windows" {
		return ""
	}
	var b strings.Builder
	b.WriteString("set -eu\n")
	b.WriteString("stage=\"$STELLA_NATIVE_PUBLIC_DIR.tmp.$$\"\n")
	b.WriteString("trap 'rm -rf \"$stage\"' EXIT\n")
	b.WriteString("if [ -f \"$STELLA_NATIVE_PUBLIC_DIR/.selection-complete\" ]; then exit 0; fi\n")
	b.WriteString("rm -rf \"$stage\"\n")
	b.WriteString("mkdir -p \"$stage/installs\" \"$(dirname \"$STELLA_NATIVE_PUBLIC_DIR\")\"\n")
	for _, tool := range tools {
		alias := tool.PublicName
		if alias == "" {
			alias = tool.Lookup
		}
		key := nativeInstallKey(tool)
		fmt.Fprintf(&b, "install_dir=$(\"$STELLA_HOME/bin/mise\" where %s)\n", shellQuotePOSIX(tool.Key))
		fmt.Fprintf(&b, "binary_path=$(\"$STELLA_HOME/bin/mise\" which %s)\n", shellQuotePOSIX(tool.Lookup))
		b.WriteString("case \"$binary_path\" in \"$install_dir\"/*) ;; *) echo 'mise binary escaped install' >&2; exit 1 ;; esac\n")
		fmt.Fprintf(&b, "rel=\"${binary_path#\"$install_dir\"/}\"\n")
		fmt.Fprintf(&b, "mkdir -p \"$stage/installs/%s\"\n", key)
		fmt.Fprintf(&b, "cp -R \"$install_dir/.\" \"$stage/installs/%s/\"\n", key)
		fmt.Fprintf(&b, "ln -s \"installs/%s/$rel\" \"$stage\"/%s\n", key, shellQuotePOSIX(alias))
	}
	b.WriteString("touch \"$stage/.selection-complete\"\n")
	b.WriteString("if [ -e \"$STELLA_NATIVE_PUBLIC_DIR\" ]; then exit 0; fi\n")
	b.WriteString("mv \"$stage\" \"$STELLA_NATIVE_PUBLIC_DIR\"\n")
	b.WriteString("trap - EXIT\n")
	return b.String()
}

func shellQuotePOSIX(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func sandboxMisePrepareCommand() string {
	if runtime.GOOS == "windows" {
		return `if not exist "%STELLA_MISE_CONTEXT_DIR%" mkdir "%STELLA_MISE_CONTEXT_DIR%" && if not exist "%MISE_CONFIG_DIR%" mkdir "%MISE_CONFIG_DIR%" && if not exist "%MISE_SHIMS_DIR%" mkdir "%MISE_SHIMS_DIR%" && if not exist "%MISE_CACHE_DIR%" mkdir "%MISE_CACHE_DIR%" && if not exist "%MISE_STATE_DIR%" mkdir "%MISE_STATE_DIR%"`
	}
	return `mkdir -p "$MISE_CONFIG_DIR" "$(dirname "$MISE_GLOBAL_CONFIG_FILE")" "$MISE_SHIMS_DIR" "$MISE_CACHE_DIR" "$MISE_STATE_DIR"`
}

func sandboxMiseInstallCommand() string {
	if runtime.GOOS == "windows" {
		return `"%STELLA_HOME%\bin\mise.exe" trust "%MISE_GLOBAL_CONFIG_FILE%" && "%STELLA_HOME%\bin\mise.exe" install && "%STELLA_HOME%\bin\mise.exe" reshim`
	}
	return `"$STELLA_HOME/bin/mise" trust "$MISE_GLOBAL_CONFIG_FILE" && "$STELLA_HOME/bin/mise" install && "$STELLA_HOME/bin/mise" reshim`
}

func runContextInstall(ctx context.Context, stellaHome string, plan BinaryInstallPlan, tools []miseTool) (retErr error) {
	return runContextInstallWithCore(ctx, stellaHome, plan, tools, false)
}

func runContextInstallWithCore(ctx context.Context, stellaHome string, plan BinaryInstallPlan, tools []miseTool, includeCore bool) (retErr error) {
	miseInstallMu.Lock()
	defer miseInstallMu.Unlock()
	if nativePublicationComplete(plan.PublicDir, nativeSelectionAliases(stellaHome, plan, tools, includeCore)) {
		return nil
	}

	miseBin, err := findMiseBin(stellaHome)
	if err != nil {
		return err
	}
	privateRoot := filepath.Join(stellaHome, ".mise-private")
	if err := ensureNativePrivateRoot(privateRoot); err != nil {
		return err
	}
	tempDir, err := os.MkdirTemp(privateRoot, "install-")
	if err != nil {
		return fmt.Errorf("manifest: create native install dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(tempDir) }()
	tempConfig := filepath.Join(tempDir, "config.toml")
	content, err := renderMiseTOML(tools)
	if err != nil {
		return err
	}
	if err := os.WriteFile(tempConfig, []byte(content), 0o600); err != nil {
		return fmt.Errorf("manifest: write native mise config: %w", err)
	}
	if err := os.Chmod(tempConfig, 0o600); err != nil {
		return fmt.Errorf("manifest: chmod native mise config: %w", err)
	}
	systemConfig := filepath.Join(tempDir, "system.toml")
	if err := os.WriteFile(systemConfig, nil, 0o600); err != nil {
		return fmt.Errorf("manifest: write native system mise config: %w", err)
	}
	if err := os.Chmod(systemConfig, 0o600); err != nil {
		return fmt.Errorf("manifest: chmod native system mise config: %w", err)
	}
	defer func() {
		if err := removeNativeMiseConfig(tempConfig); err != nil && !errors.Is(err, os.ErrNotExist) {
			retErr = errors.Join(retErr, fmt.Errorf("manifest: remove native mise config: %w", err))
		}
		retErr = errors.Join(retErr, os.RemoveAll(tempDir))
	}()

	shimsDir := filepath.Join(tempDir, "shims")
	env, err := nativeMiseInstallEnv(stellaHome, plan.DataDir, shimsDir, tempDir, tempConfig, systemConfig)
	if err != nil {
		return err
	}
	for _, args := range [][]string{{"trust", tempConfig}, {"install"}} {
		if err := runMise(ctx, miseBin, env, tempDir, args...); err != nil {
			return fmt.Errorf("manifest: mise %s: %w", args[0], err)
		}
	}
	if err := materializeNativeSelectionWithCore(ctx, stellaHome, plan, tools, miseBin, env, tempDir, includeCore); err != nil {
		return err
	}
	return nil
}

// nativeMiseInstallEnv gives a context install its own config files and
// project ceiling. The ceiling stops mise's upward search before any parent
// mise.toml/.tool-versions file can contribute tools to the selection.
func nativeMiseInstallEnv(stellaHome, dataDir, shimsDir, configRoot, globalConfig, systemConfig string) ([]string, error) {
	env, err := isolatedMiseEnvAt(stellaHome, dataDir, shimsDir)
	if err != nil {
		return nil, err
	}
	ceiling, err := canonicalNativePath(configRoot)
	if err != nil {
		return nil, fmt.Errorf("resolve native mise config ceiling: %w", err)
	}
	configDir := filepath.Join(filepath.Dir(globalConfig), "config")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		return nil, fmt.Errorf("create native mise config dir: %w", err)
	}
	return append(env,
		"MISE_CONFIG_DIR="+configDir,
		"MISE_GLOBAL_CONFIG_FILE="+globalConfig,
		"MISE_SYSTEM_CONFIG_FILE="+systemConfig,
		"MISE_TRUSTED_CONFIG_PATHS="+strings.Join([]string{globalConfig, systemConfig}, string(filepath.ListSeparator)),
		"MISE_PROJECT_ROOT="+ceiling,
		"MISE_CEILING_PATHS="+ceiling,
	), nil
}

func ensureNativePrivateRoot(root string) error {
	info, err := os.Lstat(root)
	if err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("manifest: native private root must be a directory: %s", root)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("manifest: inspect native private root: %w", err)
	} else if err := os.MkdirAll(root, 0o700); err != nil {
		return fmt.Errorf("manifest: create native private root: %w", err)
	}
	if err := os.Chmod(root, 0o700); err != nil {
		return fmt.Errorf("manifest: protect native private root: %w", err)
	}
	return nil
}

func removeNativeMiseConfig(path string) error { return os.Remove(path) }

func runMiseOutput(ctx context.Context, miseBin string, env []string, dir string, args ...string) (string, error) {
	var stdout bytes.Buffer
	cmd := managedCommandContext(ctx, miseBin, args...)
	cmd.Dir = dir
	cmd.Env = env
	cmd.Stdout = &stdout
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		return "", closedMiseError(ctx, args[0], err)
	}
	output := strings.TrimSpace(stdout.String())
	if output == "" {
		return "", fmt.Errorf("mise %s returned empty output", args[0])
	}
	return output, nil
}

func materializeNativeSelection(ctx context.Context, stellaHome string, plan BinaryInstallPlan, tools []miseTool, miseBin string, env []string, dir string) error {
	return materializeNativeSelectionWithCore(ctx, stellaHome, plan, tools, miseBin, env, dir, false)
}

func materializeNativeSelectionWithCore(ctx context.Context, stellaHome string, plan BinaryInstallPlan, tools []miseTool, miseBin string, env []string, dir string, includeCore bool) error {
	aliases := nativeSelectionAliases(stellaHome, plan, tools, includeCore)
	return publishNativeSelection(plan.PublicDir, aliases, func(publicDir string) error {
		selectionPlan := plan
		selectionPlan.PublicDir = publicDir
		selectionPlan.PublicBinDir = publicDir
		return materializeNativeSelectionAtWithCore(ctx, stellaHome, selectionPlan, tools, miseBin, env, dir, includeCore)
	})
}

func nativeSelectionAliases(stellaHome string, plan BinaryInstallPlan, tools []miseTool, includeCore bool) []string {
	aliases := nativeCoreAliases(stellaHome)
	if includeCore {
		aliases = nativeRuntimeAliases(stellaHome, plan.embeddedNames)
	}
	for _, tool := range tools {
		aliasName := tool.PublicName
		if aliasName == "" {
			aliasName = tool.Lookup
		}
		aliases = appendUniqueNativeAlias(aliases, aliasName)
	}
	return aliases
}

func materializeNativeSelectionAtWithCore(ctx context.Context, stellaHome string, plan BinaryInstallPlan, tools []miseTool, miseBin string, env []string, dir string, includeCore bool) error {
	if err := os.MkdirAll(plan.PublicBinDir, 0o755); err != nil {
		return fmt.Errorf("manifest: create native public bin: %w", err)
	}
	copyCore := copyNativeCoreBinaries
	if includeCore {
		copyCore = func(home, dir string) error {
			return copyNativeRuntimeBinaries(home, dir, plan.embeddedNames)
		}
	}
	if err := copyCore(stellaHome, plan.PublicBinDir); err != nil {
		return err
	}
	for _, tool := range tools {
		publicName := tool.PublicName
		if publicName == "" {
			publicName = tool.Lookup
		}
		installDir, err := runMiseOutput(ctx, miseBin, env, dir, "where", tool.Key)
		if err != nil {
			return fmt.Errorf("manifest: locate native install %q: %w", publicName, err)
		}
		binaryPath, err := runMiseOutput(ctx, miseBin, env, dir, "which", tool.Lookup)
		if err != nil {
			return fmt.Errorf("manifest: locate native binary %q: %w", publicName, err)
		}
		installDir, err = canonicalNativePath(installDir)
		if err != nil {
			return fmt.Errorf("manifest: resolve native install %q: %w", publicName, err)
		}
		binaryPath, err = canonicalNativePath(binaryPath)
		if err != nil {
			return fmt.Errorf("manifest: resolve native binary %q: %w", publicName, err)
		}
		rel, err := filepath.Rel(installDir, binaryPath)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
			return fmt.Errorf("manifest: native binary %q escapes selected install", publicName)
		}
		targetRoot := filepath.Join(plan.PublicDir, "installs", nativeInstallKey(tool))
		if err := copyNativeTree(installDir, targetRoot); err != nil {
			return fmt.Errorf("manifest: copy native install %q: %w", publicName, err)
		}
		aliasName := tool.PublicName
		if aliasName == "" {
			aliasName = tool.Lookup
		}
		if err := publishNativeAlias(filepath.Join(plan.PublicBinDir, aliasName), filepath.Join(targetRoot, rel)); err != nil {
			return fmt.Errorf("manifest: publish native binary %q: %w", aliasName, err)
		}
	}
	return nil
}

// canonicalNativePath makes paths reported by separate mise commands
// comparable on platforms where an OS-managed alias such as /var resolves to
// /private/var. The subsequent relative-path check still rejects binaries
// whose resolved target is outside the resolved install root.
func canonicalNativePath(path string) (string, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	return filepath.Clean(resolved), nil
}

func closedMiseError(ctx context.Context, stage string, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return fmt.Errorf("mise %s: %w", stage, ctxErr)
	}
	var exitErr interface{ ExitCode() int }
	if errors.As(err, &exitErr) {
		return fmt.Errorf("mise %s failed with exit code %d", stage, exitErr.ExitCode())
	}
	return fmt.Errorf("mise %s failed", stage)
}

func materializeNativeCoreSelection(stellaHome string, plan BinaryInstallPlan) error {
	return publishNativeSelection(plan.PublicDir, nativeCoreAliases(stellaHome), func(publicDir string) error {
		return copyNativeCoreBinaries(stellaHome, publicDir)
	})
}

func materializeNativeRuntimeSelection(stellaHome string, plan BinaryInstallPlan) error {
	return publishNativeSelection(plan.PublicDir, nativeRuntimeAliases(stellaHome, plan.embeddedNames), func(publicDir string) error {
		return copyNativeRuntimeBinaries(stellaHome, publicDir, plan.embeddedNames)
	})
}

func nativeCoreAliases(stellaHome string) []string {
	aliases := make([]string, 0, 1)
	names := []string{".stella-shell-env"}
	for _, name := range names {
		if _, err := os.Stat(filepath.Join(stellaHome, "bin", name)); err == nil {
			aliases = append(aliases, name)
		}
	}
	return aliases
}

func nativeRuntimeAliases(stellaHome string, names []string) []string {
	aliases := nativeCoreAliases(stellaHome)
	for _, name := range names {
		publicName := runtimeBinaryName(name)
		if _, err := os.Stat(filepath.Join(stellaHome, "bin", publicName)); err == nil {
			aliases = appendUniqueNativeAlias(aliases, publicName)
		}
	}
	return aliases
}

func appendUniqueNativeAlias(aliases []string, alias string) []string {
	if alias == "" {
		return aliases
	}
	if slices.Contains(aliases, alias) {
		return aliases
	}
	return append(aliases, alias)
}

func publishNativeSelection(root string, aliases []string, build func(string) error) error {
	nativePublicationMu.Lock()
	defer nativePublicationMu.Unlock()

	if nativePublicationComplete(root, aliases) {
		return nil
	}
	if _, err := os.Lstat(root); err == nil {
		return fmt.Errorf("manifest: native selection %q exists but is incomplete", root)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("manifest: inspect native selection %q: %w", root, err)
	}
	parent := filepath.Dir(root)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return fmt.Errorf("manifest: create native selection parent: %w", err)
	}
	temp, err := os.MkdirTemp(parent, ".native-selection-")
	if err != nil {
		return fmt.Errorf("manifest: create native selection staging dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(temp) }()
	if err := build(temp); err != nil {
		return err
	}
	if err := os.Chmod(temp, 0o755); err != nil {
		return fmt.Errorf("manifest: finalize native selection staging dir: %w", err)
	}
	if nativePublicationComplete(root, aliases) {
		return nil
	}
	if _, err := os.Lstat(root); err == nil {
		return fmt.Errorf("manifest: native selection %q appeared incomplete during publication", root)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("manifest: inspect native selection %q before publication: %w", root, err)
	}
	if err := os.Rename(temp, root); err != nil {
		return fmt.Errorf("manifest: publish native selection: %w", err)
	}
	return nil
}

func nativePublicationComplete(root string, aliases []string) bool {
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return false
	}
	for _, alias := range aliases {
		info, err := os.Stat(filepath.Join(root, alias))
		if err != nil || info.IsDir() {
			return false
		}
	}
	return true
}

func nativeInstallKey(tool miseTool) string {
	digest := sha256.Sum256([]byte(tool.Key + "\x00" + tool.Lookup))
	return hex.EncodeToString(digest[:8])
}

func copyNativeCoreBinaries(stellaHome, publicBin string) error {
	names := []string{".stella-shell-env"}
	for _, name := range names {
		source := filepath.Join(stellaHome, "bin", name)
		if _, err := os.Stat(source); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return fmt.Errorf("manifest: inspect core runtime %q: %w", name, err)
		}
		if err := copyNativeFile(source, filepath.Join(publicBin, name)); err != nil {
			return fmt.Errorf("manifest: publish core runtime %q: %w", name, err)
		}
	}
	return nil
}

func copyNativeRuntimeBinaries(stellaHome, publicBin string, names []string) error {
	if err := copyNativeCoreBinaries(stellaHome, publicBin); err != nil {
		return err
	}
	for _, name := range names {
		source := filepath.Join(stellaHome, "bin", runtimeBinaryName(name))
		if _, err := os.Stat(source); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return fmt.Errorf("manifest: inspect core runtime %q: %w", name, err)
		}
		publicName := runtimeBinaryName(name)
		if name == "xberg" {
			if err := materializeBundledBinary(publicBin, name, source); err != nil {
				return fmt.Errorf("manifest: publish core runtime %q: %w", name, err)
			}
			continue
		}
		if err := copyNativeFile(source, filepath.Join(publicBin, publicName)); err != nil {
			return fmt.Errorf("manifest: publish core runtime %q: %w", name, err)
		}
	}
	return nil
}

func materializeBundledBinary(publicBin, name, source string) error {
	resolved, err := filepath.EvalSymlinks(source)
	if err != nil {
		return fmt.Errorf("manifest: resolve bundled binary %q: %w", name, err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return fmt.Errorf("manifest: stat bundled binary %q: %w", name, err)
	}
	var target string
	root := filepath.Join(publicBin, "bundled", name)
	if info.IsDir() {
		return fmt.Errorf("manifest: bundled binary %q resolves to a directory", name)
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return fmt.Errorf("manifest: create bundled runtime %q: %w", name, err)
	}
	if err := copyNativeFile(resolved, filepath.Join(root, filepath.Base(resolved))); err != nil {
		return fmt.Errorf("manifest: copy bundled binary %q: %w", name, err)
	}
	target = filepath.Join(root, filepath.Base(resolved))
	if linkInfo, linkErr := os.Lstat(source); linkErr == nil && linkInfo.Mode()&os.ModeSymlink != 0 {
		// A versioned embedded bundle is exposed through a launcher symlink.
		bundleDir := filepath.Dir(resolved)
		if filepath.Clean(bundleDir) != filepath.Clean(filepath.Dir(source)) {
			if err := os.RemoveAll(root); err != nil {
				return err
			}
			if err := copyNativeTree(bundleDir, root); err != nil {
				return fmt.Errorf("manifest: copy bundled runtime %q: %w", name, err)
			}
			target = filepath.Join(root, filepath.Base(resolved))
		}
	}
	return publishNativeAlias(filepath.Join(publicBin, name), target)
}

func copyNativeTree(source, destination string) error {
	lexicalRoot, err := filepath.Abs(source)
	if err != nil {
		return fmt.Errorf("resolve source root: %w", err)
	}
	root, err := filepath.EvalSymlinks(source)
	if err != nil {
		return err
	}
	info, err := os.Stat(root)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("source is not a directory")
	}
	sourceRoot, err := os.OpenRoot(root)
	if err != nil {
		return fmt.Errorf("open source root: %w", err)
	}
	defer func() { _ = sourceRoot.Close() }()
	return fs.WalkDir(sourceRoot.FS(), ".", func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		dest := destination
		if name != "." {
			dest = filepath.Join(destination, filepath.FromSlash(name))
		}
		if entry.Type()&os.ModeSymlink != 0 {
			target, err := sourceRoot.Readlink(name)
			if err != nil {
				return err
			}
			portableTarget, err := validateNativeSymlinkTarget(sourceRoot.FS(), root, lexicalRoot, name, target)
			if err != nil {
				return fmt.Errorf("symlink %q: %w", name, err)
			}
			if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
				return err
			}
			return os.Symlink(portableTarget, dest)
		}
		if entry.IsDir() {
			return os.MkdirAll(dest, 0o755)
		}
		return copyNativeRootFile(sourceRoot, name, dest)
	})
}

func validateNativeSymlinkTarget(root fs.FS, canonicalRoot, lexicalRoot, name, target string) (string, error) {
	links, ok := root.(fs.ReadLinkFS)
	if !ok {
		return "", errors.New("source root does not support symlink inspection")
	}
	target, err := portableNativeSymlinkTarget(canonicalRoot, lexicalRoot, name, target)
	if err != nil {
		return "", err
	}
	current := pathpkg.Clean(pathpkg.Join(pathpkg.Dir(name), target))
	seen := make(map[string]struct{})
	for {
		if current == ".." || strings.HasPrefix(current, "../") || !fs.ValidPath(current) {
			return "", errors.New("target escapes install")
		}
		if _, exists := seen[current]; exists {
			return "", errors.New("target contains a symlink cycle")
		}
		seen[current] = struct{}{}
		info, err := fs.Lstat(root, current)
		if err != nil {
			return "", fmt.Errorf("resolve target: %w", err)
		}
		if info.Mode()&fs.ModeSymlink == 0 {
			return target, nil
		}
		next, err := links.ReadLink(current)
		if err != nil {
			return "", fmt.Errorf("read target: %w", err)
		}
		next, err = portableNativeSymlinkTarget(canonicalRoot, lexicalRoot, current, next)
		if err != nil {
			return "", err
		}
		current = pathpkg.Clean(pathpkg.Join(pathpkg.Dir(current), next))
	}
}

func portableNativeSymlinkTarget(canonicalRoot, lexicalRoot, name, target string) (string, error) {
	if target == "" || strings.ContainsRune(target, '\\') {
		return "", fmt.Errorf("absolute or unsafe target %q is not portable", target)
	}
	if !filepath.IsAbs(target) && !strings.HasPrefix(target, "/") {
		return target, nil
	}
	if canonicalRoot == "" || lexicalRoot == "" {
		return "", fmt.Errorf("absolute or unsafe target %q is not portable", target)
	}
	targetAbs := filepath.Clean(target)
	rel, err := filepath.Rel(canonicalRoot, targetAbs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		rel, err = filepath.Rel(lexicalRoot, targetAbs)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
			return "", fmt.Errorf("absolute or unsafe target %q is not portable", target)
		}
		targetAbs = filepath.Join(canonicalRoot, rel)
	}
	linkAbs := filepath.Join(canonicalRoot, filepath.FromSlash(name))
	portable, err := filepath.Rel(filepath.Dir(linkAbs), targetAbs)
	if err != nil || portable == "" {
		return "", fmt.Errorf("absolute or unsafe target %q is not portable", target)
	}
	return filepath.ToSlash(portable), nil
}

func copyNativeRootFile(root *os.Root, name, destination string) error {
	file, err := root.Open(name)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("source is not a regular file")
	}
	data, err := io.ReadAll(file)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(destination, data, info.Mode().Perm()); err != nil {
		return err
	}
	return os.Chmod(destination, info.Mode().Perm())
}

func copyNativeFile(source, destination string) error {
	file, err := os.Open(source)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("source is not a regular file")
	}
	data, err := io.ReadAll(file)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(destination, data, info.Mode().Perm()); err != nil {
		return err
	}
	return os.Chmod(destination, info.Mode().Perm())
}

func publishNativeAlias(alias, target string) error {
	if err := os.Remove(alias); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	rel, err := filepath.Rel(filepath.Dir(alias), target)
	if err != nil {
		return err
	}
	return os.Symlink(rel, alias)
}

func clearNativeMisePaths(env map[string]string) {
	for _, key := range []string{"MISE_SHIMS_DIR", "MISE_TRUSTED_CONFIG_PATHS", "MISE_SYSTEM_CONFIG_FILE"} {
		delete(env, key)
	}
	// A native system selection must not expose the shared system mise tree. A
	// user selection still needs its own writable tree for InstallSandboxBinaries;
	// the explicit auto-install flag is the trusted marker already emitted by
	// RuntimeMiseEnv for that private scope.
	if env["MISE_NOT_FOUND_AUTO_INSTALL"] == "true" {
		return
	}
	for _, key := range []string{"MISE_DATA_DIR", "MISE_CONFIG_DIR", "MISE_CACHE_DIR", "MISE_STATE_DIR", "MISE_GLOBAL_CONFIG_FILE"} {
		delete(env, key)
	}
}

func sandboxMiseEnv(base map[string]string, plan BinaryInstallPlan) map[string]string {
	env := maps.Clone(base)
	env["MISE_DATA_DIR"] = plan.DataDir
	env["MISE_CONFIG_DIR"] = filepath.Join(plan.DataDir, "config")
	env["MISE_CACHE_DIR"] = filepath.Join(plan.DataDir, "cache")
	env["MISE_STATE_DIR"] = filepath.Join(plan.DataDir, "state")
	env["MISE_SHIMS_DIR"] = plan.ShimsDir
	env["MISE_GLOBAL_CONFIG_FILE"] = plan.ConfigPath
	env["STELLA_MISE_CONTEXT_DIR"] = filepath.Dir(plan.ConfigPath)
	trusted := []string{plan.ConfigPath}
	if system := base["MISE_SYSTEM_CONFIG_FILE"]; system != "" {
		trusted = append(trusted, system)
	}
	env["MISE_TRUSTED_CONFIG_PATHS"] = strings.Join(trusted, string(filepath.ListSeparator))
	return env
}

func prependPath(pathValue, entry string) string {
	if entry == "" {
		return pathValue
	}
	if pathValue == "" {
		return entry
	}
	return entry + string(filepath.ListSeparator) + pathValue
}

func miseToolsFromSpecs(specs []pkgplugins.PluginBinarySpec, keep func(pkgplugins.PluginBinarySpec) bool) ([]miseTool, error) {
	tools := make([]miseTool, 0, len(specs))
	seen := make(map[string]miseTool)
	for _, spec := range specs {
		if !keep(spec) {
			continue
		}
		if spec.Name == "" || spec.Tool == "" {
			return nil, fmt.Errorf("manifest: binary %q has incomplete identity", spec.Name)
		}
		tool := miseToolFromBinary(ManifestBinary{
			Name: spec.Name, Tool: spec.Tool, Version: spec.Version, Options: maps.Clone(spec.Options),
		})
		if previous, exists := seen[tool.Key]; exists && !reflect.DeepEqual(previous, tool) {
			return nil, fmt.Errorf("manifest: selected binaries disagree on mise tool %q", tool.Key)
		}
		if _, exists := seen[tool.Key]; !exists {
			seen[tool.Key] = tool
			tools = append(tools, tool)
		}
	}
	return tools, nil
}

func binarySelectionIdentity(specs []pkgplugins.PluginBinarySpec) (string, error) {
	canonical := slices.Clone(specs)
	slices.SortFunc(canonical, func(left, right pkgplugins.PluginBinarySpec) int {
		for _, pair := range [][2]string{{left.PluginID, right.PluginID}, {left.Name, right.Name}, {left.Tool, right.Tool}, {left.Version, right.Version}, {left.ConfigID, right.ConfigID}, {left.Scope, right.Scope}} {
			if pair[0] != pair[1] {
				return strings.Compare(pair[0], pair[1])
			}
		}
		if left.Revision < right.Revision {
			return -1
		}
		if left.Revision > right.Revision {
			return 1
		}
		return 0
	})
	for _, spec := range canonical {
		if spec.PluginID == "" || spec.ConfigID == "" || spec.Scope == "" || spec.Name == "" || spec.Tool == "" {
			return "", fmt.Errorf("manifest: binary %q is missing resource identity", spec.Name)
		}
		if err := validateNativeBinaryName(spec.Name); err != nil {
			return "", fmt.Errorf("manifest: binary %q: %w", spec.Name, err)
		}
		switch spec.Scope {
		case string(plugin.ScopeSystem), string(plugin.ScopeSystemAgent), string(plugin.ScopeUser), string(plugin.ScopeUserAgent):
		default:
			return "", fmt.Errorf("manifest: binary %q has unknown resource scope %q", spec.Name, spec.Scope)
		}
		if spec.Revision <= 0 {
			return "", fmt.Errorf("manifest: binary %q has non-positive config revision", spec.Name)
		}
	}
	payload, err := json.Marshal(canonical)
	if err != nil {
		return "", fmt.Errorf("manifest: encode binary selection identity: %w", err)
	}
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:16]), nil
}

func validateNativeBinaryName(name string) error {
	if name == "" || name == "." || name == ".." || filepath.Base(name) != name || strings.ContainsAny(name, `/\\`) {
		return errors.New("has unsafe path name")
	}
	if strings.ContainsRune(name, 0) {
		return errors.New("contains NUL")
	}
	return nil
}
