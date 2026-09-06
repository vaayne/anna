package server

import (
	"context"
	"fmt"
	"net/http"
	"sort"

	apiserver "github.com/CherryHQ/stella/api/server"
	"github.com/CherryHQ/stella/api/types"
	"github.com/CherryHQ/stella/internal/agent"
	coretools "github.com/CherryHQ/stella/internal/agent/sandbox"
	"github.com/CherryHQ/stella/internal/agent/settingspolicy"
	"github.com/CherryHQ/stella/internal/mcp"
	pluginpkg "github.com/CherryHQ/stella/internal/plugin"
	pkgplugins "github.com/CherryHQ/stella/pkg/plugins"
	"github.com/CherryHQ/stella/pkg/toolmeta"
)

const (
	agentToolSourceCore    = "core"
	agentToolSourceBuiltin = "builtin"
	agentToolSourcePlugin  = "plugin"
	agentToolSourceMCP     = "mcp"

	agentToolControlOverride = "override"
	agentToolControlSystem   = "system"

	agentToolReasonCoreSandbox        = "core_sandbox"
	agentToolReasonSettingsPolicy     = "settings_policy"
	agentToolReasonRuntimeUnavailable = "runtime_unavailable"

	agentToolFamilyCore   = "core_tools"
	agentToolFamilyPlugin = "plugin_tools"
	agentToolFamilyOther  = "other_tools"
)

func (s *Server) ListAgentTools(w http.ResponseWriter, r *http.Request, id string) {
	ctx := r.Context()
	agentRow, code, msg := s.requireAgentAccess(ctx, id)
	if code != 0 {
		writeError(w, code, msg)
		return
	}
	// Settings policy metadata is owner-managed configuration. Reuse the Agent
	// PEP instead of deriving ownership from a client-visible creator id.
	canManage := false
	if _, manageCode, manageMsg := s.requireAgentManage(ctx, id); manageCode == 0 {
		canManage = true
	} else if manageCode >= http.StatusInternalServerError {
		writeError(w, manageCode, manageMsg)
		return
	}
	items, err := s.agentTools(ctx, id, canManage && agentRow.SystemSettingsToolsEnabled)
	if err != nil {
		s.writeInternalError(w, err)
		return
	}
	writeData(w, http.StatusOK, types.AgentToolList{Tools: items})
}

func (s *Server) UpdateAgentTool(w http.ResponseWriter, r *http.Request, id string, toolName string) {
	ctx := r.Context()
	info := UserFromContext(ctx)
	if info == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	managedAgent, code, msg := s.requireAgentManage(ctx, id)
	if code != 0 {
		writeError(w, code, msg)
		return
	}
	if agent.IsCoreToolName(toolName) {
		writeError(w, http.StatusBadRequest, "core sandbox tools are system-managed")
		return
	}
	if _, isSettingsAction := settingspolicy.Lookup(toolName); isSettingsAction {
		// A profile request is not a trusted foreground session. Settings actions
		// are catalogued separately and runner availability always wins, so this
		// endpoint must not persist an override that can never register the tool.
		writeError(w, http.StatusBadRequest, "system settings are policy-managed")
		return
	}
	identity, overridable, err := s.agentToolOverrideAllowed(ctx, info.UserID, id, toolName)
	if err != nil {
		s.writeInternalError(w, err)
		return
	}
	if !overridable {
		writeError(w, http.StatusBadRequest, "tool is not currently registered for this agent")
		return
	}

	var req apiserver.UpdateAgentToolJSONRequestBody
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	scope := agent.ToolOverrideScopeUserAgent
	if req.Scope != nil && *req.Scope != "" {
		scope = string(*req.Scope)
	}
	agentCtxID := ""
	if isAgentToolOverrideScope(scope) {
		agentCtxID = id
	}
	userID, agentID, ok := s.resolveScope(w, r, info, scope, agentCtxID)
	if !ok {
		return
	}

	if req.Enabled == nil {
		if err := s.toolOverrides.Clear(ctx, agent.ToolOverrideKey{
			Identity: identity,
			Scope:    scope,
			UserID:   userID,
			AgentID:  agentID,
		}); err != nil {
			s.writeInternalError(w, err)
			return
		}
	} else {
		if err := s.toolOverrides.Set(ctx, agent.ToolOverrideWrite{
			Identity: identity,
			Scope:    scope,
			UserID:   userID,
			AgentID:  agentID,
			Enabled:  *req.Enabled,
		}); err != nil {
			s.writeInternalError(w, err)
			return
		}
	}

	items, err := s.agentTools(ctx, id, managedAgent.SystemSettingsToolsEnabled)
	if err != nil {
		s.writeInternalError(w, err)
		return
	}
	for _, item := range items {
		if item.Name == toolName {
			writeData(w, http.StatusOK, item)
			return
		}
	}
	writeError(w, http.StatusBadRequest, "tool is not managed here")
}

