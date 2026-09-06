package reflect

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/CherryHQ/stella/internal/agent"
	"github.com/CherryHQ/stella/internal/agent/session"
	"github.com/CherryHQ/stella/internal/authz"
	"github.com/CherryHQ/stella/internal/memory"
	"github.com/CherryHQ/stella/internal/memory/memorytest"
	"github.com/CherryHQ/stella/internal/platform/config"
	"github.com/CherryHQ/stella/pkg/ai"
	pkgplugins "github.com/CherryHQ/stella/pkg/plugins"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

// fakeWatermarks is a test double for watermarker.
type fakeWatermarks struct {
	mu        sync.RWMutex
	lineMarks map[string]time.Time
	lineSeqs  map[string]int64
}

func newFakeWatermarks() *fakeWatermarks {
	return &fakeWatermarks{
		lineMarks: make(map[string]time.Time),
		lineSeqs:  make(map[string]int64),
	}
}

func (f *fakeWatermarks) getLine(_ context.Context, sessionID string, line reflectLine) (reviewWatermark, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := fakeLineWatermarkKey(sessionID, line)
	return reviewWatermark{At: f.lineMarks[key], Seq: f.lineSeqs[key]}, nil
}

func (f *fakeWatermarks) setLine(_ context.Context, sessionID string, line reflectLine, mark reviewWatermark) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := fakeLineWatermarkKey(sessionID, line)
	f.lineMarks[key] = mark.At
	if mark.Seq > 0 {
		f.lineSeqs[key] = mark.Seq
	} else {
		delete(f.lineSeqs, key)
	}
	return nil
}

func (f *fakeWatermarks) setLineMark(sessionID string, line reflectLine, at time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := fakeLineWatermarkKey(sessionID, line)
	f.lineMarks[key] = at
	delete(f.lineSeqs, key)
}

func (f *fakeWatermarks) lineMark(sessionID string, line reflectLine) time.Time {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.lineMarks[fakeLineWatermarkKey(sessionID, line)]
}

func (f *fakeWatermarks) lineSeq(sessionID string, line reflectLine) int64 {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.lineSeqs[fakeLineWatermarkKey(sessionID, line)]
}

func fakeLineWatermarkKey(sessionID string, line reflectLine) string {
	return sessionID + ":" + string(line)
}

type mapStateStore struct {
	values map[string]map[string]any
}

func newMapStateStore() *mapStateStore {
	return &mapStateStore{values: make(map[string]map[string]any)}
}

func (s *mapStateStore) Get(_ context.Context, scope pkgplugins.StateScope, key string) (map[string]any, bool, error) {
	value, ok := s.values[mapStateKey(scope, key)]
	if !ok {
		return nil, false, nil
	}
	out := make(map[string]any, len(value))
	maps.Copy(out, value)
	return out, true, nil
}

func (s *mapStateStore) Set(_ context.Context, scope pkgplugins.StateScope, key string, value map[string]any) error {
	copyValue := make(map[string]any, len(value))
	maps.Copy(copyValue, value)
	s.values[mapStateKey(scope, key)] = copyValue
	return nil
}

func (s *mapStateStore) Delete(_ context.Context, scope pkgplugins.StateScope, key string) error {
	delete(s.values, mapStateKey(scope, key))
	return nil
}

func mapStateKey(scope pkgplugins.StateScope, key string) string {
	normalized := scope.Normalize()
	return normalized.Kind + ":" + normalized.ID + ":" + key
}

type reviewListOnlyFake struct {
	*memorytest.Fake
	reviewCalled bool
	latestSeq    map[string]int64
}

type reviewHistoryProvider struct {
	messages []memory.ReviewMessage
}

func (p *reviewHistoryProvider) Name() string {
	return "review-history-test"
}

func (p *reviewHistoryProvider) Bootstrap(context.Context, memory.Session) error {
	return nil
}

func (p *reviewHistoryProvider) Append(context.Context, memory.Session, ...ai.Message) error {
	return nil
}

func (p *reviewHistoryProvider) Assemble(context.Context, memory.Session, int, int) ([]ai.Message, error) {
	return nil, nil
}

func (p *reviewHistoryProvider) Stats(context.Context, memory.Session) (memory.SessionStats, error) {
	return memory.SessionStats{}, nil
}

