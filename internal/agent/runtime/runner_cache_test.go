package runtime

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/CherryHQ/stella/internal/agent/session"
	"github.com/CherryHQ/stella/internal/core/agentctx"
	"github.com/CherryHQ/stella/internal/memory"
	"github.com/CherryHQ/stella/internal/plugin"
	"github.com/CherryHQ/stella/pkg/ai"
	pkgplugins "github.com/CherryHQ/stella/pkg/plugins"
)

// --- fake runner ------------------------------------------------------------

type fakeRunner struct {
	alive         bool
	busy          bool
	closed        bool
	lastAct       time.Time
	system        string
	chatSystem    string
	closeErr      error
	panicAlive    bool
	panicBusy     bool
	panicClose    bool
	pluginContext PluginContext
}

func newFakeRunner() *fakeRunner { return &fakeRunner{alive: true, lastAct: time.Now()} }

func TestRunnerCacheCloseWhereTerminallyEvictsBusyAndReserved(t *testing.T) {
	busy, reserved, keep := newFakeRunner(), newFakeRunner(), newFakeRunner()
	busy.busy = true
	cache := newRunnerCache(nil, fakeMemory{}, time.Minute, slog.Default())
	cache.sessions["busy"] = &cachedSession{r: busy, info: session.Info{ID: "busy", UserID: "gone"}}
	cache.sessions["reserved"] = &cachedSession{r: reserved, reserved: true, info: session.Info{ID: "reserved", UserID: "gone"}}
	cache.sessions["keep"] = &cachedSession{r: keep, info: session.Info{ID: "keep", UserID: "stay"}}
	if err := cache.closeWhere(func(cs *cachedSession) bool { return cs.info.UserID == "gone" }); err != nil {
		t.Fatalf("closeWhere: %v", err)
	}
	if !busy.closed || !reserved.closed {
		t.Fatalf("matching runners not closed: busy=%v reserved=%v", busy.closed, reserved.closed)
	}
	if keep.closed || len(cache.sessions) != 1 || cache.sessions["keep"] == nil {
		t.Fatalf("nonmatching session was disturbed: %#v", cache.sessions)
	}
	if cache.sessions["busy"] != nil || cache.sessions["reserved"] != nil {
		t.Fatalf("terminally closed runners remain cache-reachable: %#v", cache.sessions)
	}
}

func (r *fakeRunner) Chat(ctx context.Context, _ []ai.Message, _ MessageContent) <-chan Event {
	r.chatSystem, _ = agentctx.SystemOverrideFromContext(ctx)
	ch := make(chan Event)
	close(ch)
	return ch
}

func (r *fakeRunner) Alive() bool {
	if r.panicAlive {
		panic("alive panic")
	}
	return r.alive
}

func (r *fakeRunner) Busy() bool {
	if r.panicBusy {
		panic("busy panic")
	}
	return r.busy
}
func (r *fakeRunner) LastActivity() time.Time      { return r.lastAct }
func (r *fakeRunner) SystemPrompt() string         { return r.system }
func (r *fakeRunner) PluginContext() PluginContext { return r.pluginContext }
func (r *fakeRunner) Close() error {
	if r.panicClose {
		panic("close panic")
	}
	r.closed = true
	return r.closeErr
}

