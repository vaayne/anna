package server

import (
	"testing"

	"github.com/CherryHQ/stella/internal/mcp"
	"github.com/CherryHQ/stella/pkg/toolmeta"
)

func TestMCPToolNameRequiresPluginNamespace(t *testing.T) {
	if got, ok := mcpToolName(mcp.Registration{Namespace: "settings_server"}, mcp.CatalogTool{Name: "list"}); !ok || got != "settings_server__list" {
		t.Fatalf("mcpToolName with namespace = %q, %v; want settings_server__list, true", got, ok)
	}
	if got, ok := mcpToolName(mcp.Registration{Name: "settings_server"}, mcp.CatalogTool{Name: "list"}); ok || got != "" {
		t.Fatalf("mcpToolName without namespace = %q, %v; want empty, false", got, ok)
	}
}

func TestToolFamilyUsesRegistryBeforeStableFallbacks(t *testing.T) {
	s := &Server{toolMeta: toolmeta.NewRegistry(
		toolmeta.ActionTool{Name: "goal_list", Family: "goal", Action: "list"},
	)}

	cases := []struct {
		name   string
		source string
		want   string
	}{
		{name: "goal_list", source: agentToolSourceBuiltin, want: "goal"},
		// Plugins never inherit a builtin family, even if a duplicate name reaches
		// this helper before the runner's collision guard rejects registration.
		{name: "goal_list", source: agentToolSourcePlugin, want: agentToolFamilyPlugin},
		{name: "bash", source: agentToolSourceCore, want: agentToolFamilyCore},
		{name: "notify", source: agentToolSourceBuiltin, want: agentToolFamilyOther},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := s.toolFamily(tc.name, tc.source); got != tc.want {
				t.Errorf("toolFamily(%q, %q) = %q, want %q", tc.name, tc.source, got, tc.want)
			}
		})
	}
}

func TestToolInputSchema(t *testing.T) {
	if got := toolInputSchema(nil); got != nil {
		t.Errorf("nil schema should map to nil, got %v", got)
	}
	if got := toolInputSchema(map[string]any{}); got != nil {
		t.Errorf("empty schema should map to nil, got %v", got)
	}
	schema := map[string]any{"type": "object", "required": []any{"action"}}
	got := toolInputSchema(schema)
	if got == nil {
		t.Fatal("non-empty schema should be returned")
	}
	if (*got)["type"] != "object" {
		t.Errorf("schema content not preserved: %v", *got)
	}
}
