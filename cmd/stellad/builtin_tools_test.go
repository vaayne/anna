package main

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/CherryHQ/stella/internal/agent"
	agentsandbox "github.com/CherryHQ/stella/internal/agent/sandbox"
	sessionaccess "github.com/CherryHQ/stella/internal/agent/session/access"
	"github.com/CherryHQ/stella/internal/connections"
	"github.com/CherryHQ/stella/internal/controlplane"
	"github.com/CherryHQ/stella/internal/goal"
	"github.com/CherryHQ/stella/internal/library"
	"github.com/CherryHQ/stella/internal/library/recally"
	"github.com/CherryHQ/stella/internal/mcp"
	"github.com/CherryHQ/stella/internal/memory"
	"github.com/CherryHQ/stella/internal/scheduler"
	sharepkg "github.com/CherryHQ/stella/internal/share"
	skillstool "github.com/CherryHQ/stella/internal/skill"
	"github.com/CherryHQ/stella/internal/vault"
	workflowpkg "github.com/CherryHQ/stella/internal/workflow"
	pkgchannel "github.com/CherryHQ/stella/pkg/channel"
	"github.com/CherryHQ/stella/pkg/toolmeta"
	"github.com/CherryHQ/stella/plugins/email"
)

type stubNotifier struct{}

func (stubNotifier) Notify(context.Context, pkgchannel.Notification) error { return nil }

func (stubNotifier) NotifyUser(context.Context, string, pkgchannel.Notification) error { return nil }

// defaultModelFacingTools is a golden record of the tool names a default
// deployment puts in front of the model: the sandbox core tools, the skills
// tool, and every builtin. It exists for the action-split work (#1173), where
// one tool name becomes several: this list is the one place a rename, addition,
// or removal has to show up as a reviewable diff instead of being discovered in
// production. Update it deliberately, together with the skills, prompts, and
// tool_override rows that carry the same names — never just to make the test
// pass.
//
// Deliberately not covered are only the plugin-provided, remote MCP, and
// per-run tools whose names vary by deployment. Settings management tools are
// fixed builtins and therefore belong in this golden record.
var defaultModelFacingTools = []string{
	"bash",
	"view_image",
	"skill_installed_search",
	"skill_load",
	"memory_read",
	"memory_search",
	"notify",
	"goal_cancel",
	"goal_create",
	"goal_get",
	"goal_list",
	"session_create",
	"session_get",
	"session_list",
	"session_send",
	"library_search",
	"scheduler__job_create",
	"scheduler__job_delete",
	"scheduler__job_get",
	"scheduler__job_list",
	"scheduler__job_pause",
	"scheduler__job_resume",
	"scheduler__job_update",
	"workflow_get",
	"workflow_list",
	"workflow_run",
	"workflow_save",
	"oauth_connect",
	"oauth_disconnect",
	"oauth_flow_status",
	"oauth_list",
	"email__account_list",
	"email__message_list",
	"email__message_read",
	"email__message_send",
	"share_create_article",
	"share_create_artifact",
	"share_list",
	"share_revoke",
	"recally__article_get",
	"recally__article_list",
	"recally__article_save",
	"recally__digest_get",
	"recally__digest_save",
	"recally__entry_add",
	"recally__entry_list",
	"recally__entry_update",
	"recally__feed_add",
	"recally__feed_list",
	"recally__feed_poll",
	"recally__feed_remove",
	"vault_secret_delete",
	"vault_secret_list",
	"vault_secret_set",
	"settings_agent_create",
	"settings_agent_delete",
	"settings_agent_get",
	"settings_agent_list",
	"settings_agent_update",
	"settings_agent_tool_delete",
	"settings_agent_tool_list",
	"settings_agent_tool_update",
	"settings_library_file_delete",
	"settings_library_file_get",
	"settings_library_file_list",
	"settings_library_file_upload",
	"settings_skill_create",
	"settings_skill_delete",
	"settings_skill_get",
	"settings_skill_list",
	"settings_skill_update",
	"settings_provider_list",
	"settings_provider_get",
	"settings_provider_create",
	"settings_provider_update",
	"settings_provider_delete",
	"settings_default_model_get",
	"settings_default_model_update",
	"settings_embedding_setting_get",
	"settings_embedding_setting_update",
	"settings_plugin_list",
	"settings_plugin_enable",
	"settings_plugin_disable",
	"settings_mcp_server_list",
	"settings_mcp_server_get",
	"settings_mcp_server_create",
	"settings_mcp_server_update",
	"settings_mcp_server_delete",
	"settings_mcp_server_probe",
}