func (s *Server) agentTools(ctx context.Context, agentID string, includeSettings bool) ([]types.AgentTool, error) {
	info := UserFromContext(ctx)
	if info == nil {
		return nil, nil
	}
	overrides, err := s.toolOverrides.Fetch(ctx, info.UserID, agentID)
	if err != nil {
		return nil, err
	}
	pluginSnapshot, err := s.toolSnapshot(ctx, info, agentID)
	if err != nil {
		return nil, err
	}

	items := make([]types.AgentTool, 0)
	for _, core := range coretools.ToolDefinitionsWithAvailability() {
		def := core.Definition
		items = append(items, systemAgentTool(def.Name, def.Description, agentToolSourceCore, agentToolReasonCoreSandbox, s.toolFamily(def.Name, agentToolSourceCore), false, toolInputSchema(def.InputSchema)))
	}

	// RunnerParams intentionally has no ForegroundHuman flag here. A profile
	// request has no trusted session context, so Settings actions are rendered as
	// policy metadata below instead of invoking settingspolicy.Available.
	params := agent.RunnerParams{UserID: info.UserID, AgentID: agentID}
	for _, entry := range s.builtinTools {
		def, ok := entry.Definition()
		if !ok || agent.IsCoreToolName(def.Name) {
			continue
		}
		if policy, isSettingsAction := settingspolicy.Lookup(def.Name); isSettingsAction {
			// A manager sees the enabled policy catalog. Its absence is the Profile's
			// authoritative disabled state; viewers receive neither signal.
			if includeSettings {
				items = append(items, systemAgentTool(def.Name, def.Description, agentToolSourceBuiltin, agentToolReasonSettingsPolicy, policy.Family, policy.AdminRequired, toolInputSchema(def.InputSchema)))
			}
			continue
		}

		available, err := builtinAvailable(ctx, entry, params)
		if err != nil {
			return nil, fmt.Errorf("resolve availability for tool %q: %w", def.Name, err)
		}
		if !available {
			items = append(items, runtimeUnavailableAgentTool(
				def.Name,
				def.Description,
				s.toolFamily(def.Name, agentToolSourceBuiltin),
				entry.UnavailableReason,
				toolInputSchema(def.InputSchema),
			))
			continue
		}
		identity, err := s.toolIdentity(def.Name)
		if err != nil {
			return nil, err
		}
		decision := agent.ResolveToolOverride(true, identity, overrides)
		items = append(items, overrideAgentTool(def.Name, def.Description, agentToolSourceBuiltin, s.toolFamily(def.Name, agentToolSourceBuiltin), decision, toolInputSchema(def.InputSchema)))
	}

	if s.pluginHost != nil && s.pluginSvc != nil {
		specs, err := s.pluginHost.EnabledToolSpecs(ctx, pluginSnapshot)
		if err != nil {
			return nil, err
		}
		for _, spec := range specs {
			if agent.IsCoreToolName(spec.Name) {
				continue
			}
			identity, name, ok := pluginToolProjection(s.toolMeta, spec, pluginSnapshot)
			if !ok {
				continue
			}
			decision := agent.ResolveToolOverride(true, identity, overrides)
			items = append(items, overrideAgentTool(name, spec.Description, agentToolSourcePlugin, s.toolFamily(name, agentToolSourcePlugin), decision, nil))
		}
	}

	// MCP tools come from the resolved registrations' persisted catalogs, one
	// row per remote tool, override-controlled like builtins. The server-level
	// lifecycle stays on the MCP registration; an unhealthy server still lists
	// its tools with an availability_reason because the override is editable —
	// it just has no effect until the server is healthy again.
	if s.mcpSvc != nil {
		regs, err := s.mcpSvc.RegistrationsForSnapshot(ctx, pluginSnapshot)
		if err != nil {
			return nil, err
		}
		for _, reg := range regs {
			reason := mcpAvailabilityReason(reg, s.mcpSvc.HasUserCredential(ctx, reg, info.UserID))
			for _, tool := range reg.Tools {
				name, ok := mcpToolName(reg, tool)
				if !ok {
					// Legacy registrations have no trusted plugin namespace. They
					// cannot be published under a guessed model-facing name.
					continue
				}
				identity, ok := mcpToolIdentity(reg, tool)
				if !ok {
					// A malformed registration may still carry a display namespace,
					// but without a plugin/local identity it cannot be controlled by
					// a name-only override.
					identity = agent.ToolIdentity{}
				}
				decision := agent.ResolveToolOverride(true, identity, overrides)
				item := overrideAgentTool(name, tool.Description, agentToolSourceMCP, "mcp:"+reg.Name, decision, toolInputSchema(tool.InputSchema))
				if reason != "" {
					availability := types.AgentToolAvailabilityReason(reason)
					item.AvailabilityReason = &availability
				}
				items = append(items, item)
			}
		}
	}

	sort.SliceStable(items, func(i, j int) bool {
		left, right := agentToolSortFamily(items[i]), agentToolSortFamily(items[j])
		if left != right {
			return left < right
		}
		return items[i].Name < items[j].Name
	})
	return items, nil
}

