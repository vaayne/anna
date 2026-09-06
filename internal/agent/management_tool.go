package agent

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode"

	"github.com/CherryHQ/stella/internal/agent/settingspolicy"
	"github.com/CherryHQ/stella/internal/authz"
	agentaccess "github.com/CherryHQ/stella/internal/core/access"
	"github.com/CherryHQ/stella/internal/platform/config"
	"github.com/CherryHQ/stella/pkg/toolmeta"
	"github.com/CherryHQ/stella/pkg/tools"
)

const agentToolListSibling = "settings_agent_list"

var agentManagementDescriptions = map[string]string{
	"list":   "List up to the requested number of agents you can use or manage. Results say when more agents exist.",
	"get":    "Read one agent's safe configuration and version. Use its version for settings_agent_update or settings_agent_delete.",
	"create": "Create an agent without provider credentials. The result includes the server-selected ID; add credentials only in the Web UI.",
	"update": "Update an agent using the version from settings_agent_get. Re-read the agent if the version has changed.",
	"delete": "Delete an agent using the version from settings_agent_get. This is irreversible and refuses a stale version.",
}

var agentOverrideDescriptions = map[string]string{
	"list":   "List generated tools and their exact user-agent override versions for one manageable agent.",
	"update": "Set one tool override using the version from settings_agent_tool_list. A first override requires the absent version.",
	"delete": "Clear one tool override using the version from settings_agent_tool_list, restoring the default visibility decision.",
}

// ManagementTool is one exact Agent or Agent-tool-override action. It is kept
// separate from the runtime's ordinary AgentActor tools: Settings require the
// fresh direct-human Authority installed only for an admitted Stella turn.
type ManagementTool struct {
	agentSpec    *SettingsAgentActionTool
	overrideSpec *SettingsAgentToolActionTool
	management   func() *agentaccess.Management
	overrides    *ToolOverrideStore
	registry     func() *toolmeta.Registry
	mcpCatalog   MCPCatalogFunc
}

// MCPCatalogEntry is the management projection of one trusted MCP tool. Name
// is a display projection; Identity is the only policy key.
type MCPCatalogEntry struct {
	Name     string
	Identity ToolIdentity
	Family   string
}

// MCPCatalogFunc resolves the authority-bound common snapshot for one agent.
// It returns an error instead of silently producing an empty catalog, because
// an unavailable catalog must not turn Update/Delete into a successful no-op.
type MCPCatalogFunc func(ctx context.Context, authority authz.Authority, agentID string) ([]MCPCatalogEntry, error)

func NewManagementTool(spec SettingsAgentActionTool, management func() *agentaccess.Management) *ManagementTool {
	return &ManagementTool{agentSpec: &spec, management: management}
}

func NewToolOverrideManagementTool(spec SettingsAgentToolActionTool, management func() *agentaccess.Management, overrides *ToolOverrideStore, registry func() *toolmeta.Registry, mcpCatalog MCPCatalogFunc) *ManagementTool {
	return &ManagementTool{overrideSpec: &spec, management: management, overrides: overrides, registry: registry, mcpCatalog: mcpCatalog}
}

func (t *ManagementTool) Definition() tools.Definition {
	if t.agentSpec != nil {
		return t.agentSpec.Definition(agentManagementDescriptions[t.agentSpec.Action])
	}
	return t.overrideSpec.Definition(agentOverrideDescriptions[t.overrideSpec.Action])
}

func (t *ManagementTool) Execute(ctx context.Context, args map[string]any) (string, error) {
	if t == nil || t.management == nil {
		return "", fmt.Errorf("agent management is unavailable — try again later")
	}
	management := t.management()
	if management == nil {
		return "", fmt.Errorf("agent management is unavailable — try again later")
	}
	userID := authz.UserIDFromContext(ctx)
	authority, err := authz.DirectAuthority(ctx, userID)
	if err != nil {
		return "", authz.MapToolError(t.toolName(), agentToolListSibling, err)
	}
	var out any
	if t.agentSpec != nil {
		out, err = SettingsAgentDispatch(ctx, agentManagementHandler{management: management, authority: authority}, t.agentSpec.Action, args)
	} else {
		if t.overrides == nil || t.registry == nil || t.registry() == nil {
			return "", fmt.Errorf("agent tool override management is unavailable — try again later")
		}
		out, err = SettingsAgentToolDispatch(ctx, agentOverrideHandler{management: management, authority: authority, overrides: t.overrides, registry: t.registry(), mcpCatalog: t.mcpCatalog}, t.overrideSpec.Action, args)
	}
	if err != nil {
		return "", authz.MapToolError(t.toolName(), agentToolListSibling, err)
	}
	return tools.MarshalResult(out)
}

