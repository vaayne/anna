package main

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/CherryHQ/stella/internal/plugin"
	coreplugins "github.com/CherryHQ/stella/plugins/core"
)

func TestPluginBackendPolicyRejectsCoreRuntimeBinaryNames(t *testing.T) {
	policy := pluginBackendPolicy(false)
	for _, resource := range coreplugins.RuntimeResources() {
		t.Run("reserved/"+resource.Name, func(t *testing.T) {
			definition, payload := testCLIBackendDefinition(t, resource.Name)
			enabled := false
			config := plugin.Config{
				ID: "config", PluginID: definition.ID, Namespace: definition.Namespace,
				Scope: plugin.ScopeSystem, Enabled: &enabled, Payload: payload, Revision: 1,
			}
			if err := policy.Validate(t.Context(), definition, config, nil); !errors.Is(err, plugin.ErrInvalidConfig) {
				t.Fatalf("reserved binary %q error = %v, want ErrInvalidConfig", resource.Name, err)
			}
		})
	}

	definition, payload := testCLIBackendDefinition(t, "ordinary-tool")
	enabled := false
	config := plugin.Config{
		ID: "config", PluginID: definition.ID, Namespace: definition.Namespace,
		Scope: plugin.ScopeSystem, Enabled: &enabled, Payload: payload, Revision: 1,
	}
	if err := policy.Validate(t.Context(), definition, config, nil); err != nil {
		t.Fatalf("ordinary binary error = %v, want nil", err)
	}

	definition, _ = testCLIBackendDefinition(t, "ordinary-tool")
	_, reservedPayload := testCLIBackendDefinition(t, coreplugins.RuntimeResources()[0].Name)
	config = plugin.Config{
		ID: "config", PluginID: definition.ID, Namespace: definition.Namespace,
		Scope: plugin.ScopeSystem, Enabled: &enabled, Payload: reservedPayload, Revision: 1,
	}
	if err := policy.Validate(t.Context(), definition, config, nil); !errors.Is(err, plugin.ErrInvalidConfig) {
		t.Fatalf("reserved config binary error = %v, want ErrInvalidConfig", err)
	}
}

func testCLIBackendDefinition(t *testing.T, binaryName string) (plugin.Definition, json.RawMessage) {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"description": "test",
		"binaries":    []map[string]string{{"name": binaryName, "tool": "github:owner/tool", "version": "1.0.0"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return plugin.Definition{
		ID: "tool/test", Namespace: "test", DisplayName: "Test",
		Backend: plugin.BackendCLI, Source: plugin.SourceBuiltin,
		ImplementationKey: "tool/test", Spec: payload, Revision: 1,
	}, payload
}