// defaultToolNames is the same surface the runner assembles, minus the pieces
// that need a live deployment. Services stay nil: a tool's Definition never
// reaches its service. The notifier and the vault are the two exceptions,
// because a nil one removes its tool from the set entirely.
func defaultToolNames(t *testing.T) []string {
	t.Helper()
	names := make([]string, 0, len(skillstool.RuntimeActionTools()))
	for _, spec := range skillstool.RuntimeActionTools() {
		names = append(names, spec.Name)
	}
	for _, core := range agentsandbox.ToolDefinitionsWithAvailability() {
		names = append(names, core.Definition.Name)
	}
	for _, builtin := range newBuiltinTools(builtinToolDeps{Notifier: stubNotifier{}, Vault: &vault.Service{}}) {
		definition, ok := builtin.Definition()
		if !ok || definition.Name == "" {
			t.Fatalf("builtin tool has no usable definition: %#v", definition)
		}
		names = append(names, definition.Name)
	}
	slices.Sort(names)
	return names
}

func TestEmailBuiltinsDeclareConfigUnavailableReason(t *testing.T) {
	want := make(map[string]bool, len(email.ActionTools()))
	for _, spec := range email.ActionTools() {
		want[spec.Name] = true
	}
	for _, builtin := range newBuiltinTools(builtinToolDeps{Notifier: stubNotifier{}, Vault: &vault.Service{}}) {
		definition, ok := builtin.Definition()
		if !ok || !want[definition.Name] {
			continue
		}
		if builtin.UnavailableReason != agent.ToolUnavailableReasonEmailConfigRequired {
			t.Errorf("email builtin %q UnavailableReason = %q, want %q", definition.Name, builtin.UnavailableReason, agent.ToolUnavailableReasonEmailConfigRequired)
		}
		delete(want, definition.Name)
	}
	if len(want) != 0 {
		t.Fatalf("email actions missing from builtin inventory: %v", want)
	}
}

func TestDefaultToolNamesMatchGolden(t *testing.T) {
	got := defaultToolNames(t)
	want := slices.Clone(defaultModelFacingTools)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("the model-facing tool names changed.\n got: %q\nwant: %q\nIf this is intended, update defaultModelFacingTools along with the skills, prompts, and docs naming these tools.", got, want)
	}
}

func TestBuiltinRuntimeProjectionPreservesOrder(t *testing.T) {
	got := builtinDefinitionNames(newBuiltinTools(builtinToolDeps{Notifier: stubNotifier{}, Vault: &vault.Service{}}))
	var want []string
	appendSpecs := func(specs []toolmeta.ActionTool) {
		for _, spec := range specs {
			want = append(want, spec.Name)
		}
	}
	appendSpecs(memory.ActionTools())
	want = append(want, "notify")
	appendSpecs(goal.ActionTools())
	appendSpecs(sessionaccess.ActionTools())
	appendSpecs(library.RuntimeActionTools())
	appendSpecs(library.SettingsLibraryActionTools())
	appendSpecs(skillstool.SettingsSkillActionTools())
	appendSpecs(scheduler.ActionTools())
	appendSpecs(workflowpkg.ActionTools())
	appendSpecs(connections.ActionTools())
	appendSpecs(email.ActionTools())
	appendSpecs(sharepkg.ActionTools())
	appendSpecs(recally.ActionTools())
	appendSpecs(vault.ActionTools())
	appendSpecs(agent.SettingsAgentActionTools())
	appendSpecs(agent.SettingsAgentToolActionTools())
	appendSpecs(controlplane.SettingsProviderActionTools())
	appendSpecs(controlplane.SettingsDefaultModelActionTools())
	appendSpecs(controlplane.SettingsEmbeddingSettingActionTools())
	appendSpecs(controlplane.SettingsPluginActionTools())
	appendSpecs(mcp.SettingsMcpActionTools())
	if !slices.Equal(got, want) {
		t.Fatalf("builtin runtime order = %v, want %v", got, want)
	}
}

