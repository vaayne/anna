package server_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/CherryHQ/stella/internal/mcp"
	"github.com/CherryHQ/stella/internal/server"
)

// fakeCatalog is a scripted mcp.Catalog for handler tests.
type fakeCatalog struct {
	page       mcp.CatalogPage
	err        error
	gotQ       string
	gotSize    int
	gotToken   string
	gotSource  string
	gotID      string
	getErr     error
	getErrType error
}

func (f *fakeCatalog) Search(_ context.Context, q string, pageSize int, pageToken string) (mcp.CatalogPage, error) {
	f.gotQ, f.gotSize, f.gotToken = q, pageSize, pageToken
	if f.err != nil {
		return mcp.CatalogPage{}, f.err
	}
	return f.page, nil
}

func (f *fakeCatalog) Get(_ context.Context, source, id string) (mcp.CatalogServer, error) {
	f.gotSource, f.gotID = source, id
	if f.getErrType != nil {
		return mcp.CatalogServer{}, f.getErrType
	}
	if f.getErr != nil {
		return mcp.CatalogServer{}, f.getErr
	}
	if len(f.page.Servers) == 0 {
		return mcp.CatalogServer{}, errNotFoundCatalog{}
	}
	return f.page.Servers[0], nil
}

type errNotFoundCatalog struct{}

func (errNotFoundCatalog) Error() string { return "mcp: registry server not found" }

func rateLimitErr(retryAfter string) error {
	return &mcp.RegistryRateLimitError{RetryAfter: retryAfter}
}

func setupRegistryEnv(t *testing.T, catalog mcp.Catalog) *testEnv {
	t.Helper()
	env := setupAdmin(t)
	env.rebuild(t, func(d *server.Deps) {
		d.MCPCatalog = catalog
	})
	return env
}

func TestRegistryListClampsPageSize(t *testing.T) {
	catalog := &fakeCatalog{page: mcp.CatalogPage{Servers: []mcp.CatalogServer{
		{Source: "official", ID: "com.notion/mcp", Name: "com.notion/mcp", URL: "https://mcp.notion.com/mcp", Transport: "streamable_http", Auth: "none"},
	}}}
	env := setupRegistryEnv(t, catalog)

	for _, tc := range []struct{ request, want int }{
		{-1, 20}, {0, 20}, {5, 5}, {500, 50},
	} {
		rr := doRequest(t, env, http.MethodGet, "/api/mcp/registry/servers?page_size=-1", nil)
		_ = rr
		_ = tc
		break
	}
	// One request per clamp case, checked via the recorded page size.
	for _, tc := range []struct {
		query string
		want  int
	}{
		{"", 20},
		{"page_size=0", 20},
		{"page_size=5", 5},
		{"page_size=500", 50},
	} {
		rr := doRequest(t, env, http.MethodGet, "/api/mcp/registry/servers?"+tc.query, nil)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d (body: %s)", rr.Code, rr.Body.String())
		}
		if catalog.gotSize != tc.want {
			t.Fatalf("page_size %q -> catalog received %d, want %d", tc.query, catalog.gotSize, tc.want)
		}
	}
}

func TestRegistryListReturnsServersAndToken(t *testing.T) {
	catalog := &fakeCatalog{page: mcp.CatalogPage{
		Servers: []mcp.CatalogServer{{
			Source: "official", ID: "com.smithery/x", Name: "com.smithery/x",
			URL: "https://x.example/mcp", Transport: "streamable_http", Auth: "bearer",
			Headers: []mcp.CatalogHeader{{Name: "Authorization", Template: "Bearer {key}", Required: true, Secret: true}},
		}},
		NextPageToken: "official:cursor-2",
	}}
	env := setupRegistryEnv(t, catalog)

	rr := doRequest(t, env, http.MethodGet, "/api/mcp/registry/servers?q=smith&page_size=20&page_token=official%3Acursor-1", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d (body: %s)", rr.Code, rr.Body.String())
	}
	if catalog.gotQ != "smith" || catalog.gotToken != "official:cursor-1" {
		t.Fatalf("params = q %q token %q", catalog.gotQ, catalog.gotToken)
	}
	var got struct {
		Servers []struct {
			ID     string `json:"id"`
			Auth   string `json:"auth"`
			Header []struct {
				Template string `json:"template"`
			} `json:"headers"`
		} `json:"servers"`
		NextPageToken *string `json:"next_page_token"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Servers) != 1 || got.Servers[0].Auth != "bearer" {
		t.Fatalf("servers = %#v", got.Servers)
	}
	if got.Servers[0].Header[0].Template != "Bearer {key}" {
		t.Fatalf("header template = %#v", got.Servers[0].Header)
	}
	if got.NextPageToken == nil || *got.NextPageToken != "official:cursor-2" {
		t.Fatalf("next_page_token = %v", got.NextPageToken)
	}
}

func TestRegistryRateLimitedMapsTo503WithRetryAfter(t *testing.T) {
	catalog := &fakeCatalog{err: rateLimitErr("30")}
	env := setupRegistryEnv(t, catalog)

	rr := doRequest(t, env, http.MethodGet, "/api/mcp/registry/servers", nil)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (body: %s)", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("Retry-After"); got != "30" {
		t.Fatalf("Retry-After = %q, want 30", got)
	}
}

func TestRegistryDetailNotFound(t *testing.T) {
	catalog := &fakeCatalog{getErr: errNotFoundCatalog{}}
	env := setupRegistryEnv(t, catalog)

	rr := doRequest(t, env, http.MethodGet, "/api/mcp/registry/servers/official/com.missing%2Fmcp", nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body: %s)", rr.Code, rr.Body.String())
	}
}
