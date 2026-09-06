package mcp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/CherryHQ/stella/internal/agent"
	"github.com/CherryHQ/stella/internal/authz"
	"github.com/CherryHQ/stella/internal/vault"
	"github.com/CherryHQ/stella/pkg/ai"
	"github.com/CherryHQ/stella/pkg/db/pgnull"
	"github.com/CherryHQ/stella/pkg/db/sqlc"
	pkgtools "github.com/CherryHQ/stella/pkg/tools"
)

// fakeDB is an in-memory mcp.DB for unit tests.
type fakeDB struct {
	mu              sync.Mutex // Probe runs concurrently from provider discovery
	created         []sqlc.CreateMCPServerParams
	gets            int
	rows            map[string]sqlc.McpServer // id -> row
	forCtx          []sqlc.McpServer          // canned legacy-row fixture
	byScope         []sqlc.McpServer
	updated         []sqlc.UpdateMCPServerByScopeParams
	probeResults    []sqlc.UpdateMCPServerProbeResultParams
	statusUpdates   []sqlc.UpdateMCPServerStatusParams
	renames         []sqlc.RenameToolOverridePrefixParams
	flows           map[string]sqlc.McpOauthFlow
	flowConsumed    map[string]bool
	deletedPrefixes []string
	deleted         []string
	createFn        func(sqlc.CreateMCPServerParams) (sqlc.McpServer, error)
}

func newFakeDB() *fakeDB {
	return &fakeDB{rows: map[string]sqlc.McpServer{}, flows: map[string]sqlc.McpOauthFlow{}, flowConsumed: map[string]bool{}}
}

func (d *fakeDB) CreateMCPOAuthFlow(_ context.Context, arg sqlc.CreateMCPOAuthFlowParams) (sqlc.McpOauthFlow, error) {
	row := sqlc.McpOauthFlow{
		ID: arg.ID, ServerID: arg.ServerID, UserID: arg.UserID,
		CredentialScope: arg.CredentialScope, CredentialUserID: arg.CredentialUserID, CredentialAgentID: arg.CredentialAgentID,
		PkceVerifier: arg.PkceVerifier, OauthConfig: arg.OauthConfig, ExpiresAt: arg.ExpiresAt,
	}
	d.flows[arg.ID] = row
	return row, nil
}

func (d *fakeDB) ConsumeMCPOAuthFlow(_ context.Context, id string) (sqlc.McpOauthFlow, error) {
	row, ok := d.flows[id]
	if !ok || d.flowConsumed[id] || !row.ExpiresAt.After(time.Now()) {
		return sqlc.McpOauthFlow{}, pgx.ErrNoRows
	}
	d.flowConsumed[id] = true
	row.ConsumedAt = pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true}
	return row, nil
}

func (d *fakeDB) CreateMCPServer(_ context.Context, arg sqlc.CreateMCPServerParams) (sqlc.McpServer, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.created = append(d.created, arg)
	if d.createFn != nil {
		return d.createFn(arg)
	}
	row := sqlc.McpServer{
		ID: arg.ID, Scope: arg.Scope, UserID: arg.UserID, AgentID: arg.AgentID,
		Name: arg.Name, Url: arg.Url, Transport: arg.Transport, AuthType: arg.AuthType,
		CredentialRef: arg.CredentialRef, Enabled: arg.Enabled, Metadata: arg.Metadata,
	}
	d.rows[arg.ID] = row
	return row, nil
}

func (d *fakeDB) GetMCPServerByID(_ context.Context, id string) (sqlc.McpServer, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.gets++
	return d.rows[id], nil
}

func (d *fakeDB) ListMCPServersByScope(_ context.Context, _ sqlc.ListMCPServersByScopeParams) ([]sqlc.McpServer, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.byScope, nil
}

func (d *fakeDB) ListMCPServersForAgentContext(_ context.Context, _ sqlc.ListMCPServersForAgentContextParams) ([]sqlc.McpServer, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.forCtx, nil
}

func (d *fakeDB) UpdateMCPServerByScope(_ context.Context, arg sqlc.UpdateMCPServerByScopeParams) (sqlc.McpServer, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.updated = append(d.updated, arg)
	row := d.rows[arg.ID]
	row.Scope = arg.NewScope
	row.UserID = arg.NewUserID
	row.AgentID = arg.NewAgentID
	row.Name = arg.Name
	row.Url = arg.Url
	row.Transport = arg.Transport
	row.AuthType = arg.AuthType
	row.CredentialRef = arg.CredentialRef
	row.Enabled = arg.Enabled
	d.rows[arg.ID] = row
	return row, nil
}

func (d *fakeDB) UpdateMCPServerByScopeIfVersion(ctx context.Context, arg sqlc.UpdateMCPServerByScopeIfVersionParams) (sqlc.McpServer, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	row, ok := d.rows[arg.ID]
	if !ok || !row.UpdatedAt.Equal(arg.ExpectedUpdatedAt) {
		return sqlc.McpServer{}, pgx.ErrNoRows
	}
	return d.UpdateMCPServerByScope(ctx, sqlc.UpdateMCPServerByScopeParams{
		NewScope: arg.NewScope, NewUserID: arg.NewUserID, NewAgentID: arg.NewAgentID,
		Name: arg.Name, Url: arg.Url, Transport: arg.Transport, AuthType: arg.AuthType,
		CredentialRef: arg.CredentialRef, Enabled: arg.Enabled, ID: arg.ID, Scope: arg.Scope,
		UserID: arg.UserID, AgentID: arg.AgentID,
	})
}

func (d *fakeDB) UpdateMCPServerProbeResult(_ context.Context, arg sqlc.UpdateMCPServerProbeResultParams) (sqlc.McpServer, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.probeResults = append(d.probeResults, arg)
	row := d.rows[arg.ID]
	row.Status, row.StatusError, row.ProbedAt, row.Tools = arg.Status, arg.StatusError, arg.ProbedAt, arg.Tools
	d.rows[arg.ID] = row
	return row, nil
}

func (d *fakeDB) UpdateMCPServerStatus(_ context.Context, arg sqlc.UpdateMCPServerStatusParams) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.statusUpdates = append(d.statusUpdates, arg)
	row := d.rows[arg.ID]
	row.Status, row.StatusError = arg.Status, arg.StatusError
	d.rows[arg.ID] = row
	return nil
}

func (d *fakeDB) CountMCPServersByNameExcluding(_ context.Context, arg sqlc.CountMCPServersByNameExcludingParams) (int64, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var n int64
	for id, row := range d.rows {
		if id != arg.ID && row.Name == arg.Name {
			n++
		}
	}
	return n, nil
}

func (d *fakeDB) GetMCPServerIDByURLOnScope(_ context.Context, arg sqlc.GetMCPServerIDByURLOnScopeParams) (string, error) {
	// Mirror the query: same URL in the same scope/owner bucket.
	for _, row := range d.rows {
		if row.Url == arg.Url && row.Scope == arg.Scope &&
			textOrEmpty(row.UserID) == textOrEmpty(arg.UserID) &&
			textOrEmpty(row.AgentID) == textOrEmpty(arg.AgentID) {
			return row.ID, nil
		}
	}
	return "", pgx.ErrNoRows
}

