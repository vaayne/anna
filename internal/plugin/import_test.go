package plugin

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/CherryHQ/stella/pkg/toolmeta"
)

func TestNormalizeLegacyMCPKeepsIdentityAndSecretBoundaries(t *testing.T) {
	catalog := NewCatalog()
	plan, err := NormalizeLegacySnapshot(LegacySnapshot{MCP: []LegacyMCPRegistration{{
		ID: "0198f9a4-1b2c-7def-8123-456789abcdef", Scope: string(ScopeUser), UserID: "user-1",
		Name: "GitHub Cloud", URL: "https://mcp.example.test", Transport: "sse", AuthType: "oauth",
		CredentialMode: "shared", OAuthClientSecretExists: true,
		Metadata: map[string]any{"oauth": map[string]any{"client_id": "client-1", "client_secret_ref": "MCP_OAUTH_CLIENT_0198F9A4_1B2C_7DEF_8123_456789ABCDEF"}},
		Tools:    json.RawMessage(`[{"name":"create-issue"}]`), Enabled: true,
	}}}, catalog, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Definitions) != 1 || len(plan.Configs) != 1 {
		t.Fatalf("plan sizes = %d definitions / %d configs", len(plan.Definitions), len(plan.Configs))
	}
	definition, config := plan.Definitions[0], plan.Configs[0]
	if definition.ID != "custom/0198f9a4-1b2c-7def-8123-456789abcdef" || definition.Namespace != "GitHub_Cloud" {
		t.Fatalf("MCP identity = %q/%q", definition.ID, definition.Namespace)
	}
	if string(definition.Spec) != `{}` || definition.ImplementationKey != "mcp" || definition.CreatorUserID != "user-1" {
		t.Fatalf("MCP definition safety fields = spec=%s key=%q creator=%q", definition.Spec, definition.ImplementationKey, definition.CreatorUserID)
	}
	if strings.Contains(string(definition.Spec), "example.test") || strings.Contains(string(definition.Spec), "secret") {
		t.Fatalf("definition contains endpoint or secret material: %s", definition.Spec)
	}
	if !strings.Contains(string(config.Payload), "https://mcp.example.test") || strings.Contains(string(config.Payload), "create-issue") || strings.Contains(string(config.Payload), "tool_map") {
		t.Fatalf("config mixed MCP observation into backend payload: %s", config.Payload)
	}
	if strings.Contains(string(config.Payload), "MCP_OAUTH_CLIENT_KEPT") {
		t.Fatal("OAuth locator duplicated into payload")
	}
	if strings.Contains(string(config.Payload), `"name"`) {
		t.Fatalf("config duplicated definition name: %s", config.Payload)
	}
	if strings.Contains(string(config.CredentialRefs), "MCP_TOKEN_") || !strings.Contains(string(config.CredentialRefs), "MCP_OAUTH_CLIENT_0198F9A4_1B2C_7DEF_8123_456789ABCDEF") {
		t.Fatalf("credential boundary broken: payload=%s refs=%s", config.Payload, config.CredentialRefs)
	}
}

