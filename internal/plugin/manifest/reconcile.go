package manifest

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"time"
)

// ReconcileResult summarizes one reconcile run.
type ReconcileResult struct {
	EnabledCount int
	// Per-plugin results, keyed by plugin ID
	Plugins map[string]PluginReconcileResult
}

// PluginReconcileResult holds the result for a single plugin.
type PluginReconcileResult struct {
	PluginID string
	Binaries []BinaryReconcileResult
	// Skills not yet implemented (bundled-only in v1), but reserved for future use
	Err error
}

// BinaryReconcileResult holds the result for a single binary within a plugin.
type BinaryReconcileResult struct {
	Name     string
	Version  string
	CacheHit bool
	Err      error
}

// LoadState reads the manifest state file at path. If the file does not exist,
// an empty state is returned.
func LoadState(path string) (*ManifestState, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return &ManifestState{Plugins: make(map[string]PluginInstallState)}, nil
		}
		return nil, err
	}
	var s ManifestState
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, err
	}
	if s.Plugins == nil {
		s.Plugins = make(map[string]PluginInstallState)
	}
	return &s, nil
}

// SaveState writes the manifest state to path atomically (write to .tmp then rename).
func SaveState(path string, s *ManifestState) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// isCacheHit returns true if the state already records the binary at the given
// version spec. It compares against the requested spec, not the resolved
// version, so a partial spec (e.g. "2.40") still hits after resolving to a
// concrete version. Returns false for an empty spec (latest) so mise always
// verifies the install.
func isCacheHit(state *ManifestState, pluginID, binaryName, spec string) bool {
	if spec == "" {
		return false
	}
	ps, ok := state.Plugins[pluginID]
	if !ok {
		return false
	}
	for _, b := range ps.Binaries {
		if b.Name == binaryName && b.Spec == spec {
			return true
		}
	}
	return false
}

// upsertBinaryState updates or appends a binary install record in state.
func upsertBinaryState(state *ManifestState, pluginID string, install BinaryInstallState) {
	ps := state.Plugins[pluginID]
	for i, b := range ps.Binaries {
		if b.Name == install.Name {
			ps.Binaries[i] = install
			state.Plugins[pluginID] = ps
			return
		}
	}
	ps.Binaries = append(ps.Binaries, install)
	state.Plugins[pluginID] = ps
}