func (d *fakeDB) UpdateMCPServerMetadata(_ context.Context, arg sqlc.UpdateMCPServerMetadataParams) error {
	row := d.rows[arg.ID]
	row.Metadata = arg.Metadata
	d.rows[arg.ID] = row
	return nil
}

func (d *fakeDB) RenameToolOverridePrefix(_ context.Context, arg sqlc.RenameToolOverridePrefixParams) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.renames = append(d.renames, arg)
	return nil
}

func (d *fakeDB) DeleteToolOverridesByPrefix(_ context.Context, prefix string) (int64, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.deletedPrefixes = append(d.deletedPrefixes, prefix)
	return 0, nil
}

func (d *fakeDB) DeleteMCPServerByScope(_ context.Context, arg sqlc.DeleteMCPServerByScopeParams) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.deleted = append(d.deleted, arg.ID)
	delete(d.rows, arg.ID)
	return nil
}

func (d *fakeDB) DeleteMCPServerByScopeIfVersion(_ context.Context, arg sqlc.DeleteMCPServerByScopeIfVersionParams) (int64, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	row, ok := d.rows[arg.ID]
	if !ok || !row.UpdatedAt.Equal(arg.ExpectedUpdatedAt) {
		return 0, nil
	}
	d.deleted = append(d.deleted, arg.ID)
	return 1, nil
}

func TestToolMutationSchemasRequireNonEmptyExpectedVersion(t *testing.T) {
	for _, action := range []string{"update", "delete"} {
		var schema map[string]any
		for _, spec := range SettingsMcpActionTools() {
			if spec.Action == action {
				if err := json.Unmarshal([]byte(spec.InputSchemaJSON), &schema); err != nil {
					t.Fatalf("decode %s schema: %v", action, err)
				}
				break
			}
		}
		properties, ok := schema["properties"].(map[string]any)
		if !ok {
			t.Fatalf("%s schema has no properties: %#v", action, schema)
		}
		expected, ok := properties["expected_version"].(map[string]any)
		if !ok || expected["minLength"] != float64(1) {
			t.Fatalf("%s expected_version schema = %#v, want minLength 1", action, expected)
		}
	}
}

func TestToolMutationDispatchRejectsBlankExpectedVersionBeforeService(t *testing.T) {
	svc, userID, _, ctx := commonMCPTestService(t)
	configID, _, _ := seedCommonConfig(t, svc.pool, userID, 1, AuthTypeNone)
	authority, err := authz.NewUserAuthority(authz.UserID(userID), true)
	if err != nil {
		t.Fatalf("new authority: %v", err)
	}
	access, err := NewAccess(svc, nil, nil).Begin(authority)
	if err != nil {
		t.Fatalf("begin access: %v", err)
	}
	handler := managementHandler{access: access}
	for _, tc := range []struct {
		action string
		args   map[string]any
	}{
		{action: "update", args: map[string]any{"expected_version": "", "enabled": false}},
		{action: "delete", args: map[string]any{"expected_version": ""}},
	} {
		t.Run(tc.action, func(t *testing.T) {
			args := make(map[string]any, len(tc.args)+1)
			maps.Copy(args, tc.args)
			args["id"] = configID
			if _, err := SettingsMcpDispatch(ctx, handler, tc.action, args); !errors.Is(err, ErrVersionConflict) {
				t.Fatalf("dispatch = %v, want version conflict", err)
			}
		})
	}
	var revision int64
	if err := svc.pool.QueryRow(t.Context(), `SELECT revision FROM plugin_config WHERE id = $1::uuid`, configID).Scan(&revision); err != nil {
		t.Fatal(err)
	}
	if revision != 1 {
		t.Fatalf("blank version changed config revision to %d", revision)
	}
}

func TestUpdateIfVersionRejectsChangedRegistration(t *testing.T) {
	svc, userID, _, ctx := commonMCPTestService(t)
	configID, _, _ := seedCommonConfig(t, svc.pool, userID, 1, AuthTypeNone)
	observed, err := svc.Get(ctx, configID, ScopeUser, userID, "")
	if err != nil {
		t.Fatalf("get common registration: %v", err)
	}

	// Simulate a committed write between the caller's get and its mutation.
	if _, err := svc.pool.Exec(t.Context(), `UPDATE plugin_config SET revision = revision + 1, updated_at = now() WHERE id = $1::uuid`, configID); err != nil {
		t.Fatal(err)
	}
	name := "after"
	_, err = svc.UpdateIfVersion(ctx, UpdateInput{ID: configID, Scope: ScopeUser, UserID: userID, Name: &name}, observed.Version())
	if !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("UpdateIfVersion error = %v, want version conflict", err)
	}
	if err := svc.DeleteIfVersion(ctx, configID, ScopeUser, userID, "", observed.Version()); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("DeleteIfVersion error = %v, want version conflict", err)
	}
	var count int
	if err := svc.pool.QueryRow(t.Context(), `SELECT count(*) FROM plugin_config WHERE id = $1::uuid`, configID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatal("stale delete removed common config")
	}
}

func TestToolVersionPathsRejectBlankBeforeMutation(t *testing.T) {
	svc, userID, _, ctx := commonMCPTestService(t)
	configID, _, _ := seedCommonConfig(t, svc.pool, userID, 1, AuthTypeNone)
	name := "after"
	if _, err := svc.UpdateIfVersion(ctx, UpdateInput{ID: configID, Scope: ScopeUser, UserID: userID, Name: &name}, ""); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("blank UpdateIfVersion = %v, want version conflict", err)
	}
	if err := svc.DeleteIfVersion(ctx, configID, ScopeUser, userID, "", ""); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("blank DeleteIfVersion = %v, want version conflict", err)
	}
	var revision int64
	if err := svc.pool.QueryRow(t.Context(), `SELECT revision FROM plugin_config WHERE id = $1::uuid`, configID).Scan(&revision); err != nil {
		t.Fatal(err)
	}
	if revision != 1 {
		t.Fatalf("blank version changed config revision to %d", revision)
	}
}

// fakeVault records the plaintext handed to it, keyed by name, and returns it
// back on GetScoped. It stands in for the age-encrypting vault in unit tests;
// the integration test proves the ciphertext is unreadable at rest.
type fakeVault struct {
	stored map[string]string
}

func newFakeVault() *fakeVault { return &fakeVault{stored: map[string]string{}} }

func commonMCPTestService(t *testing.T) (*Service, string, string, context.Context) {
	t.Helper()
	svc, _, userID, _ := setupInternal(t)
	pluginID := "custom/" + uuid.NewString()
	namespace := "mcp_test_" + strings.ReplaceAll(pluginID[len("custom/"):], "-", "")[:8]
	if _, err := svc.pool.Exec(t.Context(), `
		INSERT INTO plugin_definition(id, namespace, display_name, backend, source,
			implementation_key, spec, default_enabled, revision, creator_user_id)
		VALUES ($1, $2, $3, 'mcp', 'custom', 'mcp', '{}'::jsonb, false, 1, $4::uuid)`,
		pluginID, namespace, "MCP test", userID); err != nil {
		t.Fatalf("seed common MCP definition: %v", err)
	}
	preparePluginToolOverrideIdentitySchema(t, svc.pool)
	authority, err := authz.NewUserAuthority(authz.UserID(userID), true)
	if err != nil {
		t.Fatalf("new test authority: %v", err)
	}
	return svc, userID, pluginID, authz.WithAuthority(t.Context(), authority)
}

