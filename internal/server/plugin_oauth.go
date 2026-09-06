package server

import (
	"errors"
	"net/http"

	"github.com/google/uuid"

	apitypes "github.com/CherryHQ/stella/api/types"
	"github.com/CherryHQ/stella/internal/authz"
	agentaccess "github.com/CherryHQ/stella/internal/core/access"
	"github.com/CherryHQ/stella/internal/mcp"
)

// pluginOAuthRegistration applies the plugin route's parent identity before
// delegating visibility and scope ownership to MCP Access. GetVisible is
// deliberately used here: a per-user OAuth config in a system scope is
// visible to a user who is authorizing their own account, even though that
// user cannot manage the system config through plugin Access.
func (s *Server) pluginOAuthRegistration(w http.ResponseWriter, r *http.Request, kind, name, configID string) (*mcp.Access, mcp.Registration, bool) {
	access, _, ok := s.beginMCPAccess(w, r)
	if !ok {
		return nil, mcp.Registration{}, false
	}
	if configID == "" {
		writeError(w, http.StatusBadRequest, "config_id is required")
		return nil, mcp.Registration{}, false
	}
	reg, err := access.GetVisible(r.Context(), configID)
	if err != nil {
		writePluginOAuthError(w, err)
		return nil, mcp.Registration{}, false
	}
	if reg.PluginID != pluginID(kind, name) {
		writeError(w, http.StatusNotFound, "not found")
		return nil, mcp.Registration{}, false
	}
	if reg.AuthType != mcp.AuthTypeOAuth {
		writeError(w, http.StatusBadRequest, "plugin configuration does not use OAuth")
		return nil, mcp.Registration{}, false
	}
	return access, reg, true
}

func writePluginOAuthError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, mcp.ErrOAuthClientInitializationRequired),
		errors.Is(err, authz.ErrForbidden), errors.Is(err, authz.ErrNotFound),
		errors.Is(err, agentaccess.ErrForbidden), errors.Is(err, agentaccess.ErrNotFound):
		writePluginError(w, err)
	default:
		writeError(w, http.StatusBadRequest, "OAuth authorization could not be completed")
	}
}

// StartPluginConfigOAuth handles the common config-scoped OAuth action. The
// callback URL remains the existing MCP protocol endpoint; the durable flow
// carries the plugin config identity and revision through the exchange.
func (s *Server) StartPluginConfigOAuth(w http.ResponseWriter, r *http.Request, kind, name, configID string) {
	access, _, ok := s.pluginOAuthRegistration(w, r, kind, name, configID)
	if !ok {
		return
	}
	_, authorizationURL, flowID, expiresAt, err := access.StartOAuth(r.Context(), configID, s.mcpOAuthCallbackURL())
	if err != nil {
		writePluginOAuthError(w, err)
		return
	}
	writeData(w, http.StatusCreated, apitypes.MCPOAuthStart{
		AuthorizationUrl: authorizationURL,
		FlowId:           flowID,
		ExpiresAt:        expiresAt.UTC(),
	})
}

// DisconnectPluginConfigOAuth removes only the credential bundle selected by
// MCP Access for this caller, then returns a safe typed config projection.
func (s *Server) DisconnectPluginConfigOAuth(w http.ResponseWriter, r *http.Request, kind, name, configID string) {
	access, reg, ok := s.pluginOAuthRegistration(w, r, kind, name, configID)
	if !ok {
		return
	}
	updated, err := access.Disconnect(r.Context(), configID, reg.Scope, reg.AgentID)
	if err != nil {
		writePluginOAuthError(w, err)
		return
	}
	view, err := pluginMCPRegistrationView(updated)
	if err != nil {
		writePluginOAuthError(w, err)
		return
	}
	writeData(w, http.StatusOK, view)
}

func pluginMCPRegistrationView(reg mcp.Registration) (apitypes.PluginConfig, error) {
	parsedID, err := uuid.Parse(reg.ID)
	if err != nil {
		return apitypes.PluginConfig{}, err
	}
	var userID *uuid.UUID
	if reg.UserID != "" {
		parsed, err := uuid.Parse(reg.UserID)
		if err != nil {
			return apitypes.PluginConfig{}, err
		}
		userID = &parsed
	}
	var agentID *string
	if reg.AgentID != "" {
		agentID = &reg.AgentID
	}
	enabled := reg.Enabled
	summary := apitypes.PluginMCPBackendSummary{
		Backend:                     apitypes.Mcp,
		Transport:                   apitypes.PluginMCPBackendSummaryTransport(reg.Transport),
		AuthType:                    apitypes.PluginMCPBackendSummaryAuthType(reg.AuthType),
		CredentialMode:              apitypes.PluginMCPBackendSummaryCredentialMode(reg.CredentialMode),
		EndpointConfigured:          reg.URL != "",
		BearerConfigured:            reg.CredentialRef != "",
		OauthClientIdConfigured:     reg.OAuthClientID != "",
		OauthClientSecretConfigured: reg.OAuthClientSecretRef != "",
	}
	var backendSummary apitypes.PluginConfig_BackendSummary
	if err := backendSummary.FromPluginMCPBackendSummary(summary); err != nil {
		return apitypes.PluginConfig{}, err
	}
	createdAt, updatedAt := reg.CreatedAt.UTC(), reg.UpdatedAt.UTC()
	revision := reg.ConfigRevision
	return apitypes.PluginConfig{
		Id: parsedID, PluginId: reg.PluginID, Scope: apitypes.PluginConfigScope(reg.Scope),
		UserId: userID, AgentId: agentID, IsEnabled: &enabled,
		BackendSummary: backendSummary, Revision: &revision,
		CreatedAt: &createdAt, UpdatedAt: &updatedAt,
	}, nil
}
