package mcp

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/CherryHQ/stella/internal/authz"
	"github.com/CherryHQ/stella/internal/plugin"
)

func TestMoveConfigScopeAuthNonePreservesIDAndUsesTargetOwner(t *testing.T) {
	svc, _, userID, agentID := setupInternal(t)
	svc.bindVault = nil
	svc.vault = nil
	configID, pluginID, _ := seedCommonConfig(t, svc.pool, userID, 7, AuthTypeNone)
	authority := mustMoveAuthority(t, userID, true)

	reg, err := svc.MoveConfigScope(t.Context(), authority, ScopeMoveRequest{
		PluginID: pluginID, ConfigID: configID, ExpectedRevision: 7,
		TargetScope: plugin.ScopeUserAgent, TargetAgentID: agentID,
	})
	if err != nil {
		t.Fatalf("MoveConfigScope: %v", err)
	}
	if reg.ID != configID || reg.Scope != ScopeUserAgent || reg.UserID != userID || reg.AgentID != agentID {
		t.Fatalf("moved registration = %#v", reg)
	}
	assertMoveRow(t, svc, configID, string(plugin.ScopeUserAgent), userID, agentID, 8)
}

func TestMoveConfigScopeAuthNoneCleansHistoricalCredentialFamily(t *testing.T) {
	svc, _, userID, agentID := setupInternal(t)
	// Cleanup is keyed by the locked config UUID and does not need an active
	// vault key. This also proves auth-none moves work without a vault binding.
	svc.bindVault = nil
	svc.vault = nil
	configID, pluginID, _ := seedCommonConfig(t, svc.pool, userID, 7, AuthTypeNone)
	for _, prefix := range []string{"MCP_TOKEN_", "MCP_OAUTH_", "MCP_OAUTH_CLIENT_"} {
		name := prefix + strings.ToUpper(strings.ReplaceAll(configID, "-", "_"))
		if _, err := svc.pool.Exec(t.Context(), `INSERT INTO vault_entry (id, scope, user_id, name, ciphertext) VALUES ($1, 'user', $2, $3, 'historical')`, uuid.NewString(), userID, name); err != nil {
			t.Fatal(err)
		}
	}
	authority := mustMoveAuthority(t, userID, true)
	if _, err := svc.MoveConfigScope(t.Context(), authority, ScopeMoveRequest{
		PluginID: pluginID, ConfigID: configID, ExpectedRevision: 7,
		TargetScope: plugin.ScopeUserAgent, TargetAgentID: agentID,
	}); err != nil {
		t.Fatalf("MoveConfigScope: %v", err)
	}
	var remaining int
	if err := svc.pool.QueryRow(t.Context(), `SELECT count(*) FROM vault_entry WHERE name IN ($1, $2, $3)`,
		"MCP_TOKEN_"+strings.ToUpper(strings.ReplaceAll(configID, "-", "_")),
		"MCP_OAUTH_"+strings.ToUpper(strings.ReplaceAll(configID, "-", "_")),
		"MCP_OAUTH_CLIENT_"+strings.ToUpper(strings.ReplaceAll(configID, "-", "_"))).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 0 {
		t.Fatalf("historical credential family rows = %d, want 0", remaining)
	}
}

func TestMoveConfigScopeBearerReplacesExactVaultTuple(t *testing.T) {
	svc, _, userID, agentID := setupInternal(t)
	configID, pluginID, _ := seedCommonConfig(t, svc.pool, userID, 7, AuthTypeBearer)
	seedBearerMoveRefs(t, svc, configID, plugin.ScopeUser, userID, "")
	if err := svc.vault.SetScoped(t.Context(), ScopeUser, userID, "", credentialName(configID), "old-token"); err != nil {
		t.Fatal(err)
	}
	authority := mustMoveAuthority(t, userID, true)

	reg, err := svc.MoveConfigScope(t.Context(), authority, ScopeMoveRequest{
		PluginID: pluginID, ConfigID: configID, ExpectedRevision: 7,
		TargetScope: plugin.ScopeUserAgent, TargetAgentID: agentID,
		Replacement: strPtr("new-token"),
	})
	if err != nil {
		t.Fatalf("MoveConfigScope: %v", err)
	}
	if reg.ID != configID || reg.Scope != ScopeUserAgent || reg.ConfigRevision != 8 {
		t.Fatalf("moved registration = %#v", reg)
	}
	if got, err := svc.vault.GetScoped(t.Context(), ScopeUser, userID, "", credentialName(configID)); err == nil && got != "" {
		t.Fatalf("old bearer tuple still contains %q", got)
	}
	if got, err := svc.vault.GetScoped(t.Context(), ScopeUserAgent, userID, agentID, credentialName(configID)); err != nil || got != "new-token" {
		t.Fatalf("target bearer = %q, err = %v", got, err)
	}
}