func (p *reviewHistoryProvider) Close() error {
	return nil
}

func (p *reviewHistoryProvider) LoadReviewHistory(context.Context, string) ([]memory.ReviewMessage, error) {
	out := make([]memory.ReviewMessage, len(p.messages))
	copy(out, p.messages)
	return out, nil
}

func (f *reviewListOnlyFake) ListInfo(ctx context.Context, opts memory.ListOptions) ([]memory.SessionInfo, error) {
	if authz.UserIDFromContext(ctx) == "" && opts.UserID == "" {
		return nil, fmt.Errorf("missing user context")
	}
	return f.Fake.ListInfo(ctx, opts)
}

func (f *reviewListOnlyFake) ListInfoForReview(ctx context.Context, opts memory.ListOptions) ([]memory.SessionInfo, error) {
	f.reviewCalled = true
	infos, err := f.Fake.ListInfo(ctx, opts)
	if err != nil {
		return nil, err
	}
	for i := range infos {
		infos[i].LatestSeq = f.latestSeq[infos[i].ID]
	}
	return infos, nil
}

func seedFakeSession(t *testing.T, fake *memorytest.Fake, id, agentID string, userID string, lastActive time.Time) {
	t.Helper()
	ctx := context.Background()
	sess := memory.Session{ID: id, AgentID: agentID, UserID: userID}
	if err := fake.Bootstrap(ctx, sess); err != nil {
		t.Fatal(err)
	}
	if err := fake.SaveInfo(ctx, memory.SessionInfo{
		ID:         id,
		AgentID:    agentID,
		UserID:     userID,
		LastActive: lastActive,
	}); err != nil {
		t.Fatal(err)
	}
	if err := fake.Append(ctx, sess, ai.UserMessage{Content: "hello from " + id, Timestamp: lastActive}); err != nil {
		t.Fatal(err)
	}
}

func listUnreviewedFromTestRegistry(t *testing.T, svc *Service, provider memory.Provider, agentID string) ([]reviewTarget, error) {
	t.Helper()
	registry, err := session.NewRegistry(provider, agentID)
	if err != nil {
		return nil, err
	}
	return svc.listUnreviewedFromRegistry(context.Background(), registry, agentID)
}

func TestListUnreviewed_SkipsAnonymous(t *testing.T) {
	fake := memorytest.New()
	svc := &Service{memory: fake, wm: newFakeWatermarks(), log: testLogger()}

	now := time.Now().UTC()
	seedFakeSession(t, fake, "s1", "a", "", now)  // anonymous
	seedFakeSession(t, fake, "s2", "a", "1", now) // has user

	targets, err := listUnreviewedFromTestRegistry(t, svc, fake, "a")
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 1 {
		t.Fatalf("expected 1 review target, got %d", len(targets))
	}
	if targets[0].session.ID != "s2" {
		t.Errorf("expected session s2, got %s", targets[0].session.ID)
	}
}

func TestListUnreviewed_ReturnsReviewTargets(t *testing.T) {
	fake := memorytest.New()
	svc := &Service{memory: fake, wm: newFakeWatermarks(), log: testLogger()}

	now := time.Now().UTC()
	seedFakeSession(t, fake, "s1", "a", "1", now)

	targets, err := listUnreviewedFromTestRegistry(t, svc, fake, "a")
	if err != nil {
		t.Fatal(err)
	}
	assertReviewTargets(t, targets)
	if len(targets) != 1 {
		t.Fatalf("expected 1 review target, got %d", len(targets))
	}
}

func assertReviewTargets(t *testing.T, _ []reviewTarget) {
	t.Helper()
}

func TestListUnreviewed_UsesReviewListerWithoutUserScope(t *testing.T) {
	fake := &reviewListOnlyFake{Fake: memorytest.New()}
	svc := &Service{memory: fake, wm: newFakeWatermarks(), log: testLogger()}

	seedFakeSession(t, fake.Fake, "s1", "a", "1", time.Now().UTC())
	targets, err := listUnreviewedFromTestRegistry(t, svc, fake, "a")
	if err != nil {
		t.Fatal(err)
	}
	if !fake.reviewCalled {
		t.Fatal("expected ListInfoForReview to be used")
	}
	if len(targets) != 1 {
		t.Fatalf("expected 1 review target, got %d", len(targets))
	}
}

