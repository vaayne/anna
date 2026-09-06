package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/google/uuid"

	apiserver "github.com/CherryHQ/stella/api/server"
	apitypes "github.com/CherryHQ/stella/api/types"
	"github.com/CherryHQ/stella/internal/authz"
	agentaccess "github.com/CherryHQ/stella/internal/core/access"
	"github.com/CherryHQ/stella/internal/mcp"
	pluginpkg "github.com/CherryHQ/stella/internal/plugin"
)

// Transport responses use generated API types and project backend payloads
// through the typed, discriminator-backed summary constructors below.
var errPluginCapabilityUnavailable = errors.New("plugin backend capability unavailable")

func (s *Server) beginPluginAccess(w http.ResponseWriter, r *http.Request) (*pluginpkg.Access, authz.Authority, bool) {
	info := UserFromContext(r.Context())
	if info == nil {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return nil, authz.Authority{}, false
	}
	authority, err := info.authority()
	if err != nil {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return nil, authz.Authority{}, false
	}
	if s.pluginSvc == nil {
		writeError(w, http.StatusServiceUnavailable, "plugin service unavailable")
		return nil, authority, false
	}
	access, err := s.pluginSvc.Begin(authority)
	if err != nil {
		writePluginError(w, err)
		return nil, authority, false
	}
	return access, authority, true
}

func writePluginError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	message := "internal error"
	switch {
	case errors.Is(err, pluginpkg.ErrConflict), errors.Is(err, mcp.ErrVersionConflict):
		status, message = http.StatusConflict, "resource revision conflict"
	case errors.Is(err, pluginpkg.ErrBuiltinConfig):
		status, message = http.StatusConflict, "builtin system configuration cannot be deleted"
	case errors.Is(err, pluginpkg.ErrUnknownScope), errors.Is(err, pluginpkg.ErrInvalidConfig), errors.Is(err, pluginpkg.ErrInvalidDefinition):
		status, message = http.StatusBadRequest, "invalid plugin request"
	case errors.Is(err, mcp.ErrOAuthClientInitializationRequired):
		status, message = http.StatusConflict, "administrator must initialize this connection before users can authorize their own accounts"
	case errors.Is(err, agentaccess.ErrForbidden):
		status, message = http.StatusForbidden, "forbidden"
	case errors.Is(err, agentaccess.ErrNotFound):
		status, message = http.StatusNotFound, "not found"
	case errors.Is(err, authz.ErrForbidden):
		status, message = http.StatusForbidden, "forbidden"
	case errors.Is(err, authz.ErrNotFound):
		status, message = http.StatusNotFound, "not found"
	case errors.Is(err, errPluginCapabilityUnavailable):
		status, message = http.StatusServiceUnavailable, "plugin backend capability unavailable"
	}
	writeError(w, status, message)
}

func pluginID(kind, name string) string {
	return kind + "/" + name
}

func pluginDefinitionView(def pluginpkg.Definition) (apitypes.PluginDefinition, error) {
	spec, err := safeDefinitionSpec(def)
	if err != nil {
		return apitypes.PluginDefinition{}, err
	}
	isBuiltin, isDefault := def.Source == pluginpkg.SourceBuiltin, def.DefaultEnabled
	revision := def.Revision
	createdAt, updatedAt := def.CreatedAt.UTC(), def.UpdatedAt.UTC()
	return apitypes.PluginDefinition{
		Id: def.ID, Namespace: def.Namespace, DisplayName: def.DisplayName,
		Backend: apitypes.PluginDefinitionBackend(def.Backend), IsBuiltin: &isBuiltin,
		IsDefaultEnabled: &isDefault, Spec: spec, Revision: &revision,
		CreatedAt: &createdAt, UpdatedAt: &updatedAt,
	}, nil
}