func TestMoveConfigScopeBearerRequiresReplacementWithoutMutation(t *testing.T) {
	svc, _, userID, agentID := setupInternal(t)
	configID, pluginID, _ := seedCommonConfig(t, svc.pool, userID, 7, AuthTypeBearer)
	seedBearerMoveRefs(t, svc, configID, plugin.ScopeUser, userID, "")
	if err := svc.vault.SetScoped(t.Context(), ScopeUser, userID, "", credentialName(configID), "old-token"); err != nil {
		t.Fatal(err)
	}
	authority := mustMoveAuthority(t, userID, true)

	_, err := svc.MoveConfigScope(t.Context(), authority, ScopeMoveRequest{
		PluginID: pluginID, ConfigID: configID, ExpectedRevision: 7,
		TargetScope: plugin.ScopeUserAgent, TargetAgentID: agentID,
	})
	if !errors.Is(err, ErrScopeMoveBearerReplacement) {
		t.Fatalf("missing replacement error = %v, want %v", err, ErrScopeMoveBearerReplacement)
	}
	assertMoveRow(t, svc, configID, string(plugin.ScopeUser), userID, "", 7)
	if got, err := svc.vault.GetScoped(t.Context(), ScopeUser, userID, "", credentialName(configID)); err != nil || got != "old-token" {
		t.Fatalf("old bearer after rejected move = %q, err = %v", got, err)
	}
}

func TestMoveConfigScopeBearerVaultFailureRollsBackConfigAndOldToken(t *testing.T) {
	svc, _, userID, agentID := setupInternal(t)
	configID, pluginID, _ := seedCommonConfig(t, svc.pool, userID, 7, AuthTypeBearer)
	seedBearerMoveRefs(t, svc, configID, plugin.ScopeUser, userID, "")
	if err := svc.vault.SetScoped(t.Context(), ScopeUser, userID, "", credentialName(configID), "old-token"); err != nil {
		t.Fatal(err)
	}
	previousBind := svc.bindVault
	svc.bindVault = func(tx pgx.Tx) Vault { return failingScopeMoveVault{Vault: previousBind(tx)} }
	authority := mustMoveAuthority(t, userID, true)

	_, err := svc.MoveConfigScope(t.Context(), authority, ScopeMoveRequest{
		PluginID: pluginID, ConfigID: configID, ExpectedRevision: 7,
		TargetScope: plugin.ScopeUserAgent, TargetAgentID: agentID,
		Replacement: strPtr("new-token"),
	})
	if err == nil {
		t.Fatal("vault failure must reject the move")
	}
	assertMoveRow(t, svc, configID, string(plugin.ScopeUser), userID, "", 7)
	if got, err := svc.vault.GetScoped(t.Context(), ScopeUser, userID, "", credentialName(configID)); err != nil || got != "old-token" {
		t.Fatalf("old bearer after rollback = %q, err = %v", got, err)
	}
}

func TestMoveConfigScopeRejectsStaleCASAndUnauthorizedTarget(t *testing.T) {
	t.Run("stale CAS", func(t *testing.T) {
		svc, _, userID, agentID := setupInternal(t)
		configID, pluginID, _ := seedCommonConfig(t, svc.pool, userID, 7, AuthTypeNone)
		authority := mustMoveAuthority(t, userID, true)
		_, err := svc.MoveConfigScope(t.Context(), authority, ScopeMoveRequest{
			PluginID: pluginID, ConfigID: configID, ExpectedRevision: 6,
			TargetScope: plugin.ScopeUserAgent, TargetAgentID: agentID,
		})
		if !errors.Is(err, plugin.ErrConflict) {
			t.Fatalf("stale CAS error = %v, want %v", err, plugin.ErrConflict)
		}
		assertMoveRow(t, svc, configID, string(plugin.ScopeUser), userID, "", 7)
	})

	t.Run("unauthorized target", func(t *testing.T) {
		svc, _, userID, _ := setupInternal(t)
		configID, pluginID, _ := seedCommonConfig(t, svc.pool, userID, 7, AuthTypeNone)
		authority := mustMoveAuthority(t, userID, false)
		_, err := svc.MoveConfigScope(t.Context(), authority, ScopeMoveRequest{
			PluginID: pluginID, ConfigID: configID, ExpectedRevision: 7,
			TargetScope: plugin.ScopeSystem,
		})
		if !errors.Is(err, authz.ErrForbidden) {
			t.Fatalf("unauthorized target error = %v, want %v", err, authz.ErrForbidden)
		}
		assertMoveRow(t, svc, configID, string(plugin.ScopeUser), userID, "", 7)
	})
}

