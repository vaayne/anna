package main

import (
	"context"

	"github.com/CherryHQ/stella/internal/agent"
	sessionaccess "github.com/CherryHQ/stella/internal/agent/session/access"
	"github.com/CherryHQ/stella/internal/agent/settingspolicy"
	"github.com/CherryHQ/stella/internal/authz"
	"github.com/CherryHQ/stella/internal/connections"
	"github.com/CherryHQ/stella/internal/controlplane"
	agentaccess "github.com/CherryHQ/stella/internal/core/access"
	"github.com/CherryHQ/stella/internal/goal"
	"github.com/CherryHQ/stella/internal/library"
	"github.com/CherryHQ/stella/internal/library/recally"
	"github.com/CherryHQ/stella/internal/mcp"
	"github.com/CherryHQ/stella/internal/memory"
	"github.com/CherryHQ/stella/internal/notify"
	pluginpkg "github.com/CherryHQ/stella/internal/plugin"
	"github.com/CherryHQ/stella/internal/scheduler"
	sharepkg "github.com/CherryHQ/stella/internal/share"
	"github.com/CherryHQ/stella/internal/skill"
	"github.com/CherryHQ/stella/internal/vault"
	workflowpkg "github.com/CherryHQ/stella/internal/workflow"
	pkgplugins "github.com/CherryHQ/stella/pkg/plugins"
	"github.com/CherryHQ/stella/pkg/toolmeta"
	pkgtools "github.com/CherryHQ/stella/pkg/tools"
	"github.com/CherryHQ/stella/plugins/email"
)

// builtinToolDeps names every service the default builtin tool set is built
// from. Assembling the set in one function keeps the list of tools a deployment
// puts in front of the model enumerable by a test, rather than spread across
// three appends in the middle of service construction.
type builtinToolDeps struct {
	Notifier        pkgplugins.Notifier
	Memory          memory.Provider
	Recall          memory.RecallSource
	GroupRecall     memory.GroupRecallSource
	Goal            *goal.Service
	Session         *sessionaccess.Service
	Library         *library.Service
	Scheduler       *scheduler.Service
	Workflow        *workflowpkg.Service
	Credentials     *connections.Service
	Email           *email.Service
	EmailTool       email.ToolDeps
	Share           *sharepkg.Service
	Recally         *recally.Service
	Vault           *vault.Service
	AgentManagement func() *agentaccess.Management
	ToolOverrides   *agent.ToolOverrideStore
	ToolMeta        func() *toolmeta.Registry
	SkillManagement *skill.Management
	SettingsAdmin   settingspolicy.AdminLookup
	SettingsAgents  settingspolicy.AgentLookup
	ControlPlane    func() *controlplane.Service
	PluginService   func() *pluginpkg.Service
	MCPAccess       func() *mcp.Access
	MCPCatalog      agent.MCPCatalogFunc
}

// toolAvailable is the fail-closed visibility predicate: a check that cannot be
// resolved reports the error rather than guessing a tool into or out of view.
type toolAvailable = func(context.Context, agent.RunnerParams) (bool, error)

// splitBuiltins turns a family's generated ActionTools into one registration
// entry per action. newTool builds the adapter for one action; every entry
// shares the same availability check, because visibility is a property of the
// family's service, not of the individual action.
//
// Adding an action to a split family therefore needs no edit here: the entry
// appears as soon as toolgen emits it.
func splitBuiltins(specs []toolmeta.ActionTool, newTool func(toolmeta.ActionTool) pkgtools.Tool, available toolAvailable) []agent.BuiltinTool {
	out := make([]agent.BuiltinTool, 0, len(specs))
	for _, spec := range specs {
		out = append(out, agent.BuiltinTool{Tool: newTool(spec), Available: available})
	}
	return out
}