// Migration 41 deliberately leaves tool policy identity for the coordinated
// policy migration. These tests install the reviewed shape locally so the
// common config fixture can exercise plugin/local identities without changing
// production DDL.
func preparePluginToolOverrideIdentitySchema(t *testing.T, pool interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
},
) {
	t.Helper()
	if _, err := pool.Exec(t.Context(), `
		ALTER TABLE tool_override ALTER COLUMN tool_name DROP NOT NULL;
		ALTER TABLE tool_override DROP CONSTRAINT IF EXISTS tool_override_tool_name_scope_user_id_agent_id_key;
		CREATE UNIQUE INDEX IF NOT EXISTS test_tool_override_plugin_identity
			ON tool_override (plugin_id, local_tool_name, scope, user_id, agent_id) NULLS NOT DISTINCT
			WHERE tool_name IS NULL;
	`); err != nil {
		t.Fatalf("prepare plugin tool override identity schema: %v", err)
	}
}

func commonMCPRegistration(t *testing.T, authType string) (*Service, Registration, string, context.Context) {
	t.Helper()
	svc, _, userID, _ := setupInternal(t)
	configID, _, _ := seedCommonConfig(t, svc.pool, userID, 1, authType)
	authority, err := authz.NewUserAuthority(authz.UserID(userID), true)
	if err != nil {
		t.Fatalf("new test authority: %v", err)
	}
	ctx := authz.WithAuthority(t.Context(), authority)
	reg, err := svc.Get(ctx, configID, ScopeUser, userID, "")
	if err != nil {
		t.Fatalf("get common MCP registration: %v", err)
	}
	reg.Name, reg.Namespace = "gh", "gh"
	return svc, reg, userID, ctx
}

func vaultKey(scope, userID, agentID, name string) string {
	return scope + "|" + userID + "|" + agentID + "|" + name
}

func (v *fakeVault) SetScoped(_ context.Context, scope, userID, agentID, name, plaintext string) error {
	v.stored[vaultKey(scope, userID, agentID, name)] = plaintext
	return nil
}

func (v *fakeVault) SetSystemScoped(_ context.Context, scope, agentID, name, plaintext string) error {
	v.stored[vaultKey(scope, "", agentID, name)] = plaintext
	return nil
}

func (v *fakeVault) GetScoped(_ context.Context, scope, userID, agentID, name string) (string, error) {
	return v.stored[vaultKey(scope, userID, agentID, name)], nil
}

func (v *fakeVault) DeleteScoped(_ context.Context, scope, userID, agentID, name string) error {
	delete(v.stored, vaultKey(scope, userID, agentID, name))
	return nil
}

func (v *fakeVault) DeleteSystemScoped(_ context.Context, scope, agentID, name string) error {
	delete(v.stored, vaultKey(scope, "", agentID, name))
	return nil
}

func TestValidTransportRejectsStdio(t *testing.T) {
	if ValidTransport("stdio") {
		t.Fatal("stdio must be rejected")
	}
	if !ValidTransport(TransportStreamableHTTP) || !ValidTransport(TransportSSE) {
		t.Fatal("streamable_http and sse must be accepted")
	}
	if ValidTransport("") || ValidTransport("websocket") {
		t.Fatal("only HTTP-based transports are valid")
	}
}

func TestValidateEndpointURLRejectsUnsafeTargets(t *testing.T) {
	bad := []string{
		"ftp://example.com/mcp",
		"https://user:pass@example.com/mcp",
		"http://localhost/mcp",
		"http://127.0.0.1/mcp",
		"http://10.0.0.1/mcp",
		"http://172.16.0.1/mcp",
		"http://192.168.1.1/mcp",
		"http://169.254.169.254/latest/meta-data",
		"http://100.64.0.1/mcp",
		"http://100.127.255.254/mcp",
		"http://[::1]/mcp",
		"http://[64:ff9b::c000:201]/mcp",
		"http://[::ffff:100.64.0.1]/mcp",
		"https://example.com/mcp?token=secret",
		"https://example.com/mcp#secret",
	}
	for _, raw := range bad {
		if err := (EndpointPolicy{}).validateEndpointURL(raw); err == nil {
			t.Fatalf("validateEndpointURL(%q) succeeded, want rejection", raw)
		}
	}
	for _, raw := range []string{
		"https://example.com/mcp",
		"http://100.63.255.255/mcp",
		"http://100.128.0.0/mcp",
		"https://[64:ff9b:1::1]/mcp",
	} {
		if err := (EndpointPolicy{}).validateEndpointURL(raw); err != nil {
			t.Fatalf("public endpoint %q rejected: %v", raw, err)
		}
	}
}

type recordingRoundTripper struct{ request *http.Request }

func (r *recordingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	r.request = req
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       http.NoBody,
		Request:    req,
	}, nil
}