func TestGeneratedMetadataKeepsCompleteStaticFamilies(t *testing.T) {
	var got []string
	for _, family := range generatedFamilies() {
		for _, spec := range family {
			got = append(got, spec.Name)
		}
	}
	var want []string
	appendSpecs := func(specs []toolmeta.ActionTool) {
		for _, spec := range specs {
			want = append(want, spec.Name)
		}
	}
	appendSpecs(goal.ActionTools())
	appendSpecs(scheduler.ActionTools())
	appendSpecs(workflowpkg.ActionTools())
	appendSpecs(connections.ActionTools())
	appendSpecs(email.ActionTools())
	appendSpecs(sharepkg.ActionTools())
	appendSpecs(vault.ActionTools())
	appendSpecs(recally.ActionTools())
	appendSpecs(sessionaccess.ActionTools())
	appendSpecs(skillstool.SkillActionTools())
	appendSpecs(skillstool.SettingsSkillActionTools())
	appendSpecs(memory.ActionTools())
	appendSpecs(library.LibraryActionTools())
	appendSpecs(library.SettingsLibraryActionTools())
	appendSpecs(agent.SettingsAgentActionTools())
	appendSpecs(agent.SettingsAgentToolActionTools())
	appendSpecs(controlplane.SettingsProviderActionTools())
	appendSpecs(controlplane.SettingsDefaultModelActionTools())
	appendSpecs(controlplane.SettingsEmbeddingSettingActionTools())
	appendSpecs(controlplane.SettingsPluginActionTools())
	appendSpecs(mcp.SettingsMcpActionTools())
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("generated metadata names = %v, want complete static inventory %v", got, want)
	}
}

func TestBuiltinRuntimeKeepsDefinitionsWithoutOptionalServices(t *testing.T) {
	got := newBuiltinTools(builtinToolDeps{})
	full := builtinDefinitionNames(newBuiltinTools(builtinToolDeps{Notifier: stubNotifier{}, Vault: &vault.Service{}}))
	want := make([]string, 0, len(full))
	for _, name := range full {
		if name != "notify" && !strings.HasPrefix(name, "vault_") {
			want = append(want, name)
		}
	}
	if !slices.Equal(builtinDefinitionNames(got), want) {
		t.Fatalf("no-service runtime names = %v, want complete projection without optional tools %v", builtinDefinitionNames(got), want)
	}
	for _, builtin := range got {
		definition, ok := builtin.Definition()
		if !ok {
			t.Fatalf("builtin has no static definition: %#v", builtin)
		}
		if strings.HasPrefix(definition.Name, "settings_library_") || strings.HasPrefix(definition.Name, "settings_skill_") {
			if builtin.Build == nil {
				t.Errorf("settings runtime tool %q lost its runtime builder", definition.Name)
			}
		}
	}
}

func builtinDefinitionNames(builtins []agent.BuiltinTool) []string {
	names := make([]string, 0, len(builtins))
	for _, builtin := range builtins {
		definition, ok := builtin.Definition()
		if !ok {
			continue
		}
		names = append(names, definition.Name)
	}
	return names
}