func builtinAvailable(ctx context.Context, entry agent.BuiltinTool, params agent.RunnerParams) (bool, error) {
	if entry.Available == nil {
		return true, nil
	}
	return entry.Available(ctx, params)
}

func overrideAgentTool(name, description, source, family string, decision agent.ToolOverrideDecision, inputSchema *map[string]any) types.AgentTool {
	control := types.AgentToolControl(agentToolControlOverride)
	enabled := decision.Enabled
	origin := decision.Origin
	item := types.AgentTool{Name: name, Description: description, Source: source, Control: control, Enabled: &enabled, Origin: &origin, InputSchema: inputSchema}
	if family != "" {
		item.Family = &family
	}
	return item
}

func systemAgentTool(name, description, source, reason, family string, adminRequired bool, inputSchema *map[string]any) types.AgentTool {
	control := types.AgentToolControl(agentToolControlSystem)
	policyReason := types.AgentToolPolicyReason(reason)
	item := types.AgentTool{Name: name, Description: description, Source: source, Control: control, PolicyReason: &policyReason, InputSchema: inputSchema}
	if family != "" {
		item.Family = &family
	}
	if adminRequired {
		item.AdminRequired = &adminRequired
	}
	return item
}

// runtimeUnavailableAgentTool publishes a concrete prerequisite only after the
// runner's availability predicate has established that the builtin is absent.
// This keeps Profile setup CTAs tied to server-owned configuration state rather
// than a client-side name convention.
func runtimeUnavailableAgentTool(name, description, family string, availabilityReason agent.ToolUnavailableReason, inputSchema *map[string]any) types.AgentTool {
	item := systemAgentTool(name, description, agentToolSourceBuiltin, agentToolReasonRuntimeUnavailable, family, false, inputSchema)
	if availabilityReason == agent.ToolUnavailableReasonEmailConfigRequired {
		reason := types.EmailConfigRequired
		item.AvailabilityReason = &reason
	}
	return item
}

