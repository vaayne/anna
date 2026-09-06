package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/oauth2"

	"github.com/CherryHQ/stella/internal/authz"
	"github.com/CherryHQ/stella/pkg/db/sqlc"
)

// seedOAuthRegistration inserts a common MCP definition/config directly so the
// loopback URL skips validateEndpointURL (which only allows public hosts).
func seedOAuthRegistration(t *testing.T, pool *pgxpool.Pool, scope, userID, agentID, rawURL string) Registration {
	t.Helper()
	id := uuid.NewString()
	pluginID := "custom/" + id
	namespace := "oauth_" + id[:8]
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO plugin_definition(id, namespace, display_name, backend, source,
			implementation_key, spec, default_enabled, revision, creator_user_id)
		VALUES ($1, $2, $3, 'mcp', 'custom', 'mcp', '{}'::jsonb, false, 1, NULLIF($4, '')::uuid)`,
		pluginID, namespace, "oauth-"+id[:8], nullableTestText(userID)); err != nil {
		t.Fatalf("seed oauth definition: %v", err)
	}
	payload := `{"url":"` + rawURL + `","transport":"streamable_http","auth_type":"oauth","credential_mode":"shared"}`
	refs := `{"oauth_bundle":{"name":"` + oauthBundleName(id) + `","mode":"shared","scope":"` + scope + `","user_id":"` + userID + `","agent_id":"` + agentID + `"}}`
	if scope == ScopeSystemAgent || scope == ScopeSystem {
		refs = `{"oauth_bundle":{"name":"` + oauthBundleName(id) + `","mode":"shared","scope":"` + scope + `","user_id":"` + userID + `","agent_id":"` + agentID + `"}}`
	}
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO plugin_config(id, plugin_id, namespace, scope, user_id, agent_id,
			enabled, config, credential_refs, revision)
		VALUES ($1::uuid, $2, $3, $4, NULLIF($5, '')::uuid, NULLIF($6, ''), true, $7::jsonb, $8::jsonb, 1)`,
		id, pluginID, namespace, scope, userID, agentID, payload, refs); err != nil {
		t.Fatalf("seed oauth config: %v", err)
	}
	// Schema 39 still owns the OAuth flow FK. This dormant identity row is a
	// test-only bridge until the coordinated NOT VALID FK retarget; production
	// MCP reads and writes never consult it.
	if _, err := sqlc.New(pool).CreateMCPServer(context.Background(), sqlc.CreateMCPServerParams{
		ID: id, Scope: scope,
		UserID:  pgtype.Text{String: userID, Valid: userID != ""},
		AgentID: pgtype.Text{String: agentID, Valid: agentID != ""},
		Name:    "dormant-flow-fk-" + id[:8], Url: rawURL,
		Transport: TransportStreamableHTTP, AuthType: AuthTypeNone,
		Enabled: false, Metadata: json.RawMessage(`{}`), CredentialMode: CredentialModeShared,
	}); err != nil {
		t.Fatalf("seed dormant OAuth flow FK identity: %v", err)
	}
	return Registration{
		ID: id, PluginID: pluginID, Namespace: namespace, ConfigRevision: 1,
		Scope: scope, UserID: userID, AgentID: agentID, Name: "oauth-" + id[:8], URL: rawURL,
		Transport: TransportStreamableHTTP, AuthType: AuthTypeOAuth, Enabled: true,
		CredentialMode: CredentialModeShared,
	}
}