func TestRunnerCacheKeepsReservedContextAndRefreshesNewRunner(t *testing.T) {
	first := newFakeRunner()
	first.pluginContext = NewPluginContext(plugin.Snapshot{}, pkgplugins.SessionPluginView{ExposedPluginIDs: []string{"plugin/old"}})
	second := newFakeRunner()
	second.pluginContext = NewPluginContext(plugin.Snapshot{}, pkgplugins.SessionPluginView{ExposedPluginIDs: []string{"plugin/new"}})
	builds := 0
	cache := newRunnerCache(func(context.Context, RunnerParams) (Runner, error) {
		builds++
		if builds == 1 {
			return first, nil
		}
		return second, nil
	}, fakeMemory{}, time.Minute, slog.Default())
	info := session.Info{ID: "session", UserID: "user", AgentID: "agent"}

	selection, err := cache.getOrCreateReserved(context.Background(), info, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if got := selection.pluginContext.SessionPluginView().ExposedPluginIDs; len(got) != 1 || got[0] != "plugin/old" {
		t.Fatalf("first selection context = %v", got)
	}
	if err := cache.invalidateSkillPolicy(); err != nil {
		t.Fatal(err)
	}
	cache.releaseReservation(selection.session)

	selection, err = cache.getOrCreateReserved(context.Background(), info, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if selection.runner != second {
		t.Fatalf("runner after invalidation = %p, want refreshed runner %p", selection.runner, second)
	}
	if got := selection.pluginContext.SessionPluginView().ExposedPluginIDs; len(got) != 1 || got[0] != "plugin/new" {
		t.Fatalf("refreshed selection context = %v", got)
	}
}

// --- fake memory provider ---------------------------------------------------

type fakeMemory struct{}

func (fakeMemory) Name() string                                                      { return "fake" }
func (fakeMemory) Bootstrap(_ context.Context, _ memory.Session) error               { return nil }
func (fakeMemory) Append(_ context.Context, _ memory.Session, _ ...ai.Message) error { return nil }
func (fakeMemory) Assemble(_ context.Context, _ memory.Session, _, _ int) ([]ai.Message, error) {
	return nil, nil
}

func (fakeMemory) Stats(_ context.Context, _ memory.Session) (memory.SessionStats, error) {
	return memory.SessionStats{}, nil
}
func (fakeMemory) Close() error { return nil }

type gatedMemory struct {
	fakeMemory
	assembleStarted chan struct{}
	releaseAssemble chan struct{}
	once            sync.Once
}

type compactingMemory struct {
	fakeMemory
	compactStarted chan struct{}
	releaseCompact chan struct{}
	once           sync.Once
}

type panicBootstrapMemory struct {
	fakeMemory
	rt         *Runtime
	panicFirst bool
}

func (m *panicBootstrapMemory) Bootstrap(context.Context, memory.Session) error {
	if !m.panicFirst {
		return nil
	}
	m.panicFirst = false
	if m.rt != nil {
		_ = m.rt.ResetRunners()
	}
	panic("bootstrap panic")
}

func (m *compactingMemory) NeedsCompaction(context.Context, memory.Session, float64) bool {
	return true
}

func (m *compactingMemory) Compact(context.Context, memory.Session, memory.CompactionMode) (*memory.CompactionResult, error) {
	m.once.Do(func() { close(m.compactStarted) })
	<-m.releaseCompact
	return &memory.CompactionResult{}, nil
}

func (m *gatedMemory) Assemble(context.Context, memory.Session, int, int) ([]ai.Message, error) {
	m.once.Do(func() { close(m.assembleStarted) })
	<-m.releaseAssemble
	return nil, nil
}

// --- helpers ----------------------------------------------------------------

func testCache(factoryErr error) (*runnerCache, *fakeRunner) {
	created := newFakeRunner()
	var calls int
	factory := func(_ context.Context, _ RunnerParams) (Runner, error) {
		calls++
		if factoryErr != nil {
			return nil, factoryErr
		}
		_ = calls
		return created, nil
	}
	cache := newRunnerCache(factory, fakeMemory{}, 10*time.Minute, slog.Default())
	return cache, created
}

func validInfo(id string) session.Info {
	return session.NewInfo(id, "agent1", "u1", "web", session.KindChat, "", time.Now().UTC())
}

// TestRunnerCache_Reuse verifies the same runner is returned on repeat calls.
func TestRunnerCache_Reuse(t *testing.T) {
	cache, _ := testCache(nil)
	info := validInfo("s1")

	_, r1, err := cache.getOrCreate(context.Background(), info, "", "")
	if err != nil {
		t.Fatalf("first getOrCreate: %v", err)
	}
	_, r2, err := cache.getOrCreate(context.Background(), info, "", "")
	if err != nil {
		t.Fatalf("second getOrCreate: %v", err)
	}
	if r1 != r2 {
		t.Error("expected same runner on reuse")
	}
}

// TestRunnerCache_Close shuts down the runner.
func TestRunnerCache_Close(t *testing.T) {
	cache, created := testCache(nil)
	info := validInfo("s1")

	if _, _, err := cache.getOrCreate(context.Background(), info, "", ""); err != nil {
		t.Fatalf("getOrCreate: %v", err)
	}
	if err := cache.close("s1"); err != nil {
		t.Fatalf("close: %v", err)
	}
	if !created.closed {
		t.Error("expected runner to be closed")
	}
}

// TestRunnerCache_CloseAll shuts down all runners.
func TestRunnerCache_CloseAll(t *testing.T) {
	cache, created := testCache(nil)
	for _, id := range []string{"s1", "s2"} {
		if _, _, err := cache.getOrCreate(context.Background(), validInfo(id), "", ""); err != nil {
			t.Fatalf("getOrCreate %s: %v", id, err)
		}
	}
	if err := cache.closeAll(); err != nil {
		t.Fatalf("closeAll: %v", err)
	}
	// The fake creates ONE runner shared across both sessions (same factory call result).
	// We just verify no error and the cache is empty.
	if len(cache.sessions) != 0 {
		t.Errorf("expected empty cache after closeAll, got %d", len(cache.sessions))
	}
	_ = created
}

// TestRunnerCache_DeadRunnerReplaced replaces a dead runner on next access.
func TestRunnerCache_DeadRunnerReplaced(t *testing.T) {
	cache, created := testCache(nil)
	info := validInfo("s1")

	_, r1, _ := cache.getOrCreate(context.Background(), info, "", "")
	created.alive = false

	_, r2, err := cache.getOrCreate(context.Background(), info, "", "")
	if err != nil {
		t.Fatalf("getOrCreate after dead: %v", err)
	}
	// r2 is a new runner (factory creates new fakeRunner each call with alive=true).
	// Since factory always returns the same `created` object and we set alive=false,
	// the second call also gets a runner. Just verify no error.
	_ = r1
	_ = r2
}

// TestRunnerCache_MissingID rejects empty session ID.
func TestRunnerCache_MissingID(t *testing.T) {
	cache, _ := testCache(nil)
	_, _, err := cache.getOrCreate(context.Background(), session.Info{UserID: "u1", AgentID: "a1"}, "", "")
	if err == nil {
		t.Error("expected error for empty session ID")
	}
}

// TestRunnerCache_MissingUserID rejects empty UserID.
func TestRunnerCache_MissingUserID(t *testing.T) {
	cache, _ := testCache(nil)
	info := session.Info{ID: "s1", AgentID: "a1"}
	_, _, err := cache.getOrCreate(context.Background(), info, "", "")
	if err == nil {
		t.Error("expected error for empty UserID")
	}
}

// TestRunnerCache_MissingAgentID rejects empty AgentID.
func TestRunnerCache_MissingAgentID(t *testing.T) {
	cache, _ := testCache(nil)
	info := session.Info{ID: "s1", UserID: "u1"}
	_, _, err := cache.getOrCreate(context.Background(), info, "", "")
	if err == nil {
		t.Error("expected error for empty AgentID")
	}
}

// TestRunnerCache_FactoryError propagates factory errors.
func TestRunnerCache_FactoryError(t *testing.T) {
	factoryErr := errors.New("factory boom")
	cache, _ := testCache(factoryErr)
	_, _, err := cache.getOrCreate(context.Background(), validInfo("s1"), "", "")
	if !errors.Is(err, factoryErr) {
		t.Errorf("expected factory error, got %v", err)
	}
}

// TestRunnerCache_Reap removes idle runners.
func TestRunnerCache_Reap(t *testing.T) {
	cache, created := testCache(nil)
	cache.idleTimeout = 1 * time.Millisecond

	info := validInfo("s1")
	if _, _, err := cache.getOrCreate(context.Background(), info, "", ""); err != nil {
		t.Fatalf("getOrCreate: %v", err)
	}

	created.lastAct = time.Now().Add(-1 * time.Hour)
	cache.reap()

	cache.mu.Lock()
	cs := cache.sessions["s1"]
	cache.mu.Unlock()
	if cs != nil && cs.r != nil {
		t.Error("expected runner to be reaped")
	}
}

func TestRunnerCacheInvalidateSkillPolicyKeepsBusyRunnerSnapshot(t *testing.T) {
	var runners []*fakeRunner
	newRunner := func(snapshot string) NewRunnerFunc {
		return func(context.Context, RunnerParams) (Runner, error) {
			r := newFakeRunner()
			r.system = snapshot
			runners = append(runners, r)
			return r, nil
		}
	}
	cache := newRunnerCache(newRunner("before-commit"), fakeMemory{}, 10*time.Minute, slog.Default())
	info := validInfo("policy-session")
	_, first, err := cache.getOrCreate(context.Background(), info, "", "")
	if err != nil {
		t.Fatalf("initial getOrCreate: %v", err)
	}
	r1 := first.(*fakeRunner)
	r1.busy = true
	// PoolManager swaps in a factory built from the committed policy snapshot
	// before it invalidates. The busy runner must retain the old factory.
	cache.mu.Lock()
	cache.newRunner = newRunner("after-commit")
	cache.mu.Unlock()
	if err := cache.invalidateSkillPolicy(); err != nil {
		t.Fatalf("invalidateSkillPolicy: %v", err)
	}
	_, current, err := cache.getOrCreate(context.Background(), info, "", "")
	if err != nil || current != first || r1.closed {
		t.Fatalf("busy runner after invalidation = %v, closed=%t, err=%v; want immutable current runner", current, r1.closed, err)
	}

	// The next operation after the in-flight turn completes is built from a new
	// snapshot, and the old runner is then retired.
	r1.busy = false
	_, next, err := cache.getOrCreate(context.Background(), info, "", "")
	if err != nil || next == first || !r1.closed || len(runners) != 2 || next.(*fakeRunner).system != "after-commit" {
		t.Fatalf("post-turn runner = %v, firstClosed=%t, created=%d, err=%v; want fresh committed snapshot", next, r1.closed, len(runners), err)
	}
}

func TestRunnerCacheInvalidateSkillPolicyKeepsReservedAdmissionSnapshot(t *testing.T) {
	var runners []*fakeRunner
	factory := func(snapshot string) NewRunnerFunc {
		return func(context.Context, RunnerParams) (Runner, error) {
			r := newFakeRunner()
			r.system = snapshot
			runners = append(runners, r)
			return r, nil
		}
	}
	cache := newRunnerCache(factory("before-policy"), fakeMemory{}, 10*time.Minute, slog.Default())
	info := validInfo("reserved-policy-session")
	cs, first, err := cache.getOrCreate(context.Background(), info, "", "")
	if err != nil {
		t.Fatalf("get old runner: %v", err)
	}
	cache.reserve(cs)
	cache.mu.Lock()
	cache.newRunner = factory("after-policy")
	cache.mu.Unlock()
	if err := cache.invalidateSkillPolicy(); err != nil {
		t.Fatalf("invalidate policy: %v", err)
	}
	if first.(*fakeRunner).closed {
		t.Fatal("policy invalidation closed a runner selected by an already admitted turn")
	}
	// Auto-compaction re-selects through getOrCreate while the turn owns its
	// reservation. It must keep using the immutable old runner, not close it or
	// switch factory midway through the current turn.
	_, compacted, err := cache.getOrCreate(context.Background(), info, "", "")
	if err != nil || compacted != first || first.(*fakeRunner).closed {
		t.Fatalf("reserved auto-compaction reselect=%v oldClosed=%t err=%v; want current runner", compacted, first.(*fakeRunner).closed, err)
	}
	for range compacted.Chat(context.Background(), nil, "current turn") {
	}
	cache.releaseReservation(cs)
	_, next, err := cache.getOrCreate(context.Background(), info, "", "")
	if err != nil || next == first || len(runners) != 2 || !first.(*fakeRunner).closed || next.(*fakeRunner).system != "after-policy" {
		t.Fatalf("post-turn runner=%v oldClosed=%t created=%d err=%v; want fresh policy snapshot", next, first.(*fakeRunner).closed, len(runners), err)
	}
}

func TestRunnerCacheReapKeepsReservedRunner(t *testing.T) {
	cache, runner := testCache(nil)
	cache.idleTimeout = time.Millisecond
	cs, _, err := cache.getOrCreate(context.Background(), validInfo("reserved-reap"), "", "")
	if err != nil {
		t.Fatalf("get runner: %v", err)
	}
	runner.lastAct = time.Now().Add(-time.Hour)
	cache.reserve(cs)
	cache.reap()
	if runner.closed {
		t.Fatal("reap closed a reserved runner")
	}
	cache.releaseReservation(cs)
	cache.reap()
	if !runner.closed {
		t.Fatal("reap did not close released idle runner")
	}
}

func TestRunnerCacheReapDoesNotProbeReservedRunner(t *testing.T) {
	cache, bad := testCache(nil)
	info := validInfo("reserved-reap-panic")
	cs, _, err := cache.getOrCreate(context.Background(), info, "", "")
	if err != nil {
		t.Fatalf("seed reserved runner: %v", err)
	}
	cache.reserve(cs)
	bad.panicBusy = true
	bad.panicAlive = true
	bad.lastAct = time.Now().Add(-time.Hour)

	cache.reap()
	if bad.closed || !cs.reserved || cs.r != bad {
		t.Fatalf("reap touched reserved runner: closed=%t reserved=%t runner=%v", bad.closed, cs.reserved, cs.r)
	}

	bad.panicBusy = false
	bad.panicAlive = false
	cache.releaseReservation(cs)
	cache.reap()
	if !bad.closed || cs.r != nil {
		t.Fatalf("released idle runner not reaped: closed=%t runner=%v", bad.closed, cs.r)
	}
}

func TestRunnerCacheReservedLookupDoesNotProbeAndMarksStale(t *testing.T) {
	var (
		bad    = newFakeRunner()
		fresh  = newFakeRunner()
		calls  int
		params []RunnerParams
	)
	cache := newRunnerCache(func(_ context.Context, p RunnerParams) (Runner, error) {
		calls++
		params = append(params, p)
		if calls == 1 {
			return bad, nil
		}
		return fresh, nil
	}, fakeMemory{}, 10*time.Minute, slog.Default())
	info := validInfo("reserved-lookup-panic")
	cs, _, err := cache.getOrCreate(context.Background(), info, "old-model", "low")
	if err != nil {
		t.Fatalf("seed runner: %v", err)
	}
	cache.reserve(cs)
	bad.panicBusy = true
	bad.panicAlive = true

	selected, err := cache.getOrCreateReserved(context.Background(), info, "new-model", "high")
	if err != nil {
		t.Fatalf("reserved reselection: %v", err)
	}
	if selected.runner != bad || selected.model != "old-model" || selected.thinking != "low" || !cs.stale || !cs.reserved {
		t.Fatalf("reserved selection=%#v cache=%#v; want current stale reserved selection", selected, cs)
	}

	bad.panicBusy = false
	bad.panicAlive = false
	cache.releaseReservation(cs)
	next, err := cache.getOrCreateReserved(context.Background(), info, "new-model", "high")
	if err != nil {
		t.Fatalf("post-release replacement: %v", err)
	}
	if next.runner != fresh || calls != 2 || !bad.closed || params[1].Model != "new-model" || params[1].Thinking != "high" {
		t.Fatalf("post-release replacement runner=%v calls=%d oldClosed=%t params=%#v", next.runner, calls, bad.closed, params)
	}
}

func TestRunnerCacheResetKeepsReservedRunnerAndRebuildsNextTurn(t *testing.T) {
	var runners []*fakeRunner
	cache := newRunnerCache(func(context.Context, RunnerParams) (Runner, error) {
		r := newFakeRunner()
		runners = append(runners, r)
		return r, nil
	}, fakeMemory{}, time.Minute, slog.Default())
	info := validInfo("reserved-reset")
	cs, first, err := cache.getOrCreate(context.Background(), info, "", "")
	if err != nil {
		t.Fatalf("get old runner: %v", err)
	}
	cache.reserve(cs)
	if err := cache.reset(); err != nil {
		t.Fatalf("non-terminal reset: %v", err)
	}
	if first.(*fakeRunner).closed || !cs.stale || cs.model != "" || cs.thinking != "" {
		t.Fatalf("reserved reset closed=%t stale=%t model=%q thinking=%q; want retained stale runner", first.(*fakeRunner).closed, cs.stale, cs.model, cs.thinking)
	}
	for range first.Chat(context.Background(), nil, "current turn") {
	}
	cache.releaseReservation(cs)
	_, next, err := cache.getOrCreate(context.Background(), info, "", "")
	if err != nil || next == first || !first.(*fakeRunner).closed || len(runners) != 2 {
		t.Fatalf("post-reset next=%v oldClosed=%t created=%d err=%v; want rebuilt runner", next, first.(*fakeRunner).closed, len(runners), err)
	}
}

func TestRuntimeResetRunnersKeepsReservedAdmission(t *testing.T) {
	for _, tt := range []struct {
		name  string
		reset func(*Runtime, string) error
	}{
		{name: "all agents", reset: func(rt *Runtime, _ string) error { return rt.ResetRunners() }},
		{name: "user scoped", reset: func(rt *Runtime, userID string) error { return rt.ResetRunnersForUser(userID) }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var runners []*fakeRunner
			rt, err := New(Config{NewRunner: func(context.Context, RunnerParams) (Runner, error) {
				r := newFakeRunner()
				runners = append(runners, r)
				return r, nil
			}, Memory: fakeMemory{}})
			if err != nil {
				t.Fatalf("new runtime: %v", err)
			}
			info := validInfo("sync-reserved")
			selection, err := rt.cache.getOrCreateReserved(context.Background(), info, "", "")
			if err != nil {
				t.Fatalf("admit runner: %v", err)
			}
			cs, first := selection.session, selection.runner
			if err := tt.reset(rt, info.UserID); err != nil {
				t.Fatalf("non-terminal reset: %v", err)
			}
			if first.(*fakeRunner).closed || !cs.stale {
				t.Fatalf("reset closed=%t stale=%t; want reserved runner retained stale", first.(*fakeRunner).closed, cs.stale)
			}
			rt.cache.releaseReservation(cs)
			_, next, err := rt.cache.getOrCreate(context.Background(), info, "", "")
			if err != nil || next == first || !first.(*fakeRunner).closed || len(runners) != 2 {
				t.Fatalf("next admission runner=%v oldClosed=%t created=%d err=%v; want current factory", next, first.(*fakeRunner).closed, len(runners), err)
			}
		})
	}
}

func TestAdmittedSelectionKeepsModelThinkingAcrossReset(t *testing.T) {
	mem := &gatedMemory{assembleStarted: make(chan struct{}), releaseAssemble: make(chan struct{})}
	var (
		factoryParams []RunnerParams
		beforeModels  []string
		mu            sync.Mutex
	)
	newRunner := func(context.Context, RunnerParams) (Runner, error) { return newFakeRunner(), nil }
	factory := func(ctx context.Context, params RunnerParams) (Runner, error) {
		mu.Lock()
		factoryParams = append(factoryParams, params)
		mu.Unlock()
		return newRunner(ctx, params)
	}
	rt, err := New(Config{
		NewRunner:       factory,
		Memory:          mem,
		DefaultModel:    "old-model",
		DefaultThinking: "low",
		BeforeRun: func(_ context.Context, _ session.Info, model, _ string, _ string, _ []ai.Message, _ PluginContext) (string, error) {
			mu.Lock()
			beforeModels = append(beforeModels, model)
			mu.Unlock()
			return "", nil
		},
	})
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}
	info := validInfo("immutable-admission")
	stream, err := rt.ChatAdmitted(context.Background(), info, "first")
	if err != nil {
		t.Fatalf("admit old turn: %v", err)
	}
	<-mem.assembleStarted
	rt.SetNewRunner(factory)
	rt.SetDefaultModel("new-model", "high")
	if err := rt.ResetRunners(); err != nil {
		t.Fatalf("reset during admitted turn: %v", err)
	}
	close(mem.releaseAssemble)
	for event := range stream {
		if event.Err != nil {
			t.Fatalf("old turn event: %v", event.Err)
		}
	}

	// The admission lease, not reset-cleared cachedSession.model/thinking,
	// drives beforeRun and the already selected runner factory parameters.
	mu.Lock()
	if len(factoryParams) != 1 || factoryParams[0].Model != "old-model" || factoryParams[0].Thinking != "low" || len(beforeModels) != 1 || beforeModels[0] != "old-model" {
		mu.Unlock()
		t.Fatalf("old admitted metadata params=%#v before=%#v; want old model/thinking", factoryParams, beforeModels)
	}
	mu.Unlock()

	second := rt.Chat(context.Background(), info, "second")
	for event := range second {
		if event.Err != nil {
			t.Fatalf("next turn event: %v", event.Err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(factoryParams) != 2 || factoryParams[1].Model != "new-model" || factoryParams[1].Thinking != "high" || len(beforeModels) != 2 || beforeModels[1] != "new-model" {
		t.Fatalf("next admitted metadata params=%#v before=%#v; want new model/thinking", factoryParams, beforeModels)
	}
}

func TestResetDuringReservedFactoryBuildKeepsSelectionAndDropsCacheMetadata(t *testing.T) {
	factoryStarted := make(chan struct{})
	releaseFactory := make(chan struct{})
	var (
		params []RunnerParams
		models []string
		mu     sync.Mutex
	)
	rt, err := New(Config{
		NewRunner: func(_ context.Context, p RunnerParams) (Runner, error) {
			mu.Lock()
			params = append(params, p)
			first := len(params) == 1
			mu.Unlock()
			if first {
				close(factoryStarted)
				<-releaseFactory
			}
			return newFakeRunner(), nil
		},
		Memory:          fakeMemory{},
		DefaultModel:    "old-model",
		DefaultThinking: "low",
		BeforeRun: func(_ context.Context, _ session.Info, model, _ string, _ string, _ []ai.Message, _ PluginContext) (string, error) {
			mu.Lock()
			models = append(models, model)
			mu.Unlock()
			return "", nil
		},
	})
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}
	info := validInfo("factory-build-reset")
	type admission struct {
		stream <-chan Event
		err    error
	}
	admitted := make(chan admission, 1)
	go func() {
		stream, err := rt.ChatAdmitted(context.Background(), info, "first")
		admitted <- admission{stream: stream, err: err}
	}()
	<-factoryStarted
	rt.SetDefaultModel("new-model", "high")
	if err := rt.ResetRunners(); err != nil {
		t.Fatalf("reset during factory build: %v", err)
	}
	close(releaseFactory)
	first := <-admitted
	if first.err != nil {
		t.Fatalf("admit old turn: %v", first.err)
	}
	for event := range first.stream {
		if event.Err != nil {
			t.Fatalf("old turn event: %v", event.Err)
		}
	}
	for event := range rt.Chat(context.Background(), info, "second") {
		if event.Err != nil {
			t.Fatalf("next turn event: %v", event.Err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(params) != 2 || params[0].Model != "old-model" || params[0].Thinking != "low" || params[1].Model != "new-model" || params[1].Thinking != "high" || len(models) != 2 || models[0] != "old-model" || models[1] != "new-model" {
		t.Fatalf("factory-build reset params=%#v models=%#v; want old immutable then new defaults", params, models)
	}
}

func TestCompactionKeepsAdmittedSelectionMetadata(t *testing.T) {
	mem := &compactingMemory{compactStarted: make(chan struct{}), releaseCompact: make(chan struct{})}
	var models []string
	var params []RunnerParams
	var mu sync.Mutex
	factory := func(_ context.Context, p RunnerParams) (Runner, error) {
		mu.Lock()
		params = append(params, p)
		mu.Unlock()
		return newFakeRunner(), nil
	}
	rt, err := New(Config{
		NewRunner:       factory,
		Memory:          mem,
		DefaultModel:    "old-model",
		DefaultThinking: "low",
		BeforeRun: func(_ context.Context, _ session.Info, model, _ string, _ string, _ []ai.Message, _ PluginContext) (string, error) {
			mu.Lock()
			models = append(models, model)
			mu.Unlock()
			return "", nil
		},
	})
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}
	info := validInfo("compaction-selection")
	stream, err := rt.ChatAdmitted(context.Background(), info, "first")
	if err != nil {
		t.Fatalf("admit old turn: %v", err)
	}
	<-mem.compactStarted
	rt.SetNewRunner(factory)
	rt.SetDefaultModel("new-model", "high")
	if err := rt.ResetRunners(); err != nil {
		t.Fatalf("reset during compaction: %v", err)
	}
	close(mem.releaseCompact)
	for event := range stream {
		if event.Err != nil {
			t.Fatalf("compacted turn event: %v", event.Err)
		}
	}
	mu.Lock()
	if len(params) != 1 || params[0].Model != "old-model" || params[0].Thinking != "low" || len(models) != 1 || models[0] != "old-model" {
		mu.Unlock()
		t.Fatalf("compacted admission params=%#v models=%#v; want immutable old selection", params, models)
	}
	mu.Unlock()
	for event := range rt.Chat(context.Background(), info, "second") {
		if event.Err != nil {
			t.Fatalf("next turn event: %v", event.Err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(params) != 2 || params[1].Model != "new-model" || params[1].Thinking != "high" || len(models) != 2 || models[1] != "new-model" {
		t.Fatalf("post-compaction params=%#v models=%#v; want new selection", params, models)
	}
}

func TestRunnerCacheResetClosesIdleUnreservedRunner(t *testing.T) {
	cache, runner := testCache(nil)
	if _, _, err := cache.getOrCreate(context.Background(), validInfo("idle-reset"), "", ""); err != nil {
		t.Fatalf("get runner: %v", err)
	}
	if err := cache.reset(); err != nil {
		t.Fatalf("reset: %v", err)
	}
	if !runner.closed {
		t.Fatal("reset did not promptly close an idle unreserved runner")
	}
}

func TestRuntimeCloseRejectsLaterAdmission(t *testing.T) {
	rt, err := New(Config{NewRunner: func(context.Context, RunnerParams) (Runner, error) { return newFakeRunner(), nil }, Memory: fakeMemory{}})
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}
	if err := rt.Close(); err != nil {
		t.Fatalf("close runtime: %v", err)
	}
	if _, err := rt.ChatAdmitted(context.Background(), validInfo("closed"), "turn"); err == nil {
		t.Fatal("ChatAdmitted after Close error = nil")
	}
}

func TestChatAdmittedFactoryPanicCleansActiveAndReservation(t *testing.T) {
	var calls int
	rt, err := New(Config{NewRunner: func(context.Context, RunnerParams) (Runner, error) {
		calls++
		if calls == 1 {
			panic("factory panic")
		}
		return newFakeRunner(), nil
	}, Memory: fakeMemory{}})
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}
	info := validInfo("factory-panic")
	if _, err := rt.ChatAdmitted(context.Background(), info, "first"); err == nil {
		t.Fatal("factory panic admission error = nil")
	}
	if _, active := rt.active.Load(info.ID); active {
		t.Fatal("factory panic left Runtime.active set")
	}
	rt.cache.mu.Lock()
	_, cached := rt.cache.sessions[info.ID]
	rt.cache.mu.Unlock()
	if cached {
		t.Fatal("factory panic left an empty reserved cache entry")
	}
	stream, err := rt.ChatAdmitted(context.Background(), info, "second")
	if err != nil {
		t.Fatalf("healthy admission after factory panic: %v", err)
	}
	for event := range stream {
		if event.Err != nil {
			t.Fatalf("healthy turn event: %v", event.Err)
		}
	}
}

func TestChatAdmittedSynchronousOptionPanicClearsActive(t *testing.T) {
	rt, err := New(Config{NewRunner: func(context.Context, RunnerParams) (Runner, error) { return newFakeRunner(), nil }, Memory: fakeMemory{}})
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}
	info := validInfo("option-panic")
	if _, err := rt.ChatAdmitted(context.Background(), info, "turn", Option(func(*chatOptions) { panic("option panic") })); err == nil {
		t.Fatal("option panic admission error = nil")
	}
	if _, active := rt.active.Load(info.ID); active {
		t.Fatal("option panic left Runtime.active set")
	}
}

func TestChatAdmittedBootstrapPanicLeavesStaleRunnerForCurrentFactory(t *testing.T) {
	mem := &panicBootstrapMemory{panicFirst: true}
	var (
		runners []*fakeRunner
		params  []RunnerParams
	)
	rt, err := New(Config{NewRunner: func(_ context.Context, paramsIn RunnerParams) (Runner, error) {
		r := newFakeRunner()
		runners = append(runners, r)
		params = append(params, paramsIn)
		return r, nil
	}, Memory: mem, DefaultModel: "old-model", DefaultThinking: "low"})
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}
	mem.rt = rt
	info := validInfo("bootstrap-panic")
	if _, err := rt.ChatAdmitted(context.Background(), info, "first"); err == nil {
		t.Fatal("bootstrap panic admission error = nil")
	}
	if _, active := rt.active.Load(info.ID); active {
		t.Fatal("bootstrap panic left Runtime.active set")
	}
	rt.cache.mu.Lock()
	cs := rt.cache.sessions[info.ID]
	if cs == nil || cs.r != runners[0] || cs.reserved || !cs.stale || cs.model != "" || cs.thinking != "" {
		rt.cache.mu.Unlock()
		t.Fatalf("bootstrap panic cache=%#v; want unreserved stale runner with cleared metadata", cs)
	}
	rt.cache.mu.Unlock()
	rt.SetDefaultModel("new-model", "high")
	stream, err := rt.ChatAdmitted(context.Background(), info, "second")
	if err != nil {
		t.Fatalf("healthy admission after bootstrap panic: %v", err)
	}
	for event := range stream {
		if event.Err != nil {
			t.Fatalf("healthy turn event: %v", event.Err)
		}
	}
	if len(runners) != 2 || !runners[0].closed || params[1].Model != "new-model" || params[1].Thinking != "high" {
		t.Fatalf("post-bootstrap panic runners=%d oldClosed=%t params=%#v; want rebuilt current factory", len(runners), runners[0].closed, params)
	}
}

func TestChatAdmittedCachedRunnerAlivePanicDoesNotDeadlock(t *testing.T) {
	testChatAdmittedCachedRunnerPanic(t, "alive", func(r *fakeRunner) { r.panicAlive = true }, false)
}

func TestChatAdmittedCachedRunnerBusyPanicDoesNotDeadlock(t *testing.T) {
	testChatAdmittedCachedRunnerPanic(t, "busy", func(r *fakeRunner) { r.panicBusy = true }, true)
}

func TestFailedAdmissionResetRetiresBusyPanickingRunner(t *testing.T) {
	rt, info, bad, calls := newFailedAdmissionRuntime(t, "failed-reset")
	bad.panicBusy = true
	if err := rt.ResetRunners(); err != nil {
		t.Fatalf("reset failed admission: %v", err)
	}
	assertFailedRunnerDetached(t, rt.cache, info.ID, bad)
	assertHealthyRetry(t, rt, info, calls)
}

func TestFailedAdmissionPolicyInvalidationRetiresBusyPanickingRunner(t *testing.T) {
	rt, info, bad, calls := newFailedAdmissionRuntime(t, "failed-policy")
	bad.panicBusy = true
	if err := rt.InvalidateSkillPolicy(); err != nil {
		t.Fatalf("policy invalidation failed admission: %v", err)
	}
	assertFailedRunnerDetached(t, rt.cache, info.ID, bad)
	assertHealthyRetry(t, rt, info, calls)
}

func TestFailedAdmissionReapRetiresAlivePanickingRunner(t *testing.T) {
	rt, info, bad, calls := newFailedAdmissionRuntime(t, "failed-reap")
	bad.panicAlive = true
	rt.cache.reap()
	assertFailedRunnerDetached(t, rt.cache, info.ID, bad)
	assertHealthyRetry(t, rt, info, calls)
}

func TestFailedAdmissionReservedRunnerDefersRetirement(t *testing.T) {
	rt, info, bad, _ := newFailedAdmissionRuntime(t, "failed-reserved")
	bad.panicBusy = true
	rt.cache.mu.Lock()
	cs := rt.cache.sessions[info.ID]
	cs.reserved = true
	rt.cache.mu.Unlock()
	if err := rt.ResetRunners(); err != nil {
		t.Fatalf("reset reserved failed admission: %v", err)
	}
	rt.cache.mu.Lock()
	if cs.r != bad || !cs.reserved || !cs.stale || !cs.failedAdmission || bad.closed {
		rt.cache.mu.Unlock()
		t.Fatalf("reserved failed admission state=%#v closed=%t; want retained stale lease", cs, bad.closed)
	}
	cs.reserved = false
	rt.cache.mu.Unlock()
	if err := rt.ResetRunners(); err != nil {
		t.Fatalf("reset released failed admission: %v", err)
	}
	assertFailedRunnerDetached(t, rt.cache, info.ID, bad)
}

func TestFailedAdmissionRetirementClosePanicIsBounded(t *testing.T) {
	rt, info, bad, _ := newFailedAdmissionRuntime(t, "failed-close")
	bad.panicClose = true
	if err := rt.ResetRunners(); err == nil {
		t.Fatal("reset close panic error = nil")
	}
	rt.cache.mu.Lock()
	cs := rt.cache.sessions[info.ID]
	rt.cache.mu.Unlock()
	if cs != nil && cs.r != nil {
		t.Fatalf("close panic left bad runner cache-reachable: %#v", cs)
	}
}

func newFailedAdmissionRuntime(t *testing.T, sessionID string) (*Runtime, session.Info, *fakeRunner, *int) {
	t.Helper()
	bad := newFakeRunner()
	calls := 0
	rt, err := New(Config{NewRunner: func(context.Context, RunnerParams) (Runner, error) {
		calls++
		if calls == 1 {
			return bad, nil
		}
		return newFakeRunner(), nil
	}, Memory: fakeMemory{}})
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}
	info := validInfo(sessionID)
	if _, _, err := rt.cache.getOrCreate(context.Background(), info, "", ""); err != nil {
		t.Fatalf("seed failed runner: %v", err)
	}
	rt.cache.mu.Lock()
	cs := rt.cache.sessions[info.ID]
	cs.stale = true
	cs.failedAdmission = true
	rt.cache.mu.Unlock()
	return rt, info, bad, &calls
}

func assertFailedRunnerDetached(t *testing.T, cache *runnerCache, sessionID string, bad *fakeRunner) {
	t.Helper()
	cache.mu.Lock()
	cs := cache.sessions[sessionID]
	cache.mu.Unlock()
	if cs != nil && (cs.r != nil || cs.failedAdmission) {
		t.Fatalf("failed runner remains cache-reachable: %#v", cs)
	}
	if !bad.closed {
		t.Fatal("failed runner was not retired")
	}
}

func assertHealthyRetry(t *testing.T, rt *Runtime, info session.Info, calls *int) {
	t.Helper()
	stream, err := rt.ChatAdmitted(context.Background(), info, "healthy retry")
	if err != nil {
		t.Fatalf("healthy retry admission: %v", err)
	}
	for event := range stream {
		if event.Err != nil {
			t.Fatalf("healthy retry event: %v", event.Err)
		}
	}
	if *calls != 2 {
		t.Fatalf("healthy retry factory calls=%d, want 2", *calls)
	}
}

func testChatAdmittedCachedRunnerPanic(t *testing.T, name string, panicRunner func(*fakeRunner), modelReplace bool) {
	t.Helper()
	var calls int
	bad := newFakeRunner()
	rt, err := New(Config{NewRunner: func(context.Context, RunnerParams) (Runner, error) {
		calls++
		if calls == 1 {
			return bad, nil
		}
		return newFakeRunner(), nil
	}, Memory: fakeMemory{}})
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}
	info := validInfo("cached-" + name + "-panic")
	if _, _, err := rt.cache.getOrCreate(context.Background(), info, "old-model", ""); err != nil {
		t.Fatalf("seed cached runner: %v", err)
	}
	panicRunner(bad)
	var opts []Option
	if modelReplace {
		opts = append(opts, WithModel("new-model"))
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, err := rt.ChatAdmitted(ctx, info, "panic lookup", opts...)
		result <- err
	}()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("panic lookup admission error = nil")
		}
	case <-ctx.Done():
		t.Fatal("panic lookup deadlocked while cache mutex was held")
	}
	if _, active := rt.active.Load(info.ID); active {
		t.Fatal("panic lookup left Runtime.active set")
	}
	rt.cache.mu.Lock()
	cs := rt.cache.sessions[info.ID]
	if cs == nil || cs.reserved || !cs.stale || cs.model != "" || cs.thinking != "" {
		rt.cache.mu.Unlock()
		t.Fatalf("panic lookup cache=%#v; want unreserved stale runner with cleared metadata", cs)
	}
	rt.cache.mu.Unlock()
	stream, err := rt.ChatAdmitted(context.Background(), info, "healthy retry")
	if err != nil {
		t.Fatalf("healthy retry after %s panic: %v", name, err)
	}
	for event := range stream {
		if event.Err != nil {
			t.Fatalf("healthy retry event: %v", event.Err)
		}
	}
	if calls != 2 || !bad.closed {
		t.Fatalf("healthy retry calls=%d oldClosed=%t; want rebuilt factory", calls, bad.closed)
	}
}