func TestNormalizeLegacyMCPRejectsNonDerivedCredentialLocators(t *testing.T) {
	base := LegacyMCPRegistration{
		ID: "0198f9a4-1b2c-7def-8123-456789abcdef", Scope: string(ScopeSystem),
		Name: "github", URL: "https://mcp.example.test", Transport: "sse", Enabled: true,
	}
	for name, row := range map[string]LegacyMCPRegistration{
		"bearer": func() LegacyMCPRegistration {
			row := base
			row.AuthType, row.CredentialRef = legacyMCPAuthBearer, "MCP_TOKEN_wrong"
			return row
		}(),
		"oauth metadata": func() LegacyMCPRegistration {
			row := base
			row.AuthType = legacyMCPAuthOAuth
			row.Metadata = map[string]any{"oauth": map[string]any{"client_secret_ref": "MCP_OAUTH_CLIENT_wrong"}}
			return row
		}(),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := NormalizeLegacySnapshot(LegacySnapshot{MCP: []LegacyMCPRegistration{row}}, NewCatalog(), nil)
			if !errors.Is(err, ErrLegacyMigrationConflict) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestNormalizeLegacyMCPDoesNotInventAbsentOAuthSecret(t *testing.T) {
	row := LegacyMCPRegistration{
		ID: "0198f9a4-1b2c-7def-8123-456789abcdef", Scope: string(ScopeSystem),
		Name: "github", URL: "https://mcp.example.test", Transport: "sse", AuthType: legacyMCPAuthOAuth,
		Metadata: map[string]any{"oauth": map[string]any{"client_id": "client-1"}}, Enabled: true,
	}
	plan, err := NormalizeLegacySnapshot(LegacySnapshot{MCP: []LegacyMCPRegistration{row}}, NewCatalog(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(plan.Configs[0].CredentialRefs), "oauth_client_secret") {
		t.Fatalf("invented absent OAuth secret locator: %s", plan.Configs[0].CredentialRefs)
	}
}

func TestNormalizeLegacyMCPRejectsSecretWithoutClientID(t *testing.T) {
	row := LegacyMCPRegistration{
		ID: "0198f9a4-1b2c-7def-8123-456789abcdef", Scope: string(ScopeSystem),
		Name: "github", URL: "https://mcp.example.test", Transport: "sse", AuthType: legacyMCPAuthOAuth,
		Metadata: map[string]any{"oauth": map[string]any{}}, OAuthClientSecretExists: true, Enabled: true,
	}
	_, err := NormalizeLegacySnapshot(LegacySnapshot{MCP: []LegacyMCPRegistration{row}}, NewCatalog(), nil)
	if !errors.Is(err, ErrLegacyMigrationConflict) || !strings.Contains(err.Error(), "client_id") {
		t.Fatalf("OAuth secret without client_id error = %v", err)
	}
}

func TestNormalizeLegacyRejectsNamespaceAndPayloadCollisions(t *testing.T) {
	_, err := NormalizeLegacySnapshot(LegacySnapshot{MCP: []LegacyMCPRegistration{
		{ID: "0198f9a4-1b2c-7def-8123-456789abcdea", Scope: string(ScopeSystem), Name: "foo-bar", URL: "https://one.test", Transport: "sse", AuthType: "none", Enabled: true},
		{ID: "0198f9a4-1b2c-7def-8123-456789abcdeb", Scope: string(ScopeSystem), Name: "foo_bar", URL: "https://two.test", Transport: "sse", AuthType: "none", Enabled: true},
	}}, NewCatalog(), nil)
	if !errors.Is(err, ErrLegacyMigrationConflict) {
		t.Fatalf("namespace collision error = %v", err)
	}

	def := Definition{ID: "tool/test", Namespace: "test", DisplayName: "Test", Backend: BackendCLI, Source: SourceBuiltin, ImplementationKey: "tool/test", Spec: json.RawMessage(`{"name":"test"}`), DefaultEnabled: true, Revision: 1}
	catalog := NewCatalog()
	if err := catalog.Register(def); err != nil {
		t.Fatal(err)
	}
	_, err = NormalizeLegacySnapshot(LegacySnapshot{
		Plugins:           []LegacyPlugin{{ID: def.ID, Enabled: true, Config: json.RawMessage(`{"same":1}`)}},
		ManifestOverrides: []LegacyManifestOverride{{PluginID: def.ID, Config: `{"$sparse":true,"same":2}`}},
	}, catalog, nil)
	if !errors.Is(err, ErrLegacyMigrationConflict) {
		t.Fatalf("payload collision error = %v", err)
	}
}

func TestNormalizeLegacyRejectsUnsupportedMCPMetadata(t *testing.T) {
	_, err := NormalizeLegacySnapshot(LegacySnapshot{MCP: []LegacyMCPRegistration{{
		ID: "0198f9a4-1b2c-7def-8123-456789abcdef", Scope: string(ScopeSystem), Name: "github", URL: "https://mcp.example.test", Transport: "sse", AuthType: "none", Enabled: true,
		Metadata: map[string]any{"token": "secret"},
	}}}, NewCatalog(), nil)
	if !errors.Is(err, ErrLegacyMigrationConflict) || !strings.Contains(err.Error(), "metadata") {
		t.Fatalf("unsupported metadata error = %v", err)
	}
}

func TestNormalizeLegacyRejectsManifestIdentityChange(t *testing.T) {
	def := Definition{ID: "tool/test", Namespace: "test", DisplayName: "Test", Backend: BackendCLI, Source: SourceBuiltin, ImplementationKey: "tool/test", Spec: json.RawMessage(`{}`), DefaultEnabled: true, Revision: 1}
	catalog := NewCatalog()
	if err := catalog.Register(def); err != nil {
		t.Fatal(err)
	}
	_, err := NormalizeLegacySnapshot(LegacySnapshot{ManifestOverrides: []LegacyManifestOverride{
		{PluginID: def.ID, Config: `{"$sparse":true,"display_name":"Other"}`},
	}}, catalog, nil)
	if !errors.Is(err, ErrLegacyMigrationConflict) || !strings.Contains(err.Error(), "display_name") {
		t.Fatalf("identity override error = %v", err)
	}
}

func TestNormalizeLegacyRejectsLiteralSessionEnv(t *testing.T) {
	def := Definition{ID: "tool/test", Namespace: "test", DisplayName: "Test", Backend: BackendCLI, Source: SourceBuiltin, ImplementationKey: "tool/test", Spec: json.RawMessage(`{}`), DefaultEnabled: true, Revision: 1}
	catalog := NewCatalog()
	if err := catalog.Register(def); err != nil {
		t.Fatal(err)
	}
	_, err := NormalizeLegacySnapshot(LegacySnapshot{ManifestOverrides: []LegacyManifestOverride{
		{PluginID: def.ID, Config: `{"$sparse":true,"session_env":[{"env_var":"TOKEN","source":"literal","value":"secret"}]}`},
	}}, catalog, nil)
	if !errors.Is(err, ErrLegacyMigrationConflict) || !strings.Contains(err.Error(), "session_env") {
		t.Fatalf("literal session env error = %v", err)
	}
}

func TestNormalizeLegacyMapsCustomCLIToStableCustomIdentity(t *testing.T) {
	oldID := "tool/private-cli"
	plan, err := NormalizeLegacySnapshot(LegacySnapshot{
		ManifestOverrides: []LegacyManifestOverride{{PluginID: oldID, Enabled: importBoolPtr(true), Config: `{"name":"private-cli","display_name":"Private CLI","prompt":"use it"}`}},
		Plugins:           []LegacyPlugin{{ID: oldID, Enabled: true, Config: json.RawMessage(`{"version":"1"}`)}},
	}, NewCatalog(), nil)
	if err != nil {
		t.Fatal(err)
	}
	wantID := legacyCustomDefinitionID(oldID)
	if len(plan.Definitions) != 1 || plan.Definitions[0].ID != wantID || len(plan.Configs) != 1 || plan.Configs[0].PluginID != wantID || plan.Configs[0].ID != strings.TrimPrefix(wantID, "custom/") {
		t.Fatalf("custom identity mapping = %#v / %#v", plan.Definitions, plan.Configs)
	}
}

func TestNormalizeLegacyAllowsSameNamespaceForDifferentOwners(t *testing.T) {
	plan, err := NormalizeLegacySnapshot(LegacySnapshot{MCP: []LegacyMCPRegistration{
		{ID: "0198f9a4-1b2c-7def-8123-456789abcdea", Scope: string(ScopeUser), UserID: "user-a", Name: "git-hub", URL: "https://one.test", Transport: "sse", AuthType: "none", Enabled: true},
		{ID: "0198f9a4-1b2c-7def-8123-456789abcdeb", Scope: string(ScopeUser), UserID: "user-b", Name: "git hub", URL: "https://two.test", Transport: "sse", AuthType: "none", Enabled: true},
	}}, NewCatalog(), nil)
	if err != nil || len(plan.Configs) != 2 {
		t.Fatalf("different-owner namespace result = %#v, err=%v", plan.Configs, err)
	}
}

func TestConvertLegacyToolOverrideResolvesExactMCPRegistration(t *testing.T) {
	registration := LegacyMCPRegistration{ID: "0198f9a4-1b2c-7def-8123-456789abcdef", Scope: string(ScopeUser), UserID: "user-1", Name: "GitHub", Tools: json.RawMessage(`[{"name":"create-issue"}]`)}
	migration, err := ConvertLegacyToolOverride(LegacyToolOverride{ID: "override-1", ToolName: "mcp__GitHub__create_issue", Scope: string(ScopeUser), UserID: "user-1", Enabled: false}, []LegacyMCPRegistration{registration}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if migration.NewName != "GitHub__create_issue" || migration.ConfigID != registration.ID || migration.Enabled {
		t.Fatalf("migration = %#v", migration)
	}
}

func importBoolPtr(value bool) *bool { return &value }

func testLegacyToolMetadata() *toolmeta.Registry {
	return toolmeta.NewRegistry(
		toolmeta.ActionTool{Name: "email__message_send", PluginID: "system/email", Namespace: "email", LocalName: "message_send", Family: "email"},
		toolmeta.ActionTool{Name: "recally__entry_add", PluginID: "system/recally", Namespace: "recally", LocalName: "entry_add", Family: "recally"},
		toolmeta.ActionTool{Name: "scheduler__job_create", PluginID: "system/scheduler", Namespace: "scheduler", LocalName: "job_create", Family: "scheduler"},
	)
}

func TestPreviewIncludesShippedNamespaceClaimsWithoutInventingConfigIDs(t *testing.T) {
	catalog := NewCatalog()
	if err := catalog.Register(testDefinition()); err != nil {
		t.Fatal(err)
	}
	registration := LegacyMCPRegistration{
		ID: "0198f9a4-1b2c-7def-8123-456789abcdef", Scope: string(ScopeSystem),
		Name: "test", URL: "https://mcp.example.test", Transport: "sse", AuthType: "none", Enabled: true,
	}
	if _, err := NormalizeLegacySnapshot(LegacySnapshot{MCP: []LegacyMCPRegistration{registration}}, catalog, nil); !errors.Is(err, ErrLegacyMigrationConflict) {
		t.Fatalf("unconfigured builtin namespace collision = %v", err)
	}
	plan, err := NormalizeLegacySnapshot(LegacySnapshot{}, catalog, nil)
	if err != nil || len(plan.Configs) != 0 {
		t.Fatalf("default claim fabricated config identity: %#v, %v", plan.Configs, err)
	}
	registration.Scope, registration.UserID = string(ScopeUser), "user"
	if _, err := NormalizeLegacySnapshot(LegacySnapshot{MCP: []LegacyMCPRegistration{registration}}, catalog, nil); err != nil {
		t.Fatalf("different scope namespace claim denied: %v", err)
	}
	registration.Name = "git--hub"
	if _, err := NormalizeLegacySnapshot(LegacySnapshot{MCP: []LegacyMCPRegistration{registration}}, catalog, nil); err == nil {
		t.Fatal("invalid normalized namespace accepted without a discovered tool catalog")
	}
}

func TestToolOverrideMigrationHandlesSharedRegistrationAndGoPlugins(t *testing.T) {
	registration := LegacyMCPRegistration{ID: "0198f9a4-1b2c-7def-8123-456789abcdef", Scope: "system", Name: "shared", Tools: json.RawMessage(`[{"name":"read"}]`)}
	override := LegacyToolOverride{ID: "policy", ToolName: "mcp__shared__read", Scope: "user", UserID: "user"}
	mapped, err := ConvertLegacyToolOverride(override, []LegacyMCPRegistration{registration}, nil)
	if err != nil || mapped.PluginID != "custom/"+registration.ID || mapped.UserID != "user" || mapped.Enabled {
		t.Fatalf("shared deny mapping = %#v, %v", mapped, err)
	}
	private := registration
	private.ID, private.Scope, private.UserID = "0198f9a4-1b2c-7def-8123-456789abcdea", "user", "user"
	if _, err := ConvertLegacyToolOverride(override, []LegacyMCPRegistration{registration, private}, nil); !errors.Is(err, ErrLegacyMigrationConflict) {
		t.Fatalf("ambiguous policy mapping = %v", err)
	}
	for _, name := range []string{"email_message_send", "recally_entry_add", "scheduler_job_create"} {
		override.ToolName = name
		mapped, err := ConvertLegacyToolOverride(override, nil, testLegacyToolMetadata())
		if err != nil || mapped.PluginID == "" || mapped.Enabled || !strings.Contains(mapped.NewName, "__") {
			t.Fatalf("Go plugin policy %s = %#v, %v", name, mapped, err)
		}
	}
	override.ToolName = "email_impostor"
	mapped, err = ConvertLegacyToolOverride(override, nil, testLegacyToolMetadata())
	if err != nil || mapped.PluginID != "" {
		t.Fatal("a prefix invented a plugin-owned tool")
	}
}

func TestNormalizeLegacyChannelMigratesOnlyPlatformCapability(t *testing.T) {
	const secret = "fakeappsecret"
	legacyConfig := json.RawMessage(`{"app_id":"fake-app","app_secret":"fakeappsecret"}`)
	snapshot := LegacySnapshot{
		Plugins:  []LegacyPlugin{{ID: "channel/feishu", Enabled: false, Config: legacyConfig}},
		Channels: []LegacyChannel{{ID: "feishu-work", Type: "feishu"}},
	}
	originalConfig := append(json.RawMessage(nil), snapshot.Plugins[0].Config...)
	catalog := NewCatalog()
	if err := catalog.Register(testChannelDefinition("feishu")); err != nil {
		t.Fatal(err)
	}

	plan, err := NormalizeLegacySnapshot(snapshot, catalog, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Configs) != 1 {
		t.Fatalf("channel config count = %d", len(plan.Configs))
	}
	config := plan.Configs[0]
	if config.PluginID != "channel/feishu" || config.Enabled == nil || *config.Enabled {
		t.Fatalf("channel capability = %#v", config)
	}
	if string(config.Payload) != `{}` || len(config.CredentialRefs) != 0 {
		t.Fatalf("channel config copied legacy material: payload=%s refs=%s", config.Payload, config.CredentialRefs)
	}
	definitionBytes, err := json.Marshal(plan.Definitions)
	if err != nil {
		t.Fatal(err)
	}
	configBytes, err := json.Marshal(plan.Configs)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(definitionBytes), secret) || strings.Contains(string(configBytes), secret) {
		t.Fatal("channel secret escaped into target definition or config")
	}
	if string(snapshot.Plugins[0].Config) != string(originalConfig) {
		t.Fatal("legacy channel mirror mutated")
	}
}

func TestNormalizeLegacyChannelRejectsPayloadWithoutInstance(t *testing.T) {
	const secret = "fakeappsecret"
	catalog := NewCatalog()
	if err := catalog.Register(testChannelDefinition("feishu")); err != nil {
		t.Fatal(err)
	}
	_, err := NormalizeLegacySnapshot(LegacySnapshot{Plugins: []LegacyPlugin{{
		ID: "channel/feishu", Enabled: true, Config: json.RawMessage(`{"app_secret":"fakeappsecret"}`),
	}}}, catalog, nil)
	if !errors.Is(err, ErrLegacyMigrationConflict) || !strings.Contains(err.Error(), "channel/feishu") {
		t.Fatalf("missing channel instance error = %v", err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatal("migration error exposed channel secret")
	}
}

func TestNormalizeLegacyChannelAllowsEmptyPayloadWithoutInstance(t *testing.T) {
	catalog := NewCatalog()
	if err := catalog.Register(testChannelDefinition("feishu")); err != nil {
		t.Fatal(err)
	}
	plan, err := NormalizeLegacySnapshot(LegacySnapshot{Plugins: []LegacyPlugin{{
		ID: "channel/feishu", Enabled: false, Config: json.RawMessage(`{}`),
	}}}, catalog, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Configs) != 1 || plan.Configs[0].Enabled == nil || *plan.Configs[0].Enabled || string(plan.Configs[0].Payload) != `{}` {
		t.Fatalf("empty channel capability = %#v", plan.Configs)
	}
}

func testChannelDefinition(channelType string) Definition {
	return Definition{
		ID: "channel/" + channelType, Namespace: channelType, DisplayName: channelType,
		Backend: BackendGo, Source: SourceBuiltin, ImplementationKey: "channel/" + channelType,
		Spec: json.RawMessage(`{}`), DefaultEnabled: true, Revision: 1,
	}
}
