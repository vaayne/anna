package sandbox

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path"
	"path/filepath"
	"strings"

	oauth "github.com/CherryHQ/stella/internal/connections/oauth"
	"github.com/CherryHQ/stella/internal/plugin/manifest"
	pkgplugins "github.com/CherryHQ/stella/pkg/plugins"
	pkgsandbox "github.com/CherryHQ/stella/pkg/sandbox"
)

func runnerFilesystemPolicy(paths Paths, cfg Config) (pkgsandbox.FilesystemPolicy, map[string]string) {
	mounts := []pkgsandbox.Mount{
		{SandboxPath: pkgsandbox.MountWorkspace, Access: pkgsandbox.MountReadWrite},
	}
	sources := map[string]string{pkgsandbox.MountWorkspace: paths.WorkspaceRoot}
	if userData := userDataDirHost(paths, cfg); userData != "" {
		mounts = append(mounts, pkgsandbox.Mount{SandboxPath: pkgsandbox.MountUserData, Access: pkgsandbox.MountReadWrite})
		sources[pkgsandbox.MountUserData] = userData
	}
	coreSelection := nativeCoreSelection(cfg)
	for _, name := range pkgsandbox.StellaHomeSandboxDirs() {
		sandboxPath := path.Join(pkgsandbox.MountStellaHome, strings.ReplaceAll(name, "\\", "/"))
		source := filepath.Join(paths.StellaHome, name)
		if name == "bin" && coreSelection != "" {
			source = coreSelection
		}
		mounts = append(mounts, pkgsandbox.Mount{
			SandboxPath: sandboxPath,
			Access:      pkgsandbox.MountReadOnly,
		})
		sources[sandboxPath] = source
	}
	agentDelegates := filepath.Join(paths.AgentRoot, ".agents", "delegates")
	if info, err := os.Stat(agentDelegates); cfg.AgentID != "" && filepath.Clean(paths.AgentRoot) != filepath.Clean(paths.WorkspaceRoot) && err == nil && info.IsDir() {
		sandboxPath := path.Join(pkgsandbox.MountStellaHome, "agents", cfg.AgentID, ".agents", "delegates")
		mounts = append(mounts, pkgsandbox.Mount{SandboxPath: sandboxPath, Access: pkgsandbox.MountReadOnly})
		sources[sandboxPath] = agentDelegates
	}
	if paths.BuiltinBundle != "" {
		mounts = append(mounts, pkgsandbox.Mount{SandboxPath: pkgsandbox.MountBuiltinSkills, Access: pkgsandbox.MountReadOnly})
		sources[pkgsandbox.MountBuiltinSkills] = paths.BuiltinBundle
	}
	if cfg.ContextBinaryPlan != nil {
		appendNativeSecondarySelectionMount(&mounts, sources, paths.StellaHome, cfg.ContextBinaryPlan.PublicDir, coreSelection)
	}
	if cfg.CoreRuntimePlan != nil {
		appendNativeSecondarySelectionMount(&mounts, sources, paths.StellaHome, cfg.CoreRuntimePlan.PublicDir, coreSelection)
	}
	if cfg.UserBinaryPlan != nil {
		appendNativeSecondarySelectionMount(&mounts, sources, paths.StellaHome, cfg.UserBinaryPlan.PublicDir, coreSelection)
	}
	if cfg.ManagedBinaryRoot != "" {
		sandboxPath := remapStellaHomePolicyPath(cfg.ManagedBinaryRoot, paths.StellaHome)
		mounts = append(mounts, pkgsandbox.Mount{SandboxPath: sandboxPath, Access: pkgsandbox.MountReadWrite})
		sources[sandboxPath] = cfg.ManagedBinaryRoot
	}
	if miseDir := miseUserDirHost(paths, cfg); miseDir != "" {
		sandboxPath := remapStellaHomePolicyPath(miseDir, paths.StellaHome)
		mounts = append(mounts, pkgsandbox.Mount{
			SandboxPath: sandboxPath,
			Access:      pkgsandbox.MountReadWrite,
		})
		sources[sandboxPath] = miseDir
	}
	workingDir := pkgsandbox.MountWorkspace
	if rel, ok := pkgsandbox.POSIXPathRelative(paths.WorkspaceRoot, paths.WorkDir); ok && rel != "." {
		workingDir = path.Join(workingDir, filepath.ToSlash(rel))
	}
	return pkgsandbox.FilesystemPolicy{WorkingDir: workingDir, Mounts: mounts}, sources
}

