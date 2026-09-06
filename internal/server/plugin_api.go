package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"slices"

	apiserver "github.com/CherryHQ/stella/api/server"
	apitypes "github.com/CherryHQ/stella/api/types"
	pluginpkg "github.com/CherryHQ/stella/internal/plugin"
)

func (s *Server) ListPlugins(w http.ResponseWriter, r *http.Request, params apiserver.ListPluginsParams) {
	access, _, ok := s.beginPluginAccess(w, r)
	if !ok {
		return
	}
	limit, offset, err := parsePageParams(params.PageSize, params.PageToken)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid pagination parameters")
		return
	}
	definitions, err := access.ListDefinitions(r.Context())
	if err != nil {
		writePluginError(w, err)
		return
	}
	page, next := nextPageTokenForRows(definitions, limit, offset)
	items := make([]apitypes.PluginDefinition, 0, len(page))
	for _, definition := range page {
		item, err := pluginDefinitionView(definition)
		if err != nil {
			writePluginError(w, err)
			return
		}
		items = append(items, item)
	}
	writeData(w, http.StatusOK, apitypes.PluginList{Plugins: items, NextPageToken: stringPtrOrNil(next)})
}

func (s *Server) GetPlugin(w http.ResponseWriter, r *http.Request, kind, name string) {
	s.getPluginDefinition(w, r, kind, name)
}

func (s *Server) CreatePlugin(w http.ResponseWriter, r *http.Request) {
	access, authority, ok := s.beginPluginAccess(w, r)
	if !ok {
		return
	}
	data, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	var request apitypes.CreatePluginRequest
	var raw map[string]json.RawMessage
	if json.Unmarshal(data, &request) != nil || json.Unmarshal(data, &raw) != nil || raw == nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if request.DisplayName == "" || request.Namespace == "" || request.Backend == "" || request.InitialConfig.Scope == "" {
		writeError(w, http.StatusBadRequest, "display_name, namespace, backend, and initial_config are required")
		return
	}
	if rawContainsAnyKey(data, "credential_refs") {
		writePluginError(w, pluginpkg.ErrInvalidConfig)
		return
	}
	if request.Backend == apitypes.CreatePluginRequestBackend(pluginpkg.BackendMCP) && (request.InitialConfig.Config != nil || request.InitialConfig.Credentials != nil) {
		s.createMCPPlugin(w, r, authority, request)
		return
	}
	if request.InitialConfig.Credentials != nil || rawContainsAnyKey(data, "credentials", "credential_refs") {
		writePluginError(w, errPluginCapabilityUnavailable)
		return
	}
	definition := pluginpkg.Definition{DisplayName: request.DisplayName, Namespace: request.Namespace, Backend: pluginpkg.Backend(request.Backend), Spec: mustJSON(request.DefinitionSpec)}
	config := pluginpkg.Config{Scope: pluginpkg.Scope(request.InitialConfig.Scope), Enabled: request.InitialConfig.IsEnabled, Payload: mustJSONPtr(request.InitialConfig.Config)}
	if request.InitialConfig.AgentId != nil {
		config.AgentID = *request.InitialConfig.AgentId
	}
	createdDefinition, createdConfig, err := access.CreateCustom(r.Context(), definition, config)
	if err != nil {
		writePluginError(w, err)
		return
	}
	definitionView, err := pluginDefinitionView(createdDefinition)
	if err != nil {
		writePluginError(w, err)
		return
	}
	configView, err := pluginConfigView(createdDefinition, createdConfig)
	if err != nil {
		writePluginError(w, err)
		return
	}
	writeData(w, http.StatusCreated, apitypes.CreatePluginResponse{Plugin: definitionView, Config: configView})
}

func (s *Server) UpdatePlugin(w http.ResponseWriter, r *http.Request, kind, name string) {
	access, _, ok := s.beginPluginAccess(w, r)
	if !ok {
		return
	}
	data, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	var request apitypes.UpdatePluginRequest
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &request); err != nil || json.Unmarshal(data, &raw) != nil || raw == nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if value, exists := raw["display_name"]; exists && bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
		writeError(w, http.StatusBadRequest, "display_name cannot be null")
		return
	}
	if request.ExpectedRevision < 1 {
		writeError(w, http.StatusBadRequest, "expected_revision must be a positive integer")
		return
	}
	description := request.Description
	if value, exists := raw["description"]; exists && bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
		cleared := ""
		description = &cleared
	}
	definition, err := access.UpdateDefinition(r.Context(), pluginID(kind, name), request.ExpectedRevision, pluginpkg.DefinitionPatch{DisplayName: request.DisplayName, Description: description})
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

