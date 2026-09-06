package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/CherryHQ/stella/api/types"
	"github.com/CherryHQ/stella/internal/authz"
)

func TestStatusReportsActiveSandboxBackendToUnauthenticatedCallers(t *testing.T) {
	t.Setenv("STELLA_SANDBOX_BACKEND", "bridge")
	rr := httptest.NewRecorder()
	(&Server{}).GetStatus(rr, httptest.NewRequest(http.MethodGet, "/api/status", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	var got types.StatusResponse
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.SandboxBackend == nil || *got.SandboxBackend != "bridge" {
		t.Fatalf("sandbox_backend = %v, want bridge", got.SandboxBackend)
	}
}

func TestStatusPluginsRequiresAdminAuthority(t *testing.T) {
	authority, err := authz.NewUserAuthority("user-1", false)
	if err != nil {
		t.Fatal(err)
	}
	if got := (&Server{}).statusPlugins(context.Background(), authority); got != nil {
		t.Fatalf("ordinary user status plugins = %#v, want nil", got)
	}
}
