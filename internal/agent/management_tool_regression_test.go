package agent

import (
	"context"
	"errors"
	"testing"

	"github.com/CherryHQ/stella/internal/authz"
	"github.com/CherryHQ/stella/pkg/toolmeta"
)

type recordingAgentToolHandler struct{ update SettingsAgentUpdateInput }

func (h *recordingAgentToolHandler) Create(context.Context, SettingsAgentCreateInput) (any, error) {
	return nil, nil
}

func (h *recordingAgentToolHandler) Delete(context.Context, SettingsAgentDeleteInput) (any, error) {
	return nil, nil
}

func (h *recordingAgentToolHandler) Get(context.Context, SettingsAgentGetInput) (any, error) {
	return nil, nil
}

func (h *recordingAgentToolHandler) List(context.Context, SettingsAgentListInput) (any, error) {
	return nil, nil
}

func (h *recordingAgentToolHandler) Update(_ context.Context, in SettingsAgentUpdateInput) (any, error) {
	h.update = in
	return nil, nil
}

func TestSettingsAgentUpdateInputPreservesExplicitEmptyStrings(t *testing.T) {
	h := &recordingAgentToolHandler{}
	_, err := SettingsAgentDispatch(context.Background(), h, "update", map[string]any{
		"id": "agent-1", "expected_version": "v1",
		"model": "", "system_prompt": "", "soul": "",
	})
	if err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]*string{
		"model": h.update.Model, "system_prompt": h.update.SystemPrompt, "soul": h.update.Soul,
	} {
		if value == nil || *value != "" {
			t.Errorf("%s = %#v, want explicit empty string", name, value)
		}
	}
}

func TestAgentToolOverridesExcludeSettingsPolicyActions(t *testing.T) {
	registry := toolmeta.NewRegistry(
		toolmeta.ActionTool{Name: "settings_agent_list", Family: "agent", Action: "list"},
		toolmeta.ActionTool{Name: "vault_secret_list", Family: "vault", Action: "list"},
		toolmeta.ActionTool{Name: "email__send", PluginID: "system/email", Namespace: "email", LocalName: "send", Family: "email", Action: "send"},
	)
	h := agentOverrideHandler{registry: registry, mcpCatalog: func(context.Context, authz.Authority, string) ([]MCPCatalogEntry, error) {
		return []MCPCatalogEntry{{Name: "gh__create_issue", Identity: ToolIdentity{PluginID: "custom/gh", LocalToolName: "create_issue"}, Family: "mcp:gh"}}, nil
	}}
	ctx := context.Background()
	if _, ok, err := h.managedTool(ctx, "", "settings_agent_list"); err != nil || ok {
		t.Fatal("Settings action must not be listed, updated, or deleted as an override")
	}
	if _, ok, err := h.managedTool(ctx, "", "vault_secret_list"); err != nil || !ok {
		t.Fatal("ordinary generated action must remain override-managed")
	}
	identity, ok := h.registryToolIdentity("email__send")
	if !ok || identity != (ToolIdentity{PluginID: "system/email", LocalToolName: "send"}) {
		t.Fatalf("plugin registry identity = %+v, %v", identity, ok)
	}
	// A legacy MCP name has no trusted plugin/local identity. The catalog keeps
	// it visible for inventory, but the settings handler must not persist a
	// name-only override.
	if _, ok, err := h.managedTool(ctx, "", "gh__create_issue"); err != nil || !ok {
		t.Fatal("trusted MCP tool must be override-managed")
	}
}

func TestAgentToolOverrideCatalogErrorPropagates(t *testing.T) {
	want := errors.New("snapshot unavailable")
	h := agentOverrideHandler{mcpCatalog: func(context.Context, authz.Authority, string) ([]MCPCatalogEntry, error) {
		return nil, want
	}}
	if _, ok, err := h.managedTool(context.Background(), "agent-1", "gh__list"); !errors.Is(err, want) || ok {
		t.Fatalf("managedTool = %v, %v; want propagated catalog error and false", err, ok)
	}
}

func TestAgentToolOverrideRejectsMalformedRegistryIdentity(t *testing.T) {
	registry := toolmeta.NewRegistry(
		toolmeta.ActionTool{Name: "email__send", PluginID: "system/email", Namespace: "email"},
		toolmeta.ActionTool{Name: "fake__tool", Namespace: "fake", LocalName: "tool"},
	)
	h := agentOverrideHandler{registry: registry}
	for _, name := range []string{"email__send", "fake__tool"} {
		if identity, ok := h.registryToolIdentity(name); ok {
			t.Fatalf("malformed registry identity %q = %+v, want rejection", name, identity)
		}
	}
}
