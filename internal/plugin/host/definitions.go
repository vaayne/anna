package host

import (
	"fmt"

	"github.com/CherryHQ/stella/internal/plugin"
	pkgplugins "github.com/CherryHQ/stella/pkg/plugins"
)

// BuiltinDefinitions projects the code registry used to compose this host.
// The registry supplies implementation identities; persisted state is never
// consulted when producing release defaults. Instance AdminSpec defaults are
// separate from platform plugin configuration and must not become its base.
func (h *Host) BuiltinDefinitions(code *pkgplugins.Catalog) ([]plugin.Definition, error) {
	if code == nil {
		return nil, nil
	}
	metadata := make(map[string]pkgplugins.PluginInfo)
	for _, info := range h.ListRegisteredPlugins() {
		metadata[info.ID] = info
	}
	definitions := make([]plugin.Definition, 0, len(code.Names()))
	for _, id := range code.Names() {
		info, exists := metadata[id]
		if !exists {
			return nil, fmt.Errorf("plugin %s: code registration missing from host", id)
		}
		definition := plugin.Definition{
			ID: id, Namespace: info.Name, DisplayName: info.DisplayName,
			Backend: plugin.BackendGo, Source: plugin.SourceBuiltin,
			ImplementationKey: id, Spec: []byte(`{}`), DefaultEnabled: true, Revision: 1,
		}
		if err := definition.Validate(); err != nil {
			return nil, fmt.Errorf("plugin %s: %w", id, err)
		}
		definitions = append(definitions, definition)
	}
	return definitions, nil
}
