package mcp

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strings"

	"github.com/CherryHQ/stella/internal/authz"
	"github.com/CherryHQ/stella/pkg/tools"
)

const managementToolSibling = "settings_mcp_server_list"

var managementToolDescriptions = map[string]string{
	"list":   "List up to 50 MCP registrations in one authorized scope. Bearer credentials are never returned.",
	"get":    "Read one authorized MCP registration and its version. Bearer credentials are never returned.",
	"create": "Register a no-auth MCP server in an authorized scope. Credentials must be configured in the Web UI.",
	"update": "Update safe MCP metadata using the version from settings_mcp_server_get. Bearer credentials and their scope cannot change here.",
	"delete": "Delete an MCP registration using the version from settings_mcp_server_get. This refuses a stale version.",
	"probe":  "Probe one authorized MCP registration: connect, list its tools, and persist the result. A failed probe still returns the server with a redacted error status.",
}

// ManagementTool adapts exact generated MCP actions to the authority-bound
// registration service. It never accepts a bearer, credential reference, or a
// caller-supplied user identity.
type ManagementTool struct {
	spec   SettingsMcpActionTool
	access func() *Access
}

func NewManagementTool(spec SettingsMcpActionTool, access func() *Access) *ManagementTool {
	return &ManagementTool{spec: spec, access: access}
}

func (t *ManagementTool) Definition() tools.Definition {
	return t.spec.Definition(managementToolDescriptions[t.spec.Action])
}

func (t *ManagementTool) Execute(ctx context.Context, args map[string]any) (string, error) {
	if t == nil || t.access == nil || t.access() == nil {
		return "", fmt.Errorf("MCP management is unavailable — try again later")
	}
	authority, err := authz.DirectAuthority(ctx, authz.UserIDFromContext(ctx))
	if err != nil {
		return "", authz.MapToolError(t.spec.Name, managementToolSibling, err)
	}
	access, err := t.access().Begin(authority)
	if err != nil {
		return "", authz.MapToolError(t.spec.Name, managementToolSibling, err)
	}
	out, err := SettingsMcpDispatch(ctx, managementHandler{access: access}, t.spec.Action, args)
	if err != nil {
		return "", authz.MapToolError(t.spec.Name, managementToolSibling, err)
	}
	return tools.MarshalResult(out)
}

type managementView struct {
	ID                   string `json:"id"`
	Scope                string `json:"scope"`
	AgentID              string `json:"agent_id,omitempty"`
	Name                 string `json:"name"`
	URL                  string `json:"url"`
	EndpointRedacted     bool   `json:"endpoint_redacted,omitempty"`
	Transport            string `json:"transport"`
	AuthType             string `json:"auth_type"`
	Enabled              bool   `json:"enabled"`
	CredentialConfigured bool   `json:"credential_configured"`
	Status               string `json:"status"`
	StatusError          string `json:"status_error,omitempty"`
	ToolCount            int    `json:"tool_count"`
	Version              string `json:"version"`
}

func managementProjection(r Registration) managementView {
	endpoint, redacted := safeManagementEndpoint(r.URL)
	return managementView{ID: r.ID, Scope: r.Scope, AgentID: r.AgentID, Name: r.Name, URL: endpoint, EndpointRedacted: redacted, Transport: r.Transport, AuthType: r.AuthType, Enabled: r.Enabled, CredentialConfigured: r.CredentialRef != "", Status: r.Status, StatusError: r.StatusError, ToolCount: len(r.Tools), Version: r.Version()}
}

// safeManagementEndpoint fails closed for legacy database rows that predate the
// URL policy. In particular, userinfo and query strings can contain secrets.
func safeManagementEndpoint(raw string) (string, bool) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return "", true
	}
	redacted := u.User != nil || u.Path != "" || u.RawPath != "" || u.RawQuery != "" || u.Fragment != ""
	return u.Scheme + "://" + u.Host, redacted
}

type managementHandler struct{ access *Access }