func (s *Server) DeletePlugin(w http.ResponseWriter, r *http.Request, kind, name string, params apiserver.DeletePluginParams) {
	s.deletePluginDefinition(w, r, kind, name, params.ExpectedRevision)
}

func (s *Server) ListPluginConfigs(w http.ResponseWriter, r *http.Request, kind, name string, params apiserver.ListPluginConfigsParams) {
	s.listPluginConfigs(w, r, kind, name, params.Scope, params.AgentId, params.PageSize, params.PageToken)
}

func (s *Server) CreatePluginConfig(w http.ResponseWriter, r *http.Request, kind, name string) {
	access, authority, ok := s.beginPluginAccess(w, r)
	if !ok {
		return
	}
	data, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	var request apitypes.CreatePluginConfigRequest
	var raw map[string]json.RawMessage
	if json.Unmarshal(data, &request) != nil || json.Unmarshal(data, &raw) != nil || raw == nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if rawContainsAnyKey(data, "credential_refs") {
		writePluginError(w, pluginpkg.ErrInvalidConfig)
		return
	}
	config := pluginpkg.Config{PluginID: pluginID(kind, name), Scope: pluginpkg.Scope(request.Scope), Enabled: request.IsEnabled, Payload: mustJSONPtr(request.Config)}
	if request.AgentId != nil {
		config.AgentID = *request.AgentId
	}
	definition, err := access.GetDefinition(r.Context(), config.PluginID)
	if err != nil {
		writePluginError(w, err)
		return
	}
	if definition.Backend == pluginpkg.BackendMCP && (request.Config != nil || request.Credentials != nil) {
		created, err := s.createMCPPluginConfig(r.Context(), authority, access, definition, request)
		if err != nil {
			writePluginError(w, err)
			return
		}
		view, err := pluginConfigView(definition, created)
		if err != nil {
			writePluginError(w, err)
			return
		}
		writeData(w, http.StatusCreated, view)
		return
	}
	if request.Credentials != nil {
		writePluginError(w, errPluginCapabilityUnavailable)
		return
	}
	created, err := access.CreateConfig(r.Context(), config)
	if err != nil {
		writePluginError(w, err)
		return
	}
	view, err := pluginConfigView(definition, created)
	if err != nil {
		writePluginError(w, err)
		return
	}
	writeData(w, http.StatusCreated, view)
}

func (s *Server) GetPluginConfig(w http.ResponseWriter, r *http.Request, kind, name, configId string) {
	s.getPluginConfig(w, r, kind, name, configId)
}

func (s *Server) UpdatePluginConfig(w http.ResponseWriter, r *http.Request, kind, name, configId string) {
	access, authority, ok := s.beginPluginAccess(w, r)
	if !ok {
		return
	}
	data, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	var request apitypes.UpdatePluginConfigRequest
	var raw map[string]json.RawMessage
	if json.Unmarshal(data, &request) != nil || json.Unmarshal(data, &raw) != nil || raw == nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if rawContainsAnyKey(data, "credential_refs") {
		writePluginError(w, pluginpkg.ErrInvalidConfig)
		return
	}
	if request.ExpectedRevision < 1 {
		writeError(w, http.StatusBadRequest, "expected_revision must be a positive integer")
		return
	}
	current, err := access.GetConfig(r.Context(), pluginID(kind, name), configId)
	if err != nil {
		writePluginError(w, err)
		return
	}
	backendDefinition, err := access.GetDefinition(r.Context(), current.PluginID)
	if err != nil {
		writePluginError(w, err)
		return
	}
	if backendDefinition.Backend == pluginpkg.BackendMCP && (request.Config != nil || request.Credentials != nil) {
		updated, err := s.updateMCPPluginConfig(r.Context(), authority, access, current, request, raw)
		if err != nil {
			writePluginError(w, err)
			return
		}
		view, err := pluginConfigView(backendDefinition, updated)
		if err != nil {
			writePluginError(w, err)
			return
		}
		writeData(w, http.StatusOK, view)
		return
	}
	if request.Credentials != nil {
		writePluginError(w, errPluginCapabilityUnavailable)
		return
	}
	patch := pluginpkg.ConfigPatch{}
	if value, exists := raw["is_enabled"]; exists {
		patch.EnabledSet = true
		if !bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			patch.Enabled = request.IsEnabled
		}
	}
	if value, exists := raw["config"]; exists {
		patch.PayloadSet = true
		if !bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			patch.Payload = value
		}
	}
	if value, exists := raw["binary_versions"]; exists {
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) || request.BinaryVersions == nil {
			writeError(w, http.StatusBadRequest, "binary_versions must be an object")
			return
		}
		patch.BinaryVersionsSet = true
		patch.BinaryVersions = *request.BinaryVersions
	}
	if request.ResetFields != nil {
		patch.ResetFields = *request.ResetFields
	}
	updated, err := access.UpdateConfig(r.Context(), pluginID(kind, name), configId, request.ExpectedRevision, patch)
	if err != nil {
		writePluginError(w, err)
		return
	}
	definition, err := access.GetDefinition(r.Context(), updated.PluginID)
	if err != nil {
		writePluginError(w, err)
		return
	}
	view, err := pluginConfigView(definition, updated)
	if err != nil {
		writePluginError(w, err)
		return
	}
	writeData(w, http.StatusOK, view)
}

