package host

import (
	"cmp"
	"context"
	"fmt"
	"slices"

	"github.com/CherryHQ/stella/internal/plugin"
	"github.com/CherryHQ/stella/internal/plugin/manifest"
	pkgplugins "github.com/CherryHQ/stella/pkg/plugins"
)

// SessionPluginView projects one already-authorized plugin snapshot for a
// runner. The snapshot is the only source of selected config and enabled state.
func (h *Host) SessionPluginView(snapshot plugin.Snapshot) (pkgplugins.SessionPluginView, error) {
	definitions := snapshot.Definitions()
	view := pkgplugins.SessionPluginView{
		RegisteredPluginIDs: make([]string, 0, len(definitions)),
	}
	for _, definition := range definitions {
		view.RegisteredPluginIDs = append(view.RegisteredPluginIDs, definition.ID)

		resolved, ok := snapshot.Get(definition.ID)
		if !ok {
			return pkgplugins.SessionPluginView{}, fmt.Errorf("resolve plugin %q", definition.ID)
		}
		if !resolved.Effective.IsEffectivelyEnabled {
			continue
		}

		// A namespace winner is the only definition allowed to advertise its
		// exported resources. ID resolution remains independent for callers such
		// as channel/background integrations.
		winner, err := snapshot.ResolveNamespace(definition.Namespace)
		if err != nil {
			return pkgplugins.SessionPluginView{}, fmt.Errorf("resolve namespace %q: %w", definition.Namespace, err)
		}
		if winner.PluginID != definition.ID || !winner.IsEffectivelyEnabled {
			continue
		}
		identity, err := selectedResourceIdentity(definition, resolved)
		if err != nil {
			return pkgplugins.SessionPluginView{}, err
		}
		view.ExposedPluginIDs = append(view.ExposedPluginIDs, definition.ID)

		switch definition.Backend {
		case plugin.BackendCLI:
			if err := validateResolvedCLIPayload(definition, resolved); err != nil {
				return pkgplugins.SessionPluginView{}, err
			}
			payload, err := manifest.DecodeCLIPayload(resolved.Effective.Payload, "selected CLI payload")
			if err != nil {
				return pkgplugins.SessionPluginView{}, fmt.Errorf("plugin %q: %w", definition.ID, err)
			}
			appendCLIResources(&view, identity, payload)
		case plugin.BackendGo:
			appendGoResources(h, &view, identity, definition)
		case plugin.BackendMCP:
			// MCP resources are discovered by its provider, not from a CLI
			// payload. Its enabled identity is still in the view above.
		}
	}

	slices.Sort(view.RegisteredPluginIDs)
	slices.Sort(view.ExposedPluginIDs)
	slices.SortFunc(view.SessionEnvSpecs, func(left, right pkgplugins.SessionEnvSpec) int {
		if left.EnvVar != right.EnvVar {
			return cmp.Compare(left.EnvVar, right.EnvVar)
		}
		if left.PluginID != right.PluginID {
			return cmp.Compare(left.PluginID, right.PluginID)
		}
		return cmp.Compare(left.ConfigID, right.ConfigID)
	})
	slices.SortFunc(view.BinarySpecs, func(left, right pkgplugins.PluginBinarySpec) int {
		if left.PluginID != right.PluginID {
			return cmp.Compare(left.PluginID, right.PluginID)
		}
		if left.Name != right.Name {
			return cmp.Compare(left.Name, right.Name)
		}
		return cmp.Compare(left.ConfigID, right.ConfigID)
	})
	return view, nil
}

// validateResolvedCLIPayload re-runs the backend boundary after resolution.
// A config saved while disabled may be structurally valid but incomplete; a
// capability lift must not turn that dormant payload into an executable one.
func validateResolvedCLIPayload(definition plugin.Definition, resolved plugin.ResolvedPlugin) error {
	if resolved.Config == nil {
		return fmt.Errorf("plugin %q is enabled without a selected config", definition.ID)
	}
	config := *resolved.Config
	enabled := true
	config.Enabled = &enabled
	config.Payload = resolved.Effective.Payload
	if err := manifest.ValidatePayload(context.Background(), definition, config, nil); err != nil {
		return fmt.Errorf("validate selected CLI payload for plugin %q: %w", definition.ID, err)
	}
	return nil
}

func selectedResourceIdentity(definition plugin.Definition, resolved plugin.ResolvedPlugin) (pkgplugins.PluginResourceIdentity, error) {
	if resolved.Config == nil {
		return pkgplugins.PluginResourceIdentity{}, fmt.Errorf("plugin %q is enabled without a selected config", definition.ID)
	}
	return pkgplugins.PluginResourceIdentity{
		PluginID: definition.ID,
		ConfigID: resolved.Config.ID,
		Scope:    string(resolved.Config.Scope),
		Revision: resolved.Config.Revision,
	}, nil
}

func appendCLIResources(view *pkgplugins.SessionPluginView, identity pkgplugins.PluginResourceIdentity, payload manifest.CLIPayload) {
	for _, binary := range payload.Binaries {
		view.BinarySpecs = append(view.BinarySpecs, pkgplugins.PluginBinarySpec{
			PluginResourceIdentity: identity,
			Name:                   binary.Name,
			Tool:                   binary.Tool,
			Version:                binary.Version,
			Options:                cloneMap(binary.Options),
		})
	}
	for _, env := range payload.SessionEnvs {
		view.SessionEnvSpecs = append(view.SessionEnvSpecs, pkgplugins.SessionEnvSpec{
			PluginID:        identity.PluginID,
			ConfigID:        identity.ConfigID,
			Scope:           identity.Scope,
			Revision:        identity.Revision,
			EnvVar:          env.EnvVar,
			Source:          pkgplugins.SessionEnvSource(env.Source),
			Value:           env.Value,
			Required:        env.Required,
			OAuthProviderID: payload.OAuthProvider,
		})
	}
}

func appendGoResources(h *Host, view *pkgplugins.SessionPluginView, identity pkgplugins.PluginResourceIdentity, definition plugin.Definition) {
	if definition.Source != plugin.SourceBuiltin || definition.ImplementationKey != definition.ID {
		return
	}
	h.mu.RLock()
	_, registered := h.pluginIDs[definition.ImplementationKey]
	envs := append([]pkgplugins.SessionEnvSpec(nil), h.sessionEnvRegs[definition.ID]...)
	h.mu.RUnlock()
	if !registered {
		return
	}
	for _, env := range envs {
		env.PluginID = identity.PluginID
		env.ConfigID = identity.ConfigID
		env.Scope = identity.Scope
		env.Revision = identity.Revision
		view.SessionEnvSpecs = append(view.SessionEnvSpecs, env)
	}
}