func TestMoveConfigScopeRejectsOAuthBeforeMutation(t *testing.T) {
	svc, _, userID, agentID := setupInternal(t)
	configID, pluginID, _ := seedCommonConfig(t, svc.pool, userID, 7, AuthTypeOAuth)
	authority := mustMoveAuthority(t, userID, true)

	_, err := svc.MoveConfigScope(t.Context(), authority, ScopeMoveRequest{
		PluginID: pluginID, ConfigID: configID, ExpectedRevision: 7,
		TargetScope: plugin.ScopeUserAgent, TargetAgentID: agentID,
	})
	if !errors.Is(err, ErrScopeMoveOAuth) {
		t.Fatalf("OAuth move error = %v, want %v", err, ErrScopeMoveOAuth)
	}
	assertMoveRow(t, svc, configID, string(plugin.ScopeUser), userID, "", 7)
}

func TestUpdateAndUpdateIfVersionReachCommonScopeMove(t *testing.T) {
	svc, _, userID, agentID := setupInternal(t)
	svc.bindVault = nil
	svc.vault = nil
	configID, _, _ := seedCommonConfig(t, svc.pool, userID, 7, AuthTypeNone)
	authority := mustMoveAuthority(t, userID, true)
	ctx := authz.WithAuthority(context.Background(), authority)
	before, err := svc.Get(ctx, configID, ScopeUser, userID, "")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	newURL := "https://mcp-stale-version.example.test"
	if _, err := svc.Update(ctx, UpdateInput{ID: configID, Scope: ScopeUser, UserID: userID, URL: &newURL, ExpectedVersion: before.Version()}); err != nil {
		t.Fatalf("prepare stale version: %v", err)
	}
	if _, err := svc.UpdateIfVersion(ctx, UpdateInput{
		ID: configID, Scope: ScopeUser, UserID: userID,
		NewScope: strPtr(ScopeUserAgent), NewUserID: userID, NewAgentID: agentID,
	}, before.Version()); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("stale public scope move error = %v, want %v", err, ErrVersionConflict)
	}
	before, err = svc.Get(ctx, configID, ScopeUser, userID, "")
	if err != nil {
		t.Fatalf("Get after stale check: %v", err)
	}

	updated, err := svc.UpdateIfVersion(ctx, UpdateInput{
		ID: configID, Scope: ScopeUser, UserID: userID,
		NewScope: strPtr(ScopeUserAgent), NewUserID: userID, NewAgentID: agentID,
	}, before.Version())
	if err != nil {
		t.Fatalf("UpdateIfVersion scope move: %v", err)
	}
	if updated.ID != configID || updated.Scope != ScopeUserAgent || updated.AgentID != agentID {
		t.Fatalf("UpdateIfVersion result = %#v", updated)
	}
}

func TestUpdateMovesBearerWithReplacement(t *testing.T) {
	svc, _, userID, agentID := setupInternal(t)
	configID, _, _ := seedCommonConfig(t, svc.pool, userID, 7, AuthTypeBearer)
	seedBearerMoveRefs(t, svc, configID, plugin.ScopeUser, userID, "")
	if err := svc.vault.SetScoped(t.Context(), ScopeUser, userID, "", credentialName(configID), "old-token"); err != nil {
		t.Fatal(err)
	}
	authority := mustMoveAuthority(t, userID, true)
	ctx := authz.WithAuthority(context.Background(), authority)
	before, err := svc.Get(ctx, configID, ScopeUser, userID, "")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	updated, err := svc.Update(ctx, UpdateInput{
		ID: configID, Scope: ScopeUser, UserID: userID,
		NewScope: strPtr(ScopeUserAgent), NewUserID: userID, NewAgentID: agentID,
		Token: strPtr("new-token"), ExpectedVersion: before.Version(),
	})
	if err != nil {
		t.Fatalf("Update bearer scope move: %v", err)
	}
	if updated.Scope != ScopeUserAgent || updated.AgentID != agentID {
		t.Fatalf("Update bearer result = %#v", updated)
	}
	if got, err := svc.vault.GetScoped(t.Context(), ScopeUserAgent, userID, agentID, credentialName(configID)); err != nil || got != "new-token" {
		t.Fatalf("target bearer = %q, err = %v", got, err)
	}
}