func (t *ManagementTool) toolName() string {
	if t.agentSpec != nil {
		return t.agentSpec.Name
	}
	return t.overrideSpec.Name
}

type agentManagementHandler struct {
	management *agentaccess.Management
	authority  authz.Authority
}

func (h agentManagementHandler) List(ctx context.Context, in SettingsAgentListInput) (any, error) {
	limit := 50
	if in.Limit != 0 {
		limit = in.Limit
	}
	agents, truncated, err := h.management.ListForTool(ctx, h.authority, limit)
	if err != nil {
		return nil, err
	}
	out := make([]agentToolView, 0, len(agents))
	for _, agent := range agents {
		out = append(out, projectAgent(agent))
	}
	return map[string]any{"agents": out, "truncated": truncated}, nil
}

func (h agentManagementHandler) Get(ctx context.Context, in SettingsAgentGetInput) (any, error) {
	agent, err := h.management.GetForTool(ctx, h.authority, in.Id)
	if err != nil {
		return nil, err
	}
	return projectAgent(agent), nil
}

func (h agentManagementHandler) Create(ctx context.Context, in SettingsAgentCreateInput) (any, error) {
	candidate := config.Agent{
		ID: slugAgentID(in.Id, in.Name), Name: in.Name, Model: in.Model,
		ModelThinking: in.ModelThinking, ModelStrong: in.ModelStrong,
		ModelStrongThinking: in.ModelStrongThinking, ModelFast: in.ModelFast,
		ModelFastThinking: in.ModelFastThinking, SystemPrompt: in.SystemPrompt,
		Soul: in.Soul, Scope: in.Scope, Enabled: true,
	}
	if in.Enabled != nil {
		candidate.Enabled = *in.Enabled
	}
	if err := validateToolAgent(candidate); err != nil {
		return nil, err
	}
	created, err := h.management.CreateForTool(ctx, h.authority, candidate)
	if err != nil {
		return nil, err
	}
	return projectAgent(created), nil
}

func (h agentManagementHandler) Update(ctx context.Context, in SettingsAgentUpdateInput) (any, error) {
	current, err := h.management.GetForTool(ctx, h.authority, in.Id)
	if err != nil {
		return nil, err
	}
	candidate := current.Agent
	if in.Name != "" {
		candidate.Name = in.Name
	}
	if in.Model != nil {
		candidate.Model = *in.Model
	}
	if in.ModelThinking != "" {
		candidate.ModelThinking = in.ModelThinking
	}
	if in.ModelStrong != "" {
		candidate.ModelStrong = in.ModelStrong
	}
	if in.ModelStrongThinking != "" {
		candidate.ModelStrongThinking = in.ModelStrongThinking
	}
	if in.ModelFast != "" {
		candidate.ModelFast = in.ModelFast
	}
	if in.ModelFastThinking != "" {
		candidate.ModelFastThinking = in.ModelFastThinking
	}
	if in.SystemPrompt != nil {
		candidate.SystemPrompt = *in.SystemPrompt
	}
	if in.Soul != nil {
		candidate.Soul = *in.Soul
	}
	if in.Scope != "" {
		candidate.Scope = in.Scope
	}
	if in.Enabled != nil {
		candidate.Enabled = *in.Enabled
	}
	if err := validateToolAgent(candidate); err != nil {
		return nil, err
	}
	if _, _, err := h.management.UpdateIfVersion(ctx, h.authority, candidate, in.ExpectedVersion); err != nil {
		return nil, err
	}
	// Re-read through the PEP snapshot boundary so a successful response never
	// combines this update's fields with a later version (or vice versa).
	updated, err := h.management.GetForTool(ctx, h.authority, candidate.ID)
	if err != nil {
		return nil, err
	}
	return projectAgent(updated), nil
}

func (h agentManagementHandler) Delete(ctx context.Context, in SettingsAgentDeleteInput) (any, error) {
	if err := h.management.DeleteIfVersion(ctx, h.authority, in.Id, in.ExpectedVersion); err != nil {
		return nil, err
	}
	return map[string]string{"id": in.Id, "status": "deleted"}, nil
}