func TestAuthRoundTripperClonesRequestBeforeAddingBearer(t *testing.T) {
	request, err := http.NewRequest(http.MethodGet, "https://example.com/mcp", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	request.Header.Set("X-Caller", "unchanged")
	base := &recordingRoundTripper{}
	response, err := (&authRoundTripper{base: base, bearer: "secret"}).RoundTrip(request)
	if err != nil {
		t.Fatalf("round trip: %v", err)
	}
	if err := response.Body.Close(); err != nil {
		t.Fatalf("close response body: %v", err)
	}
	if base.request == request {
		t.Fatal("outbound request must be cloned before adding authorization")
	}
	if got := base.request.Header.Get("Authorization"); got != "Bearer secret" {
		t.Fatalf("outbound authorization = %q, want bearer token", got)
	}
	if got := request.Header.Get("Authorization"); got != "" {
		t.Fatalf("original request authorization = %q, want unchanged", got)
	}
	if got := request.Header.Get("X-Caller"); got != "unchanged" {
		t.Fatalf("original request header = %q, want unchanged", got)
	}
}

func TestAuthRoundTripperOmitsEmptyBearer(t *testing.T) {
	request, err := http.NewRequest(http.MethodGet, "https://example.com/mcp", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	base := &recordingRoundTripper{}
	response, err := (&authRoundTripper{base: base}).RoundTrip(request)
	if err != nil {
		t.Fatalf("round trip: %v", err)
	}
	if err := response.Body.Close(); err != nil {
		t.Fatalf("close response body: %v", err)
	}
	if got := base.request.Header.Get("Authorization"); got != "" {
		t.Fatalf("outbound authorization = %q, want none", got)
	}
}

func TestSafeHTTPClientRedirectPolicy(t *testing.T) {
	client := safeHTTPClient("secret", EndpointPolicy{})
	first, err := http.NewRequest(http.MethodGet, "https://example.com/mcp", nil)
	if err != nil {
		t.Fatalf("new first request: %v", err)
	}
	for _, tc := range []struct {
		name    string
		target  string
		wantErr bool
	}{
		{name: "same origin", target: "https://EXAMPLE.COM/next"},
		{name: "other public origin", target: "https://other.example.com/next", wantErr: true},
		{name: "unsafe private target", target: "http://127.0.0.1/next", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			target, err := http.NewRequest(http.MethodGet, tc.target, nil)
			if err != nil {
				t.Fatalf("new redirect request: %v", err)
			}
			err = client.CheckRedirect(target, []*http.Request{first})
			if (err != nil) != tc.wantErr {
				t.Fatalf("CheckRedirect(%q) error = %v, wantErr = %v", tc.target, err, tc.wantErr)
			}
		})
	}
}

func TestBuildTransportRejectsStdio(t *testing.T) {
	if _, err := buildBearerTransport(Registration{Transport: "stdio", URL: "http://x"}, "", EndpointPolicy{}); err == nil {
		t.Fatal("buildTransport must reject stdio")
	}
	tr, err := buildBearerTransport(Registration{Transport: TransportStreamableHTTP, URL: "http://x"}, "tok", EndpointPolicy{})
	if err != nil {
		t.Fatalf("streamable_http: %v", err)
	}
	if _, ok := tr.(*mcpsdk.StreamableClientTransport); !ok {
		t.Fatalf("streamable_http: got %T", tr)
	}
	tr, err = buildBearerTransport(Registration{Transport: TransportSSE, URL: "http://x"}, "", EndpointPolicy{})
	if err != nil {
		t.Fatalf("sse: %v", err)
	}
	if _, ok := tr.(*mcpsdk.SSEClientTransport); !ok {
		t.Fatalf("sse: got %T", tr)
	}
}

func TestCreateRejectsStdioTransport(t *testing.T) {
	err := validateRegistration(ScopeUser, "gh", "http://x", "stdio", AuthTypeNone, EndpointPolicy{})
	if err == nil {
		t.Fatal("Create must reject stdio transport")
	}
}

func TestCreateBearerStoresTokenInVaultNotRow(t *testing.T) {
	svc, userID, pluginID, ctx := commonMCPTestService(t)
	const token = "secret-bearer-123"

	reg, err := svc.Create(ctx, CreateInput{
		PluginID: pluginID, Scope: ScopeUser, UserID: userID, Name: "gh", URL: "https://mcp.example.com",
		Transport: TransportStreamableHTTP, AuthType: AuthTypeBearer, Token: token,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// The row must reference the credential, never carry the secret itself.
	configs, err := sqlc.New(svc.pool).ListPluginConfigs(ctx, reg.PluginID)
	if err != nil || len(configs) != 1 {
		t.Fatalf("common config rows = %d, err=%v", len(configs), err)
	}
	if !strings.Contains(string(configs[0].CredentialRefs), credentialName(reg.ID)) {
		t.Fatal("credential_ref must be set for bearer auth")
	}
	if strings.Contains(string(configs[0].CredentialRefs), token) || strings.Contains(string(configs[0].Config), token) {
		t.Fatal("common config must not contain the bearer token")
	}

	// The token must have been handed to the vault under the credential name.
	got, err := svc.vault.GetScoped(ctx, ScopeUser, userID, "", reg.CredentialRef)
	if err != nil {
		t.Fatalf("token was not stored in the vault under %q: %v", reg.CredentialRef, err)
	}
	if got != token {
		t.Fatalf("vault stored %q, want the raw token %q", got, token)
	}

	// And the service can read it back for connecting.
	back, err := svc.BearerToken(ctx, reg)
	if err != nil {
		t.Fatalf("BearerToken: %v", err)
	}
	if back != token {
		t.Fatalf("BearerToken = %q, want %q", back, token)
	}

	// The credential name must be a valid vault entry name.
	if err := vault.ValidateName(reg.CredentialRef); err != nil {
		t.Fatalf("credential name %q is not a valid vault name: %v", reg.CredentialRef, err)
	}
}

func strPtr(s string) *string { return &s }

func TestUpdateBearerRejectsScopeMoveWithoutReplacement(t *testing.T) {
	svc, userID, pluginID, ctx := commonMCPTestService(t)
	reg, err := svc.Create(ctx, CreateInput{
		PluginID: pluginID, Scope: ScopeUser, UserID: userID, Name: "gh", URL: "https://old.example.com",
		Transport: TransportStreamableHTTP, AuthType: AuthTypeBearer, Token: "secret",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	_, err = svc.Update(ctx, UpdateInput{
		ID: reg.ID, Scope: ScopeUser, UserID: userID,
		NewScope: strPtr(ScopeUserAgent), NewUserID: userID, NewAgentID: "oauth-test-agent",
		URL: strPtr("https://new.example.com"),
	})
	if err == nil {
		t.Fatal("moving bearer registration without replacement credentials must fail")
	}
	if got, err := svc.vault.GetScoped(ctx, ScopeUserAgent, userID, "oauth-test-agent", reg.CredentialRef); err == nil && got != "" {
		t.Fatal("scope move must not copy the existing bearer")
	}
	if got, err := svc.vault.GetScoped(ctx, ScopeUser, userID, "", reg.CredentialRef); err != nil || got != "secret" {
		t.Fatalf("original token = %q, want unchanged", got)
	}
}

func TestUpdateAuthNonePurgesToken(t *testing.T) {
	svc, userID, pluginID, ctx := commonMCPTestService(t)
	reg, err := svc.Create(ctx, CreateInput{
		PluginID: pluginID, Scope: ScopeUser, UserID: userID, Name: "gh", URL: "https://mcp.example.com",
		Transport: TransportStreamableHTTP, AuthType: AuthTypeBearer, Token: "secret",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	updated, err := svc.Update(ctx, UpdateInput{
		ID: reg.ID, Scope: ScopeUser, UserID: userID, AuthType: strPtr(AuthTypeNone),
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if updated.AuthType != AuthTypeNone || updated.CredentialRef != "" {
		t.Fatalf("updated auth = %q cred = %q", updated.AuthType, updated.CredentialRef)
	}
	if got, err := svc.vault.GetScoped(ctx, ScopeUser, userID, "", reg.CredentialRef); err == nil && got != "" {
		t.Fatal("token should be deleted when auth switches to none")
	}
}

func TestCreateBearerRequiresTokenAndVault(t *testing.T) {
	// Missing token.
	svc, userID, pluginID, ctx := commonMCPTestService(t)
	if _, err := svc.Create(ctx, CreateInput{
		PluginID: pluginID, Scope: ScopeUser, UserID: userID, Name: "gh", URL: "https://mcp.example.test",
		Transport: TransportStreamableHTTP, AuthType: AuthTypeBearer,
	}); err == nil {
		t.Fatal("bearer without token must fail")
	}
	// No vault configured.
	svc.bindVault = nil
	if _, err := svc.Create(ctx, CreateInput{
		PluginID: pluginID, Scope: ScopeUser, UserID: userID, Name: "gh", URL: "https://mcp.example.test",
		Transport: TransportStreamableHTTP, AuthType: AuthTypeBearer, Token: "t",
	}); err == nil {
		t.Fatal("bearer without vault must fail")
	}
}

func TestCreateScopeOwnerValidation(t *testing.T) {
	cases := []struct {
		name    string
		in      CreateInput
		wantErr bool
	}{
		{"user ok", CreateInput{Scope: ScopeUser, UserID: "u1", Name: "n", URL: "http://x"}, false},
		{"user missing user_id", CreateInput{Scope: ScopeUser, Name: "n", URL: "http://x"}, true},
		{"user with agent", CreateInput{Scope: ScopeUser, UserID: "u1", AgentID: "a1", Name: "n", URL: "http://x"}, true},
		{"user_agent ok", CreateInput{Scope: ScopeUserAgent, UserID: "u1", AgentID: "a1", Name: "n", URL: "http://x"}, false},
		{"user_agent missing agent", CreateInput{Scope: ScopeUserAgent, UserID: "u1", Name: "n", URL: "http://x"}, true},
		{"system ok", CreateInput{Scope: ScopeSystem, Name: "n", URL: "http://x"}, false},
		{"system with user", CreateInput{Scope: ScopeSystem, UserID: "u1", Name: "n", URL: "http://x"}, true},
		{"system_agent ok", CreateInput{Scope: ScopeSystemAgent, AgentID: "a1", Name: "n", URL: "http://x"}, false},
		{"bad scope", CreateInput{Scope: "nope", Name: "n", URL: "http://x"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateScopeOwner(tc.in.Scope, tc.in.UserID, tc.in.AgentID)
			if tc.wantErr != (err != nil) {
				t.Fatalf("scope validation(%+v) err=%v, wantErr=%v", tc.in, err, tc.wantErr)
			}
		})
	}
}

type fakeMCPClient struct {
	tools      []*mcpsdk.Tool
	listErr    error
	listFn     func(context.Context) ([]*mcpsdk.Tool, error)
	result     *mcpsdk.CallToolResult
	callErr    error
	callFn     func(context.Context, string, map[string]any) (*mcpsdk.CallToolResult, error)
	closeOnce  sync.Once
	closeCalls atomic.Int32
	closeCount atomic.Int32
}

func (c *fakeMCPClient) ListTools(ctx context.Context) ([]*mcpsdk.Tool, error) {
	if c.listFn != nil {
		return c.listFn(ctx)
	}
	return c.tools, c.listErr
}

func (c *fakeMCPClient) CallTool(ctx context.Context, name string, args map[string]any) (*mcpsdk.CallToolResult, error) {
	if c.callFn != nil {
		return c.callFn(ctx, name, args)
	}
	return c.result, c.callErr
}

func (c *fakeMCPClient) Close() error {
	c.closeCalls.Add(1)
	c.closeOnce.Do(func() { c.closeCount.Add(1) })
	return nil
}

// seedRow inserts a registration row with probe state for provider tests.
func seedRow(d *fakeDB, id, name, status string, probedAt time.Time, tools []CatalogTool) {
	toolsJSON, err := json.Marshal(tools)
	if err != nil {
		toolsJSON = []byte("[]")
	}
	d.rows[id] = sqlc.McpServer{
		ID: id, Scope: ScopeUser, UserID: pgnull.Text("u1"), Name: name, Url: "https://mcp.example.com",
		Transport: TransportStreamableHTTP, AuthType: AuthTypeNone, Enabled: true,
		Status: status, ProbedAt: pgtype.Timestamptz{Time: probedAt, Valid: !probedAt.IsZero()}, Tools: toolsJSON,
		UpdatedAt: probedAt,
	}
	d.forCtx = append(d.forCtx, d.rows[id])
}

func catalogRow(d *fakeDB, id, name string, toolNames ...string) {
	now := time.Now().UTC()
	tools := make([]CatalogTool, 0, len(toolNames))
	for _, n := range toolNames {
		tools = append(tools, CatalogTool{Name: n, Description: "desc", InputSchema: map[string]any{"type": "object"}})
	}
	seedRow(d, id, name, StatusOK, now, tools)
}

func providerRegistration(d *fakeDB, id, namespace string) Registration {
	reg := registrationFromRow(d.rows[id])
	reg.Namespace = namespace
	return reg
}

func TestToolProviderUsesPersistedCatalogWithoutConnecting(t *testing.T) {
	db := newFakeDB()
	catalogRow(db, "srv1", "gh", "create_issue")
	svc := NewService(db, newFakeVault())
	connects := 0
	svc.connect = func(context.Context, Registration, CredentialOwner) (RemoteClient, error) {
		connects++
		return &fakeMCPClient{}, nil
	}
	provider := NewToolProvider(svc)

	tools := provider.toolsForRegistrations(context.Background(), []Registration{providerRegistration(db, "srv1", "gh")}, true, "u1")
	if connects != 0 {
		t.Fatalf("connects = %d, want 0 with a fresh persisted catalog", connects)
	}
	if len(tools) != 1 || tools[0].Definition().Name != "gh__create_issue" {
		t.Fatalf("tools = %#v, want the cataloged tool", tools)
	}
}

func TestToolProviderStaleCatalogTriggersColdDiscovery(t *testing.T) {
	svc, reg, userID, ctx := commonMCPRegistration(t, AuthTypeNone)
	reg.Status = StatusOK
	reg.ProbedAt = time.Now().UTC().Add(-25 * time.Hour)
	reg.Tools = []CatalogTool{{Name: "old_tool", InputSchema: map[string]any{"type": "object"}}}
	svc.probeTimeout = time.Second
	connects := 0
	svc.connect = func(context.Context, Registration, CredentialOwner) (RemoteClient, error) {
		connects++
		return &fakeMCPClient{tools: []*mcpsdk.Tool{
			{Name: "new_tool", Description: "fresh", InputSchema: map[string]any{"type": "object"}},
		}}, nil
	}
	provider := NewToolProvider(svc)

	tools := provider.toolsForRegistrations(ctx, []Registration{reg}, true, userID)
	if connects != 1 {
		t.Fatalf("connects = %d, want 1 cold discovery", connects)
	}
	if len(tools) != 1 || tools[0].Definition().Name != "gh__new_tool" {
		t.Fatalf("tools = %#v, want tools from the refreshed catalog", tools)
	}
	stateCount := 0
	if err := svc.pool.QueryRow(t.Context(), `SELECT count(*) FROM mcp_connection_state WHERE config_id = $1::uuid`, reg.ID).Scan(&stateCount); err != nil {
		t.Fatal(err)
	}
	if stateCount != 1 {
		t.Fatalf("cold discovery result not persisted: %d rows", stateCount)
	}
}

func TestToolProviderNeedsAuthSkippedWithoutConnecting(t *testing.T) {
	db := newFakeDB()
	seedRow(db, "srv1", "gh", StatusNeedsAuth, time.Now().UTC(), nil)
	svc := NewService(db, newFakeVault())
	svc.connect = func(context.Context, Registration, CredentialOwner) (RemoteClient, error) {
		t.Fatal("needs_auth server must be skipped without connecting")
		return nil, nil
	}
	if tools := NewToolProvider(svc).toolsForRegistrations(context.Background(), []Registration{providerRegistration(db, "srv1", "gh")}, true, "u1"); len(tools) != 0 {
		t.Fatalf("tools = %d, want none", len(tools))
	}
}

func TestToolProviderDiscoversServersConcurrently(t *testing.T) {
	svc, reg, userID, ctx := commonMCPRegistration(t, AuthTypeNone)
	svc.probeTimeout = time.Second
	var current atomic.Int32
	var maxSeen atomic.Int32
	svc.connect = func(ctx context.Context, reg Registration, _ CredentialOwner) (RemoteClient, error) {
		cur := current.Add(1)
		for {
			max := maxSeen.Load()
			if cur <= max || maxSeen.CompareAndSwap(max, cur) {
				break
			}
		}
		select {
		case <-time.After(50 * time.Millisecond):
		case <-ctx.Done():
		}
		current.Add(-1)
		return &fakeMCPClient{tools: []*mcpsdk.Tool{{Name: reg.Name, InputSchema: map[string]any{"type": "object"}}}}, nil
	}
	provider := NewToolProvider(svc)
	provider.concurrency = 3

	regs := []Registration{reg}
	for i := 1; i < 3; i++ {
		scope, ownerUser, ownerAgent := ScopeUser, userID, ""
		switch i {
		case 1:
			scope, ownerAgent = ScopeUserAgent, "oauth-test-agent"
		case 2:
			scope, ownerUser, ownerAgent = ScopeSystemAgent, "", "oauth-test-agent"
		}
		copy, err := svc.Create(ctx, CreateInput{
			PluginID: reg.PluginID, Scope: scope, UserID: ownerUser, AgentID: ownerAgent,
			Name: fmt.Sprintf("gh%d", i), URL: "https://mcp.example.test",
			Transport: TransportStreamableHTTP, AuthType: AuthTypeNone,
		})
		if err != nil {
			t.Fatalf("Create concurrent config %d: %v", i, err)
		}
		copy.Name = fmt.Sprintf("gh%d", i)
		copy.Namespace = reg.Namespace
		regs = append(regs, copy)
	}
	tools := provider.toolsForRegistrations(ctx, regs, true, userID)
	if len(tools) != 3 {
		t.Fatalf("tools = %d, want 3", len(tools))
	}
	if maxSeen.Load() < 2 {
		t.Fatalf("MCP discovery ran serially; max concurrency = %d", maxSeen.Load())
	}
}

func TestToolProviderFailedDiscoveryPersistsErrorAndSkips(t *testing.T) {
	svc, reg, userID, ctx := commonMCPRegistration(t, AuthTypeNone)
	reg.Name, reg.Namespace, reg.Status, reg.ProbedAt, reg.Tools = "broken", "broken", StatusUnknown, time.Time{}, nil
	svc.probeTimeout = time.Second
	client := &fakeMCPClient{listErr: errors.New("list failed")}
	svc.connect = func(context.Context, Registration, CredentialOwner) (RemoteClient, error) { return client, nil }

	if tools := NewToolProvider(svc).toolsForRegistrations(ctx, []Registration{reg}, true, userID); len(tools) != 0 {
		t.Fatalf("tools = %d, want none after discovery failure", len(tools))
	}
	var status string
	if err := svc.pool.QueryRow(t.Context(), `SELECT status FROM mcp_connection_state WHERE config_id = $1::uuid`, reg.ID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != StatusError {
		t.Fatalf("discovery failure not persisted: %q", status)
	}
	if got := client.closeCount.Load(); got != 1 {
		t.Fatalf("failed discovery close count = %d, want 1 (probe closes its client)", got)
	}
}

func TestToolProviderSkipsCollidingToolNames(t *testing.T) {
	db := newFakeDB()
	catalogRow(db, "srv1", "git-hub", "foo-bar")
	catalogRow(db, "srv2", "git_hub", "foo-bar")
	svc := NewService(db, newFakeVault())
	svc.connect = func(context.Context, Registration, CredentialOwner) (RemoteClient, error) {
		t.Fatal("persisted catalogs must not connect")
		return nil, nil
	}

	tools := NewToolProvider(svc).toolsForRegistrations(context.Background(), []Registration{
		providerRegistration(db, "srv1", "git_hub"),
		providerRegistration(db, "srv2", "git_hub"),
	}, false, "u1")
	if len(tools) != 1 {
		t.Fatalf("tools = %d, want one duplicate skipped", len(tools))
	}
	if got := tools[0].Definition().Name; got != "git_hub__foo_bar" {
		t.Fatalf("tool name = %q", got)
	}
}

func TestProbeSuccessPersistsToolsAndStatus(t *testing.T) {
	svc, reg, userID, ctx := commonMCPRegistration(t, AuthTypeNone)
	svc.probeTimeout = time.Second
	svc.connect = func(context.Context, Registration, CredentialOwner) (RemoteClient, error) {
		return &fakeMCPClient{tools: []*mcpsdk.Tool{
			{Name: "create_issue", Description: "Create issue", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"title": map[string]any{"type": "string"}}}, Annotations: &mcpsdk.ToolAnnotations{ReadOnlyHint: true}},
		}}, nil
	}

	updated, err := svc.Probe(ctx, reg, svc.CredentialOwner(reg, userID))
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if updated.Status != StatusOK || updated.StatusError != "" {
		t.Fatalf("status = %q/%q, want ok with no error", updated.Status, updated.StatusError)
	}
	if updated.ProbedAt.IsZero() {
		t.Fatal("probed_at not set")
	}
	if len(updated.Tools) != 1 || updated.Tools[0].Name != "create_issue" {
		t.Fatalf("tools = %#v, want the remote tool snapshot", updated.Tools)
	}
	if updated.Tools[0].InputSchema["type"] != "object" {
		t.Fatalf("input_schema = %#v", updated.Tools[0].InputSchema)
	}
	if got, ok := updated.Tools[0].Annotations["readOnlyHint"].(bool); !ok || !got {
		t.Fatalf("annotations = %#v, want readOnlyHint true", updated.Tools[0].Annotations)
	}
	if updated.ConfigRevision != reg.ConfigRevision {
		t.Fatal("probe must not change config revision")
	}
}

func TestProbeFailurePersistsRedactedError(t *testing.T) {
	svc, reg, userID, ctx := commonMCPRegistration(t, AuthTypeNone)
	svc.probeTimeout = time.Second
	svc.connect = func(context.Context, Registration, CredentialOwner) (RemoteClient, error) {
		return nil, fmt.Errorf("dial https://user:pass@example.com/mcp?token=secret failed")
	}

	updated, err := svc.Probe(ctx, reg, svc.CredentialOwner(reg, userID))
	if err != nil {
		t.Fatalf("Probe must not fail the caller: %v", err)
	}
	if updated.Status != StatusError {
		t.Fatalf("status = %q, want error", updated.Status)
	}
	msg := updated.StatusError
	for _, secret := range []string{"user:pass", "token=secret", "https://mcp.example.com"} {
		if strings.Contains(msg, secret) {
			t.Fatalf("probe error leaked %q: %s", secret, msg)
		}
	}
	if msg != "MCP probe failed" {
		t.Fatalf("probe error = %q, want bounded error", msg)
	}
}

func TestIsCredentialRejectionIncludesOAuthHint(t *testing.T) {
	if !isCredentialRejection(fmt.Errorf("wrapped: %s", credentialRejectedHint)) {
		t.Fatal("OAuth reconnect hint must classify as a credential rejection")
	}
}

func TestProbeCredentialRejectionSetsNeedsAuth(t *testing.T) {
	svc, reg, userID, ctx := commonMCPRegistration(t, AuthTypeBearer)
	svc.probeTimeout = time.Second
	svc.connect = func(context.Context, Registration, CredentialOwner) (RemoteClient, error) {
		return &fakeMCPClient{listErr: errors.New("tools/list: Unauthorized")}, nil
	}

	updated, err := svc.Probe(ctx, reg, svc.CredentialOwner(reg, userID))
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if updated.Status != StatusNeedsAuth {
		t.Fatalf("status = %q, want needs_auth", updated.Status)
	}
}

func TestProbeSkipsOAuthWhenObservationNeedsAuth(t *testing.T) {
	svc, reg, userID, ctx := commonMCPRegistration(t, AuthTypeOAuth)
	called := false
	svc.connect = func(context.Context, Registration, CredentialOwner) (RemoteClient, error) {
		called = true
		return nil, errors.New("dead refresh token must not be retried")
	}
	authority, ok := authz.AuthorityFromContext(ctx)
	if !ok {
		t.Fatal("test context has no authority")
	}
	if err := svc.persistCommonStatus(ctx, reg, svc.CredentialOwner(reg, userID), StatusNeedsAuth, credentialRejectedHint); err != nil {
		t.Fatalf("seed needs_auth observation: %v", err)
	}
	access, err := NewAccess(svc, nil, nil).Begin(authority)
	if err != nil {
		t.Fatalf("begin MCP access: %v", err)
	}

	updated, err := access.Probe(ctx, reg.ID, ScopeUser, "")
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if called {
		t.Fatal("Probe connected with a terminal needs_auth observation")
	}
	if updated.Status != StatusNeedsAuth {
		t.Fatalf("status = %q, want needs_auth", updated.Status)
	}
	updated, err = access.Probe(ctx, reg.ID, ScopeUser, "")
	if err != nil {
		t.Fatalf("repeat Probe: %v", err)
	}
	if called {
		t.Fatal("repeat Probe connected with a terminal needs_auth observation")
	}
	if updated.Status != StatusNeedsAuth {
		t.Fatalf("repeat status = %q, want needs_auth", updated.Status)
	}
}

func TestExecuteContentConvertsImageBlocks(t *testing.T) {
	proxy := &toolProxy{
		reg:        Registration{Name: "gh", Namespace: "gh"},
		remoteName: "render",
		def:        pkgtools.Definition{Name: "gh__render"},
		svc:        NewService(newFakeDB(), newFakeVault()),
	}
	svc := proxy.svc
	svc.connect = func(context.Context, Registration, CredentialOwner) (RemoteClient, error) {
		return &fakeMCPClient{result: &mcpsdk.CallToolResult{Content: []mcpsdk.Content{
			&mcpsdk.TextContent{Text: "here"},
			&mcpsdk.ImageContent{Data: []byte{0x89, 'P', 'N', 'G'}, MIMEType: "image/png"},
			&mcpsdk.AudioContent{Data: []byte("x"), MIMEType: "audio/wav"}, // non-text/image block: JSON-encoded as text
		}}}, nil
	}
	_ = svc

	blocks, err := proxy.ExecuteContent(context.Background(), nil)
	if err != nil {
		t.Fatalf("ExecuteContent: %v", err)
	}
	if len(blocks) != 3 {
		t.Fatalf("blocks = %d, want 3", len(blocks))
	}
	if tc, ok := blocks[0].(ai.TextContent); !ok || tc.Text != "here" {
		t.Fatalf("block 0 = %#v, want text", blocks[0])
	}
	ic, ok := blocks[1].(ai.ImageContent)
	if !ok {
		t.Fatalf("block 1 = %T, want ai.ImageContent", blocks[1])
	}
	if ic.MimeType != "image/png" {
		t.Fatalf("mime = %q", ic.MimeType)
	}
	if got, err := base64.StdEncoding.DecodeString(ic.Data); err != nil || string(got) != "\x89PNG" {
		t.Fatalf("image data = %q, %v", ic.Data, err)
	}
	// Execute keeps the string contract via the same path.
	text, err := proxy.Execute(context.Background(), nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.HasPrefix(text, "here\n[image: image/png]") {
		t.Fatalf("Execute text = %q", text)
	}
}

func TestCallTimeoutReturnsContextErrorWithoutStatusChange(t *testing.T) {
	db := newFakeDB()
	db.rows["srv1"] = sqlc.McpServer{ID: "srv1", Scope: ScopeUser, Name: "gh", Url: "https://mcp.example.com", Enabled: true}
	proxy := &toolProxy{
		reg:        Registration{ID: "srv1", Name: "gh", Namespace: "gh", Metadata: map[string]any{"call_timeout_seconds": float64(1)}},
		remoteName: "slow",
		def:        pkgtools.Definition{Name: "gh__slow"},
		svc:        NewService(db, newFakeVault()),
	}
	proxy.svc.connect = func(context.Context, Registration, CredentialOwner) (RemoteClient, error) {
		return &fakeMCPClient{callFn: func(ctx context.Context, _ string, _ map[string]any) (*mcpsdk.CallToolResult, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		}}, nil
	}

	start := time.Now()
	_, err := proxy.Execute(context.Background(), nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Execute error = %v, want context deadline", err)
	}
	if elapsed := time.Since(start); elapsed < 900*time.Millisecond {
		t.Fatalf("call returned after %v, want ~1s", elapsed)
	}
	if len(db.statusUpdates) != 0 {
		t.Fatalf("timeout must not change server status: %+v", db.statusUpdates)
	}
}

func TestCredentialRejectionMarksNeedsAuth(t *testing.T) {
	svc, reg, userID, ctx := commonMCPRegistration(t, AuthTypeNone)
	reg.Name, reg.Namespace = "gh", "gh"
	proxy := &toolProxy{
		reg:        reg,
		remoteName: "list",
		def:        pkgtools.Definition{Name: "gh__list"},
		svc:        svc,
	}
	proxy.svc.connect = func(context.Context, Registration, CredentialOwner) (RemoteClient, error) {
		return &fakeMCPClient{callErr: errors.New("tools/call: Forbidden")}, nil
	}

	_, err := proxy.Execute(ctx, nil)
	if err == nil || !strings.Contains(err.Error(), "credential rejected; reconnect in the Web UI") {
		t.Fatalf("Execute error = %v, want credential-rejection guidance", err)
	}
	var status string
	if err := svc.pool.QueryRow(t.Context(), `SELECT status FROM mcp_connection_state WHERE config_id = $1::uuid AND credential_user_id IS NULL`, reg.ID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != StatusNeedsAuth {
		t.Fatalf("status = %q, want needs_auth", status)
	}
	_ = userID
}

func TestCallTimeoutDefaultsAndClamps(t *testing.T) {
	for _, tc := range []struct {
		metadata map[string]any
		want     time.Duration
	}{
		{nil, defaultCallTimeout},
		{map[string]any{"call_timeout_seconds": float64(10)}, 10 * time.Second},
		{map[string]any{"call_timeout_seconds": float64(0)}, defaultCallTimeout},
		{map[string]any{"call_timeout_seconds": float64(100000)}, maxCallTimeoutSeconds * time.Second},
		{map[string]any{"call_timeout_seconds": "ten"}, defaultCallTimeout},
	} {
		if got := callTimeout(Registration{Metadata: tc.metadata}); got != tc.want {
			t.Fatalf("callTimeout(%v) = %v, want %v", tc.metadata, got, tc.want)
		}
	}
}

func TestValidateCredentialMode(t *testing.T) {
	if err := validateCredentialMode("", AuthTypeOAuth); err != nil {
		t.Fatalf("empty = %v, want default", err)
	}
	if err := validateCredentialMode(CredentialModeShared, AuthTypeNone); err != nil {
		t.Fatalf("shared = %v", err)
	}
	if err := validateCredentialMode(CredentialModePerUser, AuthTypeOAuth); err != nil {
		t.Fatalf("per_user + oauth = %v, want accepted", err)
	}
	if err := validateCredentialMode(CredentialModePerUser, AuthTypeBearer); err == nil {
		t.Fatal("per_user without oauth must be rejected")
	}
	if err := validateCredentialMode("yolo", AuthTypeOAuth); err == nil {
		t.Fatal("unknown mode must be rejected")
	}
}

func TestHTTPDeleteIsIdempotentButCASDeleteConflictsWhenAbsent(t *testing.T) {
	svc, userID, _, ctx := commonMCPTestService(t)
	missing := uuid.NewString()
	if err := svc.Delete(ctx, missing, ScopeUser, userID, ""); err != nil {
		t.Fatalf("unconditional delete absent registration: %v", err)
	}
	if err := svc.DeleteIfVersion(ctx, missing, ScopeUser, userID, "", "version"); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("CAS delete absent registration = %v, want ErrVersionConflict", err)
	}
}

func TestDeletePurgesCredential(t *testing.T) {
	svc, userID, pluginID, ctx := commonMCPTestService(t)
	reg, err := svc.Create(ctx, CreateInput{
		PluginID: pluginID, Scope: ScopeUser, UserID: userID, Name: "gh", URL: "https://mcp.example.test",
		AuthType: AuthTypeBearer, Token: "tok",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if got, err := svc.vault.GetScoped(ctx, ScopeUser, userID, "", reg.CredentialRef); err != nil || got != "tok" {
		t.Fatalf("precondition: token should be stored, got %q/%v", got, err)
	}
	if err := svc.Delete(ctx, reg.ID, ScopeUser, userID, ""); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if got, err := svc.vault.GetScoped(ctx, ScopeUser, userID, "", reg.CredentialRef); err == nil && got != "" {
		t.Fatal("Delete must purge the vault credential")
	}
	var count int
	if err := svc.pool.QueryRow(t.Context(), `SELECT count(*) FROM plugin_config WHERE id = $1::uuid`, reg.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("common config row not deleted: %d", count)
	}
}

// Tool policy identity is the stable plugin/local pair. Renaming a config or
// deleting one config must not rewrite or remove policy rows for that plugin.
func TestPluginToolOverridesSurviveConfigRenameAndDelete(t *testing.T) {
	svc, userID, pluginID, ctx := commonMCPTestService(t)
	q := sqlc.New(svc.pool)
	create := func(scope, ownerUser string) Registration {
		t.Helper()
		reg, err := svc.Create(ctx, CreateInput{
			PluginID: pluginID, Scope: scope, UserID: ownerUser, Name: "gh", URL: "https://mcp.example.test",
			Transport: TransportStreamableHTTP, AuthType: AuthTypeNone,
		})
		if err != nil {
			t.Fatalf("Create %s config: %v", scope, err)
		}
		return reg
	}
	system := create(ScopeSystem, "")
	user := create(ScopeUser, userID)
	const localTool = "list"
	for _, arg := range []sqlc.UpsertPluginToolOverrideParams{
		{PluginID: pgnull.Text(pluginID), LocalToolName: pgnull.Text(localTool), Scope: agent.ToolOverrideScopeSystem, Enabled: false},
		{PluginID: pgnull.Text(pluginID), LocalToolName: pgnull.Text(localTool), Scope: agent.ToolOverrideScopeUser, UserID: pgnull.Text(userID), Enabled: true},
	} {
		if _, err := q.UpsertPluginToolOverride(ctx, arg); err != nil {
			t.Fatalf("seed plugin override %+v: %v", arg, err)
		}
	}

	newName := "renamed"
	updated, err := svc.Update(ctx, UpdateInput{ID: user.ID, Scope: ScopeUser, UserID: userID, Name: &newName})
	if err != nil {
		t.Fatalf("rename common config: %v", err)
	}
	if updated.Name != newName || updated.PluginID != pluginID {
		t.Fatalf("renamed registration = %#v", updated)
	}

	rows, err := q.ListToolOverridesForAgentContext(ctx, sqlc.ListToolOverridesForAgentContextParams{
		UserID: pgnull.Text(userID), AgentID: pgnull.Text("oauth-test-agent"),
	})
	if err != nil {
		t.Fatalf("list plugin overrides after rename: %v", err)
	}
	assertPluginOverrideRows(t, rows, pluginID, localTool, 2)

	if err := svc.Delete(ctx, system.ID, ScopeSystem, "", ""); err != nil {
		t.Fatalf("delete system config: %v", err)
	}
	rows, err = q.ListToolOverridesForAgentContext(ctx, sqlc.ListToolOverridesForAgentContextParams{
		UserID: pgnull.Text(userID), AgentID: pgnull.Text("oauth-test-agent"),
	})
	if err != nil {
		t.Fatalf("list plugin overrides after delete: %v", err)
	}
	assertPluginOverrideRows(t, rows, pluginID, localTool, 2)
}

func assertPluginOverrideRows(t *testing.T, rows []sqlc.ToolOverride, pluginID, localTool string, want int) {
	t.Helper()
	got := 0
	for _, row := range rows {
		if row.PluginID.Valid && row.PluginID.String == pluginID && row.LocalToolName.Valid && row.LocalToolName.String == localTool {
			got++
		}
	}
	if got != want {
		t.Fatalf("plugin override rows = %d, want %d, rows=%+v", got, want, rows)
	}
}

func TestEndpointPolicyAllowPrivate(t *testing.T) {
	private := EndpointPolicy{AllowPrivate: true}
	for _, raw := range []string{"http://127.0.0.1:8080/mcp", "http://localhost:3000/mcp", "http://10.0.0.5/mcp", "http://[::1]:9000/mcp"} {
		if err := private.validateEndpointURL(raw); err != nil {
			t.Fatalf("AllowPrivate rejected %q: %v", raw, err)
		}
		if err := (EndpointPolicy{}).validateEndpointURL(raw); err == nil {
			t.Fatalf("default policy accepted %q", raw)
		}
	}
	for _, raw := range []string{"http://0.0.0.0/mcp", "http://224.0.0.1/mcp"} {
		if err := private.validateEndpointURL(raw); err == nil {
			t.Fatalf("AllowPrivate accepted %q, want rejection", raw)
		}
	}
}

func TestIsCredentialRejectionSeesThroughConnectionFailure(t *testing.T) {
	reg := Registration{Name: "guarded", URL: "https://mcp.example.com/mcp"}
	wrapped := connectionError(reg, errors.New("initialize: Unauthorized"))
	if !strings.Contains(wrapped.Error(), "failed") || strings.Contains(wrapped.Error(), "Unauthorized") {
		t.Fatalf("connectionFailure must hide the cause text, got %q", wrapped.Error())
	}
	if !isCredentialRejection(wrapped) {
		t.Fatal("a wrapped 401 must classify as a credential rejection")
	}
	if isCredentialRejection(connectionError(reg, errors.New("dial tcp: connection refused"))) {
		t.Fatal("a wrapped dial failure must not classify as a credential rejection")
	}
	if !isCredentialRejection(connectionError(reg, errors.New(`oauth2: cannot fetch token: 400 Bad Request {"error":"invalid_grant"}`))) {
		t.Fatal("an OAuth invalid_grant refresh failure must classify as a credential rejection")
	}
}