func pluginConfigView(definition pluginpkg.Definition, config pluginpkg.Config) (apitypes.PluginConfig, error) {
	backendSummary, err := pluginBackendSummary(definition, config)
	if err != nil {
		return apitypes.PluginConfig{}, err
	}
	parsedID, err := uuid.Parse(config.ID)
	if err != nil {
		return apitypes.PluginConfig{}, err
	}
	var userID *uuid.UUID
	if config.UserID != "" {
		parsedUserID, err := uuid.Parse(config.UserID)
		if err != nil {
			return apitypes.PluginConfig{}, err
		}
		userID = &parsedUserID
	}
	var agentID *string
	if config.AgentID != "" {
		agentID = &config.AgentID
	}
	revision := config.Revision
	createdAt, updatedAt := config.CreatedAt.UTC(), config.UpdatedAt.UTC()
	return apitypes.PluginConfig{
		Id: parsedID, PluginId: config.PluginID, Scope: apitypes.PluginConfigScope(config.Scope),
		UserId: userID, AgentId: agentID, IsEnabled: config.Enabled, BackendSummary: backendSummary,
		Revision: &revision, CreatedAt: &createdAt, UpdatedAt: &updatedAt,
	}, nil
}

func safeDefinitionSpec(def pluginpkg.Definition) (map[string]any, error) {
	var source map[string]json.RawMessage
	if err := json.Unmarshal(def.Spec, &source); err != nil || source == nil {
		return nil, fmt.Errorf("invalid plugin definition spec")
	}
	out := make(map[string]any)
	for _, field := range []string{"description", "category"} {
		var value string
		if json.Unmarshal(source[field], &value) == nil && value != "" {
			out[field] = value
		}
	}
	var capabilities []string
	if json.Unmarshal(source["capabilities"], &capabilities) == nil {
		out["capabilities"] = capabilities
	}
	return out, nil
}

func (s *Server) getPluginDefinition(w http.ResponseWriter, r *http.Request, kind, name string) {
	access, _, ok := s.beginPluginAccess(w, r)
	if !ok {
		return
	}
	definition, err := access.GetDefinition(r.Context(), pluginID(kind, name))
	if err != nil {
		writePluginError(w, err)
		return
	}
	view, err := pluginDefinitionView(definition)
	if err != nil {
		writePluginError(w, err)
		return
	}
	writeData(w, http.StatusOK, view)
}

func (s *Server) deletePluginDefinition(w http.ResponseWriter, r *http.Request, kind, name string, expectedRevision int64) {
	access, _, ok := s.beginPluginAccess(w, r)
	if !ok {
		return
	}
	if expectedRevision < 1 {
		writeError(w, http.StatusBadRequest, "expected_revision must be a positive integer")
		return
	}
	if err := access.DeleteDefinition(r.Context(), pluginID(kind, name), expectedRevision); err != nil {
		writePluginError(w, err)
		return
	}
	writeNoContent(w)
}

func (s *Server) listPluginConfigs(w http.ResponseWriter, r *http.Request, kind, name string, scope *apiserver.ListPluginConfigsParamsScope, agentID *string, pageSize *int, pageToken *string) {
	access, _, ok := s.beginPluginAccess(w, r)
	if !ok {
		return
	}
	limit, offset, err := parsePageParams(pageSize, pageToken)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid pagination parameters")
		return
	}
	scopeValue := string(pluginpkg.ScopeUser)
	if scope != nil && *scope != "" {
		scopeValue = string(*scope)
	}
	requestedAgentID := ""
	if agentID != nil {
		requestedAgentID = *agentID
	}
	configs, err := access.ListConfigs(r.Context(), pluginID(kind, name), pluginpkg.Scope(scopeValue), requestedAgentID)
	if err != nil {
		writePluginError(w, err)
		return
	}
	page, next := nextPageTokenForRows(configs, limit, offset)
	items := make([]apitypes.PluginConfig, 0, len(page))
	for _, config := range page {
		definition, err := access.GetDefinition(r.Context(), config.PluginID)
		if err != nil {
			writePluginError(w, err)
			return
		}
		item, err := pluginConfigView(definition, config)
		if err != nil {
			writePluginError(w, err)
			return
		}
		items = append(items, item)
	}
	writeData(w, http.StatusOK, apitypes.PluginConfigList{Configs: items, NextPageToken: stringPtrOrNil(next)})
}