func (h managementHandler) List(ctx context.Context, in SettingsMcpListInput) (any, error) {
	scope := defaultScope(in.Scope)
	limit := in.Limit
	if limit == 0 {
		limit = 50
	}
	if limit < 1 || limit > 50 {
		return nil, fmt.Errorf("limit must be between 1 and 50")
	}
	rows, err := h.access.List(ctx, scope, in.TargetAgentId)
	if err != nil {
		return nil, err
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })
	truncated := len(rows) > limit
	if truncated {
		rows = rows[:limit]
	}
	out := make([]managementView, 0, len(rows))
	for _, row := range rows {
		out = append(out, managementProjection(row))
	}
	return map[string]any{"servers": out, "truncated": truncated}, nil
}

func (h managementHandler) Get(ctx context.Context, in SettingsMcpGetInput) (any, error) {
	reg, err := h.access.Get(ctx, in.Id, defaultScope(in.Scope), in.TargetAgentId)
	if err != nil {
		return nil, err
	}
	return managementProjection(reg), nil
}

func (h managementHandler) Create(ctx context.Context, in SettingsMcpCreateInput) (any, error) {
	reg, err := h.access.Create(ctx, CreateInput{Scope: defaultScope(in.Scope), AgentID: in.TargetAgentId, Name: in.Name, URL: in.Url, Transport: in.Transport, AuthType: AuthTypeNone})
	if err != nil {
		return nil, err
	}
	return managementProjection(reg), nil
}

func (h managementHandler) Update(ctx context.Context, in SettingsMcpUpdateInput) (any, error) {
	if strings.TrimSpace(in.ExpectedVersion) == "" {
		return nil, ErrVersionConflict
	}
	scope := defaultScope(in.Scope)
	current, err := h.access.Get(ctx, in.Id, scope, in.TargetAgentId)
	if err != nil {
		return nil, err
	}
	if current.AuthType == AuthTypeBearer && in.Url != "" && !sameMCPEndpointOrigin(current.URL, in.Url) {
		return nil, fmt.Errorf("MCP endpoint with bearer credentials must be changed in the Web UI")
	}
	var name, urlValue, transport *string
	if in.Name != "" {
		name = &in.Name
	}
	if in.Url != "" {
		urlValue = &in.Url
	}
	if in.Transport != "" {
		transport = &in.Transport
	}
	reg, err := h.access.UpdateIfVersion(ctx, UpdateInput{ID: in.Id, Scope: scope, AgentID: in.TargetAgentId, Name: name, URL: urlValue, Transport: transport, Enabled: in.Enabled}, in.ExpectedVersion)
	if err != nil {
		return nil, err
	}
	return managementProjection(reg), nil
}

func (h managementHandler) Delete(ctx context.Context, in SettingsMcpDeleteInput) (any, error) {
	if strings.TrimSpace(in.ExpectedVersion) == "" {
		return nil, ErrVersionConflict
	}
	scope := defaultScope(in.Scope)
	if err := h.access.DeleteIfVersion(ctx, in.Id, scope, in.TargetAgentId, in.ExpectedVersion); err != nil {
		return nil, err
	}
	return map[string]string{"id": in.Id, "status": "deleted"}, nil
}

func (h managementHandler) Probe(ctx context.Context, in SettingsMcpProbeInput) (any, error) {
	reg, err := h.access.Probe(ctx, in.Id, defaultScope(in.Scope), in.TargetAgentId)
	if err != nil {
		return nil, err
	}
	return managementProjection(reg), nil
}

func defaultScope(scope string) string {
	if scope == "" {
		return ScopeUser
	}
	return scope
}

func sameMCPEndpointOrigin(left, right string) bool {
	a, err := endpointOrigin(left)
	if err != nil {
		return false
	}
	b, err := endpointOrigin(right)
	return err == nil && a == b
}

func endpointOrigin(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("invalid endpoint")
	}
	return strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Host), nil
}
