package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"strings"

	apitypes "github.com/CherryHQ/stella/api/types"
	"github.com/CherryHQ/stella/internal/mcp"
	pluginpkg "github.com/CherryHQ/stella/internal/plugin"
	"github.com/CherryHQ/stella/internal/plugin/manifest"
)

// pluginBackendSummary is the only response projection of plugin.Config.Payload.
// Each constructor below decodes into a backend-owned typed shape and copies
// only fields explicitly approved for the public management response.
func pluginBackendSummary(definition pluginpkg.Definition, config pluginpkg.Config) (apitypes.PluginConfig_BackendSummary, error) {
	var summary apitypes.PluginConfig_BackendSummary
	switch definition.Backend {
	case pluginpkg.BackendCLI:
		value, err := cliBackendSummary(definition.Spec, config.Payload, config.Enabled)
		if err != nil {
			return summary, err
		}
		return summary, summary.FromPluginCLIBackendSummary(value)
	case pluginpkg.BackendMCP:
		value, err := mcpBackendSummary(definition.Spec, config.Payload, config.CredentialRefs)
		if err != nil {
			return summary, err
		}
		return summary, summary.FromPluginMCPBackendSummary(value)
	case pluginpkg.BackendGo:
		value := goBackendSummary(definition, config)
		return summary, summary.FromPluginGoBackendSummary(value)
	default:
		return summary, fmt.Errorf("%w: unsupported plugin backend %q", pluginpkg.ErrInvalidConfig, definition.Backend)
	}
}

func cliBackendSummary(definitionSpec, configPayload json.RawMessage, enabled *bool) (apitypes.PluginCLIBackendSummary, error) {
	result := apitypes.PluginCLIBackendSummary{
		Backend:                 apitypes.PluginCLIBackendSummaryBackendCli,
		Binaries:                []apitypes.PluginCLIBackendBinarySummary{},
		Skills:                  []apitypes.PluginCLIBackendSkillSummary{},
		SessionEnv:              []apitypes.PluginCLIBackendSessionEnvSummary{},
		OauthProviderConfigured: false,
	}
	// A negative config intentionally has no selected resource to project. The
	// builtin system row uses enabled=NULL and an empty overlay, so it still
	// projects its shipped resources below.
	if enabled != nil && !*enabled && emptyJSON(configPayload) {
		return result, nil
	}
	raw, err := mergeCLISummaryPayload(definitionSpec, configPayload)
	if err != nil {
		return result, err
	}
	payload, err := manifest.DecodeCLIPayload(raw, "resolved CLI payload")
	if err != nil {
		return result, err
	}
	for _, binary := range payload.Binaries {
		result.Binaries = append(result.Binaries, apitypes.PluginCLIBackendBinarySummary{
			Name: binary.Name, Version: binary.Version,
		})
	}
	for _, skill := range payload.Skills {
		result.Skills = append(result.Skills, apitypes.PluginCLIBackendSkillSummary{
			Name: skill.Name,
		})
	}
	for _, env := range payload.SessionEnvs {
		result.SessionEnv = append(result.SessionEnv, apitypes.PluginCLIBackendSessionEnvSummary{
			EnvVar: env.EnvVar, Source: env.Source, Required: env.Required,
		})
	}
	result.OauthProviderConfigured = strings.TrimSpace(payload.OAuthProvider) != ""
	return result, nil
}

func mergeCLISummaryPayload(definitionSpec, configPayload json.RawMessage) (json.RawMessage, error) {
	base := map[string]json.RawMessage{}
	if !emptyJSON(definitionSpec) {
		if err := json.Unmarshal(definitionSpec, &base); err != nil || base == nil {
			return nil, fmt.Errorf("invalid CLI definition payload")
		}
	}
	if !emptyJSON(configPayload) {
		var overlay map[string]json.RawMessage
		if err := json.Unmarshal(configPayload, &overlay); err != nil || overlay == nil {
			return nil, fmt.Errorf("invalid CLI config payload")
		}
		maps.Copy(base, overlay)
	}
	return json.Marshal(base)
}

type mcpSummaryPayload struct {
	URL            *string             `json:"url,omitempty"`
	Transport      *string             `json:"transport,omitempty"`
	AuthType       *string             `json:"auth_type,omitempty"`
	CredentialMode *string             `json:"credential_mode,omitempty"`
	Metadata       *mcpSummaryMetadata `json:"metadata,omitempty"`
}

type mcpSummaryMetadata struct {
	OAuth *mcpOAuthSummaryMetadata `json:"oauth,omitempty"`
}

type mcpOAuthSummaryMetadata struct {
	ClientID string `json:"client_id,omitempty"`
}