type agentOverrideHandler struct {
	management *agentaccess.Management
	authority  authz.Authority
	overrides  *ToolOverrideStore
	registry   *toolmeta.Registry
	mcpCatalog MCPCatalogFunc
}

func (h agentOverrideHandler) List(ctx context.Context, in SettingsAgentToolListInput) (any, error) {
	if err := h.management.ManageForTool(ctx, h.authority, in.TargetAgentId); err != nil {
		return nil, err
	}
	versions, err := h.overrides.ListVersions(ctx, string(h.authority.UserID()), in.TargetAgentId)
	if err != nil {
		return nil, err
	}
	items := make([]ToolOverrideVersion, 0, len(h.registry.Names()))
	for _, name := range h.registry.Names() {
		identity, ok := h.registryToolIdentity(name)
		if !ok {
			continue
		}
		if _, managed, err := h.managedTool(ctx, in.TargetAgentId, name); err != nil {
			return nil, err
		} else if !managed {
			continue
		}
		item, ok := versions[toolOverrideVersionKey(identity)]
		if !ok {
			item = absentOverrideVersion(identity, name)
		} else {
			// Keep the exported name as the API display value. The persisted
			// identity remains the only policy key, especially for plugin tools.
			item.ToolName = name
		}
		items = append(items, item)
	}
	// MCP tools are not in the toolmeta registry. Their exported names are
	// display values; versions are keyed by the durable plugin/local identity.
	catalog, err := h.catalog(ctx, in.TargetAgentId)
	if err != nil {
		return nil, err
	}
	for _, entry := range catalog {
		if entry.Identity.Validate() != nil || !entry.Identity.isPlugin() {
			continue
		}
		key := toolOverrideVersionKey(entry.Identity)
		item, ok := versions[key]
		if !ok {
			item = absentOverrideVersion(entry.Identity, entry.Name)
		} else {
			item.ToolName = entry.Name
		}
		item.Family = entry.Family
		items = append(items, item)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ToolName < items[j].ToolName })
	return map[string]any{"tools": items}, nil
}

func (h agentOverrideHandler) Update(ctx context.Context, in SettingsAgentToolUpdateInput) (any, error) {
	if err := h.management.ManageForTool(ctx, h.authority, in.TargetAgentId); err != nil {
		return nil, err
	}
	identity, ok, err := h.managedTool(ctx, in.TargetAgentId, in.ToolName)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("tool is not managed here")
	}
	key := h.overrideKey(in.TargetAgentId, ToolOverrideScopeUserAgent, identity)
	item, err := h.overrides.SetIfVersion(ctx, ToolOverrideWrite{Identity: key.Identity, Scope: key.Scope, UserID: key.UserID, AgentID: key.AgentID, Enabled: in.Enabled}, in.ExpectedVersion)
	if err != nil {
		return nil, mapOverrideConflict(err)
	}
	h.management.ReloadForTool(ctx, in.TargetAgentId)
	return item, nil
}

func (h agentOverrideHandler) Delete(ctx context.Context, in SettingsAgentToolDeleteInput) (any, error) {
	if err := h.management.ManageForTool(ctx, h.authority, in.TargetAgentId); err != nil {
		return nil, err
	}
	identity, ok, err := h.managedTool(ctx, in.TargetAgentId, in.ToolName)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("tool is not managed here")
	}
	key := h.overrideKey(in.TargetAgentId, ToolOverrideScopeUserAgent, identity)
	if err := h.overrides.ClearIfVersion(ctx, key, in.ExpectedVersion); err != nil {
		return nil, mapOverrideConflict(err)
	}
	h.management.ReloadForTool(ctx, in.TargetAgentId)
	return map[string]string{"tool_name": in.ToolName, "status": "default"}, nil
}

// managedTool accepts a name only when a runner in this context can actually
// register it and a trusted generated or MCP catalog supplies its identity.
func (h agentOverrideHandler) managedTool(ctx context.Context, targetAgentID, name string) (ToolIdentity, bool, error) {
	if IsCoreToolName(name) {
		return ToolIdentity{}, false, nil
	}
	if _, settingsManaged := settingspolicy.Lookup(name); settingsManaged {
		return ToolIdentity{}, false, nil
	}
	if identity, ok := h.registryToolIdentity(name); ok {
		return identity, true, nil
	}
	entries, err := h.catalog(ctx, targetAgentID)
	if err != nil {
		return ToolIdentity{}, false, err
	}
	for _, entry := range entries {
		if entry.Name == name && entry.Identity.Validate() == nil && entry.Identity.isPlugin() {
			return entry.Identity, true, nil
		}
	}
	return ToolIdentity{}, false, nil
}

