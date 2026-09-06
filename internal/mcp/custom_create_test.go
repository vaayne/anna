package mcp

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/CherryHQ/stella/internal/authz"
	"github.com/CherryHQ/stella/internal/plugin"
)

func customMCPDefinition(name string) plugin.Definition {
	return plugin.Definition{
		Namespace:   "custom_mcp_resource",
		DisplayName: name,
		Backend:     plugin.BackendMCP,
		Spec:        []byte(`{"description":"custom MCP"}`),
	}
}

func TestCreateCustomMCPUsesSharedUUIDAndStoresBearerInSameMutation(t *testing.T) {
	svc, _, userID, _ := setupInternal(t)
	authority, err := authz.NewUserAuthority(authz.UserID(userID), false)
	if err != nil {
		t.Fatal(err)
	}
	ctx := authz.WithAuthority(context.Background(), authority)
	def, cfg, err := svc.CreateCustom(ctx, customMCPDefinition("custom bearer"), CreateInput{
		Scope: ScopeUser, Name: "custom bearer", URL: "https://mcp.example.test",
		Transport: TransportStreamableHTTP, AuthType: AuthTypeBearer, Token: "bearer-secret",
	})
	if err != nil {
		t.Fatalf("CreateCustom: %v", err)
	}
	if def.Source != plugin.SourceCustom || def.ID != "custom/"+cfg.ID {
		t.Fatalf("custom identity = definition %q/config %q, want shared UUID", def.ID, cfg.ID)
	}
	if _, err := uuid.Parse(cfg.ID); err != nil {
		t.Fatalf("config id is not UUID: %v", err)
	}
	if got, err := svc.vault.GetScoped(t.Context(), ScopeUser, userID, "", credentialName(cfg.ID)); err != nil || got != "bearer-secret" {
		t.Fatalf("stored bearer = %q, err=%v", got, err)
	}
	var legacyCount, configCount int
	if err := svc.pool.QueryRow(t.Context(), `SELECT count(*) FROM mcp_server WHERE id = $1::uuid`, cfg.ID).Scan(&legacyCount); err != nil {
		t.Fatalf("read legacy row: %v", err)
	}
	if err := svc.pool.QueryRow(t.Context(), `SELECT count(*) FROM plugin_config WHERE id = $1::uuid`, cfg.ID).Scan(&configCount); err != nil {
		t.Fatalf("read common config row: %v", err)
	}
	if legacyCount != 0 || configCount != 1 {
		t.Fatalf("rows = legacy %d/common %d, want 0/1", legacyCount, configCount)
	}
}

func TestCreateCustomMCPOAuthSecretUsesConfigOwner(t *testing.T) {
	svc, _, userID, _ := setupInternal(t)
	authority, err := authz.NewUserAuthority(authz.UserID(userID), false)
	if err != nil {
		t.Fatal(err)
	}
	ctx := authz.WithAuthority(context.Background(), authority)
	_, cfg, err := svc.CreateCustom(ctx, customMCPDefinition("custom oauth"), CreateInput{
		Scope: ScopeUser, Name: "custom oauth", URL: "https://mcp.example.test",
		Transport: TransportStreamableHTTP, AuthType: AuthTypeOAuth,
		OAuthClientID: "pre-registered-client", OAuthClientSecret: "oauth-client-secret",
	})
	if err != nil {
		t.Fatalf("CreateCustom OAuth: %v", err)
	}
	if got, err := svc.vault.GetScoped(t.Context(), ScopeUser, userID, "", oauthClientSecretName(cfg.ID)); err != nil || got != "oauth-client-secret" {
		t.Fatalf("stored OAuth client secret = %q, err=%v", got, err)
	}
}

func TestAccessCreateSettingsPathCreatesNamespaceParent(t *testing.T) {
	svc, _, userID, _ := setupInternal(t)
	authority, err := authz.NewUserAuthority(authz.UserID(userID), false)
	if err != nil {
		t.Fatal(err)
	}
	access, err := NewAccess(svc, nil, nil).Begin(authority)
	if err != nil {
		t.Fatal(err)
	}
	ctx := authz.WithAuthority(context.Background(), authority)
	result, err := (managementHandler{access: access}).Create(ctx, SettingsMcpCreateInput{
		Scope: ScopeUser, Name: "settings_server", Url: "https://mcp.example.test",
	})
	if err != nil {
		t.Fatalf("settings create adapter: %v", err)
	}
	view, ok := result.(managementView)
	if !ok {
		t.Fatalf("settings create result type = %T, want managementView", result)
	}
	reg, err := access.Get(ctx, view.ID, ScopeUser, "")
	if err != nil {
		t.Fatalf("read settings-created registration: %v", err)
	}
	if reg.Namespace != "settings_server" || reg.PluginID != "custom/"+reg.ID {
		t.Fatalf("created registration = namespace %q/plugin %q, want verbatim namespace and shared UUID", reg.Namespace, reg.PluginID)
	}
	got, err := plugin.ExportedToolName(reg.Namespace, "list")
	if err != nil {
		t.Fatalf("export settings tool name: %v", err)
	}
	if got != "settings_server__list" {
		t.Fatalf("tool namespace parent = %q", got)
	}
	if _, err := access.Create(ctx, CreateInput{Scope: ScopeUser, Name: "settings server", URL: "https://mcp.example.test"}); err == nil {
		t.Fatal("invalid namespace must be rejected instead of slugified")
	}
	if _, err := access.Create(ctx, CreateInput{Scope: ScopeUser, Name: "settings_secret", URL: "https://mcp.example.test", AuthType: AuthTypeBearer, Token: "raw-secret"}); !errors.Is(err, errPluginCredentialsUnavailable) {
		t.Fatalf("settings raw credential error = %v, want unavailable", err)
	}
}

func TestCreateCustomMCPSecretFailureRollsBackDefinitionAndConfig(t *testing.T) {
	svc, _, userID, _ := setupInternal(t)
	svc.bindVault = nil
	authority, err := authz.NewUserAuthority(authz.UserID(userID), false)
	if err != nil {
		t.Fatal(err)
	}
	ctx := authz.WithAuthority(context.Background(), authority)
	def := customMCPDefinition("rollback bearer")
	_, _, err = svc.CreateCustom(ctx, def, CreateInput{
		Scope: ScopeUser, Name: def.DisplayName, URL: "https://mcp.example.test",
		Transport: TransportStreamableHTTP, AuthType: AuthTypeBearer, Token: "must-rollback",
	})
	if !errors.Is(err, errPluginCredentialsUnavailable) {
		t.Fatalf("CreateCustom without vault error = %v, want unavailable", err)
	}
	var definitions, configs, legacy int
	if err := svc.pool.QueryRow(t.Context(), `SELECT count(*) FROM plugin_definition WHERE display_name = $1`, def.DisplayName).Scan(&definitions); err != nil {
		t.Fatalf("count rolled-back definition: %v", err)
	}
	if err := svc.pool.QueryRow(t.Context(), `SELECT count(*) FROM plugin_config WHERE namespace = $1`, def.Namespace).Scan(&configs); err != nil {
		t.Fatalf("count rolled-back config: %v", err)
	}
	if err := svc.pool.QueryRow(t.Context(), `SELECT count(*) FROM mcp_server WHERE name = $1`, def.DisplayName).Scan(&legacy); err != nil {
		t.Fatalf("count rolled-back legacy row: %v", err)
	}
	if definitions != 0 || configs != 0 || legacy != 0 {
		t.Fatalf("rows after rollback = definitions %d/configs %d/legacy %d, want 0/0/0", definitions, configs, legacy)
	}
}