func (s *Server) DeletePluginConfig(w http.ResponseWriter, r *http.Request, kind, name, configId string, params apiserver.DeletePluginConfigParams) {
	s.deletePluginConfig(w, r, kind, name, configId, params.ExpectedRevision)
}

func (s *Server) ResetPluginConfig(w http.ResponseWriter, r *http.Request, kind, name, configId string) {
	var request apitypes.RevisionRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	s.resetPluginConfig(w, r, kind, name, configId, request.ExpectedRevision)
}

func (s *Server) ProbePluginConfig(w http.ResponseWriter, r *http.Request, kind, name, configId string) {
	access, authority, ok := s.beginPluginAccess(w, r)
	if !ok {
		return
	}
	config, err := access.GetConfig(r.Context(), pluginID(kind, name), configId)
	if err != nil {
		writePluginError(w, err)
		return
	}
	definition, err := access.GetDefinition(r.Context(), config.PluginID)
	if err != nil {
		writePluginError(w, err)
		return
	}
	if definition.Backend != pluginpkg.BackendMCP {
		writeError(w, http.StatusBadRequest, "plugin backend does not support probing")
		return
	}
	if s.mcpAccess == nil || s.mcpSvc == nil {
		writeCapabilityUnavailable(w, capMCP)
		return
	}
	mcpAccess, err := s.mcpAccess.Begin(authority)
	if err != nil {
		writePluginError(w, err)
		return
	}
	// The common access read above is the parent/config PEP. Reuse the
	// resulting trusted tuple instead of accepting scope or owner input from
	// the route, so MCP cannot probe a different credential owner.
	registration, err := mcpAccess.Probe(r.Context(), config.ID, string(config.Scope), config.AgentID)
	if err != nil {
		writePluginError(w, err)
		return
	}
	view, err := pluginMCPRegistrationView(registration)
	if err != nil {
		writePluginError(w, err)
		return
	}
	writeData(w, http.StatusOK, view)
}

func (s *Server) GetPluginEffective(w http.ResponseWriter, r *http.Request, kind, name string, params apiserver.GetPluginEffectiveParams) {
	s.getPluginEffective(w, r, kind, name, params.AgentId)
}

func stringPtrOrNil(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func mustJSON(value map[string]any) json.RawMessage {
	data, _ := json.Marshal(value)
	return data
}

func mustJSONPtr(value *map[string]any) json.RawMessage {
	if value == nil {
		return nil
	}
	return mustJSON(*value)
}

func rawContainsAnyKey(data []byte, keys ...string) bool {
	var value any
	if json.Unmarshal(data, &value) != nil {
		return false
	}
	return valueContainsAnyKey(value, keys)
}

func valueContainsAnyKey(value any, keys []string) bool {
	switch current := value.(type) {
	case map[string]any:
		for key, child := range current {
			if slices.Contains(keys, key) {
				return true
			}
			if valueContainsAnyKey(child, keys) {
				return true
			}
		}
	case []any:
		for _, child := range current {
			if valueContainsAnyKey(child, keys) {
				return true
			}
		}
	}
	return false
}

var _ apiserver.ServerInterface = (*Server)(nil)
