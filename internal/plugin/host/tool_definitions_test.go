package host

import (
	"testing"

	"github.com/CherryHQ/stella/internal/library/recally"
	"github.com/CherryHQ/stella/internal/plugin"
	"github.com/CherryHQ/stella/internal/scheduler"
	"github.com/CherryHQ/stella/pkg/toolmeta"
	"github.com/CherryHQ/stella/plugins/email"
)

func TestBuiltinToolDefinitionsProjectsGeneratedPluginFamilies(t *testing.T) {
	reg := toolmeta.NewRegistry(
		append(append(email.ActionTools(), recally.ActionTools()...), scheduler.ActionTools()...)...,
	)
	definitions, err := BuiltinToolDefinitions(reg)
	if err != nil {
		t.Fatal(err)
	}
	if len(definitions) != 3 {
		t.Fatalf("definitions = %d, want 3: %#v", len(definitions), definitions)
	}
	for _, want := range []struct {
		id, namespace, display string
	}{
		{"system/email", "email", "Email"},
		{"system/recally", "recally", "Recally"},
		{"system/scheduler", "scheduler", "Scheduler"},
	} {
		definition, ok := definitionByID(definitions, want.id)
		if !ok {
			t.Fatalf("missing definition %q", want.id)
		}
		if definition.Namespace != want.namespace || definition.DisplayName != want.display || definition.Backend != plugin.BackendGo || definition.Source != plugin.SourceBuiltin || definition.ImplementationKey != want.id || string(definition.Spec) != `{}` || !definition.DefaultEnabled {
			t.Fatalf("definition %q = %#v", want.id, definition)
		}
	}
}

func TestBuiltinToolDefinitionsRejectUntrustedMetadata(t *testing.T) {
	tests := []struct {
		name string
		tool toolmeta.ActionTool
	}{
		{name: "missing local", tool: toolmeta.ActionTool{Name: "email__send", PluginID: "system/email", Namespace: "email"}},
		{name: "name mismatch", tool: toolmeta.ActionTool{Name: "email__send", PluginID: "system/email", Namespace: "email", LocalName: "read"}},
		{name: "namespace separator", tool: toolmeta.ActionTool{Name: "bad__send", PluginID: "system/bad", Namespace: "bad__namespace", LocalName: "send"}},
		{name: "core metadata", tool: toolmeta.ActionTool{Name: "email__send", Namespace: "email", LocalName: "send"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := BuiltinToolDefinitions(toolmeta.NewRegistry(tt.tool)); err == nil {
				t.Fatal("accepted invalid trusted identity")
			}
		})
	}
}

func TestBuiltinToolDefinitionsRejectCollisions(t *testing.T) {
	reg := toolmeta.NewRegistry(
		toolmeta.ActionTool{Name: "email__send", PluginID: "system/email", Namespace: "email", LocalName: "send"},
		toolmeta.ActionTool{Name: "mail__read", PluginID: "system/mail", Namespace: "email", LocalName: "read"},
	)
	if _, err := BuiltinToolDefinitions(reg); err == nil {
		t.Fatal("accepted namespace collision")
	}

	reg = toolmeta.NewRegistry(
		toolmeta.ActionTool{Name: "email__send", PluginID: "system/email", Namespace: "email", LocalName: "send"},
		toolmeta.ActionTool{Name: "mail__read", PluginID: "system/email", Namespace: "mail", LocalName: "read"},
	)
	if _, err := BuiltinToolDefinitions(reg); err == nil {
		t.Fatal("accepted namespace drift")
	}
}

func definitionByID(definitions []plugin.Definition, id string) (plugin.Definition, bool) {
	for _, definition := range definitions {
		if definition.ID == id {
			return definition, true
		}
	}
	return plugin.Definition{}, false
}