func (h agentOverrideHandler) catalog(ctx context.Context, targetAgentID string) ([]MCPCatalogEntry, error) {
	if h.mcpCatalog == nil {
		return nil, nil
	}
	return h.mcpCatalog(ctx, h.authority, targetAgentID)
}

func (h agentOverrideHandler) overrideKey(targetID, scope string, identity ToolIdentity) ToolOverrideKey {
	key := ToolOverrideKey{Identity: identity, Scope: scope}
	if scope == ToolOverrideScopeUser || scope == ToolOverrideScopeUserAgent {
		key.UserID = string(h.authority.UserID())
	}
	if scope == ToolOverrideScopeSystemAgent || scope == ToolOverrideScopeUserAgent {
		key.AgentID = targetID
	}
	return key
}

func (h agentOverrideHandler) registryToolIdentity(name string) (ToolIdentity, bool) {
	if h.registry == nil {
		return ToolIdentity{}, false
	}
	spec, ok := h.registry.Lookup(name)
	if !ok {
		return ToolIdentity{}, false
	}
	identity := ToolIdentity{CoreToolName: name}
	if spec.PluginID != "" {
		if spec.Namespace == "" || spec.LocalName == "" {
			return ToolIdentity{}, false
		}
		identity = ToolIdentity{PluginID: spec.PluginID, LocalToolName: spec.LocalName}
	} else if spec.Namespace != "" || spec.LocalName != "" {
		return ToolIdentity{}, false
	}
	return identity, identity.Validate() == nil
}

func absentOverrideVersion(identity ToolIdentity, name string) ToolOverrideVersion {
	item := ToolOverrideVersion{ToolName: name, Scope: ToolOverrideScopeUserAgent, Version: ToolOverrideAbsentVersion}
	if identity.isPlugin() {
		item.Identity = &identity
	}
	return item
}

func mapOverrideConflict(err error) error {
	if errors.Is(err, config.ErrAgentVersionConflict) {
		return agentaccess.ErrConflict
	}
	return err
}

type agentToolView struct {
	ID                  string `json:"id"`
	Name                string `json:"name"`
	Model               string `json:"model,omitempty"`
	ModelThinking       string `json:"model_thinking,omitempty"`
	ModelStrong         string `json:"model_strong,omitempty"`
	ModelStrongThinking string `json:"model_strong_thinking,omitempty"`
	ModelFast           string `json:"model_fast,omitempty"`
	ModelFastThinking   string `json:"model_fast_thinking,omitempty"`
	SystemPrompt        string `json:"system_prompt,omitempty"`
	Soul                string `json:"soul,omitempty"`
	Scope               string `json:"scope"`
	Enabled             bool   `json:"enabled"`
	Version             string `json:"version"`
}

func projectAgent(in agentaccess.ToolAgent) agentToolView {
	a := in.Agent
	return agentToolView{ID: a.ID, Name: a.Name, Model: a.Model, ModelThinking: a.ModelThinking, ModelStrong: a.ModelStrong, ModelStrongThinking: a.ModelStrongThinking, ModelFast: a.ModelFast, ModelFastThinking: a.ModelFastThinking, SystemPrompt: a.SystemPrompt, Soul: a.Soul, Scope: a.Scope, Enabled: a.Enabled, Version: in.Version}
}

func validateToolAgent(a config.Agent) error {
	if strings.TrimSpace(a.Name) == "" {
		return fmt.Errorf("name is required")
	}
	if !config.ValidThinkingLevel(a.ModelThinking) || !config.ValidThinkingLevel(a.ModelStrongThinking) || !config.ValidThinkingLevel(a.ModelFastThinking) {
		return fmt.Errorf("invalid thinking level")
	}
	for _, value := range []string{a.Model, a.ModelStrong, a.ModelFast} {
		if !config.ValidModelRef(value) {
			return fmt.Errorf("invalid model reference")
		}
	}
	return nil
}

func slugAgentID(id, name string) string {
	if strings.TrimSpace(id) != "" {
		return id
	}
	var out []rune
	lastDash := true
	for _, r := range strings.ToLower(name) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			out, lastDash = append(out, r), false
		} else if !lastDash {
			out, lastDash = append(out, '-'), true
		}
	}
	value := strings.Trim(string(out), "-")
	if value == "" {
		return "agent"
	}
	return value
}