func nullableTestText(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func commonOAuthClientID(t *testing.T, pool *pgxpool.Pool, configID string) string {
	t.Helper()
	var clientID pgtype.Text
	if err := pool.QueryRow(context.Background(), `
		SELECT config #>> '{metadata,oauth,client_id}'
		FROM plugin_config WHERE id = $1::uuid`, configID).Scan(&clientID); err != nil {
		t.Fatalf("read common OAuth client id: %v", err)
	}
	if !clientID.Valid {
		return ""
	}
	return clientID.String
}

func commonOAuthTokenEndpointAuthMethod(t *testing.T, pool *pgxpool.Pool, configID string) string {
	t.Helper()
	var method pgtype.Text
	if err := pool.QueryRow(context.Background(), `
		SELECT config #>> '{metadata,oauth,token_endpoint_auth_method}'
		FROM plugin_config WHERE id = $1::uuid`, configID).Scan(&method); err != nil {
		t.Fatalf("read common OAuth token endpoint auth method: %v", err)
	}
	if !method.Valid {
		return ""
	}
	return method.String
}

func refreshCommonRegistrationRevision(t *testing.T, pool *pgxpool.Pool, reg *Registration) {
	t.Helper()
	if err := pool.QueryRow(context.Background(), `SELECT revision FROM plugin_config WHERE id = $1::uuid`, reg.ID).Scan(&reg.ConfigRevision); err != nil {
		t.Fatalf("read common config revision: %v", err)
	}
}

// startFlow drives StartOAuth against the fakes and pins the PKCE challenge on
// the fake AS so /token can validate the verifier end to end.
func startFlow(t *testing.T, svc *Service, as *fakeAS, reg Registration, userID string) (flowID string) {
	t.Helper()
	authority, err := authz.NewUserAuthority(authz.UserID(userID), false)
	if err != nil {
		t.Fatal(err)
	}
	authURL, flowID, _, err := svc.StartOAuthForAuthority(context.Background(), reg, authority, "http://192.0.2.10/api/mcp/oauth/callback")
	if err != nil {
		t.Fatalf("StartOAuth: %v", err)
	}
	_, challenge, resource := parseAuthCodeURL(t, authURL)
	if resource == "" {
		t.Fatal("authorization URL is missing the resource parameter")
	}
	as.setExpectedChallenge(challenge)
	return flowID
}

func TestOAuthFlowEndToEnd(t *testing.T) {
	withLoopbackDialer(t)
	svc, _, userID, _ := setupInternal(t)
	as := newFakeAS(t)
	mcpSrv := fakeMCPServer(t, as.ts.URL)
	as.resource = mcpSrv.URL
	reg := seedOAuthRegistration(t, svc.pool, ScopeUser, userID, "", mcpSrv.URL)
	svc.SetConnectForTesting(func(context.Context, Registration, CredentialOwner) (RemoteClient, error) {
		return &fakeMCPClient{tools: []*mcpsdk.Tool{{Name: "list", InputSchema: map[string]any{"type": "object"}}}}, nil
	})

	flowID := startFlow(t, svc, as, reg, userID)
	refreshCommonRegistrationRevision(t, svc.pool, &reg)

	updated, err := svc.CompleteOAuth(context.Background(), flowID, "auth-code")
	if err != nil {
		t.Fatalf("CompleteOAuth: %v", err)
	}
	if updated.Status != StatusOK {
		t.Fatalf("status = %q, want ok after the post-connect probe", updated.Status)
	}

	// The bundle landed in the vault at the registration's own tuple...
	bundle, err := svc.loadBundle(context.Background(), reg, svc.CredentialOwner(reg, userID))
	if err != nil || bundle == nil {
		t.Fatalf("loadBundle: %v, %v", bundle, err)
	}
	if bundle.AccessToken != "new-access" || bundle.RefreshToken != "new-refresh" {
		t.Fatalf("bundle tokens = %q/%q", bundle.AccessToken, bundle.RefreshToken)
	}
	if bundle.GrantedScope != "mcp:read" {
		t.Fatalf("granted scope = %q", bundle.GrantedScope)
	}
	// ...and the client identity was persisted so DCR runs once.
	if commonOAuthClientID(t, svc.pool, reg.ID) == "" {
		t.Fatal("DCR client id was not persisted into metadata")
	}
	if n := len(as.clients); n != 1 {
		t.Fatalf("DCR calls = %d, want 1", n)
	}

	// Replay: the flow is one-shot; a second callback must fail and must not
	// write a second bundle.
	if _, err := svc.CompleteOAuth(context.Background(), flowID, "auth-code"); err == nil {
		t.Fatal("replayed callback must fail")
	}
	if got := bundle.AccessToken; got != "new-access" {
		t.Fatalf("bundle mutated by replay: %q", got)
	}
}

func TestOAuthFlowUsesDCRTokenEndpointAuthMethod(t *testing.T) {
	withLoopbackDialer(t)
	tests := []struct {
		name       string
		response   string
		require    string
		wantStyle  oauth2.AuthStyle
		wantMethod string
	}{
		{name: "basic", response: oauthTokenEndpointAuthMethodBasic, require: oauthTokenEndpointAuthMethodBasic, wantStyle: oauth2.AuthStyleInHeader, wantMethod: oauthTokenEndpointAuthMethodBasic},
		{name: "post", response: oauthTokenEndpointAuthMethodPost, require: oauthTokenEndpointAuthMethodPost, wantStyle: oauth2.AuthStyleInParams, wantMethod: oauthTokenEndpointAuthMethodPost},
		{name: "omitted defaults basic", require: oauthTokenEndpointAuthMethodBasic, wantStyle: oauth2.AuthStyleInHeader, wantMethod: oauthTokenEndpointAuthMethodBasic},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc, _, userID, _ := setupInternal(t)
			as := newFakeAS(t)
			as.tokenEndpointAuthMethod = tt.response
			as.requireTokenAuthMethod = tt.require
			mcpSrv := fakeMCPServer(t, as.ts.URL)
			as.resource = mcpSrv.URL
			reg := seedOAuthRegistration(t, svc.pool, ScopeUser, userID, "", mcpSrv.URL)
			svc.SetConnectForTesting(func(context.Context, Registration, CredentialOwner) (RemoteClient, error) {
				return &fakeMCPClient{tools: []*mcpsdk.Tool{{Name: "list", InputSchema: map[string]any{"type": "object"}}}}, nil
			})

			flowID := startFlow(t, svc, as, reg, userID)
			as.mu.Lock()
			requestedMethod, requested := as.clients[0]["token_endpoint_auth_method"]
			as.mu.Unlock()
			if requested {
				t.Fatalf("DCR request selected token endpoint auth method %v; the AS response must decide it", requestedMethod)
			}
			refreshCommonRegistrationRevision(t, svc.pool, &reg)
			if _, err := svc.CompleteOAuth(context.Background(), flowID, "auth-code"); err != nil {
				t.Fatalf("CompleteOAuth: %v", err)
			}
			bundle, err := svc.loadBundle(context.Background(), reg, svc.CredentialOwner(reg, userID))
			if err != nil || bundle == nil {
				t.Fatalf("loadBundle: %v, %v", bundle, err)
			}
			if got := oauth2.AuthStyle(bundle.AuthStyle); got != tt.wantStyle {
				t.Fatalf("bundle auth style = %v, want %v", got, tt.wantStyle)
			}
			if got := commonOAuthTokenEndpointAuthMethod(t, svc.pool, reg.ID); got != tt.wantMethod {
				t.Fatalf("persisted token endpoint auth method = %q, want %q", got, tt.wantMethod)
			}
		})
	}
}