func TestListUnreviewed_SkipsAlreadyReviewed(t *testing.T) {
	fake := memorytest.New()
	wm := newFakeWatermarks()
	svc := &Service{memory: fake, wm: wm, log: testLogger()}

	now := time.Now().UTC()
	seedFakeSession(t, fake, "s1", "a", "1", now)
	seedFakeSession(t, fake, "s2", "a", "2", now)

	// Mark s1 as reviewed at or after its LastActive.
	wm.setLineMark("s1", reflectLineFact, now)
	wm.setLineMark("s1", reflectLineSkill, now)

	targets, err := listUnreviewedFromTestRegistry(t, svc, fake, "a")
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 1 {
		t.Fatalf("expected 1 review target, got %d", len(targets))
	}
	if targets[0].session.ID != "s2" {
		t.Errorf("expected session s2, got %s", targets[0].session.ID)
	}
}

func TestListUnreviewed_StructuredUsesOldestIncompleteLine(t *testing.T) {
	fake := memorytest.New()
	wm := newFakeWatermarks()
	svc := &Service{memory: fake, wm: wm, log: testLogger()}

	lastActive := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)
	seedFakeSession(t, fake, "s1", "a", "1", lastActive)
	wm.setLineMark("s1", reflectLineFact, lastActive)
	wm.setLineMark("s1", reflectLineSkill, lastActive.Add(-time.Hour))

	targets, err := listUnreviewedFromTestRegistry(t, svc, fake, "a")
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 1 {
		t.Fatalf("targets = %d, want one while skill line is behind", len(targets))
	}
	if !targets[0].lastReview.Equal(lastActive.Add(-time.Hour)) {
		t.Fatalf("lastReview = %v, want lagging skill boundary", targets[0].lastReview)
	}
}

func TestReviewProgressStructuredSeqDetectsTruncatedSuffix(t *testing.T) {
	ctx := context.Background()
	wm := newFakeWatermarks()
	at := time.Date(2026, 7, 17, 9, 0, 0, 0, time.UTC)
	if err := wm.setLine(ctx, "s1", reflectLineFact, reviewWatermark{At: at, Seq: 15}); err != nil {
		t.Fatal(err)
	}
	if err := wm.setLine(ctx, "s1", reflectLineSkill, reviewWatermark{At: at, Seq: 20}); err != nil {
		t.Fatal(err)
	}
	svc := &Service{wm: wm}

	_, pending, err := svc.reviewProgress(ctx, "s1", at.Add(-time.Minute), 20)
	if err != nil {
		t.Fatal(err)
	}
	if !pending {
		t.Fatal("Seq 16..20 must remain pending when the fact line stopped at Seq 15")
	}
}

func TestListUnreviewedStructuredSeqDetectsConcurrentSuffix(t *testing.T) {
	fake := &reviewListOnlyFake{Fake: memorytest.New(), latestSeq: map[string]int64{"s1": 16}}
	wm := newFakeWatermarks()
	lastActive := time.Date(2026, 7, 17, 9, 0, 0, 0, time.UTC)
	seedFakeSession(t, fake.Fake, "s1", "a", "1", lastActive)
	for _, line := range []reflectLine{reflectLineFact, reflectLineSkill} {
		if err := wm.setLine(context.Background(), "s1", line, reviewWatermark{At: lastActive.Add(time.Minute), Seq: 15}); err != nil {
			t.Fatal(err)
		}
	}
	svc := &Service{memory: fake, wm: wm, log: testLogger()}

	targets, err := listUnreviewedFromTestRegistry(t, svc, fake, "a")
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 1 || targets[0].session.ID != "s1" {
		t.Fatalf("targets = %#v, want s1 despite LastActive not advancing", targets)
	}
}

