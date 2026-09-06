package server

import (
	"errors"
	"net/http"
	"net/url"
	"strings"

	apiserver "github.com/CherryHQ/stella/api/server"
	apitypes "github.com/CherryHQ/stella/api/types"
	"github.com/CherryHQ/stella/internal/authz"
	"github.com/CherryHQ/stella/internal/mcp"
)

// beginMCPAccess authenticates the request and starts the common MCP access
// session used by agent views and plugin-scoped OAuth actions.
func (s *Server) beginMCPAccess(w http.ResponseWriter, r *http.Request) (*mcp.Access, *AuthInfo, bool) {
	if s.mcpSvc == nil || s.mcpAccess == nil {
		writeCapabilityUnavailable(w, capMCP)
		return nil, nil, false
	}
	info := UserFromContext(r.Context())
	if info == nil {
		writeError(w, http.StatusUnauthorized, "not authenticated")
		return nil, nil, false
	}
	authority, err := info.authority()
	if err != nil {
		writeError(w, http.StatusUnauthorized, "not authenticated")
		return nil, nil, false
	}
	access, err := s.mcpAccess.Begin(authority)
	if err != nil {
		writeError(w, http.StatusForbidden, "forbidden")
		return nil, nil, false
	}
	return access, info, true
}

func mcpAgentID(id *string) string {
	if id == nil {
		return ""
	}
	return *id
}

func writeMCPError(w http.ResponseWriter, err error) {
	if errors.Is(err, authz.ErrForbidden) {
		writeError(w, http.StatusForbidden, "forbidden")
		return
	}
	if errors.Is(err, mcp.ErrVersionConflict) {
		writeError(w, http.StatusConflict, "registration changed; re-read it and retry")
		return
	}
	var duplicate *mcp.DuplicateServerError
	if errors.As(err, &duplicate) {
		writeError(w, http.StatusConflict, duplicate.Error())
		return
	}
	writeError(w, http.StatusBadRequest, err.Error())
}

// ListAgentMcpServers returns the MCP registrations effective for one agent
// after name-precedence dedup, with provenance for the UI: which scopes lost
// to each winner, and whether the caller can manage the row at all.
func (s *Server) ListAgentMcpServers(w http.ResponseWriter, r *http.Request, id string) {
	ctx := r.Context()
	if s.mcpSvc == nil || s.mcpAccess == nil {
		writeCapabilityUnavailable(w, capMCP)
		return
	}
	if _, code, msg := s.requireAgentAccess(ctx, id); code != 0 {
		writeError(w, code, msg)
		return
	}
	info := UserFromContext(ctx)
	if info == nil {
		writeError(w, http.StatusUnauthorized, "not authenticated")
		return
	}
	authority, err := info.authority()
	if err != nil {
		writeError(w, http.StatusUnauthorized, "not authenticated")
		return
	}
	access, err := s.mcpAccess.Begin(authority)
	if err != nil {
		writeError(w, http.StatusForbidden, "forbidden")
		return
	}
	snapshot, err := s.toolSnapshot(ctx, info, id)
	if err != nil {
		writeMCPError(w, err)
		return
	}
	registrations, err := s.mcpSvc.RegistrationsForSnapshot(ctx, snapshot)
	if err != nil {
		writeMCPError(w, err)
		return
	}
	out := make([]apitypes.AgentMCPServer, len(registrations))
	for i, registration := range registrations {
		out[i] = agentMCPServerResponse(registration, access.CanRead(ctx, registration))
	}
	writeData(w, http.StatusOK, apitypes.AgentMCPServerList{Servers: out})
}

// agentMCPServerResponse is the only effective-agent projection boundary. It
// intentionally does not expose endpoint, credential locators, or OAuth state
// from the owner-scoped registration.
func agentMCPServerResponse(registration mcp.Registration, readable bool) apitypes.AgentMCPServer {
	tools := make([]apitypes.MCPTool, len(registration.Tools))
	for i, tool := range registration.Tools {
		tools[i] = apitypes.MCPTool{Name: tool.Name}
		if tool.Description != "" {
			description := tool.Description
			tools[i].Description = &description
		}
		if tool.InputSchema != nil {
			inputSchema := tool.InputSchema
			tools[i].InputSchema = &inputSchema
		}
		if tool.Annotations != nil {
			annotations := tool.Annotations
			tools[i].Annotations = &annotations
		}
	}
	return apitypes.AgentMCPServer{
		PluginId:       registration.PluginID,
		ConfigId:       registration.ID,
		Namespace:      registration.Namespace,
		Scope:          apitypes.AgentMCPServerScope(registration.Scope),
		Enabled:        registration.Enabled,
		CredentialMode: apitypes.AgentMCPServerCredentialMode(registration.CredentialMode),
		NeedsAuth:      registration.Status == mcp.StatusNeedsAuth,
		Status:         apitypes.AgentMCPServerStatus(registration.Status),
		Tools:          tools,
		Readable:       readable,
	}
}

// mcpOAuthCallbackPath is the redirect URI path registered with authorization
// servers. It hangs off the configured base URL rather than the request origin
// because dynamic client registration persists the redirect URI once per
// registration; a per-request origin would break the next user's flow.
const mcpOAuthCallbackPath = "/api/mcp/oauth/callback"

func (s *Server) mcpOAuthCallbackURL() string {
	return strings.TrimRight(s.baseURL, "/") + mcpOAuthCallbackPath
}

// McpOAuthCallback handles GET /api/mcp/oauth/callback. It is unauthenticated:
// consuming the flow row via state re-identifies the initiating user.
func (s *Server) McpOAuthCallback(w http.ResponseWriter, r *http.Request, params apiserver.McpOAuthCallbackParams) {
	if s.mcpSvc == nil {
		writeCapabilityUnavailable(w, capMCP)
		return
	}
	if params.Code == "" || params.State == "" {
		http.Redirect(w, r, "/settings/mcp?oauth_error=invalid_request", http.StatusFound)
		return
	}
	reg, err := s.mcpSvc.CompleteOAuth(r.Context(), params.State, params.Code)
	if err != nil {
		s.log.Warn("mcp oauth callback failed", "error", err)
		http.Redirect(w, r, "/settings/mcp?oauth_error="+mcpOAuthErrorSlug(err), http.StatusFound)
		return
	}
	http.Redirect(w, r, "/settings/mcp?connected="+url.QueryEscape(reg.ID), http.StatusFound)
}

// mcpOAuthErrorSlug maps a callback failure to a fixed enum for the redirect
// URL. Provider error text is never echoed into the URL.
func mcpOAuthErrorSlug(err error) string {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "unknown, expired, or already used"):
		return "expired"
	case strings.Contains(msg, "exchange authorization code"):
		return "exchange_failed"
	default:
		return "internal"
	}
}
