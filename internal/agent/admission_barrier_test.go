package agent

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	agentruntime "github.com/CherryHQ/stella/internal/agent/runtime"
	"github.com/CherryHQ/stella/internal/agent/session"
	"github.com/CherryHQ/stella/internal/memory/memorytest"
	"github.com/CherryHQ/stella/internal/platform/config"
	"github.com/CherryHQ/stella/internal/skill/policy"
	"github.com/CherryHQ/stella/pkg/ai"
)

type barrierRunner struct {
	snapshot string
	started  chan struct{}
	release  chan struct{}
	busy     atomic.Bool
	closed   atomic.Bool
	once     sync.Once
}

func (r *barrierRunner) Chat(context.Context, []ai.Message, agentruntime.MessageContent) <-chan agentruntime.Event {
	r.busy.Store(true)
	r.once.Do(func() { close(r.started) })
	out := make(chan agentruntime.Event, 1)
	go func() {
		<-r.release
		r.busy.Store(false)
		out <- agentruntime.Event{Text: r.snapshot}
		close(out)
	}()
	return out
}
func (r *barrierRunner) Alive() bool                  { return true }
func (r *barrierRunner) Busy() bool                   { return r.busy.Load() }
func (r *barrierRunner) LastActivity() time.Time      { return time.Now() }
func (r *barrierRunner) SystemPrompt() string         { return r.snapshot }
func (r *barrierRunner) PluginContext() PluginContext { return PluginContext{} }
func (r *barrierRunner) Close() error                 { r.closed.Store(true); return nil }

func newBarrierService(t *testing.T) (*Service, *agentruntime.Runtime, chan *barrierRunner) {
	t.Helper()
	runners := make(chan *barrierRunner, 8)
	factory := func(snapshot string) agentruntime.NewRunnerFunc {
		return func(context.Context, agentruntime.RunnerParams) (agentruntime.Runner, error) {
			r := &barrierRunner{snapshot: snapshot, started: make(chan struct{}), release: make(chan struct{})}
			runners <- r
			return r, nil
		}
	}
	rt, err := agentruntime.New(agentruntime.Config{NewRunner: factory("old"), Memory: memorytest.New()})
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}
	return &Service{Runtime: rt, AgentID: "agent"}, rt, runners
}

func barrierInfo(id string) session.Info {
	return session.Info{ID: id, UserID: "user", AgentID: "agent", Kind: string(session.KindChat), Channel: string(session.ChannelWeb)}
}

func waitBarrierRunner(t *testing.T, runners <-chan *barrierRunner) *barrierRunner {
	t.Helper()
	select {
	case r := <-runners:
		select {
		case <-r.started:
		case <-time.After(time.Second):
			t.Fatal("runner was created but did not start")
		}
		return r
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for runner")
		return nil
	}
}

func waitText(t *testing.T, stream <-chan Event) string {
	t.Helper()
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	var text string
	for {
		select {
		case event, ok := <-stream:
			if !ok {
				return text
			}
			if event.Err != nil {
				t.Fatalf("turn failed: %v", event.Err)
			}
			text += event.Text
		case <-timer.C:
			t.Fatal("timed out waiting for turn completion")
			return ""
		}
	}
}

// A policy mutation owns the per-Agent barrier until its committed snapshot is
// installed and old runners are stale. A waiting channel/web turn can therefore
// only be admitted against the new factory.
func TestAdmissionBarrierPolicyCommitPrecedesWaitingTurn(t *testing.T) {
	svc, rt, runners := newBarrierService(t)
	entered := make(chan struct{})
	releaseMutation := make(chan struct{})
	mutationDone := make(chan error, 1)
	go func() {
		mutationDone <- svc.withAdmissionBarrier(func() error {
			close(entered)
			<-releaseMutation // stand-in for the row-locked DB mutation and commit
			rt.SetNewRunner(func(context.Context, agentruntime.RunnerParams) (agentruntime.Runner, error) {
				r := &barrierRunner{snapshot: "new", started: make(chan struct{}), release: make(chan struct{})}
				runners <- r
				return r, nil
			})
			return rt.InvalidateSkillPolicy()
		})
	}()
	<-entered

	admitted := make(chan (<-chan Event), 1)
	go func() {
		stream, err := svc.admit(context.Background(), barrierInfo("after-commit"), "turn")
		if err != nil {
			t.Errorf("admit waiting turn: %v", err)
			return
		}
		admitted <- stream
	}()
	select {
	case <-admitted:
		t.Fatal("turn admitted while policy mutation owned barrier")
	case <-time.After(30 * time.Millisecond):
	}
	close(releaseMutation)
	if err := <-mutationDone; err != nil {
		t.Fatalf("commit/invalidate: %v", err)
	}
	stream := <-admitted
	r := waitBarrierRunner(t, runners)
	if r.snapshot != "new" {
		t.Fatalf("post-commit turn runner snapshot = %q, want new", r.snapshot)
	}
	close(r.release)
	if got := waitText(t, stream); got != "new" {
		t.Fatalf("post-commit turn output = %q, want new", got)
	}
}

