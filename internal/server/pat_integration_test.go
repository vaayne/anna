package server_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/CherryHQ/stella/internal/auth"
	"github.com/CherryHQ/stella/internal/platform/config"
	pluginpkg "github.com/CherryHQ/stella/internal/plugin"
	"github.com/CherryHQ/stella/internal/server"
)

// These integration tests drive the real server mux (middleware Resolve+Enforce
// -> generated handlers -> Postgres) for Personal Access Tokens. They cover the
// wired-up path the internal/credential unit tests cannot reach: a PAT minted
// through the create endpoint, presented as a Bearer against real routes, and
// the resulting rows in personal_access_token. The admin's legacy full-access
// bearer (env.bearerToken) creates the admin PATs that exercise the new boundary.

// mintPAT creates a PAT via POST /api/users/me/tokens using the given bearer and
// returns the one-time plaintext plus the token id.
func mintPAT(t *testing.T, env *testEnv, bearer, name string) (plaintext, id string) {
	t.Helper()
	rr := doBearerRequest(t, env.srv, bearer, http.MethodPost, "/api/users/me/tokens",
		map[string]any{"name": name})
	if rr.Code != http.StatusCreated {
		t.Fatalf("mintPAT(%s): status = %d, body = %s", name, rr.Code, rr.Body.String())
	}
	var resp struct {
		Token string `json:"token"`
		PAT   struct {
			ID    string `json:"id"`
			Last4 string `json:"last4"`
		} `json:"personal_access_token"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("mintPAT(%s): decode: %v", name, err)
	}
	if resp.Token == "" || resp.PAT.ID == "" {
		t.Fatalf("mintPAT(%s): empty token/id: %s", name, rr.Body.String())
	}
	return resp.Token, resp.PAT.ID
}

// TestPATAuthority proves PATs enter every API route with the owner's current
// authority while the handler/domain boundary keeps non-admin users constrained.
func TestPATAuthority(t *testing.T) {
	env := setupAdmin(t)
	plugins := pluginpkg.NewService(env.db, env.deps.AgentAccess, pluginpkg.NewCatalog(), pluginpkg.BackendPolicy{}, func(_ context.Context, fn func() error) error { return fn() })
	env.rebuild(t, func(d *server.Deps) { d.PluginService = plugins })
	adminPAT, _ := mintPAT(t, env, env.bearerToken, "admin_control_plane")

	cases := []struct {
		name         string
		method, path string
		want         int
	}{
		{"account identity", http.MethodGet, "/api/auth/me", http.StatusOK},
		{"admin users", http.MethodGet, "/api/users", http.StatusOK},
		{"provider control plane", http.MethodGet, "/api/providers", http.StatusOK},
		{"model control plane", http.MethodGet, "/api/models", http.StatusOK},
		{"channel control plane", http.MethodGet, "/api/channels", http.StatusOK},
		{"plugin control plane", http.MethodGet, "/api/plugins", http.StatusOK},
		{"non-api page", http.MethodGet, "/agents", http.StatusForbidden},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rr := doBearerRequest(t, env.srv, adminPAT, c.method, c.path, nil)
			if rr.Code != c.want {
				t.Fatalf("%s %s: status = %d, want %d (body %s)", c.method, c.path, rr.Code, c.want, rr.Body.String())
			}
		})
	}

	_, userBearer := createTestUserWithToken(t, env.authStore, env.oidcStore, "pat_regular", auth.RoleUser)
	userPAT, _ := mintPAT(t, env, userBearer, "regular")
	if rr := doBearerRequest(t, env.srv, userPAT, http.MethodGet, "/api/users", nil); rr.Code != http.StatusForbidden {
		t.Fatalf("normal PAT GET /api/users: want 403, got %d (%s)", rr.Code, rr.Body.String())
	}
	if rr := doBearerRequest(t, env.srv, userPAT, http.MethodGet, "/api/agents", nil); rr.Code != http.StatusOK {
		t.Fatalf("normal PAT GET /api/agents: want 200, got %d (%s)", rr.Code, rr.Body.String())
	}
	restrictedID := createTestAgent(t, env, config.Agent{
		Name:    "PAT ownership boundary",
		Model:   "anthropic/claude-sonnet-4-6",
		Scope:   config.AgentScopeRestricted,
		Enabled: true,
	})
	if rr := doBearerRequest(t, env.srv, userPAT, http.MethodGet, "/api/agents/"+restrictedID, nil); rr.Code != http.StatusForbidden {
		t.Fatalf("normal PAT GET admin-owned restricted agent: want 403, got %d (%s)", rr.Code, rr.Body.String())
	}
}

func TestProvisioningTokenCannotReadProviderEvidence(t *testing.T) {
	env := setupAdmin(t)
	providerID := "eval-evidence"
	if rr := doRequest(t, env, http.MethodPost, "/api/providers", map[string]any{
		"id": providerID, "type": "openai", "name": "Eval evidence", "enabled": true, "api_key": "sk-test", "base_url": "https://gateway.example.test/v1",
		"models": map[string]any{"gpt-test": map[string]any{"enabled": true, "cost": map[string]any{"input": 1.25, "output": 2.5}}},
	}); rr.Code != http.StatusCreated {
		t.Fatalf("create provider: status=%d body=%s", rr.Code, rr.Body.String())
	}
	provisioning := createProvisioningToken(t, env.bearerToken, env, "eval-evidence", nil)
	evidencePath := "/api/providers/" + providerID + "/evidence?model_id=gpt-test"
	if rr := doBearerRequest(t, env.srv, provisioning.Token, http.MethodGet, evidencePath, nil); rr.Code != http.StatusForbidden {
		t.Fatalf("provisioning provider read: status=%d want 403 body=%s", rr.Code, rr.Body.String())
	}
	adminPAT, _ := mintPAT(t, env, env.bearerToken, "eval-provider-evidence")
	if rr := doBearerRequest(t, env.srv, adminPAT, http.MethodGet, evidencePath, nil); rr.Code != http.StatusOK {
		t.Fatalf("admin PAT provider evidence: status=%d want 200 body=%s", rr.Code, rr.Body.String())
	} else if strings.Contains(rr.Body.String(), "api_key") || strings.Contains(rr.Body.String(), "sk-test") {
		t.Fatalf("provider evidence leaked a credential: %s", rr.Body.String())
	}
}

// TestPATCredentialRouteFence proves the PAT-specific parent fence runs before
// the generated handlers. Every request uses an admin-owned PAT: a handler-only
// authorization check could otherwise hide a missing fence behind a 403.
func TestPATCredentialRouteFence(t *testing.T) {
	env := setupAdmin(t)
	adminPAT, _ := mintPAT(t, env, env.bearerToken, "admin_credential_fence")

	for _, tc := range []struct {
		name         string
		method, path string
		body         any
	}{
		{"cannot mint never-expiring PAT", http.MethodPost, "/api/users/me/tokens", map[string]any{"name": "escape", "never_expires": true}},
		{"cannot inspect PAT", http.MethodGet, "/api/users/me/tokens/token-1", nil},
		{"cannot create OAuth client", http.MethodPost, "/api/users/me/oauth-clients", map[string]any{}},
		{"cannot rotate OAuth client secret", http.MethodPost, "/api/users/me/oauth-clients/client-1/rotate-secret", nil},
		{"cannot list authorized apps", http.MethodGet, "/api/users/me/authorized-apps", nil},
		{"cannot revoke authorized app", http.MethodDelete, "/api/users/me/authorized-apps/client-1", nil},
		{"cannot list browser sessions", http.MethodGet, "/api/auth/sessions", nil},
		{"cannot revoke browser session", http.MethodDelete, "/api/auth/sessions/session-1", nil},
		{"cannot list own identities", http.MethodGet, "/api/users/me/identities", nil},
		{"cannot unlink own identity", http.MethodDelete, "/api/users/me/identities/identity-1", nil},
		{"cannot list another user's identities", http.MethodGet, "/api/users/target-user/identities/login", nil},
		{"cannot change password", http.MethodPatch, "/api/users/me/password", map[string]any{}},
		{"cannot generate link code", http.MethodPost, "/api/users/me/link-code", map[string]any{}},
		{"cannot change role", http.MethodPatch, "/api/users/target-user/role", map[string]any{"role": "admin"}},
		{"cannot change active state", http.MethodPatch, "/api/users/target-user/active", map[string]any{"is_active": false}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rr := doBearerRequest(t, env.srv, adminPAT, tc.method, tc.path, tc.body)
			if rr.Code != http.StatusForbidden {
				t.Fatalf("%s %s: status = %d, want 403 before handler (body %s)", tc.method, tc.path, rr.Code, rr.Body.String())
			}
		})
	}
}

// TestPAT_Lifecycle covers create-time expiry policy, one-time plaintext, and
// input validation through the real handler.
func TestPAT_Lifecycle(t *testing.T) {
	env := setupAdmin(t)
	ctx := context.Background()

	// Default expiry: no expires_at/never_expires -> ~90 days.
	plaintext, id := mintPAT(t, env, env.bearerToken, "life_default")
	var exp *time.Time
	var last4 string
	var scopes []string
	if err := env.db.QueryRow(ctx,
		"select expires_at, last4, scopes from personal_access_token where id=$1", id).Scan(&exp, &last4, &scopes); err != nil {
		t.Fatalf("query default token: %v", err)
	}
	if exp == nil {
		t.Fatal("default PAT must carry an expiry")
	}
	if d := time.Until(*exp); d < 89*24*time.Hour || d > 91*24*time.Hour {
		t.Fatalf("default expiry want ~90d, got %v", d)
	}
	if last4 != plaintext[len(plaintext)-4:] {
		t.Fatalf("last4 = %q, want suffix of plaintext %q", last4, plaintext)
	}
	if len(scopes) != 0 {
		t.Fatalf("new PAT scopes = %v, want empty legacy storage value", scopes)
	}

	// Older clients may still send scopes after the field left the API contract.
	// JSON compatibility is deliberate, but the legacy column must stay empty.
	legacyCreate := doRequest(t, env, http.MethodPost, "/api/users/me/tokens",
		map[string]any{"name": "life_legacy_scopes", "scopes": []string{"goals:read"}})
	if legacyCreate.Code != http.StatusCreated {
		t.Fatalf("legacy scoped create: status = %d (%s)", legacyCreate.Code, legacyCreate.Body.String())
	}
	var legacyResp struct {
		PAT struct {
			ID string `json:"id"`
		} `json:"personal_access_token"`
	}
	if err := json.Unmarshal(legacyCreate.Body.Bytes(), &legacyResp); err != nil {
		t.Fatalf("decode legacy create: %v", err)
	}
	var legacyScopes []string
	if err := env.db.QueryRow(ctx, "select scopes from personal_access_token where id=$1", legacyResp.PAT.ID).Scan(&legacyScopes); err != nil {
		t.Fatalf("query legacy scoped token: %v", err)
	}
	if len(legacyScopes) != 0 {
		t.Fatalf("legacy scoped create stored scopes = %v, want empty", legacyScopes)
	}

	// never_expires -> NULL expiry.
	rr := doRequest(t, env, http.MethodPost, "/api/users/me/tokens",
		map[string]any{"name": "life_never", "never_expires": true})
	if rr.Code != http.StatusCreated {
		t.Fatalf("never_expires create: status = %d (%s)", rr.Code, rr.Body.String())
	}
	var neverResp struct {
		PAT struct {
			ID string `json:"id"`
		} `json:"personal_access_token"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &neverResp); err != nil {
		t.Fatalf("decode never: %v", err)
	}
	var neverExp *time.Time
	if err := env.db.QueryRow(ctx,
		"select expires_at from personal_access_token where id=$1", neverResp.PAT.ID).Scan(&neverExp); err != nil {
		t.Fatalf("query never token: %v", err)
	}
	if neverExp != nil {
		t.Fatalf("never_expires must store NULL expiry, got %v", neverExp)
	}

	// Name and expiry validation remain unchanged; obsolete scope fields are
	// ignored for compatibility with older callers.
	bad := []struct {
		name string
		body map[string]any
	}{
		{"blank name", map[string]any{"name": ""}},
		{"past expiry", map[string]any{"name": "x", "expires_at": "2020-01-01T00:00:00Z"}},
		{"beyond max lifetime", map[string]any{
			"name":       "x",
			"expires_at": time.Now().Add(400 * 24 * time.Hour).UTC().Format(time.RFC3339),
		}},
	}
	for _, b := range bad {
		t.Run(b.name, func(t *testing.T) {
			rr := doRequest(t, env, http.MethodPost, "/api/users/me/tokens", b.body)
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("want 400, got %d (%s)", rr.Code, rr.Body.String())
			}
		})
	}
}