// mcpAvailabilityReason maps a registration's server-level state to the
// profile's availability enum. "unknown" (never probed) is reported as an
// error: the runner has no catalog to serve tools from until a probe runs.
// hasUserCredential is the per-user view: a per_user registration whose
// calling user has no bundle is needs_auth for exactly that user, even though
// the row's status still reflects the shared/owner state.
func mcpAvailabilityReason(reg mcp.Registration, hasUserCredential bool) string {
	switch {
	case !reg.Enabled:
		return "mcp_server_disabled"
	case !hasUserCredential:
		return "mcp_needs_auth"
	case reg.Status == mcp.StatusNeedsAuth:
		return "mcp_needs_auth"
	case reg.Status != mcp.StatusOK:
		return "mcp_server_error"
	default:
		return ""
	}
}

// agentToolOverrideAllowed is the mutation-side counterpart of agentTools. It
// makes the API reject an override when the runner's own availability gate would
// ignore it, rather than returning a successful but ineffective mutation.
func (s *Server) agentToolOverrideAllowed(ctx context.Context, userID, agentID, name string) (agent.ToolIdentity, bool, error) {
	if s.mcpSvc != nil {
		if info := UserFromContext(ctx); info != nil {
			snapshot, err := s.toolSnapshot(ctx, info, agentID)
			if err != nil {
				return agent.ToolIdentity{}, false, err
			}
			regs, err := s.mcpSvc.RegistrationsForSnapshot(ctx, snapshot)
			if err != nil {
				return agent.ToolIdentity{}, false, err
			}
			for _, registration := range regs {
				for _, tool := range registration.Tools {
					mcpName, ok := mcpToolName(registration, tool)
					if ok && mcpName == name {
						identity, ok := mcpToolIdentity(registration, tool)
						return identity, ok, nil
					}
				}
			}
		}
	}
	params := agent.RunnerParams{UserID: userID, AgentID: agentID}
	for _, entry := range s.builtinTools {
		definition, ok := entry.Definition()
		if !ok || definition.Name != name {
			continue
		}
		available, err := builtinAvailable(ctx, entry, params)
		if err != nil || !available {
			return agent.ToolIdentity{}, false, err
		}
		identity, err := s.toolIdentity(name)
		return identity, err == nil, err
	}
	if s.pluginHost != nil && s.pluginSvc != nil {
		info := UserFromContext(ctx)
		if info == nil {
			return agent.ToolIdentity{}, false, nil
		}
		snapshot, err := s.toolSnapshot(ctx, info, agentID)
		if err != nil {
			return agent.ToolIdentity{}, false, err
		}
		specs, err := s.pluginHost.EnabledToolSpecs(ctx, snapshot)
		if err != nil {
			return agent.ToolIdentity{}, false, err
		}
		for _, spec := range specs {
			identity, projectedName, ok := pluginToolProjection(s.toolMeta, spec, snapshot)
			if ok && projectedName == name {
				return identity, ok, nil
			}
		}
	}
	return agent.ToolIdentity{}, false, nil
}

func (s *Server) toolSnapshot(ctx context.Context, info *AuthInfo, agentID string) (pluginpkg.Snapshot, error) {
	if s.pluginSvc == nil {
		return pluginpkg.Snapshot{}, nil
	}
	authority, err := info.authority()
	if err != nil {
		return pluginpkg.Snapshot{}, err
	}
	return s.pluginSvc.ResolveSnapshot(ctx, authority, agentID)
}

func (s *Server) toolIdentity(name string) (agent.ToolIdentity, error) {
	return trustedToolIdentity(s.toolMeta, name)
}

