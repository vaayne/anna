package manifest

import (
	"encoding/json"
	"fmt"

	"github.com/CherryHQ/stella/internal/plugin"
)

// BuiltinDefinitions normalizes release assets before the catalog transaction.
// It deliberately loads the embedded input, never a resolved admin override.
func BuiltinDefinitions() ([]plugin.Definition, error) {
	manifest, err := LoadBuiltin()
	if err != nil {
		return nil, err
	}
	if err := Validate(manifest); err != nil {
		return nil, err
	}
	definitions := make([]plugin.Definition, 0, len(manifest.Plugins))
	for _, authored := range manifest.Plugins {
		spec, err := json.Marshal(map[string]any{
			"description":    authored.Description,
			"category":       authored.Category,
			"prompt":         authored.Prompt,
			"binaries":       authored.Binaries,
			"skills":         authored.Skills,
			"session_env":    authored.SessionEnvs,
			"oauth_provider": authored.OAuthProvider,
		})
		if err != nil {
			return nil, fmt.Errorf("plugin %s: %w", authored.ID, err)
		}
		definition := plugin.Definition{
			ID: authored.ID, Namespace: authored.Name, DisplayName: authored.DisplayName,
			Backend: plugin.BackendCLI, Source: plugin.SourceBuiltin,
			ImplementationKey: authored.ID, Spec: spec,
			DefaultEnabled: authored.Enabled, Revision: 1,
		}
		if err := definition.Validate(); err != nil {
			return nil, fmt.Errorf("plugin %s: %w", authored.ID, err)
		}
		definitions = append(definitions, definition)
	}
	return definitions, nil
}
