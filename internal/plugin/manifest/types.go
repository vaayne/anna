package manifest

// ManifestPluginDefinition is the editable definition of a manifest plugin.
// Resource state and server-owned metadata live on ManifestPlugin instead.
type ManifestPluginDefinition struct {
	Name          string               `json:"name" yaml:"name"`
	DisplayName   string               `json:"display_name" yaml:"display_name"`
	Description   string               `json:"description" yaml:"description"`
	Category      string               `json:"category,omitempty" yaml:"category,omitempty"`
	Prompt        string               `json:"prompt,omitempty" yaml:"prompt,omitempty"`
	Binaries      []ManifestBinary     `json:"binaries,omitempty" yaml:"binaries,omitempty"`
	Skills        []ManifestSkill      `json:"skills,omitempty" yaml:"skills,omitempty"`
	SessionEnvs   []ManifestSessionEnv `json:"session_env,omitempty" yaml:"session_env,omitempty"`
	OAuthProvider string               `json:"oauth_provider,omitempty" yaml:"oauth_provider,omitempty"`
}

type ManifestPlugin struct {
	ID      string `json:"id" yaml:"id"`
	Kind    string `json:"kind" yaml:"kind"`
	Enabled bool   `json:"enabled" yaml:"enabled"`

	// Essential marks a plugin the runtime depends on (e.g. rg/fd back the
	// Grep/Glob tools). It is shipped server policy, not editable definition.
	Essential bool `json:"essential,omitempty" yaml:"essential,omitempty"`

	// BundledBinaries are immutable release executables projected from the
	// authored manifest. They are deliberately outside the editable definition
	// and therefore cannot be enabled by a mutable plugin payload.
	BundledBinaries []string `json:"bundled_binaries,omitempty" yaml:"bundled_binaries,omitempty"`

	ManifestPluginDefinition `yaml:",inline"`

	// Builtin marks a plugin that ships with the server. It is computed when the
	// manifest is resolved, never read from a manifest or an override: a builtin
	// definition can be customized or disabled, but only an admin-added plugin
	// can be removed. `yaml:"-"` keeps it out of the shipped manifest.
	Builtin bool `json:"builtin,omitempty" yaml:"-"`

	// OverriddenFields names the definition fields an admin has taken ownership
	// of, so the editor can mark them and offer to hand one back. Like Builtin it
	// is computed at resolve time and never stored — the override row is what it
	// reports on. A boolean here would be cheaper and useless: "customized" cannot
	// tell an admin which of two pinned fields is the one they want released.
	OverriddenFields []string `json:"overridden_fields,omitempty" yaml:"-"`
}

type ManifestBinary struct {
	// Name is the binary name written to $STELLA_HOME/bin/.
	Name string `json:"name" yaml:"name"`

	// Tool is the mise tool key, e.g.:
	//   uv   bun   github:cli/cli   pipx:mypy   npm:serve   http:sentinel
	Tool string `json:"tool" yaml:"tool"`

	// Version to install; defaults to "latest" when omitted.
	Version string `json:"version,omitempty" yaml:"version,omitempty"`

	// Options are mise tool options, using the same names as mise.toml.
	Options map[string]any `json:"options,omitempty" yaml:",inline"`
}

type ManifestSkill struct {
	// Name is the local, release-owned skill identity. A manifest cannot point
	// at a repository or another source; the asset descriptor owns its bytes.
	Name string `json:"name" yaml:"name"`
}

type ManifestSessionEnv struct {
	EnvVar   string `json:"env_var" yaml:"env_var"`
	Source   string `json:"source" yaml:"source"`
	Value    string `json:"value,omitempty" yaml:"value,omitempty"`
	Required bool   `json:"required,omitempty" yaml:"required,omitempty"`
}

type ManifestOAuthFlow struct {
	Type          string `json:"type" yaml:"type"`
	AuthURL       string `json:"auth_url,omitempty" yaml:"auth_url,omitempty"`
	DeviceAuthURL string `json:"device_auth_url,omitempty" yaml:"device_auth_url,omitempty"`
	TokenURL      string `json:"token_url" yaml:"token_url"`
	AuthStyle     string `json:"auth_style,omitempty" yaml:"auth_style,omitempty"`
	PKCE          bool   `json:"pkce,omitempty" yaml:"pkce,omitempty"`
}

type ManifestOAuthProvider struct {
	ID           string              `json:"id" yaml:"id"`
	Icon         string              `json:"icon,omitempty" yaml:"icon,omitempty"`
	Scopes       []string            `json:"scopes" yaml:"scopes"`
	VaultKey     string              `json:"vault_key" yaml:"vault_key"`
	Flows        []ManifestOAuthFlow `json:"flows" yaml:"flows"`
	ClientID     string              `json:"client_id,omitempty" yaml:"client_id,omitempty"`
	ClientSecret string              `json:"client_secret,omitempty" yaml:"client_secret,omitempty"`
}

type Manifest struct {
	OAuthProviders []ManifestOAuthProvider `json:"oauth_providers,omitempty" yaml:"oauth_providers,omitempty"`
	Plugins        []ManifestPlugin        `json:"plugins" yaml:"plugins"`
}

type rawManifestPlugin struct {
	ID                       string   `yaml:"id"`
	Kind                     string   `yaml:"kind"`
	Enabled                  *bool    `yaml:"enabled"`
	Essential                bool     `yaml:"essential,omitempty"`
	BundledBinaries          []string `yaml:"bundled_binaries,omitempty"`
	ManifestPluginDefinition `yaml:",inline"`
}

type rawManifestOAuthFlow struct {
	Type          string `yaml:"type"`
	AuthURL       string `yaml:"auth_url,omitempty"`
	DeviceAuthURL string `yaml:"device_auth_url,omitempty"`
	TokenURL      string `yaml:"token_url"`
	AuthStyle     string `yaml:"auth_style,omitempty"`
	PKCE          bool   `yaml:"pkce,omitempty"`
}

type rawManifestOAuthProvider struct {
	ID           string                 `yaml:"id"`
	Icon         string                 `yaml:"icon,omitempty"`
	Scopes       []string               `yaml:"scopes"`
	VaultKey     string                 `yaml:"vault_key"`
	Flows        []rawManifestOAuthFlow `yaml:"flows"`
	ClientID     string                 `yaml:"client_id,omitempty"`
	ClientSecret string                 `yaml:"client_secret,omitempty"`
}

type rawManifest struct {
	OAuthProviders []rawManifestOAuthProvider `yaml:"oauth_providers,omitempty"`
	Plugins        []rawManifestPlugin        `yaml:"plugins"`
}
