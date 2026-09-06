package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"filippo.io/age"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	storepkg "github.com/CherryHQ/stella/cmd/stellad/store"
	"github.com/CherryHQ/stella/internal/auth"
	"github.com/CherryHQ/stella/internal/authz"
	agentaccess "github.com/CherryHQ/stella/internal/core/access"
	appdb "github.com/CherryHQ/stella/internal/db"
	"github.com/CherryHQ/stella/internal/db/dbtest"
	"github.com/CherryHQ/stella/internal/plugin"
	"github.com/CherryHQ/stella/internal/vault"
	"github.com/CherryHQ/stella/pkg/db/sqlc"
)

func noopBackendTransition(context.Context, pgx.Tx, authz.Authority, plugin.MutationKind, plugin.Definition, *plugin.Config, *plugin.Config) error {
	return nil
}

// loopbackDialer is the test dial policy: loopback reaches the fake AS and MCP
// servers; everything else still goes through the production SSRF dialer, so
// the private-IP rejection stays testable.
func loopbackDialer(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err == nil {
		if ip, ipErr := netip.ParseAddr(strings.Trim(host, "[]")); ipErr == nil && ip.IsLoopback() {
			return (&net.Dialer{}).DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		}
	}
	return EndpointPolicy{}.dialContext(ctx, network, address)
}

func withLoopbackDialer(t *testing.T) {
	t.Helper()
	prev := testingDialContext
	testingDialContext = loopbackDialer
	t.Cleanup(func() { testingDialContext = prev })
}

// fakeAS is a stand-in OAuth authorization server: AS metadata, DCR, and a
// /token endpoint that validates grant type, PKCE, and resource, and can be
// scripted per test.
type fakeAS struct {
	ts        *httptest.Server
	tokenHits atomic.Int32
	// tokenEndpointAuthMethod is the method returned by DCR. An empty value
	// exercises RFC 7591's client_secret_basic default.
	tokenEndpointAuthMethod string
	// requireTokenAuthMethod makes the fake token endpoint reject the other
	// wire shape, so tests prove AuthStyle rather than merely accepting both.
	requireTokenAuthMethod string
	// tokenStatus overrides the token response status (0 = 200).
	tokenStatus int
	// tokenBody overrides the token response body ("" = valid token JSON).
	tokenBody string
	mu        sync.Mutex
	clients   []map[string]any
	// expectedChallenge is the S256 code challenge from the authorization URL;
	// the test sets it after parsing so /token can verify PKCE end to end.
	expectedChallenge string
	// resource is the protected-resource identifier the PRM advertises; the
	// test points it at the fake MCP server once created.
	resource string
}

// writeJSON sets the content type x/oauth2 and oauthex require.
func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func newFakeAS(t *testing.T) *fakeAS {
	t.Helper()
	as := &fakeAS{}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/oauth-authorization-server", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{
			"issuer":                           as.ts.URL,
			"authorization_endpoint":           as.ts.URL + "/authorize",
			"token_endpoint":                   as.ts.URL + "/token",
			"registration_endpoint":            as.ts.URL + "/register",
			"code_challenge_methods_supported": []string{"S256"},
		})
	})
	mux.HandleFunc("POST /register", func(w http.ResponseWriter, r *http.Request) {
		var meta map[string]any
		_ = json.NewDecoder(r.Body).Decode(&meta)
		as.mu.Lock()
		as.clients = append(as.clients, meta)
		as.mu.Unlock()
		response := map[string]any{
			"client_id": "dcr-client-1", "client_secret": "dcr-secret",
			"redirect_uris": meta["redirect_uris"], "grant_types": []string{"authorization_code", "refresh_token"},
		}
		if as.tokenEndpointAuthMethod != "" {
			response["token_endpoint_auth_method"] = as.tokenEndpointAuthMethod
		}
		writeJSON(w, response)
	})
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		as.tokenHits.Add(1)
		if as.tokenStatus != 0 {
			w.WriteHeader(as.tokenStatus)
			_, _ = w.Write([]byte(as.tokenBody))
			return
		}
		if err := r.ParseForm(); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		switch as.requireTokenAuthMethod {
		case oauthTokenEndpointAuthMethodBasic:
			username, password, ok := r.BasicAuth()
			if !ok || username != "dcr-client-1" || password != "dcr-secret" || r.Form.Get("client_secret") != "" {
				w.WriteHeader(http.StatusUnauthorized)
				writeJSON(w, map[string]any{"error": "invalid_client", "expected": oauthTokenEndpointAuthMethodBasic})
				return
			}
		case oauthTokenEndpointAuthMethodPost:
			if _, _, ok := r.BasicAuth(); ok || r.Form.Get("client_secret") != "dcr-secret" {
				w.WriteHeader(http.StatusUnauthorized)
				writeJSON(w, map[string]any{"error": "invalid_client", "expected": oauthTokenEndpointAuthMethodPost})
				return
			}
		case oauthTokenEndpointAuthMethodNone:
			if _, _, ok := r.BasicAuth(); ok || r.Form.Get("client_secret") != "" {
				w.WriteHeader(http.StatusUnauthorized)
				writeJSON(w, map[string]any{"error": "invalid_client", "expected": oauthTokenEndpointAuthMethodNone})
				return
			}
		}
		if got := r.Form.Get("grant_type"); got != "authorization_code" && got != "refresh_token" {
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, map[string]any{"error": "unsupported_grant_type"})
			return
		}
		if as.expectedChallenge != "" {
			sum := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
			if base64.RawURLEncoding.EncodeToString(sum[:]) != as.expectedChallenge {
				w.WriteHeader(http.StatusBadRequest)
				writeJSON(w, map[string]any{"error": "invalid_grant"})
				return
			}
		}
		// The resource indicator is mandatory on the authorization-code grant
		// (RFC 8707); the refresh grant reuses the original audience.
		if r.Form.Get("grant_type") == "authorization_code" && r.Form.Get("resource") == "" {
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, map[string]any{"error": "invalid_request"})
			return
		}
		writeJSON(w, map[string]any{"access_token": "new-access", "refresh_token": "new-refresh", "token_type": "Bearer", "expires_in": 3600, "scope": "mcp:read"})
	})
	mux.HandleFunc("GET /.well-known/oauth-protected-resource", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{
			"resource":              as.resource,
			"authorization_servers": []string{as.ts.URL},
			"scopes_supported":      []string{"mcp:read"},
		})
	})
	as.ts = httptest.NewServer(mux)
	t.Cleanup(as.ts.Close)
	return as
}