func TestOAuthDCRRejectsUnsupportedTokenEndpointAuthMethod(t *testing.T) {
	withLoopbackDialer(t)
	svc, _, userID, _ := setupInternal(t)
	as := newFakeAS(t)
	as.tokenEndpointAuthMethod = "private_key_jwt"
	mcpSrv := fakeMCPServer(t, as.ts.URL)
	as.resource = mcpSrv.URL
	reg := seedOAuthRegistration(t, svc.pool, ScopeUser, userID, "", mcpSrv.URL)
	authority, err := authz.NewUserAuthority(authz.UserID(userID), false)
	if err != nil {
		t.Fatal(err)
	}
	_, _, _, err = svc.StartOAuthForAuthority(context.Background(), reg, authority, "http://192.0.2.10/api/mcp/oauth/callback")
	if err == nil || !strings.Contains(err.Error(), "unsupported OAuth token endpoint auth method") {
		t.Fatalf("unsupported DCR method error = %v", err)
	}
	if got := commonOAuthClientID(t, svc.pool, reg.ID); got != "" {
		t.Fatalf("client id persisted for unsupported method = %q", got)
	}
	var flowCount int
	if err := svc.pool.QueryRow(t.Context(), `SELECT count(*) FROM mcp_oauth_flow WHERE server_id = $1::uuid`, reg.ID).Scan(&flowCount); err != nil {
		t.Fatalf("count unsupported DCR flows: %v", err)
	}
	if flowCount != 0 {
		t.Fatalf("flow rows for unsupported method = %d, want 0", flowCount)
	}
}

