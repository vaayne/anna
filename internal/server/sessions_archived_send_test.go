package server_test

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/CherryHQ/stella/internal/agent"
	delegatetool "github.com/CherryHQ/stella/internal/agent/delegate"
	agentruntime "github.com/CherryHQ/stella/internal/agent/runtime"
	agentsession "github.com/CherryHQ/stella/internal/agent/session"
	sessionaccess "github.com/CherryHQ/stella/internal/agent/session/access"
	"github.com/CherryHQ/stella/internal/memory"
	"github.com/CherryHQ/stella/internal/memory/memorytest"
	"github.com/CherryHQ/stella/internal/platform/config"
	"github.com/CherryHQ/stella/pkg/ai"
)

// recordingRuntime stands in for the agent pool and reports whether a turn was
// ever started. Nothing else in this test can tell the difference between "the
// send was rejected" and "the send ran and then failed".
type recordingRuntime struct {
	chats atomic.Int64
	stops atomic.Int64
	run   *agentruntime.Runtime
}

func (r *recordingRuntime) Chat(ctx context.Context, req agent.ChatRequest) <-chan agent.Event {
	r.chats.Add(1)
	if r.run != nil {
		return r.run.Chat(ctx, agentsession.Info{
			ID: req.SessionID, UserID: req.UserID, AgentID: req.AgentID,
			Kind: string(req.Kind), Channel: string(req.Channel),
		}, req.Message, req.RuntimeOpts...)
	}
	ch := make(chan agent.Event)
	close(ch)
	return ch
}

func (r *recordingRuntime) RunManagedSession(context.Context, delegatetool.ManagedSessionRequest) (delegatetool.ManagedSessionResult, error) {
	return delegatetool.ManagedSessionResult{}, nil
}

func (r *recordingRuntime) RunConversationSession(context.Context, agentsession.Info, agent.MessageContent) <-chan agent.Event {
	r.chats.Add(1)
	ch := make(chan agent.Event)
	close(ch)
	return ch
}

func (r *recordingRuntime) StopSession(context.Context, string) bool {
	r.stops.Add(1)
	return true
}

func (r *recordingRuntime) SubscribeSession(string) (<-chan agent.Event, func()) {
	ch := make(chan agent.Event)
	close(ch)
	return ch, func() {}
}

func (r *recordingRuntime) SessionLive(string) bool { return false }

func (r *recordingRuntime) CompactAuthorizedSession(context.Context, agentsession.Info) (string, error) {
	return "", nil
}

type excludedToolsRecordingRunner struct {
	excluded chan []string
}

func (r *excludedToolsRecordingRunner) Chat(ctx context.Context, _ []ai.Message, _ agent.MessageContent) <-chan agent.Event {
	r.excluded <- agent.ExcludedToolsFromContext(ctx)
	ch := make(chan agent.Event)
	close(ch)
	return ch
}

func (*excludedToolsRecordingRunner) Alive() bool             { return true }
func (*excludedToolsRecordingRunner) Busy() bool              { return false }
func (*excludedToolsRecordingRunner) LastActivity() time.Time { return time.Now() }
func (*excludedToolsRecordingRunner) SystemPrompt() string    { return "test" }
func (*excludedToolsRecordingRunner) PluginContext() agentruntime.PluginContext {
	return agentruntime.PluginContext{}
}
func (*excludedToolsRecordingRunner) Close() error { return nil }

type recordingRuntimeManager struct {
	rt      *recordingRuntime
	lookups *atomic.Int64
}

func (m recordingRuntimeManager) GetService(string) sessionaccess.RuntimeService {
	if m.lookups != nil {
		m.lookups.Add(1)
	}
	return m.rt
}

func (m recordingRuntimeManager) Default() sessionaccess.RuntimeService {
	if m.lookups != nil {
		m.lookups.Add(1)
	}
	return m.rt
}