func (a *fakeAS) setExpectedChallenge(v string) {
	a.mu.Lock()
	a.expectedChallenge = v
	a.mu.Unlock()
}

// fakeMCPServer answers the challenge GET with a 401 pointing at the fake AS.
func fakeMCPServer(t *testing.T, asURL string) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+asURL+`/.well-known/oauth-protected-resource"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(ts.Close)
	return ts
}

// parseAuthCodeURL extracts the state (flow id) and code challenge from an
// authorization URL.
func parseAuthCodeURL(t *testing.T, raw string) (flowID, challenge, resource string) {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse auth url: %v", err)
	}
	q := u.Query()
	return q.Get("state"), q.Get("code_challenge"), q.Get("resource")
}

// setupInternal is the package-mcp twin of the mcp_test setup(): real database,
// real age-encrypted vault, one user and one agent.
func setupInternal(t *testing.T) (svc *Service, q *sqlc.Queries, userID, agentID string) {
	t.Helper()
	pool := dbtest.New(t)
	oidc := appdb.NewOIDCStore(pool)
	q = sqlc.New(pool)
	ctx := context.Background()

	masterID, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatalf("master identity: %v", err)
	}
	vaultSvc, err := vault.NewServiceForPool(pool, masterID.String(), nil)
	if err != nil {
		t.Fatalf("vault.NewService: %v", err)
	}
	user, err := oidc.CreateUser(ctx, auth.User{ID: uuid.NewString(), Email: "u@oauth.test", Name: "U"})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	pub, encPriv, err := vault.GenerateUserKeys(vaultSvc.MasterRecipient())
	if err != nil {
		t.Fatalf("GenerateUserKeys: %v", err)
	}
	if err := oidc.UpdateUserAgeKeys(ctx, user.ID, pub, encPriv); err != nil {
		t.Fatalf("UpdateUserAgeKeys: %v", err)
	}
	if _, err := q.CreateAgent(ctx, sqlc.CreateAgentParams{
		ID: "oauth-test-agent", Name: "Agent", Workspace: "/tmp/agent",
		Sandbox: json.RawMessage(`{}`), Scope: "system", Enabled: true,
	}); err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	svc = NewServiceForPool(pool, vaultSvc, func(tx pgx.Tx) Vault { return vaultSvc.WithTx(tx) })
	policy := EndpointPolicy{AllowPrivate: true}
	svc.SetEndpointPolicy(policy)
	agents := agentaccess.NewService(storepkg.NewDBStore(pool), appdb.NewAuthStore(pool))
	svc.SetPluginService(plugin.NewService(pool, agents, plugin.NewCatalog(), NewMCPBackendPolicy(policy), func(_ context.Context, fn func() error) error {
		return fn()
	}))
	return svc, q, user.ID, "oauth-test-agent"
}

// newUserForOAuthTest provisions a second vault-capable user.
func newUserForOAuthTest(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	oidc := appdb.NewOIDCStore(pool)
	ctx := context.Background()
	user, err := oidc.CreateUser(ctx, auth.User{ID: uuid.NewString(), Email: "b@oauth.test", Name: "B"})
	if err != nil {
		t.Fatalf("CreateUser B: %v", err)
	}
	masterID, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	pub, encPriv, err := vault.GenerateUserKeys(masterID.Recipient())
	if err != nil {
		t.Fatal(err)
	}
	if err := oidc.UpdateUserAgeKeys(ctx, user.ID, pub, encPriv); err != nil {
		t.Fatal(err)
	}
	return user.ID
}