// A turn that gets through admission first may retain the old immutable
// snapshot. The mutation waits for that admission, marks its busy runner stale,
// and the next admission receives the committed snapshot.
func TestAdmissionBarrierTurnPrecedesPolicyCommitAndFailureDoesNotInvalidate(t *testing.T) {
	svc, rt, runners := newBarrierService(t)
	admitted := make(chan (<-chan Event), 1)
	turnHoldingBarrier := make(chan struct{})
	releaseAdmission := make(chan struct{})
	go func() {
		_ = svc.withAdmissionBarrier(func() error {
			stream, err := svc.admitLocked(context.Background(), barrierInfo("old-turn"), "turn")
			if err != nil {
				t.Errorf("admit old turn: %v", err)
				return err
			}
			admitted <- stream
			close(turnHoldingBarrier)
			<-releaseAdmission
			return nil
		})
	}()
	<-turnHoldingBarrier
	oldStream := <-admitted
	old := waitBarrierRunner(t, runners)

	mutationStarted := make(chan struct{})
	mutationDone := make(chan error, 1)
	go func() {
		mutationDone <- svc.withAdmissionBarrier(func() error {
			close(mutationStarted)
			rt.SetNewRunner(func(context.Context, agentruntime.RunnerParams) (agentruntime.Runner, error) {
				r := &barrierRunner{snapshot: "new", started: make(chan struct{}), release: make(chan struct{})}
				runners <- r
				return r, nil
			})
			return rt.InvalidateSkillPolicy()
		})
	}()
	select {
	case <-mutationStarted:
		t.Fatal("mutation acquired barrier before prior admission released it")
	case <-time.After(30 * time.Millisecond):
	}
	close(releaseAdmission)
	if err := <-mutationDone; err != nil {
		t.Fatalf("commit/invalidate: %v", err)
	}
	if !old.Busy() { // still running; it must not be closed or replaced mid-turn.
		t.Fatal("old admitted runner stopped before its turn completed")
	}
	close(old.release)
	if got := waitText(t, oldStream); got != "old" {
		t.Fatalf("pre-commit admitted turn output = %q, want old", got)
	}

	newStream, err := svc.admit(context.Background(), barrierInfo("next-turn"), "turn")
	if err != nil {
		t.Fatalf("admit next turn: %v", err)
	}
	newRunner := waitBarrierRunner(t, runners)
	if newRunner.snapshot != "new" {
		t.Fatalf("next turn runner snapshot = %q, want new", newRunner.snapshot)
	}
	close(newRunner.release)
	if got := waitText(t, newStream); got != "new" {
		t.Fatalf("next turn output = %q, want new", got)
	}

	// A rolled-back mutation releases the barrier but cannot mark/rebuild a
	// runner. Use the old session once more to prove its existing runner remains.
	failed := svc.withAdmissionBarrier(func() error { return errors.New("rollback") })
	if failed == nil {
		t.Fatal("failed mutation error = nil")
	}
	// The successful mutation made old stale, so prove no *additional* rebuild
	// occurs on failure by reusing the new runner's session.
	reused, err := svc.admit(context.Background(), barrierInfo("next-turn"), "turn")
	if err != nil {
		t.Fatalf("admit after failed mutation: %v", err)
	}
	select {
	case unexpected := <-runners:
		t.Fatalf("failed mutation rebuilt runner with snapshot %q", unexpected.snapshot)
	case <-time.After(30 * time.Millisecond):
	}
	if got := waitText(t, reused); got != "new" {
		t.Fatalf("turn after failed mutation output = %q, want reused new snapshot", got)
	}
}