func TestSendSessionMessageAppliesExcludedToolsToOnlyThatRun(t *testing.T) {
	env := setupAdmin(t)
	recorded := make(chan []string, 2)
	runner := &excludedToolsRecordingRunner{excluded: recorded}
	run, err := agentruntime.New(agentruntime.Config{
		Memory: memorytest.New(),
		NewRunner: func(context.Context, agentruntime.RunnerParams) (agentruntime.Runner, error) {
			return runner, nil
		},
	})
	if err != nil {
		t.Fatalf("runtime.New: %v", err)
	}
	if err := env.deps.SessionAccess.BindRuntimeManager(recordingRuntimeManager{rt: &recordingRuntime{run: run}}); err != nil {
		t.Fatalf("BindRuntimeManager: %v", err)
	}
	agentID := createAgentAsUser(t, env, env.bearerToken, "Excluded Tools Agent")
	if _, err := env.db.Exec(context.Background(), `
		INSERT INTO ctx_conversation (id, session_id, channel, kind, agent_id, user_id, last_active)
		VALUES ($1, 'excluded-tools-run', 'web', 'chat', $2, $3, now())
	`, uuid.NewString(), agentID, env.adminUser.ID); err != nil {
		t.Fatalf("seed conversation: %v", err)
	}

	path := "/api/agents/" + agentID + "/sessions/excluded-tools-run/messages"
	rr := doRequest(t, env, http.MethodPost, path, map[string]any{
		"parts":          []map[string]string{{"type": "text", "text": "work"}},
		"excluded_tools": []string{"read", "write", "edit"},
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("excluded send = %d, want 200: %s", rr.Code, rr.Body.String())
	}
	if got, want := <-recorded, []string{"read", "write", "edit"}; !slices.Equal(got, want) {
		t.Fatalf("runtime excluded tools = %v, want %v", got, want)
	}

	rr = doRequest(t, env, http.MethodPost, path, map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "work again"}},
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("ordinary send = %d, want 200: %s", rr.Code, rr.Body.String())
	}
	if got := <-recorded; len(got) != 0 {
		t.Fatalf("excluded tools leaked into the next run: %v", got)
	}
}