func TestListUnreviewed_OldestFirst(t *testing.T) {
	fake := memorytest.New()
	wm := newFakeWatermarks()
	svc := &Service{memory: fake, wm: wm, log: testLogger()}

	now := time.Now().UTC()
	seedFakeSession(t, fake, "new", "a", "1", now)
	seedFakeSession(t, fake, "old", "a", "2", now.Add(-2*time.Hour))

	// Give "new" a recent watermark so it still qualifies but is "more recently reviewed".
	wm.setLineMark("new", reflectLineFact, now.Add(-10*time.Minute))
	wm.setLineMark("new", reflectLineSkill, now.Add(-10*time.Minute))

	targets, err := listUnreviewedFromTestRegistry(t, svc, fake, "a")
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 2 {
		t.Fatalf("expected 2 review targets, got %d", len(targets))
	}
	// "old" has zero watermark, so should sort first.
	if targets[0].session.ID != "old" {
		t.Errorf("expected oldest-first ordering, got %s first", targets[0].session.ID)
	}
}

func TestListUnreviewed_ZeroWatermarkTiebreaker(t *testing.T) {
	fake := memorytest.New()
	svc := &Service{memory: fake, wm: newFakeWatermarks(), log: testLogger()}

	now := time.Now().UTC()
	// Both sessions have never been reviewed (zero watermark).
	// "older" has an earlier LastActive and should sort first.
	seedFakeSession(t, fake, "newer", "a", "1", now)
	seedFakeSession(t, fake, "older", "a", "2", now.Add(-3*time.Hour))

	targets, err := listUnreviewedFromTestRegistry(t, svc, fake, "a")
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 2 {
		t.Fatalf("expected 2 review targets, got %d", len(targets))
	}
	if targets[0].session.ID != "older" {
		t.Errorf("expected older session first when both have zero watermark, got %s", targets[0].session.ID)
	}
}

func TestListUnreviewed_PerAgentLimit(t *testing.T) {
	fake := memorytest.New()
	svc := &Service{memory: fake, wm: newFakeWatermarks(), maxReviewTargetsPerAgent: 2, log: testLogger()}

	now := time.Now().UTC()
	for i := range 5 {
		seedFakeSession(t, fake, fmt.Sprintf("s%d", i), "a", fmt.Sprintf("%d", i+1), now.Add(time.Duration(i)*time.Minute))
	}

	targets, err := listUnreviewedFromTestRegistry(t, svc, fake, "a")
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 2 {
		t.Fatalf("expected 2 review targets (per-agent limit), got %d", len(targets))
	}
}

func TestListUnreviewed_UsesMaxReviewTargetsPerAgent(t *testing.T) {
	fake := memorytest.New()
	svc := &Service{
		memory:                   fake,
		wm:                       newFakeWatermarks(),
		maxReviewTargetsPerAgent: 30,
		log:                      testLogger(),
	}

	now := time.Now().UTC()
	for i := range 35 {
		seedFakeSession(t, fake, fmt.Sprintf("s%02d", i), "a", fmt.Sprintf("%d", i+1), now.Add(time.Duration(i)*time.Minute))
	}

	targets, err := listUnreviewedFromTestRegistry(t, svc, fake, "a")
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 30 {
		t.Fatalf("expected 30 review targets (per-agent drain cap), got %d", len(targets))
	}
}

func TestReviewAgentCursorRotatesAgentOrder(t *testing.T) {
	ctx := context.Background()
	state := newMapStateStore()
	svc := &Service{stateStore: state, log: testLogger()}
	agents := []config.Agent{{ID: "agent-a"}, {ID: "agent-b"}, {ID: "agent-c"}}

	if err := state.Set(ctx, pkgplugins.StateScope{Kind: pkgplugins.StateScopeGlobal}, reviewAgentCursorStateKey, map[string]any{
		"next_agent_id": "agent-b",
	}); err != nil {
		t.Fatal(err)
	}

	ordered := svc.orderAgentsForReview(ctx, agents)
	gotOrder := []string{ordered[0].ID, ordered[1].ID, ordered[2].ID}
	wantOrder := []string{"agent-b", "agent-c", "agent-a"}
	if fmt.Sprint(gotOrder) != fmt.Sprint(wantOrder) {
		t.Fatalf("ordered agents = %v, want %v", gotOrder, wantOrder)
	}

	svc.recordNextReviewAgentCursor(ctx, ordered, len(ordered))
	value, ok, err := state.Get(ctx, pkgplugins.StateScope{Kind: pkgplugins.StateScopeGlobal}, reviewAgentCursorStateKey)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || value["next_agent_id"] != "agent-c" {
		t.Fatalf("cursor after full pass = %#v, want next agent-c", value)
	}

	svc.recordNextReviewAgentCursor(ctx, ordered, 2)
	value, _, err = state.Get(ctx, pkgplugins.StateScope{Kind: pkgplugins.StateScopeGlobal}, reviewAgentCursorStateKey)
	if err != nil {
		t.Fatal(err)
	}
	if value["next_agent_id"] != "agent-a" {
		t.Fatalf("cursor after partial pass = %#v, want next unprocessed agent-a", value)
	}
}