type mcpCredentialSummaryRefs struct {
	Bearer            json.RawMessage `json:"bearer,omitempty"`
	OAuthBundle       json.RawMessage `json:"oauth_bundle,omitempty"`
	OAuthClientSecret json.RawMessage `json:"oauth_client_secret,omitempty"`
}

func mcpBackendSummary(definitionSpec, configPayload, credentialRefs json.RawMessage) (apitypes.PluginMCPBackendSummary, error) {
	definition, err := decodeMCPBackendSummaryPayload(definitionSpec)
	if err != nil {
		return apitypes.PluginMCPBackendSummary{}, err
	}
	overlay, err := decodeMCPBackendSummaryPayload(configPayload)
	if err != nil {
		return apitypes.PluginMCPBackendSummary{}, err
	}
	mergeMCPBackendSummaryPayload(&definition, overlay)

	transport := mcp.TransportStreamableHTTP
	if definition.Transport != nil && *definition.Transport != "" {
		transport = *definition.Transport
	}
	if !mcp.ValidTransport(transport) {
		return apitypes.PluginMCPBackendSummary{}, fmt.Errorf("unsupported MCP transport %q", transport)
	}
	authType := mcp.AuthTypeNone
	if definition.AuthType != nil && *definition.AuthType != "" {
		authType = *definition.AuthType
	}
	if !mcp.ValidAuthType(authType) {
		return apitypes.PluginMCPBackendSummary{}, fmt.Errorf("unsupported MCP auth type %q", authType)
	}
	credentialMode := mcp.CredentialModeShared
	if definition.CredentialMode != nil && *definition.CredentialMode != "" {
		credentialMode = *definition.CredentialMode
	}
	if !mcp.ValidCredentialMode(credentialMode) {
		return apitypes.PluginMCPBackendSummary{}, fmt.Errorf("unsupported MCP credential mode %q", credentialMode)
	}
	refs, err := decodeMCPCredentialSummaryRefs(credentialRefs)
	if err != nil {
		return apitypes.PluginMCPBackendSummary{}, err
	}
	return apitypes.PluginMCPBackendSummary{
		Backend:                     apitypes.Mcp,
		Transport:                   apitypes.PluginMCPBackendSummaryTransport(transport),
		AuthType:                    apitypes.PluginMCPBackendSummaryAuthType(authType),
		CredentialMode:              apitypes.PluginMCPBackendSummaryCredentialMode(credentialMode),
		EndpointConfigured:          definition.URL != nil && strings.TrimSpace(*definition.URL) != "",
		BearerConfigured:            configuredJSON(refs.Bearer),
		OauthClientIdConfigured:     definition.Metadata != nil && definition.Metadata.OAuth != nil && strings.TrimSpace(definition.Metadata.OAuth.ClientID) != "",
		OauthClientSecretConfigured: configuredJSON(refs.OAuthClientSecret),
	}, nil
}

func decodeMCPBackendSummaryPayload(raw json.RawMessage) (mcpSummaryPayload, error) {
	if emptyJSON(raw) {
		return mcpSummaryPayload{}, nil
	}
	var payload mcpSummaryPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return mcpSummaryPayload{}, fmt.Errorf("invalid MCP backend payload: %w", err)
	}
	return payload, nil
}

func mergeMCPBackendSummaryPayload(base *mcpSummaryPayload, overlay mcpSummaryPayload) {
	if overlay.URL != nil {
		base.URL = overlay.URL
	}
	if overlay.Transport != nil {
		base.Transport = overlay.Transport
	}
	if overlay.AuthType != nil {
		base.AuthType = overlay.AuthType
	}
	if overlay.CredentialMode != nil {
		base.CredentialMode = overlay.CredentialMode
	}
	if overlay.Metadata != nil {
		base.Metadata = overlay.Metadata
	}
}

func decodeMCPCredentialSummaryRefs(raw json.RawMessage) (mcpCredentialSummaryRefs, error) {
	if emptyJSON(raw) {
		return mcpCredentialSummaryRefs{}, nil
	}
	var refs mcpCredentialSummaryRefs
	if err := json.Unmarshal(raw, &refs); err != nil {
		return refs, fmt.Errorf("invalid MCP credential refs: %w", err)
	}
	return refs, nil
}

func goBackendSummary(definition pluginpkg.Definition, config pluginpkg.Config) apitypes.PluginGoBackendSummary {
	result := apitypes.PluginGoBackendSummary{
		Backend:    apitypes.PluginGoBackendSummaryBackendGo,
		Configured: !emptyJSON(config.Payload),
	}
	if strings.HasPrefix(definition.ID, "channel/") {
		kind := apitypes.PluginGoBackendSummaryKindChannel
		result.Kind = &kind
	}
	return result
}

func emptyJSON(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) || bytes.Equal(trimmed, []byte("{}"))
}

func configuredJSON(raw json.RawMessage) bool {
	return !emptyJSON(raw)
}