func TestOAuthTokenEndpointAuthMethodMapping(t *testing.T) {
	for _, tt := range []struct {
		method string
		style  oauth2.AuthStyle
		normal string
	}{
		{method: "", style: oauth2.AuthStyleInHeader, normal: oauthTokenEndpointAuthMethodBasic},
		{method: oauthTokenEndpointAuthMethodBasic, style: oauth2.AuthStyleInHeader, normal: oauthTokenEndpointAuthMethodBasic},
		{method: oauthTokenEndpointAuthMethodPost, style: oauth2.AuthStyleInParams, normal: oauthTokenEndpointAuthMethodPost},
		{method: oauthTokenEndpointAuthMethodNone, style: oauth2.AuthStyleInParams, normal: oauthTokenEndpointAuthMethodNone},
	} {
		style, normal, err := oauthTokenEndpointAuthStyle(tt.method)
		if err != nil || style != tt.style || normal != tt.normal {
			t.Errorf("method %q -> style=%v normal=%q err=%v, want style=%v normal=%q", tt.method, style, normal, err, tt.style, tt.normal)
		}
	}
	if _, _, err := oauthTokenEndpointAuthStyle("private_key_jwt"); err == nil {
		t.Fatal("unsupported auth method must be rejected")
	}
}

func TestAdminDCRUsesAuthoritativeUpdatedRevision(t *testing.T) {
	withLoopbackDialer(t)
	svc, _, userID, _ := setupInternal(t)
	as := newFakeAS(t)
	mcpSrv := fakeMCPServer(t, as.ts.URL)
	as.resource = mcpSrv.URL
	reg := seedOAuthRegistration(t, svc.pool, ScopeSystem, "", "", mcpSrv.URL)
	admin, err := authz.NewUserAuthority(authz.UserID(userID), true)
	if err != nil {
		t.Fatal(err)
	}
	authURL, flowID, _, err := svc.StartOAuthForAuthority(t.Context(), reg, admin, "http://192.0.2.10/api/mcp/oauth/callback")
	if err != nil {
		t.Fatalf("admin StartOAuth: %v", err)
	}
	_, challenge, _ := parseAuthCodeURL(t, authURL)
	as.setExpectedChallenge(challenge)
	var flowRevision int64
	if err := svc.pool.QueryRow(t.Context(), `SELECT (oauth_config->>'config_revision')::bigint FROM mcp_oauth_flow WHERE id = $1::uuid`, flowID).Scan(&flowRevision); err != nil {
		t.Fatalf("read DCR flow revision: %v", err)
	}
	if flowRevision != 2 {
		t.Fatalf("flow revision = %d, want authoritative revision 2", flowRevision)
	}
	if got := commonOAuthClientID(t, svc.pool, reg.ID); got != "dcr-client-1" {
		t.Fatalf("persisted DCR client id = %q", got)
	}
}