// A policy mutation that begins before a Service exists holds a shared lifecycle
// lease. Publication waits for exclusive ownership, then loads committed state.
func TestAgentSkillPolicyNoServiceBlocksPublicationUntilCommit(t *testing.T) {
	pm := NewPoolManager(nil, nil)
	const agentID = "agent"
	mutationEntered := make(chan struct{})
	commit := make(chan struct{})
	published := make(chan struct{})
	result := make(chan error, 1)
	var unexpectedRefresh atomic.Bool
	go func() {
		result <- pm.applyAgentSkillPolicyMutation(agentID, func() error {
			close(mutationEntered)
			<-commit
			return nil
		}, func(string, *Service) error {
			unexpectedRefresh.Store(true)
			return nil
		})
	}()
	<-mutationEntered
	go func() {
		_ = pm.lifecycle.lockExclusive(context.Background())
		defer pm.lifecycle.unlockExclusive()
		pm.mu.Lock()
		pm.services[agentID] = &Service{AgentID: agentID, lifecycle: pm.lifecycle}
		pm.mu.Unlock()
		close(published)
	}()
	select {
	case <-published:
		t.Fatal("Service published before no-Service mutation committed")
	default:
	}
	close(commit)
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	if unexpectedRefresh.Load() {
		t.Fatal("no-Service mutation unexpectedly refreshed a Service")
	}
	<-published
}

func TestAgentSkillPolicyUnknownCommitRefreshesBeforeReturning(t *testing.T) {
	svc, rt, runners := newBarrierService(t)
	pm := NewPoolManager(nil, nil)
	pm.services[svc.AgentID] = svc
	refreshed := false
	err := pm.applyAgentSkillPolicyMutation(svc.AgentID, func() error {
		return policy.ErrCommitOutcomeUnknown
	}, func(_ string, _ *Service) error {
		refreshed = true
		rt.SetNewRunner(func(context.Context, agentruntime.RunnerParams) (agentruntime.Runner, error) {
			r := &barrierRunner{snapshot: "committed", started: make(chan struct{}), release: make(chan struct{})}
			runners <- r
			return r, nil
		})
		return rt.InvalidateSkillPolicy()
	})
	if !errors.Is(err, policy.ErrCommitOutcomeUnknown) || !refreshed {
		t.Fatalf("unknown commit result=%v refreshed=%t", err, refreshed)
	}
	stream, err := svc.admit(context.Background(), barrierInfo("unknown-commit"), "turn")
	if err != nil {
		t.Fatalf("post-unknown admission: %v", err)
	}
	r := waitBarrierRunner(t, runners)
	if r.snapshot != "committed" {
		t.Fatalf("post-unknown runner snapshot=%q, want committed", r.snapshot)
	}
	close(r.release)
	if got := waitText(t, stream); got != "committed" {
		t.Fatalf("post-unknown output=%q, want committed", got)
	}
}

func TestAgentSkillPolicyKnownMutationFailureDoesNotRefresh(t *testing.T) {
	pm := NewPoolManager(nil, nil)
	svc := &Service{AgentID: "agent"}
	pm.services[svc.AgentID] = svc
	preCommit := errors.New("write failed")
	refreshed := false
	err := pm.applyAgentSkillPolicyMutation(svc.AgentID, func() error { return preCommit }, func(string, *Service) error {
		refreshed = true
		return nil
	})
	if !errors.Is(err, preCommit) || refreshed {
		t.Fatalf("known mutation failure result=%v refreshed=%t", err, refreshed)
	}
}

type failingPolicySnapshotStore struct {
	config.Store
	err error
}

func (s failingPolicySnapshotStore) Snapshot(context.Context, string) (*config.Snapshot, error) {
	return nil, s.err
}

func TestAgentSkillPolicyUnknownCommitRefreshFailurePoisonsRuntime(t *testing.T) {
	t.Setenv("STELLA_HOME", t.TempDir())
	config.ResetStellaHome()
	t.Cleanup(config.ResetStellaHome)
	svc, _, runners := newBarrierService(t)
	pm := NewPoolManager(failingPolicySnapshotStore{err: errors.New("snapshot unavailable")}, memorytest.New())
	pm.services[svc.AgentID] = svc

	oldStream, err := svc.admit(context.Background(), barrierInfo("old-idle"), "turn")
	if err != nil {
		t.Fatalf("seed old admission: %v", err)
	}
	old := waitBarrierRunner(t, runners)
	close(old.release)
	if got := waitText(t, oldStream); got != "old" {
		t.Fatalf("seed old output=%q, want old", got)
	}

	err = pm.ApplyAgentSkillPolicyMutation(svc.AgentID, func() error {
		return policy.ErrCommitOutcomeUnknown
	})
	if !errors.Is(err, policy.ErrCommitOutcomeUnknown) {
		t.Fatalf("unknown commit result=%v", err)
	}
	if !old.closed.Load() {
		t.Fatal("refresh failure left idle old runner live")
	}
	if _, err := svc.admit(context.Background(), barrierInfo("next-fails-closed"), "turn"); err == nil {
		t.Fatal("poisoned runtime admitted a turn with the old factory")
	}
}