func nativeCoreSelection(cfg Config) string {
	if cfg.CoreRuntimePlan != nil && cfg.CoreRuntimePlan.PublicDir != "" {
		return cfg.CoreRuntimePlan.PublicDir
	}
	return ""
}

func appendNativeSecondarySelectionMount(mounts *[]pkgsandbox.Mount, sources map[string]string, stellaHome, publicDir, core string) {
	if publicDir == "" || publicDir == core {
		return
	}
	rel, err := filepath.Rel(stellaHome, publicDir)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return
	}
	appendSelectionMount(mounts, sources, stellaHome, rel)
}

func appendSelectionMount(mounts *[]pkgsandbox.Mount, sources map[string]string, stellaHome, relative string) {
	hostPath := filepath.Join(stellaHome, relative)
	info, err := os.Stat(hostPath)
	if err != nil || !info.IsDir() {
		return
	}
	sandboxPath := path.Join(pkgsandbox.MountStellaHome, filepath.ToSlash(relative))
	*mounts = append(*mounts, pkgsandbox.Mount{SandboxPath: sandboxPath, Access: pkgsandbox.MountReadOnly})
	sources[sandboxPath] = hostPath
}

func remapStellaHomePolicyPath(hostPath, stellaHome string) string {
	if rel, ok := pkgsandbox.POSIXPathRelative(stellaHome, hostPath); ok {
		return path.Join(pkgsandbox.MountStellaHome, rel)
	}
	return hostPath
}

// userDataDirHost returns the host path of the shared user-data root mounted as
// /user, or "" for a user-less job (no principal home, so no shared root to
// mount; the agent writes only its workspace and tmp).
func userDataDirHost(paths Paths, cfg Config) string {
	if cfg.UserID == "" && cfg.GroupID == "" {
		return ""
	}
	return paths.UserDataDir
}

// miseUserDirHost returns the host path of this session's writable per-user mise
// home, under the STELLA_HOME frame ($STELLA_HOME/users/{id}/.mise-tools), a
// sibling of the user-data root rather than inside it. This keeps the per-user
// tree under the same root as the shared system tree once a backend remaps
// STELLA_HOME (bwrap's /opt/stella), so the relative seed/shim symlinks that
// bridge per-user tree -> system tree resolve identically on host and in the
// sandbox. Putting it under the /user-mounted user-data root instead would split
// the two trees across separate sandbox roots (/user vs /opt/stella) and dangle
// those symlinks (#505). The policy builder emits it as one dedicated writable
// Mount. Returns "" when there is no per-user tree: no principal, or an ID that
// fails the safe-path-component check (the session then falls back to the shared
// read-only system tree). A downgrade from an unsafe ID is logged so a malformed
// ID is diagnosable.
func miseUserDirHost(paths Paths, cfg Config) string {
	principalDir, id := misePrincipal(cfg)
	dir := pkgsandbox.MiseUserToolsDir(paths.StellaHome, principalDir, id)
	if dir == "" {
		if id != "" {
			slog.Warn("per-user mise tree disabled: unsafe id, using shared read-only system tree",
				"component", "runner_sandbox",
				"user_id", cfg.UserID,
				"group_id", cfg.GroupID,
				"principal_dir", principalDir,
			)
		}
		return ""
	}
	return dir
}

// misePrincipal returns the home subtree and ID for this session's per-principal
// temp and mise trees. Every principal lives in the single "users" subtree (the
// only top-level isolation boundary, #442): a real user keys off its raw ID, a
// channel group off the group ID under a "group-" prefix so a group can never
// collide with a user of the same raw ID into one shared writable tree. Empty dir
// when neither is set, which makes callers fall back to a session-local temp dir
// and the shared read-only system mise tree. Groups take precedence: a group
// session carries both a GroupID and a synthetic UserID and must key off the group.
func misePrincipal(cfg Config) (principalDir, id string) {
	if cfg.GroupID != "" {
		return "users", "group-" + cfg.GroupID
	}
	if cfg.UserID == "" {
		return "", ""
	}
	return "users", cfg.UserID
}