// Reconcile processes all enabled plugins in the manifest, downloading any binaries
// that are not already at the correct version according to the state file.
func Reconcile(ctx context.Context, m *Manifest, stellaHome string) ReconcileResult {
	statePath := StatePath(stellaHome)
	state, err := LoadState(statePath)
	if err != nil {
		slog.Error("manifest plugin reconcile: failed to load state", "error", err)
		state = &ManifestState{Plugins: make(map[string]PluginInstallState)}
	}

	enabledCount := 0
	for _, p := range m.Plugins {
		if p.Enabled {
			enabledCount++
		}
	}

	slog.Info("manifest plugin reconcile started", "enabled_plugins", enabledCount)

	result := ReconcileResult{
		EnabledCount: enabledCount,
		Plugins:      make(map[string]PluginReconcileResult),
	}

	// Ensure mise is available before processing any plugin binaries.
	if bootstrapErr := bootstrapMise(ctx, stellaHome); bootstrapErr != nil {
		slog.Error("manifest plugin reconcile: mise bootstrap failed", "error", bootstrapErr)
	}

	errorCount := 0

	// Collect every enabled binary into one builtin-scope mise config. The
	// config always reflects the full enabled set so runtime shims resolve any
	// of them; persisting it is offline and runs no mise commands.
	tools := enabledBuiltinTools(m)
	needInstall := false
	for _, plugin := range m.Plugins {
		if !plugin.Enabled {
			continue
		}
		for _, binary := range plugin.Binaries {
			if !isCacheHit(state, plugin.ID, binary.Name, binary.Version) {
				needInstall = true
			}
		}
	}

	var configErr, installErr error
	if len(tools) > 0 {
		if _, configErr = writeScopeConfig(stellaHome, builtinScope, tools); configErr != nil {
			slog.Error("manifest builtin config write failed", "error", configErr)
		} else if needInstall {
			slog.Info("manifest builtin tools installing", "tools", len(tools))
			if installErr = runScopeInstall(ctx, stellaHome, builtinScope); installErr != nil {
				slog.Error("manifest builtin tools install failed", "error", installErr)
			}
		}
	}

	// Rewrite shim symlinks to relative paths so they resolve inside bwrap
	// sandboxes. This is idempotent — already-relative shims are skipped.
	// Runs unconditionally because existing shims from before this fix still
	// have absolute host paths even when tools are fully cached.
	miseBin, miseErr := findMiseBin(stellaHome)
	if miseErr == nil {
		if rlErr := relinkShims(stellaHome, miseBin); rlErr != nil {
			slog.Warn("manifest plugin reconcile: relink shims failed", "error", rlErr)
		}
	}

	// Resolve concrete versions for cache misses via the persisted config in a
	// neutral cwd (no ambient project mise.toml). Cache hits skip mise entirely.
	var resolveEnv []string
	if miseErr == nil {
		resolveEnv, miseErr = scopeMiseEnv(stellaHome, builtinScope)
	}
	resolveDir, dirErr := os.MkdirTemp("", "stella-mise-resolve-*")
	if dirErr != nil && miseErr == nil {
		miseErr = dirErr
	}
	defer func() {
		if resolveDir != "" {
			_ = os.RemoveAll(resolveDir)
		}
	}()

	for _, plugin := range m.Plugins {
		if !plugin.Enabled {
			continue
		}

		pr := PluginReconcileResult{PluginID: plugin.ID}

		for _, binary := range plugin.Binaries {
			if ctx.Err() != nil {
				slog.Info("manifest plugin reconcile aborted", "reason", ctx.Err())
				result.Plugins[plugin.ID] = pr
				goto done
			}

			// Cache hit: state already records this version — report it without
			// shelling out to mise.
			if isCacheHit(state, plugin.ID, binary.Name, binary.Version) {
				slog.Info("manifest binary cache hit",
					"plugin", plugin.ID,
					"binary", binary.Name,
					"version", binary.Version)
				pr.Binaries = append(pr.Binaries, BinaryReconcileResult{
					Name:     binary.Name,
					Version:  binary.Version,
					CacheHit: true,
				})
				continue
			}

			// Cache miss: resolve the concrete installed version via the shims
			// config. Surface config/install/resolution failures per binary.
			version := binary.Version
			var binErr error
			switch {
			case configErr != nil:
				binErr = configErr
			case installErr != nil:
				binErr = installErr
			case miseErr != nil:
				binErr = miseErr
			default:
				if v, verr := resolveToolVersion(ctx, miseBin, resolveEnv, resolveDir, BinaryLookupName(binary)); verr != nil {
					binErr = verr
				} else {
					version = v
				}
			}

			if binErr != nil {
				slog.Error("manifest binary install failed",
					"plugin", plugin.ID,
					"binary", binary.Name,
					"error", binErr)
				pr.Binaries = append(pr.Binaries, BinaryReconcileResult{
					Name:    binary.Name,
					Version: binary.Version,
					Err:     binErr,
				})
				errorCount++
				continue
			}

			slog.Info("manifest binary installed",
				"plugin", plugin.ID,
				"binary", binary.Name,
				"version", version)

			pr.Binaries = append(pr.Binaries, BinaryReconcileResult{
				Name:    binary.Name,
				Version: version,
			})

			upsertBinaryState(state, plugin.ID, BinaryInstallState{
				Name:        binary.Name,
				Tool:        binary.Tool,
				Spec:        binary.Version,
				Version:     version,
				InstalledAt: time.Now(),
			})
		}

		result.Plugins[plugin.ID] = pr
	}

done:
	state.UpdatedAt = time.Now()
	if saveErr := SaveState(statePath, state); saveErr != nil {
		slog.Error("manifest plugin reconcile: failed to save state", "error", saveErr)
	}

	slog.Info("manifest plugin reconcile done",
		"enabled_plugins", enabledCount,
		"errors", errorCount)

	return result
}