// TestPAT_OwnershipAndRevoke proves session-only PAT management preserves
// ownership isolation (404 before existence leaks), plus token revocation and
// expiry enforcement at the resolver.
func TestPAT_OwnershipAndRevoke(t *testing.T) {
	env := setupAdmin(t)
	ctx := context.Background()

	// A second user's token remains visible only to that user's browser session.
	_, u2Session := createTestUserWithToken(t, env.authStore, env.oidcStore, "patuser2", auth.RoleUser)
	_, u2PATID := mintPAT(t, env, u2Session, "u2_tok")
	if rr := doRequestWithSession(t, env.srv, u2Session, http.MethodGet, "/api/users/me/tokens/"+u2PATID, nil); rr.Code != http.StatusOK {
		t.Fatalf("session GET own token: want 200, got %d (%s)", rr.Code, rr.Body.String())
	}
	_, otherSession := createTestUserWithToken(t, env.authStore, env.oidcStore, "patuser3", auth.RoleUser)
	if rr := doRequestWithSession(t, env.srv, otherSession, http.MethodGet, "/api/users/me/tokens/"+u2PATID, nil); rr.Code != http.StatusNotFound {
		t.Fatalf("session GET other user's token: want 404, got %d (%s)", rr.Code, rr.Body.String())
	}
	if rr := doRequestWithSession(t, env.srv, otherSession, http.MethodDelete, "/api/users/me/tokens/"+u2PATID, nil); rr.Code != http.StatusNotFound {
		t.Fatalf("session DELETE other user's token: want 404, got %d", rr.Code)
	}

	// Revoke invalidates the token at the auth boundary.
	tok, id := mintPAT(t, env, env.bearerToken, "revoke_me")
	if rr := doBearerRequest(t, env.srv, tok, http.MethodGet, "/api/agents", nil); rr.Code != http.StatusOK {
		t.Fatalf("pre-revoke agents: want 200, got %d", rr.Code)
	}
	if rr := doRequest(t, env, http.MethodDelete, "/api/users/me/tokens/"+id, nil); rr.Code != http.StatusNoContent {
		t.Fatalf("revoke: want 204, got %d (%s)", rr.Code, rr.Body.String())
	}
	var revokedAt *time.Time
	if err := env.db.QueryRow(ctx, "select revoked_at from personal_access_token where id=$1", id).Scan(&revokedAt); err != nil {
		t.Fatalf("query revoked_at: %v", err)
	}
	if revokedAt == nil {
		t.Fatal("revoke must set revoked_at")
	}
	if rr := doBearerRequest(t, env.srv, tok, http.MethodGet, "/api/agents", nil); rr.Code != http.StatusUnauthorized {
		t.Fatalf("post-revoke agents: want 401, got %d", rr.Code)
	}
	if rr := doRequest(t, env, http.MethodDelete, "/api/users/me/tokens/"+id, nil); rr.Code != http.StatusNotFound {
		t.Fatalf("double revoke: want 404, got %d", rr.Code)
	}

	// A past expiry is rejected by the resolver (401), same as revocation.
	tok2, id2 := mintPAT(t, env, env.bearerToken, "expire_me")
	if _, err := env.db.Exec(ctx,
		"update personal_access_token set expires_at = now() - interval '1 hour' where id=$1", id2); err != nil {
		t.Fatalf("force-expire: %v", err)
	}
	if rr := doBearerRequest(t, env.srv, tok2, http.MethodGet, "/api/agents", nil); rr.Code != http.StatusUnauthorized {
		t.Fatalf("expired token: want 401, got %d", rr.Code)
	}
}
