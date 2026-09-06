package host

import (
	"encoding/json"
	"fmt"

	"github.com/CherryHQ/stella/internal/plugin"
	pkgplugins "github.com/CherryHQ/stella/pkg/plugins"
)

// snapshotState accepts only the exposed, trusted Go implementation. A custom
// plugin claiming its namespace cannot acquire the old implementation's hooks.
func snapshotState(snapshot plugin.Snapshot, pluginID string) (pkgplugins.PluginState, bool, error) {
	resolved, ok := snapshot.Get(pluginID)
	if !ok || !resolved.Effective.IsEffectivelyEnabled {
		return pkgplugins.PluginState{}, false, nil
	}
	if resolved.Config == nil || resolved.Config.ID == "" || resolved.Config.Revision <= 0 {
		return pkgplugins.PluginState{}, false, fmt.Errorf("plugin %q has no runtime configuration identity", pluginID)
	}
	def := resolved.Definition
	if def.Source != plugin.SourceBuiltin || def.Backend != plugin.BackendGo || def.ImplementationKey != pluginID {
		return pkgplugins.PluginState{}, false, nil
	}
	winner, err := snapshot.ResolveNamespace(def.Namespace)
	if err != nil {
		return pkgplugins.PluginState{}, false, err
	}
	if winner.PluginID != pluginID || !winner.IsEffectivelyEnabled {
		return pkgplugins.PluginState{}, false, nil
	}
	var config map[string]any
	if err := json.Unmarshal(winner.Payload, &config); err != nil {
		return pkgplugins.PluginState{}, false, fmt.Errorf("plugin %q runtime config: %w", pluginID, err)
	}
	return pkgplugins.PluginState{ID: pluginID, Enabled: true, Config: config}, true, nil
}