// splitRuntimeBuiltins is splitBuiltins for a family whose tools need the
// sandbox session. The definition is still static, so the catalog can list the
// tool without building one.
func splitRuntimeBuiltins(specs []toolmeta.ActionTool, newTool func(pkgplugins.ToolBuildContext, toolmeta.ActionTool) pkgtools.Tool, spec func(toolmeta.ActionTool) pkgtools.Tool, available toolAvailable) []agent.BuiltinTool {
	out := make([]agent.BuiltinTool, 0, len(specs))
	for _, actionSpec := range specs {
		out = append(out, agent.BuiltinTool{
			Build: func(build pkgplugins.ToolBuildContext) (pkgtools.Tool, error) {
				return newTool(build, actionSpec), nil
			},
			Spec:      spec(actionSpec).Definition(),
			Available: available,
		})
	}
	return out
}

// builtinToolGroup is the one composition-time declaration for a generated
// family. Metadata is kept separate from runtime construction because some
// families expose a complete static inventory while only a runtime subset is
// executable in a runner (for example Skill and Library).
type builtinToolGroup struct {
	metadata []toolmeta.ActionTool
	runtime  func(builtinToolDeps) []agent.BuiltinTool
}

func settingsToolAvailable(d builtinToolDeps, adminOnly bool) toolAvailable {
	return settingspolicy.Available(adminOnly, d.SettingsAdmin, d.SettingsAgents)
}