func TestAdminDCRUserAgentUsesConfigCredentialOwner(t *testing.T) {
	withLoopbackDialer(t)
	svc, _, userID, agentID := setupInternal(t)
	as := newFakeAS(t)
	mcpSrv := fakeMCPServer(t, as.ts.URL)
	as.resource = mcpSrv.URL
	reg := seedOAuthRegistration(t, svc.pool, ScopeUserAgent, userID, agentID, mcpSrv.URL)
	if _, err := svc.pool.Exec(t.Context(), `UPDATE plugin_config SET config = jsonb_set(config, '{credential_mode}', '"per_user"'::jsonb), credential_refs = jsonb_set(credential_refs, '{oauth_bundle}', '{"name":"`+oauthBundleName(reg.ID)+`","mode":"per_user","owner":"per_user"}'::jsonb) WHERE id = $1::uuid`, reg.ID); err != nil {
		t.Fatalf("seed user-agent per-user config: %v", err)
	}
	reg.CredentialMode = CredentialModePerUser
	admin, err := authz.NewUserAuthority(authz.UserID(userID), true)
	if err != nil {
		t.Fatal(err)
	}
	_, flowID, _, err := svc.StartOAuthForAuthority(t.Context(), reg, admin, "http://192.0.2.10/api/mcp/oauth/callback")
	if err != nil {
		t.Fatalf("user-agent admin StartOAuth: %v", err)
	}
	var flowRevision int64
	if err := svc.pool.QueryRow(t.Context(), `SELECT (oauth_config->>'config_revision')::bigint FROM mcp_oauth_flow WHERE id = $1::uuid`, flowID).Scan(&flowRevision); err != nil {
		t.Fatalf("read user-agent DCR flow revision: %v", err)
	}
	if flowRevision != 2 {
		t.Fatalf("user-agent flow revision = %d, want authoritative revision 2", flowRevision)
	}
	secretName := oauthClientSecretName(reg.ID)
	if got, err := svc.vault.GetScoped(t.Context(), ScopeUserAgent, userID, agentID, secretName); err != nil || got != "dcr-secret" {
		t.Fatalf("user-agent config secret = %q, err %v", got, err)
	}
	if _, err := svc.vault.GetScoped(t.Context(), ScopeUser, userID, "", secretName); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("user-agent config secret leaked to caller bundle tuple: %v", err)
	}
}

func TestNonAdminSystemDCRDoesNotWriteClientOrFlow(t *testing.T) {
	withLoopbackDialer(t)
	svc, _, userID, _ := setupInternal(t)
	as := newFakeAS(t)
	mcpSrv := fakeMCPServer(t, as.ts.URL)
	as.resource = mcpSrv.URL
	reg := seedOAuthRegistration(t, svc.pool, ScopeSystem, "", "", mcpSrv.URL)
	user, err := authz.NewUserAuthority(authz.UserID(userID), false)
	if err != nil {
		t.Fatal(err)
	}
	_, _, _, err = svc.StartOAuthForAuthority(t.Context(), reg, user, "http://192.0.2.10/api/mcp/oauth/callback")
	if !errors.Is(err, ErrOAuthClientInitializationRequired) {
		t.Fatalf("non-admin DCR error = %v, want %v", err, ErrOAuthClientInitializationRequired)
	}
	if len(as.clients) != 0 {
		t.Fatalf("DCR requests = %d, want 0", len(as.clients))
	}
	if got := commonOAuthClientID(t, svc.pool, reg.ID); got != "" {
		t.Fatalf("client id written on rejected DCR = %q", got)
	}
	var flowCount int
	if err := svc.pool.QueryRow(t.Context(), `SELECT count(*) FROM mcp_oauth_flow WHERE server_id = $1::uuid`, reg.ID).Scan(&flowCount); err != nil {
		t.Fatalf("count rejected DCR flows: %v", err)
	}
	if flowCount != 0 {
		t.Fatalf("flow rows after rejected DCR = %d, want 0", flowCount)
	}
}