func (s *Server) getPluginConfig(w http.ResponseWriter, r *http.Request, kind, name, configID string) {
	access, _, ok := s.beginPluginAccess(w, r)
	if !ok {
		return
	}
	config, err := access.GetConfig(r.Context(), pluginID(kind, name), configID)
	if err != nil {
		writePluginError(w, err)
		return
	}
	definition, err := access.GetDefinition(r.Context(), config.PluginID)
	if err != nil {
		writePluginError(w, err)
		return
	}
	view, err := pluginConfigView(definition, config)
	if err != nil {
		writePluginError(w, err)
		return
	}
	writeData(w, http.StatusOK, view)
}

func (s *Server) deletePluginConfig(w http.ResponseWriter, r *http.Request, kind, name, configID string, expectedRevision int64) {
	access, _, ok := s.beginPluginAccess(w, r)
	if !ok {
		return
	}
	if expectedRevision < 1 {
		writeError(w, http.StatusBadRequest, "expected_revision must be a positive integer")
		return
	}
	if err := access.DeleteConfig(r.Context(), pluginID(kind, name), configID, expectedRevision); err != nil {
		writePluginError(w, err)
		return
	}
	writeNoContent(w)
}

func (s *Server) resetPluginConfig(w http.ResponseWriter, r *http.Request, kind, name, configID string, expectedRevision int64) {
	access, _, ok := s.beginPluginAccess(w, r)
	if !ok {
		return
	}
	if expectedRevision < 1 {
		writeError(w, http.StatusBadRequest, "expected_revision must be a positive integer")
		return
	}
	config, err := access.ResetBuiltinConfig(r.Context(), pluginID(kind, name), configID, expectedRevision)
	if err != nil {
		writePluginError(w, err)
		return
	}
	definition, err := access.GetDefinition(r.Context(), config.PluginID)
	if err != nil {
		writePluginError(w, err)
		return
	}
	view, err := pluginConfigView(definition, config)
	if err != nil {
		writePluginError(w, err)
		return
	}
	writeData(w, http.StatusOK, view)
}

func (s *Server) getPluginEffective(w http.ResponseWriter, r *http.Request, kind, name, agentID string) {
	access, authority, ok := s.beginPluginAccess(w, r)
	if !ok {
		return
	}
	if strings.TrimSpace(agentID) == "" {
		writeError(w, http.StatusBadRequest, "agent_id is required")
		return
	}
	definition, err := access.GetDefinition(r.Context(), pluginID(kind, name))
	if err != nil {
		writePluginError(w, err)
		return
	}
	userID := string(authority.UserID())
	var configs []pluginpkg.Config
	for _, scope := range []pluginpkg.Scope{pluginpkg.ScopeSystem, pluginpkg.ScopeSystemAgent, pluginpkg.ScopeUser, pluginpkg.ScopeUserAgent} {
		requestedAgentID := agentID
		if scope == pluginpkg.ScopeSystem || scope == pluginpkg.ScopeUser {
			requestedAgentID = ""
		}
		items, err := access.ListConfigs(r.Context(), definition.ID, scope, requestedAgentID)
		if err != nil {
			if errors.Is(err, authz.ErrForbidden) && !authority.IsAdmin() {
				continue
			}
			writePluginError(w, err)
			return
		}
		configs = append(configs, items...)
	}
	effective, err := pluginpkg.Resolve(definition, configs, userID, agentID)
	if err != nil {
		writePluginError(w, err)
		return
	}
	var configID, sourceScope *string
	if effective.ConfigID != "" {
		configID = &effective.ConfigID
	}
	if effective.SourceScope != "" {
		scope := string(effective.SourceScope)
		sourceScope = &scope
	}
	writeData(w, http.StatusOK, apitypes.PluginEffective{
		PluginId: effective.PluginID, Namespace: effective.Namespace, ConfigId: configID,
		SourceScope: sourceScope, IsEffectivelyEnabled: effective.IsEffectivelyEnabled,
		AvailabilityReason: effective.AvailabilityReason, Readiness: apitypes.PluginEffectiveReadiness("unknown"),
	})
}