// builtinToolGroups keeps family membership in one list. The runtime
// projection intentionally remains explicit in each entry so ordering,
// availability, wrappers, and nil-service behavior stay visible at the
// composition seam.
func builtinToolGroups() []builtinToolGroup {
	return []builtinToolGroup{
		{
			metadata: memory.ActionTools(),
			runtime: func(d builtinToolDeps) []agent.BuiltinTool {
				// One Recall per deployment, one registered tool per action: the
				// provider's capabilities are probed once, so they belong to the
				// deployment rather than to a call. The lane a call takes is still
				// chosen per turn.
				recall := memory.NewRecall(d.Memory, d.Recall, d.GroupRecall)
				return splitBuiltins(memory.ActionTools(), func(spec toolmeta.ActionTool) pkgtools.Tool {
					return memory.NewTool(recall, spec)
				}, agent.BuiltinToolAvailable)
			},
		},
		// Skill's runtime projection is assembled with the active sandbox in
		// commands.go; keep its complete generated inventory here for metadata.
		{metadata: skill.SkillActionTools()},
		{
			// notify is hand-written, so it has no generated metadata family. Its
			// position is still part of the builtin runtime ordering.
			runtime: func(d builtinToolDeps) []agent.BuiltinTool {
				if notifyTool := notify.NewTool(d.Notifier); notifyTool != nil {
					return []agent.BuiltinTool{{Tool: notifyTool}}
				}
				return nil
			},
		},
		{
			metadata: goal.ActionTools(),
			runtime: func(d builtinToolDeps) []agent.BuiltinTool {
				return splitBuiltins(goal.ActionTools(), func(spec toolmeta.ActionTool) pkgtools.Tool {
					return goal.NewTool(d.Goal, spec)
				}, agent.BuiltinToolAvailable)
			},
		},
		{
			metadata: sessionaccess.ActionTools(),
			runtime: func(d builtinToolDeps) []agent.BuiltinTool {
				return splitBuiltins(sessionaccess.ActionTools(), func(spec toolmeta.ActionTool) pkgtools.Tool {
					return sessionaccess.NewTool(d.Session, spec)
				}, func(ctx context.Context, params agent.RunnerParams) (bool, error) {
					baseline, err := agent.BuiltinToolAvailable(ctx, params)
					if err != nil {
						return false, err
					}
					return params.GroupID == "" && baseline, nil
				})
			},
		},
		{
			metadata: library.LibraryActionTools(),
			runtime: func(d builtinToolDeps) []agent.BuiltinTool {
				return splitBuiltins(library.RuntimeActionTools(), func(spec toolmeta.ActionTool) pkgtools.Tool {
					return library.NewTool(d.Library, spec)
				}, libraryToolAvailable)
			},
		},
		{
			metadata: library.SettingsLibraryActionTools(),
			runtime: func(d builtinToolDeps) []agent.BuiltinTool {
				return splitRuntimeBuiltins(library.SettingsLibraryActionTools(), func(build pkgplugins.ToolBuildContext, spec toolmeta.ActionTool) pkgtools.Tool {
					return settingspolicy.Wrap(library.NewRuntimeManagementTool(d.Library, build.Runtime, spec), d.SettingsAgents, d.SettingsAdmin)
				}, func(spec toolmeta.ActionTool) pkgtools.Tool {
					return library.NewRuntimeManagementTool(d.Library, nil, spec)
				}, settingsToolAvailable(d, false))
			},
		},
		{
			metadata: skill.SettingsSkillActionTools(),
			runtime: func(d builtinToolDeps) []agent.BuiltinTool {
				return splitRuntimeBuiltins(skill.SettingsSkillActionTools(), func(build pkgplugins.ToolBuildContext, spec toolmeta.ActionTool) pkgtools.Tool {
					return settingspolicy.Wrap(skill.NewRuntimeManagementTool(d.SkillManagement, build.Runtime, spec), d.SettingsAgents, d.SettingsAdmin)
				}, func(spec toolmeta.ActionTool) pkgtools.Tool {
					return skill.NewRuntimeManagementTool(d.SkillManagement, nil, spec)
				}, settingsToolAvailable(d, false))
			},
		},
		{
			metadata: scheduler.ActionTools(),
			runtime: func(d builtinToolDeps) []agent.BuiltinTool {
				return splitBuiltins(scheduler.ActionTools(), func(spec toolmeta.ActionTool) pkgtools.Tool {
					return scheduler.NewTool(d.Scheduler, spec)
				}, agent.BuiltinToolAvailable)
			},
		},
		{
			metadata: workflowpkg.ActionTools(),
			runtime: func(d builtinToolDeps) []agent.BuiltinTool {
				return splitBuiltins(workflowpkg.ActionTools(), func(spec toolmeta.ActionTool) pkgtools.Tool {
					return workflowpkg.NewTool(d.Workflow, spec)
				}, agent.BuiltinToolAvailable)
			},
		},
		{
			metadata: connections.ActionTools(),
			runtime: func(d builtinToolDeps) []agent.BuiltinTool {
				return splitBuiltins(connections.ActionTools(), func(spec toolmeta.ActionTool) pkgtools.Tool {
					return connections.NewTool(d.Credentials, spec)
				}, oauthToolAvailable(d.Credentials))
			},
		},
		{
			metadata: email.ActionTools(),
			runtime: func(d builtinToolDeps) []agent.BuiltinTool {
				builtins := splitBuiltins(email.ActionTools(), func(spec toolmeta.ActionTool) pkgtools.Tool {
					return email.NewTool(d.Email, spec, d.EmailTool)
				}, emailToolAvailable(d.Vault))
				for i := range builtins {
					// This declaration follows the same EMAIL_CONFIG check as
					// Available. The Profile only exposes it after that
					// authoritative check returned false.
					builtins[i].UnavailableReason = agent.ToolUnavailableReasonEmailConfigRequired
				}
				return builtins
			},
		},
		{
			metadata: sharepkg.ActionTools(),
			runtime: func(d builtinToolDeps) []agent.BuiltinTool {
				return splitBuiltins(sharepkg.ActionTools(), func(spec toolmeta.ActionTool) pkgtools.Tool {
					return sharepkg.NewTool(d.Share, spec)
				}, agent.BuiltinToolAvailable)
			},
		},
		{
			metadata: recally.ActionTools(),
			runtime: func(d builtinToolDeps) []agent.BuiltinTool {
				return splitRuntimeBuiltins(recally.ActionTools(), func(build pkgplugins.ToolBuildContext, spec toolmeta.ActionTool) pkgtools.Tool {
					return recally.NewRuntimeTool(d.Recally, build.Runtime, spec)
				}, func(spec toolmeta.ActionTool) pkgtools.Tool {
					return recally.NewTool(d.Recally, spec)
				}, agent.BuiltinToolAvailable)
			},
		},
		{
			metadata: vault.ActionTools(),
			runtime: func(d builtinToolDeps) []agent.BuiltinTool {
				if d.Vault == nil {
					return nil
				}
				return splitBuiltins(vault.ActionTools(), func(spec toolmeta.ActionTool) pkgtools.Tool {
					return vault.NewTool(d.Vault, d.Credentials, spec)
				}, agent.BuiltinToolAvailable)
			},
		},
		{
			metadata: agent.SettingsAgentActionTools(),
			runtime: func(d builtinToolDeps) []agent.BuiltinTool {
				return splitBuiltins(agent.SettingsAgentActionTools(), func(spec toolmeta.ActionTool) pkgtools.Tool {
					return settingspolicy.Wrap(agent.NewManagementTool(spec, d.AgentManagement), d.SettingsAgents, d.SettingsAdmin)
				}, settingsToolAvailable(d, false))
			},
		},
		{
			metadata: agent.SettingsAgentToolActionTools(),
			runtime: func(d builtinToolDeps) []agent.BuiltinTool {
				return splitBuiltins(agent.SettingsAgentToolActionTools(), func(spec toolmeta.ActionTool) pkgtools.Tool {
					return settingspolicy.Wrap(agent.NewToolOverrideManagementTool(spec, d.AgentManagement, d.ToolOverrides, d.ToolMeta, d.MCPCatalog), d.SettingsAgents, d.SettingsAdmin)
				}, settingsToolAvailable(d, false))
			},
		},
		{
			metadata: controlplane.SettingsProviderActionTools(),
			runtime: func(d builtinToolDeps) []agent.BuiltinTool {
				return splitBuiltins(controlplane.SettingsProviderActionTools(), func(spec toolmeta.ActionTool) pkgtools.Tool {
					return settingspolicy.Wrap(controlplane.NewProviderManagementTool(spec, d.ControlPlane), d.SettingsAgents, d.SettingsAdmin)
				}, settingsToolAvailable(d, true))
			},
		},
		{
			metadata: controlplane.SettingsDefaultModelActionTools(),
			runtime: func(d builtinToolDeps) []agent.BuiltinTool {
				return splitBuiltins(controlplane.SettingsDefaultModelActionTools(), func(spec toolmeta.ActionTool) pkgtools.Tool {
					return settingspolicy.Wrap(controlplane.NewDefaultModelManagementTool(spec, d.ControlPlane), d.SettingsAgents, d.SettingsAdmin)
				}, settingsToolAvailable(d, true))
			},
		},
		{
			metadata: controlplane.SettingsEmbeddingSettingActionTools(),
			runtime: func(d builtinToolDeps) []agent.BuiltinTool {
				return splitBuiltins(controlplane.SettingsEmbeddingSettingActionTools(), func(spec toolmeta.ActionTool) pkgtools.Tool {
					return settingspolicy.Wrap(controlplane.NewEmbeddingSettingManagementTool(spec, d.ControlPlane), d.SettingsAgents, d.SettingsAdmin)
				}, settingsToolAvailable(d, true))
			},
		},
		{
			metadata: controlplane.SettingsPluginActionTools(),
			runtime: func(d builtinToolDeps) []agent.BuiltinTool {
				return splitBuiltins(controlplane.SettingsPluginActionTools(), func(spec toolmeta.ActionTool) pkgtools.Tool {
					return settingspolicy.Wrap(controlplane.NewPluginManagementTool(spec, d.PluginService), d.SettingsAgents, d.SettingsAdmin)
				}, settingsToolAvailable(d, true))
			},
		},
		{
			metadata: mcp.SettingsMcpActionTools(),
			runtime: func(d builtinToolDeps) []agent.BuiltinTool {
				return splitBuiltins(mcp.SettingsMcpActionTools(), func(spec toolmeta.ActionTool) pkgtools.Tool {
					return settingspolicy.Wrap(mcp.NewManagementTool(spec, d.MCPAccess), d.SettingsAgents, d.SettingsAdmin)
				}, settingsToolAvailable(d, false))
			},
		},
	}
}

