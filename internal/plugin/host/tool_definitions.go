package host

import (
	"fmt"
	"sort"
	"strings"

	"github.com/CherryHQ/stella/internal/plugin"
	"github.com/CherryHQ/stella/pkg/toolmeta"
)

// BuiltinToolDefinitions projects generated Go tool metadata into the unified
// plugin definition catalog. The generated declaration is the sole authority
// for plugin ID and namespace; ownership is never derived from a tool name.
func BuiltinToolDefinitions(reg *toolmeta.Registry) ([]plugin.Definition, error) {
	if reg == nil {
		return nil, nil
	}

	identities := make(map[string]string)
	owners := make(map[string]string)
	for _, tool := range reg.Tools() {
		if tool.PluginID == "" {
			if tool.Namespace != "" || tool.LocalName != "" {
				return nil, fmt.Errorf("plugin tool %q: core declaration has plugin metadata", tool.Name)
			}
			continue
		}
		if tool.Namespace == "" || tool.LocalName == "" {
			return nil, fmt.Errorf("plugin tool %q: incomplete trusted identity", tool.Name)
		}
		exported, err := plugin.ExportedToolName(tool.Namespace, tool.LocalName)
		if err != nil {
			return nil, fmt.Errorf("plugin tool %q: %w", tool.Name, err)
		}
		if exported != tool.Name {
			return nil, fmt.Errorf("plugin tool %q: trusted identity exports %q", tool.Name, exported)
		}
		if previous, exists := identities[tool.PluginID]; exists && previous != tool.Namespace {
			return nil, fmt.Errorf("plugin %q uses namespaces %q and %q", tool.PluginID, previous, tool.Namespace)
		}
		if previous, exists := owners[tool.Namespace]; exists && previous != tool.PluginID {
			return nil, fmt.Errorf("namespace %q is owned by both plugins %q and %q", tool.Namespace, previous, tool.PluginID)
		}
		identities[tool.PluginID] = tool.Namespace
		owners[tool.Namespace] = tool.PluginID
	}

	ids := make([]string, 0, len(identities))
	for id := range identities {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	definitions := make([]plugin.Definition, 0, len(ids))
	for _, id := range ids {
		namespace := identities[id]
		definition := plugin.Definition{
			ID: id, Namespace: namespace, DisplayName: displayName(namespace),
			Backend: plugin.BackendGo, Source: plugin.SourceBuiltin,
			ImplementationKey: id, Spec: []byte(`{}`), DefaultEnabled: true, Revision: 1,
		}
		if err := definition.Validate(); err != nil {
			return nil, fmt.Errorf("plugin %q: %w", id, err)
		}
		definitions = append(definitions, definition)
	}
	return definitions, nil
}

func displayName(namespace string) string {
	if namespace == "" {
		return ""
	}
	return strings.ToUpper(namespace[:1]) + namespace[1:]
}
