package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	apitypes "github.com/CherryHQ/stella/api/types"
	"github.com/CherryHQ/stella/internal/authz"
	"github.com/CherryHQ/stella/internal/mcp"
	pluginpkg "github.com/CherryHQ/stella/internal/plugin"
)

type pluginMCPEdit struct {
	URL            *string         `json:"url"`
	Transport      *string         `json:"transport"`
	AuthType       *string         `json:"auth_type"`
	CredentialMode *string         `json:"credential_mode"`
	Metadata       *map[string]any `json:"metadata"`
	Description    *string         `json:"description"`
}

type pluginMCPSecrets struct {
	Token        *string `json:"token"`
	ClientID     *string `json:"oauth_client_id"`
	ClientSecret *string `json:"oauth_client_secret"`
}

// Only these authored fields cross into the backend. Owner tuples and Vault
// locators are derived inside the common transaction, never decoded here.
func decodePluginMCPEdit(payload, credentials *map[string]any) (pluginMCPEdit, pluginMCPSecrets, error) {
	var edit pluginMCPEdit
	var secrets pluginMCPSecrets
	decode := func(value *map[string]any, destination any) error {
		if value == nil {
			return nil
		}
		for _, field := range *value {
			if field == nil {
				return pluginpkg.ErrInvalidConfig
			}
		}
		raw, err := json.Marshal(*value)
		if err != nil {
			return pluginpkg.ErrInvalidConfig
		}
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(destination); err != nil {
			return pluginpkg.ErrInvalidConfig
		}
		return nil
	}
	if err := decode(payload, &edit); err != nil {
		return edit, secrets, err
	}
	if err := decode(credentials, &secrets); err != nil {
		return edit, secrets, err
	}
	return edit, secrets, nil
}

func pluginMCPCreateInput(authority authz.Authority, scope, agentID, name string, enabled *bool, payload, credentials *map[string]any) (mcp.CreateInput, error) {
	edit, secrets, err := decodePluginMCPEdit(payload, credentials)
	if err != nil {
		return mcp.CreateInput{}, err
	}
	input := mcp.CreateInput{Scope: scope, AgentID: agentID, Name: name, EnabledSet: true, Enabled: enabled, Description: edit.Description}
	if scope == mcp.ScopeUser || scope == mcp.ScopeUserAgent {
		input.UserID = string(authority.UserID())
	}
	if edit.URL != nil {
		input.URL = *edit.URL
	}
	if edit.Transport != nil {
		input.Transport = *edit.Transport
	}
	if edit.AuthType != nil {
		input.AuthType = *edit.AuthType
	}
	if edit.CredentialMode != nil {
		input.CredentialMode = *edit.CredentialMode
	}
	if edit.Metadata != nil {
		input.Metadata = *edit.Metadata
	}
	if secrets.Token != nil {
		input.Token = *secrets.Token
	}
	if secrets.ClientID != nil {
		input.OAuthClientID = *secrets.ClientID
	}
	if secrets.ClientSecret != nil {
		input.OAuthClientSecret = *secrets.ClientSecret
	}
	if input.URL == "" {
		return mcp.CreateInput{}, pluginpkg.ErrInvalidConfig
	}
	return input, nil
}

func (s *Server) createMCPPlugin(w http.ResponseWriter, r *http.Request, authority authz.Authority, request apitypes.CreatePluginRequest) {
	if s.mcpSvc == nil {
		writePluginError(w, errPluginCapabilityUnavailable)
		return
	}
	input, err := pluginMCPCreateInput(authority, string(request.InitialConfig.Scope), mcpAgentID(request.InitialConfig.AgentId), request.DisplayName, request.InitialConfig.IsEnabled, request.InitialConfig.Config, request.InitialConfig.Credentials)
	if err != nil {
		writePluginError(w, err)
		return
	}
	definition, config, err := s.mcpSvc.CreateCustom(authz.WithAuthority(r.Context(), authority), pluginpkg.Definition{Namespace: request.Namespace, DisplayName: request.DisplayName, Backend: pluginpkg.BackendMCP, Spec: mustJSON(request.DefinitionSpec)}, input)
	if err != nil {
		writePluginError(w, err)
		return
	}
	defView, err := pluginDefinitionView(definition)
	if err != nil {
		writePluginError(w, err)
		return
	}
	cfgView, err := pluginConfigView(definition, config)
	if err != nil {
		writePluginError(w, err)
		return
	}
	writeData(w, http.StatusCreated, apitypes.CreatePluginResponse{Plugin: defView, Config: cfgView})
}

func (s *Server) createMCPPluginConfig(ctx context.Context, authority authz.Authority, access *pluginpkg.Access, definition pluginpkg.Definition, request apitypes.CreatePluginConfigRequest) (pluginpkg.Config, error) {
	if s.mcpSvc == nil {
		return pluginpkg.Config{}, errPluginCapabilityUnavailable
	}
	input, err := pluginMCPCreateInput(authority, string(request.Scope), mcpAgentID(request.AgentId), definition.DisplayName, request.IsEnabled, request.Config, request.Credentials)
	if err != nil {
		return pluginpkg.Config{}, err
	}
	input.PluginID = definition.ID
	registration, err := s.mcpSvc.Create(authz.WithAuthority(ctx, authority), input)
	if err != nil {
		return pluginpkg.Config{}, err
	}
	return access.GetConfig(ctx, definition.ID, registration.ID)
}

func (s *Server) updateMCPPluginConfig(ctx context.Context, authority authz.Authority, access *pluginpkg.Access, current pluginpkg.Config, request apitypes.UpdatePluginConfigRequest, raw map[string]json.RawMessage) (pluginpkg.Config, error) {
	if s.mcpSvc == nil {
		return pluginpkg.Config{}, errPluginCapabilityUnavailable
	}
	if request.ExpectedRevision != current.Revision {
		return pluginpkg.Config{}, pluginpkg.ErrConflict
	}
	if request.BinaryVersions != nil || request.ResetFields != nil {
		return pluginpkg.Config{}, pluginpkg.ErrInvalidConfig
	}
	if value, exists := raw["config"]; exists && bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
		return pluginpkg.Config{}, pluginpkg.ErrInvalidConfig
	}
	edit, secrets, err := decodePluginMCPEdit(request.Config, request.Credentials)
	if err != nil {
		return pluginpkg.Config{}, err
	}
	ctx = authz.WithAuthority(ctx, authority)
	registration, err := s.mcpSvc.Get(ctx, current.ID, string(current.Scope), current.UserID, current.AgentID)
	if err != nil {
		return pluginpkg.Config{}, err
	}
	if registration.ConfigRevision != request.ExpectedRevision {
		return pluginpkg.Config{}, pluginpkg.ErrConflict
	}
	_, enabledSet := raw["is_enabled"]
	_, err = s.mcpSvc.UpdateIfVersion(ctx, mcp.UpdateInput{
		ID: current.ID, Scope: string(current.Scope), UserID: current.UserID, AgentID: current.AgentID,
		URL: edit.URL, Transport: edit.Transport, AuthType: edit.AuthType, CredentialMode: edit.CredentialMode,
		Metadata: edit.Metadata, Description: edit.Description, EnabledSet: enabledSet, Enabled: request.IsEnabled,
		Token: secrets.Token, OAuthClientID: secrets.ClientID, OAuthClientSecret: secrets.ClientSecret,
	}, registration.Version())
	if err != nil {
		return pluginpkg.Config{}, fmt.Errorf("MCP config mutation: %w", err)
	}
	return access.GetConfig(ctx, current.PluginID, current.ID)
}