// newBuiltinTools returns the builtin tools in the order they are offered to the
// runner. A nil Notifier omits notify and a nil Vault omits the vault tools;
// other missing services leave their static definitions in the inventory and
// report availability or execution errors at their existing boundaries.
func newBuiltinTools(d builtinToolDeps) []agent.BuiltinTool {
	var builtins []agent.BuiltinTool
	for _, family := range builtinToolGroups() {
		if family.runtime != nil {
			builtins = append(builtins, family.runtime(d)...)
		}
	}
	return builtins
}

// generatedFamilies is every family toolgen emits in this build. It is one list
// so that the selector registry and any test that asserts on real tool names
// read from the same source.
func generatedFamilies() [][]toolmeta.ActionTool {
	var families [][]toolmeta.ActionTool
	for _, group := range builtinToolGroups() {
		if len(group.metadata) > 0 {
			families = append(families, group.metadata)
		}
	}
	return families
}

// newToolMetaRegistry declares this build's generated tools for the name-only
// selector sites — the runner's excluded_tools filter, the delegate preset
// whitelist, and the trace hook's action attribute. Every family goes in,
// including ones whose service is unavailable at runtime: the registry answers
// "what does this name mean", not "is this tool usable right now".
func newToolMetaRegistry(families ...[]toolmeta.ActionTool) *toolmeta.Registry {
	var all []toolmeta.ActionTool
	for _, family := range families {
		all = append(all, family...)
	}
	return toolmeta.NewRegistry(all...)
}