// buildSandboxEnv constructs the Policy.Env map for a sandbox session.
// Vault secrets (if any) are used as the base so that runner-set variables
// (e.g. STELLA_HOME) always take precedence over user-defined secrets.
func buildSandboxEnv(ctx context.Context, cfg Config, paths Paths) (map[string]string, error) {
	env := make(map[string]string)
	var vaultEnv map[string]string
	sessionSecretEnv := make(map[string]string)
	cfg.SessionSecretValues.Set(nil)

	// Group sessions never load human vault secrets (D9 isolation).
	if cfg.GroupID == "" && cfg.VaultEnvLoader != nil {
		ve, err := cfg.VaultEnvLoader.LoadEnvForAgent(ctx, cfg.UserID, cfg.AgentID)
		if err != nil {
			slog.Warn("vault env injection skipped",
				"component", "runner_sandbox",
				"user_id", cfg.UserID,
				"agent_id", cfg.AgentID,
				"project_id", cfg.ProjectID,
				"error", err,
			)
		} else {
			vaultEnv = ve
			maps.Copy(env, ve)
		}
	}

	// Defense in depth: the vault-side system-managed filter is authoritative,
	// but the OAuth bundle must still never reach the sandbox.
	delete(env, oauth.VaultKeyGitHub)
	if cfg.GroupID == "" {
		if err := injectSessionEnv(ctx, cfg, env, vaultEnv, sessionSecretEnv); err != nil {
			return nil, err
		}
	}

	// The scoped sandbox token is retired; nothing may smuggle a value in
	// under its old name (e.g. a pre-validation vault row).
	delete(env, "STELLA_TOKEN")

	// Runner-set vars overlay vault entries so they always take precedence.
	maps.Copy(env, ProcessEnv(paths))
	// Runtime files are session-scoped and must never be redirected into the
	// persistent principal root (or accepted from a vault/session env entry).
	delete(env, "XDG_RUNTIME_DIR")
	// Every backend resolves tools through mise's native system < global <
	// workspace layers. Installs stay in the per-user STELLA_HOME tree so their
	// relative links to the shared system base survive backend remapping; the
	// writable global config lives under the principal's shared XDG config root.
	// Values are emitted as host paths, then local/docker render their process view.
	userConfigDir := ""
	if paths.UserDataDir != "" {
		userConfigDir = filepath.Join(paths.UserDataDir, ".config", "mise")
	}
	managedToolsDir := miseUserDirHost(paths, cfg)
	if cfg.ManagedBinaryRoot != "" {
		managedToolsDir = cfg.ManagedBinaryRoot
		env["STELLA_NATIVE_PREP"] = "true"
	}
	maps.Copy(env, manifest.RuntimeMiseEnv(
		paths.StellaHome,
		managedToolsDir,
		userConfigDir,
		paths.WorkspaceRoot,
	))
	recordSessionSecretValues(cfg.SessionSecretValues, env, vaultEnv, sessionSecretEnv)

	return env, nil
}

func recordSessionSecretValues(target *SessionSecretValues, env map[string]string, vaultEnv map[string]string, sessionSecretEnv map[string]string) {
	if target == nil {
		return
	}
	values := make([]string, 0, len(vaultEnv)+len(sessionSecretEnv))
	seen := make(map[string]struct{}, len(vaultEnv)+len(sessionSecretEnv))
	addValue := func(value string) {
		if value == "" {
			return
		}
		if _, ok := seen[value]; ok {
			return
		}
		seen[value] = struct{}{}
		values = append(values, value)
	}
	for key, value := range vaultEnv {
		if env[key] != value {
			continue
		}
		addValue(value)
	}
	for key, value := range sessionSecretEnv {
		if env[key] != value {
			continue
		}
		addValue(value)
	}
	target.Set(values)
}