func TestClearingOAuthClientIDAtomicallyRevokesCredentials(t *testing.T) {
	withLoopbackDialer(t)
	svc, _, userID, _ := setupInternal(t)
	as := newFakeAS(t)
	mcpSrv := fakeMCPServer(t, as.ts.URL)
	as.resource = mcpSrv.URL
	reg := seedOAuthRegistration(t, svc.pool, ScopeUser, userID, "", mcpSrv.URL)
	secretRef := oauthClientSecretName(reg.ID)
	refs := `{"oauth_bundle":{"name":"` + oauthBundleName(reg.ID) + `","mode":"shared","scope":"user","user_id":"` + userID + `","agent_id":""},"oauth_client_secret":{"name":"` + secretRef + `","scope":"user","user_id":"` + userID + `","agent_id":""}}`
	if _, err := svc.pool.Exec(t.Context(), `UPDATE plugin_config SET config = jsonb_set(config, '{metadata}', '{"oauth":{"client_id":"old-client"}}'::jsonb), credential_refs = $2::jsonb WHERE id = $1::uuid`, reg.ID, refs); err != nil {
		t.Fatalf("seed client secret ref: %v", err)
	}
	if err := svc.vault.SetScoped(t.Context(), ScopeUser, userID, "", secretRef, "old-secret"); err != nil {
		t.Fatal(err)
	}
	authority, err := authz.NewUserAuthority(authz.UserID(userID), false)
	if err != nil {
		t.Fatal(err)
	}
	ctx := authz.WithAuthority(context.Background(), authority)
	empty := ""
	if _, err := svc.Update(ctx, UpdateInput{ID: reg.ID, Scope: ScopeUser, UserID: userID, OAuthClientID: &empty}); err != nil {
		t.Fatalf("atomic clear: %v", err)
	}
	var storedRef string
	if err := svc.pool.QueryRow(t.Context(), `SELECT config #>> '{metadata,oauth,client_id}' FROM plugin_config WHERE id = $1::uuid`, reg.ID).Scan(&storedRef); err != nil {
		t.Fatal(err)
	}
	if storedRef != "" {
		t.Fatalf("client id after clear = %q", storedRef)
	}
	var remaining int
	if err := svc.pool.QueryRow(ctx, `SELECT count(*) FROM vault_entry WHERE name IN ($1,$2,$3)`, secretRef, credentialName(reg.ID), oauthBundleName(reg.ID)).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 0 {
		t.Fatalf("atomic clear retained %d credentials", remaining)
	}
	var hasRef bool
	if err := svc.pool.QueryRow(ctx, `SELECT credential_refs ? 'oauth_client_secret' FROM plugin_config WHERE id=$1`, reg.ID).Scan(&hasRef); err != nil {
		t.Fatal(err)
	}
	if hasRef {
		t.Fatal("cleared client retained secret locator")
	}
}

func TestOAuthFlowExpired(t *testing.T) {
	svc, _, userID, _ := setupInternal(t)
	reg := seedOAuthRegistration(t, svc.pool, ScopeUser, userID, "", "http://127.0.0.1:1/mcp")
	flowID := uuid.NewString()
	if _, err := svc.db.CreateMCPOAuthFlow(context.Background(), flowParams(flowID, reg, userID, "verifier", []byte(`{"client_id":"c","token_endpoint":"http://tok","redirect_uri":"http://cb"}`), time.Now().UTC().Add(-time.Minute))); err != nil {
		t.Fatalf("seed expired flow: %v", err)
	}
	if _, err := svc.CompleteOAuth(context.Background(), flowID, "code"); err == nil {
		t.Fatal("expired flow must fail")
	} else if !strings.Contains(err.Error(), "unknown, expired, or already used") {
		t.Fatalf("expired flow error = %v", err)
	}
}