// splitFamilyNames lists every generated tool name in the given families. The
// goal worker's exclusion list is derived from it rather than written by hand:
// a new scheduler action must not become a tool a goal worker can call just
// because nobody remembered to add its name here.
func splitFamilyNames(families ...[]toolmeta.ActionTool) []string {
	var out []string
	for _, family := range families {
		for _, spec := range family {
			out = append(out, spec.Name)
		}
	}
	return out
}

// mcpCatalogFunc adapts the MCP service to the agent package's catalog func:
// the persisted catalogs of registrations effective for one trusted authority
// and agent. Each entry carries its durable policy identity alongside the
// exported display name.
func mcpCatalogFunc(svc *mcp.Service) agent.MCPCatalogFunc {
	if svc == nil {
		return nil
	}
	return func(ctx context.Context, authority authz.Authority, agentID string) ([]agent.MCPCatalogEntry, error) {
		snapshot, err := svc.SnapshotForAuthority(ctx, authority, agentID)
		if err != nil {
			return nil, err
		}
		regs, err := svc.RegistrationsForSnapshot(ctx, snapshot)
		if err != nil {
			return nil, err
		}
		catalog := make([]agent.MCPCatalogEntry, 0)
		for _, reg := range regs {
			for _, tool := range reg.Tools {
				local := mcp.SanitizeIdent(tool.Name, "tool")
				name, err := pluginpkg.ExportedToolName(reg.Namespace, local)
				if err != nil {
					return nil, err
				}
				identity := agent.ToolIdentity{PluginID: reg.PluginID, LocalToolName: local}
				if err := identity.Validate(); err != nil {
					return nil, err
				}
				catalog = append(catalog, agent.MCPCatalogEntry{Name: name, Identity: identity, Family: "mcp:" + reg.Name})
			}
		}
		return catalog, nil
	}
}