func trustedToolIdentity(meta *toolmeta.Registry, name string) (agent.ToolIdentity, error) {
	if spec, ok := meta.Lookup(name); ok {
		if spec.PluginID == "" {
			if spec.Namespace != "" || spec.LocalName != "" {
				return agent.ToolIdentity{}, fmt.Errorf("tool %q has core/plugin metadata mismatch", name)
			}
			return agent.ToolIdentity{CoreToolName: name}, nil
		}
		identity := agent.ToolIdentity{PluginID: spec.PluginID, LocalToolName: spec.LocalName}
		if err := identity.Validate(); err != nil {
			return agent.ToolIdentity{}, err
		}
		exported, err := pluginpkg.ExportedToolName(spec.Namespace, spec.LocalName)
		if err != nil {
			return agent.ToolIdentity{}, err
		}
		if exported != name {
			return agent.ToolIdentity{}, fmt.Errorf("tool %q metadata exports %q", name, exported)
		}
		return identity, nil
	}
	return agent.ToolIdentity{CoreToolName: name}, nil
}

func pluginToolProjection(meta *toolmeta.Registry, spec pkgplugins.ToolSpec, snapshot pluginpkg.Snapshot) (agent.ToolIdentity, string, bool) {
	if meta != nil {
		if identity, err := trustedToolIdentity(meta, spec.Name); err == nil && identity.PluginID != "" {
			return identity, spec.Name, true
		}
	}
	identity := agent.ToolIdentity{PluginID: spec.PluginID, LocalToolName: spec.Name}
	if err := identity.Validate(); err != nil {
		return agent.ToolIdentity{}, "", false
	}
	if resolved, ok := snapshot.Get(spec.PluginID); ok {
		name, err := pluginpkg.ExportedToolName(resolved.Definition.Namespace, spec.Name)
		if err != nil {
			return agent.ToolIdentity{}, "", false
		}
		return identity, name, true
	}
	return identity, spec.Name, true
}

func mcpToolName(reg mcp.Registration, tool mcp.CatalogTool) (string, bool) {
	name, err := pluginpkg.ExportedToolName(reg.Namespace, mcp.SanitizeIdent(tool.Name, "tool"))
	return name, err == nil
}

func mcpToolIdentity(reg mcp.Registration, tool mcp.CatalogTool) (agent.ToolIdentity, bool) {
	if reg.PluginID == "" || reg.Namespace == "" {
		return agent.ToolIdentity{}, false
	}
	identity := agent.ToolIdentity{PluginID: reg.PluginID, LocalToolName: mcp.SanitizeIdent(tool.Name, "tool")}
	return identity, identity.Validate() == nil
}

// toolInputSchema adapts a tool definition's JSON input schema to the pointer
// shape the API type uses, returning nil for an empty schema so the field is
// omitted rather than serialized as an empty object.
func toolInputSchema(schema map[string]any) *map[string]any {
	if len(schema) == 0 {
		return nil
	}
	return &schema
}

// toolFamily is deliberately metadata-first for builtins: toolmeta declares
// generated builtin families, while hand-written or unknown surfaces fall back
// to a stable generic family. Never derive a family by splitting the tool name,
// because a plugin is free to use a generated-looking name.
func (s *Server) toolFamily(name, source string) string {
	if source == agentToolSourceBuiltin && s.toolMeta != nil {
		if family := s.toolMeta.Family(name); family != "" {
			return family
		}
	}
	switch source {
	case agentToolSourceCore:
		return agentToolFamilyCore
	case agentToolSourcePlugin:
		return agentToolFamilyPlugin
	default:
		return agentToolFamilyOther
	}
}

func agentToolSortFamily(item types.AgentTool) string {
	if item.Family != nil {
		return *item.Family
	}
	// MCP registrations intentionally live in their own top-level section.
	return "~mcp"
}

func isAgentToolOverrideScope(scope string) bool {
	return scope == agent.ToolOverrideScopeUserAgent || scope == agent.ToolOverrideScopeSystemAgent
}