type softBudgetReflectStore struct {
	agents []config.Agent
}

func (s softBudgetReflectStore) ListEnabledAgents(context.Context) ([]config.Agent, error) {
	return append([]config.Agent(nil), s.agents...), nil
}

func (s softBudgetReflectStore) Snapshot(_ context.Context, agentID string) (*config.Snapshot, error) {
	return &config.Snapshot{AgentID: agentID}, nil
}

type reflectTestServiceManager struct {
	services map[string]*agent.Service
}

func (m reflectTestServiceManager) GetService(agentID string) *agent.Service {
	return m.services[agentID]
}
func (m reflectTestServiceManager) Default() *agent.Service { return nil }

func TestRunCycleSoftBudgetFinishesCurrentTargetThenRunsCurator(t *testing.T) {
	ctx := context.Background()
	fakeMemory := memorytest.New()
	base := time.Date(2026, 7, 10, 0, 0, 0, 0, time.UTC)
	seedFakeSession(t, fakeMemory, "budget-1", "agent-a", "user-a", base.Add(time.Minute))
	seedFakeSession(t, fakeMemory, "budget-2", "agent-a", "user-a", base.Add(2*time.Minute))
	state := newMapStateStore()
	now := base
	reviewed := 0
	registry, err := session.NewRegistry(fakeMemory, "agent-a")
	if err != nil {
		t.Fatalf("new session registry: %v", err)
	}
	svc := &Service{
		memory:                   fakeMemory,
		store:                    softBudgetReflectStore{agents: []config.Agent{{ID: "agent-a"}, {ID: "agent-b"}}},
		snapshots:                softBudgetReflectStore{},
		stateStore:               state,
		wm:                       newFakeWatermarks(),
		maxReviewTargetsPerAgent: 30,
		runSoftBudget:            15 * time.Minute,
		now:                      func() time.Time { return now },
		usageCuratorStore:        fakeUsageCuratorStore{pairs: []usageCuratorPair{{UserID: "curator-user", AgentID: "curator-agent"}}},
		usageCuratorSettings:     UsageCuratorSettings{Mode: UsageCuratorModeShadow, Now: func() time.Time { return base }},
		log:                      testLogger(),
		capabilityGate:           func(context.Context, authz.Authority, string, ...string) error { return nil },
		services:                 reflectTestServiceManager{services: map[string]*agent.Service{"agent-a": {Sessions: registry}}},
	}
	reviewer := func(context.Context, *config.Snapshot, reviewTarget) error {
		reviewed++
		now = now.Add(16 * time.Minute)
		return nil
	}

	if err := svc.runCycleWithReviewer(ctx, reviewer); err != nil {
		t.Fatalf("runCycleWithReviewer: %v", err)
	}
	if reviewed != 1 {
		t.Fatalf("reviewed targets = %d, want current target only", reviewed)
	}
	cursor, ok, err := state.Get(ctx, pkgplugins.StateScope{Kind: pkgplugins.StateScopeGlobal}, reviewAgentCursorStateKey)
	if err != nil {
		t.Fatalf("read cursor: %v", err)
	}
	if !ok || cursor["next_agent_id"] != "agent-a" {
		t.Fatalf("cursor = %#v, want current agent-a for remaining targets", cursor)
	}
	curatorScope := pkgplugins.StateScope{Kind: pkgplugins.StateScopeAgent, ID: "curator-agent"}
	if _, ok, err := state.Get(ctx, curatorScope, usageCuratorPairStateKey("curator-user")); err != nil || !ok {
		t.Fatalf("curator state = ok:%v err:%v, soft stop must still run curator", ok, err)
	}
}