// TestSendToArchivedSessionConflicts covers the archived-send answer at the
// transport, which is where the distinction actually matters. `/new` archives
// a session out from under whatever is already
// holding it — a browser tab, a mobile client — and that client needs to learn
// it must move to the successor. 404 would read as a broken link and 500 as a
// server fault; only 409 says "your session is gone, get the new one".
func TestSendToArchivedSessionConflicts(t *testing.T) {
	env := setupAdmin(t)
	rt := &recordingRuntime{}
	if err := env.deps.SessionAccess.BindRuntimeManager(recordingRuntimeManager{rt: rt}); err != nil {
		t.Fatalf("BindRuntimeManager: %v", err)
	}
	agentID := createAgentAsUser(t, env, env.bearerToken, "Archived Send Agent")
	ctx := context.Background()

	if _, err := env.db.Exec(ctx, `
		INSERT INTO ctx_conversation (id, session_id, title, channel, kind, agent_id, user_id, archived, last_active)
		VALUES ($1, 'archived-send', 'Rotated away', 'web', 'chat', $2, $3, true, now())
	`, uuid.NewString(), agentID, env.adminUser.ID); err != nil {
		t.Fatalf("seed archived conversation: %v", err)
	}

	rr := doRequest(t, env, http.MethodPost,
		"/api/agents/"+agentID+"/sessions/archived-send/messages",
		map[string]any{"parts": []map[string]string{{"type": "text", "text": "still there?"}}})
	if rr.Code != http.StatusConflict {
		t.Fatalf("POST to archived session = %d, want %d (body: %s)", rr.Code, http.StatusConflict, rr.Body.String())
	}

	// The body is the error envelope, not a half-open SSE stream: a client that
	// already switched to reading events would otherwise hang on a dead turn.
	var body struct {
		Error struct {
			Code    int    `json:"code"`
			Status  string `json:"status"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal error body %q: %v", rr.Body.String(), err)
	}
	if body.Error.Code != http.StatusConflict || body.Error.Status != "ABORTED" {
		t.Fatalf("error envelope = %+v, want a 409/ABORTED", body.Error)
	}
	if body.Error.Message == "" {
		t.Fatal("409 carried no message; the client has nothing to show or act on")
	}
	if n := rt.chats.Load(); n != 0 {
		t.Fatalf("the runtime started %d turns on an archived session, want 0", n)
	}

	// A live session on the same agent still sends, so the 409 is about this
	// session's state and not a blanket rejection.
	if _, err := env.db.Exec(ctx, `
		INSERT INTO ctx_conversation (id, session_id, channel, kind, agent_id, user_id, last_active)
		VALUES ($1, 'live-send', 'web', 'chat', $2, $3, now())
	`, uuid.NewString(), agentID, env.adminUser.ID); err != nil {
		t.Fatalf("seed live conversation: %v", err)
	}
	rr = doRequest(t, env, http.MethodPost,
		"/api/agents/"+agentID+"/sessions/live-send/messages",
		map[string]any{"parts": []map[string]string{{"type": "text", "text": "still there?"}}})
	if rr.Code != http.StatusOK {
		t.Fatalf("POST to live session = %d, want %d (body: %s)", rr.Code, http.StatusOK, rr.Body.String())
	}
	if n := rt.chats.Load(); n != 1 {
		t.Fatalf("runtime turns after a live send = %d, want 1", n)
	}

	rr = doRequest(t, env, http.MethodPost,
		"/api/agents/"+agentID+"/sessions/live-send/stop", nil)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("POST stop live session = %d, want %d (body: %s)", rr.Code, http.StatusNoContent, rr.Body.String())
	}
	if n := rt.stops.Load(); n != 1 {
		t.Fatalf("runtime stops = %d, want 1", n)
	}

	_, otherToken := createTestUserWithToken(t, env.authStore, env.oidcStore, "stop-other-user", "user")
	rr = doRequestWithSession(t, env.srv, otherToken, http.MethodPost,
		"/api/agents/"+agentID+"/sessions/live-send/stop", nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("cross-user POST stop = %d, want opaque %d (body: %s)", rr.Code, http.StatusNotFound, rr.Body.String())
	}
	if n := rt.stops.Load(); n != 1 {
		t.Fatalf("denied stop reached runtime: stops = %d, want 1", n)
	}
}

func TestSharePublicationDoesNotStartOrWakeSessionCompute(t *testing.T) {
	env := setupAdmin(t)
	rt := &recordingRuntime{}
	var lookups atomic.Int64
	if err := env.deps.SessionAccess.BindRuntimeManager(recordingRuntimeManager{rt: rt, lookups: &lookups}); err != nil {
		t.Fatalf("BindRuntimeManager: %v", err)
	}
	agentID := createAgentAsUser(t, env, env.bearerToken, "Share Compute Spy Agent")
	sessionID := "share-compute-" + uuid.NewString()
	now := time.Now().UTC()
	if err := env.mem.(memory.SessionManager).SaveInfo(context.Background(), memory.SessionInfo{ID: sessionID, UserID: env.adminUser.ID, AgentID: agentID, Channel: "web", Kind: "chat", CreatedAt: now, LastActive: now}); err != nil {
		t.Fatalf("SaveInfo: %v", err)
	}
	workspace := filepath.Join(config.StellaHome(), "users", env.adminUser.ID, "agents", agentID)
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "artifact.html"), []byte("immutable snapshot"), 0o600); err != nil {
		t.Fatal(err)
	}
	rr := doRequest(t, env, http.MethodPost, "/api/shares", map[string]any{
		"source": "artifact", "session_id": sessionID, "path": "artifact.html", "scope": "agent", "agent_id": agentID,
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("POST /api/shares = %d, want %d: %s", rr.Code, http.StatusCreated, rr.Body.String())
	}
	if lookups.Load() != 0 || rt.chats.Load() != 0 || rt.stops.Load() != 0 {
		t.Fatalf("share publication touched Session compute: lookups=%d chats=%d stops=%d", lookups.Load(), rt.chats.Load(), rt.stops.Load())
	}
}