func TestUpdateScopeMoveRejectsSpoofedOwnerAndCombinedEdits(t *testing.T) {
	svc, _, userID, agentID := setupInternal(t)
	svc.bindVault = nil
	svc.vault = nil
	configID, _, _ := seedCommonConfig(t, svc.pool, userID, 7, AuthTypeNone)
	authority := mustMoveAuthority(t, userID, true)
	ctx := authz.WithAuthority(context.Background(), authority)
	before, err := svc.Get(ctx, configID, ScopeUser, userID, "")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if _, err := svc.Update(ctx, UpdateInput{
		ID: configID, Scope: ScopeUser, UserID: userID,
		NewScope: strPtr(ScopeUserAgent), NewUserID: "spoofed-user", NewAgentID: agentID,
	}); !errors.Is(err, authz.ErrForbidden) {
		t.Fatalf("spoofed target owner error = %v, want %v", err, authz.ErrForbidden)
	}
	name := "silently-dropped-name"
	if _, err := svc.Update(ctx, UpdateInput{
		ID: configID, Scope: ScopeUser, UserID: userID,
		NewScope: strPtr(ScopeUserAgent), NewUserID: userID, NewAgentID: agentID,
		Name: &name, ExpectedVersion: before.Version(),
	}); !errors.Is(err, ErrScopeMoveCombinedUpdate) {
		t.Fatalf("combined scope update error = %v, want %v", err, ErrScopeMoveCombinedUpdate)
	}
	if _, err := svc.Update(ctx, UpdateInput{
		ID: configID, Scope: ScopeUserAgent, UserID: userID,
		NewScope: strPtr(ScopeUserAgent), NewUserID: userID, NewAgentID: agentID,
	}); !errors.Is(err, authz.ErrNotFound) {
		t.Fatalf("source scope mismatch error = %v, want %v", err, authz.ErrNotFound)
	}
	assertMoveRow(t, svc, configID, string(plugin.ScopeUser), userID, "", 7)
}

func mustMoveAuthority(t *testing.T, userID string, admin bool) authz.Authority {
	t.Helper()
	authority, err := authz.NewUserAuthority(authz.UserID(userID), admin)
	if err != nil {
		t.Fatal(err)
	}
	return authority
}

func seedBearerMoveRefs(t *testing.T, svc *Service, configID string, scope plugin.Scope, userID, agentID string) {
	t.Helper()
	refs := fmt.Sprintf(`{"bearer":{"name":"%s","scope":"%s","user_id":"%s","agent_id":"%s"}}`, credentialName(configID), scope, userID, agentID)
	if _, err := svc.pool.Exec(context.Background(), `UPDATE plugin_config SET credential_refs = $1::jsonb WHERE id = $2::uuid`, refs, configID); err != nil {
		t.Fatal(err)
	}
}

func assertMoveRow(t *testing.T, svc *Service, configID, scope, userID, agentID string, revision int64) {
	t.Helper()
	var gotScope, gotUser, gotAgent string
	var gotRevision int64
	if err := svc.pool.QueryRow(context.Background(), `SELECT scope, COALESCE(user_id::text, ''), COALESCE(agent_id, ''), revision FROM plugin_config WHERE id = $1::uuid`, configID).Scan(&gotScope, &gotUser, &gotAgent, &gotRevision); err != nil {
		t.Fatal(err)
	}
	if gotScope != scope || gotUser != userID || gotAgent != agentID || gotRevision != revision {
		t.Fatalf("config row = (%q,%q,%q,%d), want (%q,%q,%q,%d)", gotScope, gotUser, gotAgent, gotRevision, scope, userID, agentID, revision)
	}
}

type failingScopeMoveVault struct{ Vault }

func (failingScopeMoveVault) SetScoped(context.Context, string, string, string, string, string) error {
	return errors.New("scope move test vault write failure")
}

func (failingScopeMoveVault) SetSystemScoped(context.Context, string, string, string, string) error {
	return errors.New("scope move test vault write failure")
}