// injectSessionEnv resolves plugin SessionEnvSpecs into env.
func injectSessionEnv(ctx context.Context, cfg Config, env map[string]string, vaultEnv map[string]string, secretEnv map[string]string) error {
	// oauthBundles caches loaded bundles per provider to avoid redundant vault hits.
	oauthBundles := make(map[string]*oauth.OAuthBundle)
	// oauthBoundVars records the env vars actually injected from OAuth so a later
	// live refresh rotates exactly those and never an explicit vault override.
	var oauthBoundVars []string
	defer func() { cfg.OAuthEnvBindings.Set(oauthBoundVars) }()
	for _, spec := range cfg.SessionEnvSpecs {
		src := string(spec.Source)
		if spec.Source == pkgplugins.SessionEnvSourceStatic {
			env[spec.EnvVar] = spec.Value
			continue
		}
		if !strings.HasPrefix(src, "oauth.") {
			if spec.Required {
				return fmt.Errorf("required session env %q (source %q) for plugin %q could not be resolved", spec.EnvVar, spec.Source, spec.PluginID)
			}
			continue
		}
		// Explicit vault secrets override OAuth-derived session env of the same name.
		if _, ok := vaultEnv[spec.EnvVar]; ok {
			continue
		}
		if cfg.TokenManager == nil {
			if spec.Required {
				return fmt.Errorf("required session env %q (source %q) for plugin %q could not be resolved", spec.EnvVar, spec.Source, spec.PluginID)
			}
			continue
		}
		providerID := spec.OAuthProviderID
		if providerID == "" {
			if spec.Required {
				return fmt.Errorf("required session env %q has oauth source but no OAuthProviderID", spec.EnvVar)
			}
			continue
		}
		bundle, ok := oauthBundles[providerID]
		if !ok {
			var err error
			bundle, err = cfg.TokenManager.GetOAuthToken(ctx, providerID, cfg.UserID, oauthMinValidity(cfg))
			if err != nil {
				slog.Debug("session env injection skipped",
					"component", "runner_sandbox",
					"user_id", cfg.UserID,
					"env_var", spec.EnvVar,
					"source", spec.Source,
					"error", err,
				)
			}
			oauthBundles[providerID] = bundle
		}
		if bundle == nil {
			if spec.Required {
				return fmt.Errorf("required session env %q (source %q) for plugin %q could not be resolved", spec.EnvVar, spec.Source, spec.PluginID)
			}
			// Provider not connected: the tool will run without this env var.
			// Expected when a user hasn't connected the tool's credential yet,
			// so keep it at Debug — the credentials page surfaces the prompt.
			slog.Debug("session env skipped: oauth provider not connected",
				"component", "runner_sandbox",
				"user_id", cfg.UserID,
				"env_var", spec.EnvVar,
				"provider", providerID,
				"plugin", spec.PluginID,
			)
			continue
		}
		field := strings.TrimPrefix(src, "oauth.")
		value, known := oauthBundleField(bundle, field)
		if !known {
			if spec.Required {
				return fmt.Errorf("required session env %q (source %q) for plugin %q: unknown oauth field %q", spec.EnvVar, spec.Source, spec.PluginID, field)
			}
			continue
		}
		if value != "" {
			env[spec.EnvVar] = value
			oauthBoundVars = append(oauthBoundVars, spec.EnvVar)
			if oauthSessionEnvFieldSecret(field) {
				secretEnv[spec.EnvVar] = value
			}
		} else if spec.Required {
			return fmt.Errorf("required session env %q (source %q) for plugin %q could not be resolved", spec.EnvVar, spec.Source, spec.PluginID)
		}
	}
	return nil
}

// oauthBundleField maps an oauth.<field> source suffix to the corresponding
// value on a resolved bundle; known is false for an unrecognized field. It is
// the single mapping shared by initial env injection (injectSessionEnv) and live
// refresh (RefreshSessionEnv) so the two never drift (#722).
func oauthBundleField(bundle *oauth.OAuthBundle, field string) (value string, known bool) {
	switch field {
	case "access_token":
		return bundle.AccessToken, true
	case "client_id":
		return bundle.ClientID, true
	case "brand":
		return bundle.Brand, true
	case "refresh_token":
		return bundle.RefreshToken, true
	default:
		return "", false
	}
}

func oauthSessionEnvFieldSecret(field string) bool {
	switch field {
	case "access_token", "refresh_token", "client_id":
		return true
	default:
		return false
	}
}
