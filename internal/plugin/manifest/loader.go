package manifest

import (
	"gopkg.in/yaml.v3"

	"github.com/CherryHQ/stella/resources"
)

// LoadBuiltin returns the manifest baked into the binary at build time. This is
// the single source of truth for plugin defaults; overrides live in the
// plugin_override DB table.
func LoadBuiltin() (*Manifest, error) {
	oauth, err := parseRawYAML(resources.BuiltinOAuthYAML())
	if err != nil {
		return nil, err
	}
	tools, err := parseRawYAML(resources.BuiltinToolsYAML())
	if err != nil {
		return nil, err
	}
	oauth.Plugins = tools.Plugins
	return rawToManifest(oauth), nil
}

func parseRawYAML(data []byte) (rawManifest, error) {
	var rm rawManifest
	if err := yaml.Unmarshal(data, &rm); err != nil {
		return rawManifest{}, err
	}
	return rm, nil
}

func rawToManifest(rm rawManifest) *Manifest {
	m := &Manifest{
		OAuthProviders: make([]ManifestOAuthProvider, 0, len(rm.OAuthProviders)),
		Plugins:        make([]ManifestPlugin, 0, len(rm.Plugins)),
	}
	for _, ro := range rm.OAuthProviders {
		m.OAuthProviders = append(m.OAuthProviders, resolveOAuthProvider(ro))
	}
	for _, rp := range rm.Plugins {
		m.Plugins = append(m.Plugins, resolvePlugin(rp))
	}
	return m
}

func resolveOAuthProvider(ro rawManifestOAuthProvider) ManifestOAuthProvider {
	flows := make([]ManifestOAuthFlow, 0, len(ro.Flows))
	for _, rf := range ro.Flows {
		flows = append(flows, ManifestOAuthFlow(rf))
	}
	return ManifestOAuthProvider{
		ID:           ro.ID,
		Icon:         ro.Icon,
		Scopes:       ro.Scopes,
		VaultKey:     ro.VaultKey,
		Flows:        flows,
		ClientID:     ro.ClientID,
		ClientSecret: ro.ClientSecret,
	}
}

func resolvePlugin(rp rawManifestPlugin) ManifestPlugin {
	enabled := false
	if rp.Enabled != nil {
		enabled = *rp.Enabled
	}
	return ManifestPlugin{
		ID:                       rp.ID,
		Kind:                     rp.Kind,
		Enabled:                  enabled,
		Essential:                rp.Essential,
		BundledBinaries:          append([]string(nil), rp.BundledBinaries...),
		ManifestPluginDefinition: rp.ManifestPluginDefinition,
	}
}