// TestOAuthPerUserBundlesAreIsolated proves a per_user flow writes the bundle
// under the initiating user's user scope only: user A's tools resolve, user B
// has no credential and is skipped by the provider.
func TestOAuthPerUserBundlesAreIsolated(t *testing.T) {
	withLoopbackDialer(t)
	svc, _, userA, _ := setupInternal(t)
	userB := newUserForOAuthTest(t, svc.pool)
	as := newFakeAS(t)
	mcpSrv := fakeMCPServer(t, as.ts.URL)
	as.resource = mcpSrv.URL
	reg := seedOAuthRegistration(t, svc.pool, ScopeUser, userA, "", mcpSrv.URL)
	// Set credential_mode per_user directly so this test exercises exact
	// credential-owner isolation without requiring an HTTP/API fixture.
	if _, err := svc.pool.Exec(context.Background(), `
		UPDATE plugin_config
		SET config = jsonb_set(config, '{credential_mode}', '"per_user"'::jsonb),
		    credential_refs = jsonb_set(credential_refs, '{oauth_bundle}',
		      '{"name":"`+oauthBundleName(reg.ID)+`","mode":"per_user","owner":"per_user"}'::jsonb)
		WHERE id = $1::uuid`, reg.ID); err != nil {
		t.Fatalf("set per_user: %v", err)
	}
	reg.CredentialMode = CredentialModePerUser
	svc.SetConnectForTesting(func(context.Context, Registration, CredentialOwner) (RemoteClient, error) {
		return &fakeMCPClient{tools: []*mcpsdk.Tool{{Name: "list", InputSchema: map[string]any{"type": "object"}}}}, nil
	})

	flowID := startFlow(t, svc, as, reg, userA)
	if _, err := svc.CompleteOAuth(context.Background(), flowID, "auth-code"); err != nil {
		t.Fatalf("CompleteOAuth (A): %v", err)
	}
	refreshCommonRegistrationRevision(t, svc.pool, &reg)

	if bundle, err := svc.loadBundle(context.Background(), reg, svc.CredentialOwner(reg, userA)); err != nil || bundle == nil {
		t.Fatalf("user A bundle = %#v, err = %v", bundle, err)
	}
	if svc.HasUserCredential(context.Background(), reg, userB) {
		t.Fatal("user B must not inherit user A's per_user bundle")
	}
	// The snapshot provider serves tools for A and skips the server for B.
	provider := NewToolProvider(svc)
	authorityA, err := authz.NewUserAuthority(authz.UserID(userA), false)
	if err != nil {
		t.Fatal(err)
	}
	snapshotA, err := svc.plugins.ResolveSnapshot(context.Background(), authorityA, "")
	if err != nil {
		t.Fatalf("resolve A snapshot: %v", err)
	}
	if tools, err := provider.ToolsForSnapshot(context.Background(), snapshotA); err != nil || len(tools) != 1 {
		t.Fatalf("A tools = %d, err=%v, want 1", len(tools), err)
	}
	authorityB, err := authz.NewUserAuthority(authz.UserID(userB), false)
	if err != nil {
		t.Fatal(err)
	}
	snapshotB, err := svc.plugins.ResolveSnapshot(context.Background(), authorityB, "")
	if err != nil {
		t.Fatalf("resolve B snapshot: %v", err)
	}
	if tools, err := provider.ToolsForSnapshot(context.Background(), snapshotB); err != nil || len(tools) != 0 {
		t.Fatalf("B tools = %d, err=%v, want 0 (no bundle)", len(tools), err)
	}
	if !svc.HasUserCredential(context.Background(), reg, userA) {
		t.Fatal("user A must have a credential after connecting")
	}
	if svc.HasUserCredential(context.Background(), reg, userB) {
		t.Fatal("user B must not inherit user A's per_user bundle")
	}
}