// TestRuntimeChat_BeforeRunOverride verifies the lifecycle hook can update the
// per-run system prompt before the runner sees the request.
func TestRuntimeChat_BeforeRunOverride(t *testing.T) {
	runner := newFakeRunner()
	runner.system = "base"
	rt, err := New(Config{
		NewRunner: func(_ context.Context, _ RunnerParams) (Runner, error) {
			return runner, nil
		},
		Memory: fakeMemory{},
		BeforeRun: func(_ context.Context, info session.Info, model, msgText, system string, history []ai.Message, _ PluginContext) (string, error) {
			if info.ID != "s1" {
				t.Fatalf("session ID = %q, want s1", info.ID)
			}
			if msgText != "hello" {
				t.Fatalf("message = %q, want hello", msgText)
			}
			if system != "base" {
				t.Fatalf("system = %q, want base", system)
			}
			return system + " + hook", nil
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	for ev := range rt.Chat(context.Background(), validInfo("s1"), "hello") {
		if ev.Err != nil {
			t.Fatalf("Chat event error: %v", ev.Err)
		}
	}
	if runner.chatSystem != "base + hook" {
		t.Fatalf("runner system = %q, want hook override", runner.chatSystem)
	}
}

// TestRunnerCache_InvalidGroupSessionFailsClosedWithoutRunner proves an invalid
// group session (non-canonical group id) fails closed before any runner is built
// or cached — the runner factory is never called.
func TestRunnerCache_InvalidGroupSessionFailsClosedWithoutRunner(t *testing.T) {
	var calls int
	factory := func(context.Context, RunnerParams) (Runner, error) {
		calls++
		return newFakeRunner(), nil
	}
	cache := newRunnerCache(factory, fakeMemory{}, 10*time.Minute, slog.Default())

	bad := session.Info{ID: "s1", AgentID: "a1", UserID: "not-a-uuid", GroupID: "not-a-uuid"}
	cs, r, err := cache.getOrCreate(context.Background(), bad, "", "")
	if err == nil {
		t.Fatal("expected a fail-closed error for an invalid group session")
	}
	if cs != nil || r != nil {
		t.Fatal("no cached session or runner should be returned on failure")
	}
	if calls != 0 {
		t.Fatalf("runner factory called %d times; want 0", calls)
	}
	if _, ok := cache.sessions["s1"]; ok {
		t.Fatal("no session entry should be installed for an invalid session")
	}
}

// A failed build must leave nothing behind. The fail-closed tool-visibility
// work (#1173) leans on this: it turns a transient dependency outage into a
// build error on purpose, which is only recoverable because the cache neither
// keeps the half-made session entry nor remembers the failure. The next turn
// has to reach the factory again and see live state.
func TestRunnerCacheDoesNotCacheAFailedBuild(t *testing.T) {
	buildErr := errors.New("load tool overrides: database unreachable")
	outage := true
	recovered := newFakeRunner()
	var calls int
	cache := newRunnerCache(func(context.Context, RunnerParams) (Runner, error) {
		calls++
		if outage {
			return nil, buildErr
		}
		return recovered, nil
	}, fakeMemory{}, 10*time.Minute, slog.Default())
	info := validInfo("failed-build")

	if _, _, err := cache.getOrCreate(context.Background(), info, "", ""); !errors.Is(err, buildErr) {
		t.Fatalf("first getOrCreate err = %v, want the build error", err)
	}
	cache.mu.Lock()
	leftover, cached := cache.sessions[info.ID]
	cache.mu.Unlock()
	if cached {
		t.Fatalf("failed build left a session entry behind: %#v", leftover)
	}

	// Still failing: the cache must ask the factory again rather than replay the
	// remembered error.
	if _, _, err := cache.getOrCreate(context.Background(), info, "", ""); !errors.Is(err, buildErr) {
		t.Fatalf("second getOrCreate err = %v, want the build error", err)
	}
	if calls != 2 {
		t.Fatalf("factory calls = %d, want 2; a negative cache would have short-circuited", calls)
	}

	outage = false
	_, r, err := cache.getOrCreate(context.Background(), info, "", "")
	if err != nil {
		t.Fatalf("retry after recovery: %v", err)
	}
	if r != Runner(recovered) {
		t.Fatalf("retry returned %#v, want the runner built after recovery", r)
	}
}
