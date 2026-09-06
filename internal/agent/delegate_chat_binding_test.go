package agent_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/CherryHQ/stella/internal/agent"
	agentruntime "github.com/CherryHQ/stella/internal/agent/runtime"
	"github.com/CherryHQ/stella/internal/agent/session"
	"github.com/CherryHQ/stella/internal/authz"
	"github.com/CherryHQ/stella/internal/core/agentctx"
	"github.com/CherryHQ/stella/internal/memory"
	"github.com/CherryHQ/stella/internal/memory/memorytest"
	"github.com/CherryHQ/stella/pkg/ai"
)

// ctxCapturingRunner records the context a turn actually reached the model loop
// with, which is where tools read the chat binding from.
type ctxCapturingRunner struct{ turnCtx context.Context } //nolint:containedctx // the captured value is the assertion

func (r *ctxCapturingRunner) Chat(ctx context.Context, _ []ai.Message, _ agentruntime.MessageContent) <-chan agentruntime.Event {
	r.turnCtx = ctx
	ch := make(chan agentruntime.Event, 1)
	ch <- agentruntime.Event{Text: "done"}
	close(ch)
	return ch
}

func (r *ctxCapturingRunner) Alive() bool             { return true }
func (r *ctxCapturingRunner) Busy() bool              { return false }
func (r *ctxCapturingRunner) LastActivity() time.Time { return time.Now() }
func (r *ctxCapturingRunner) SystemPrompt() string    { return "" }
func (r *ctxCapturingRunner) PluginContext() agentruntime.PluginContext {
	return agentruntime.PluginContext{}
}
func (r *ctxCapturingRunner) Close() error { return nil }

func newBoundaryTestService(t *testing.T, mem memory.Provider, runner agentruntime.Runner) *agent.Service {
	t.Helper()
	rt, err := agentruntime.New(agentruntime.Config{
		Memory: mem,
		NewRunner: func(context.Context, agentruntime.RunnerParams) (agentruntime.Runner, error) {
			return runner, nil
		},
	})
	if err != nil {
		t.Fatalf("agentruntime.New: %v", err)
	}
	reg, err := session.NewRegistry(mem, "agent1")
	if err != nil {
		t.Fatalf("session.NewRegistry: %v", err)
	}
	return &agent.Service{Sessions: reg, Runtime: rt, SessionAccess: fakeSessionAccessSvc{reg: reg}, SessionInbox: &fakeSessionInbox{}, AgentID: "agent1"}
}

// TestService_DelegateDropsParentChatBinding pins the second half of the
// chat -> delegate boundary. The delegate tool clears the binding when it builds
// the child context, but Delegate is the chokepoint every delegate turn passes,
// including ones a future caller wires up differently. A delegate runs in its
// own session, so carrying the parent chat's binding into it would let a
// nested run act on a conversation the delegate is not part of.
func TestService_DelegateDropsParentChatBinding(t *testing.T) {
	runner := &ctxCapturingRunner{}
	mem := memorytest.New()
	svc := newBoundaryTestService(t, mem, runner)

	ctx := authz.WithAgentID(authz.WithUserID(context.Background(), "u1"), "agent1")
	ctx = memory.WithSessionID(ctx, "source-chat")
	ctx = agentctx.WithChatBinding(ctx, agentctx.ChatBinding{Main: true, SessionKey: "agent1:main:u1"})

	if _, err := svc.Delegate(ctx, agent.DelegateRequest{UserID: "u1", AgentID: "agent1", Task: "work"}); err != nil {
		t.Fatalf("Delegate: %v", err)
	}
	if runner.turnCtx == nil {
		t.Fatal("delegate turn never reached the runner")
	}
	if binding, ok := agentctx.ChatBindingFromContext(runner.turnCtx); ok {
		t.Fatalf("delegate turn carried the parent chat binding %+v", binding)
	}
}

func TestServiceConversationDropsSourceChatBinding(t *testing.T) {
	runner := &ctxCapturingRunner{}
	svc := newBoundaryTestService(t, memorytest.New(), runner)
	target := session.Info{ID: "target-chat", UserID: "u1", AgentID: "agent1", Kind: string(session.KindChat)}

	ctx := authz.WithAgentID(authz.WithUserID(context.Background(), "u1"), "agent1")
	ctx = memory.WithSessionID(ctx, "source-chat")
	ctx = agentctx.WithChatBinding(ctx, agentctx.ChatBinding{Main: true, SessionKey: "agent1:main:u1"})
	ctx, err := agentctx.EnterSessionCall(ctx, "source-chat", target.ID)
	if err != nil {
		t.Fatal(err)
	}
	for event := range svc.RunConversationSession(ctx, target, "work") {
		if event.Err != nil {
			t.Fatalf("conversation turn: %v", event.Err)
		}
	}
	if runner.turnCtx == nil {
		t.Fatal("conversation turn never reached the runner")
	}
	if binding, ok := agentctx.ChatBindingFromContext(runner.turnCtx); ok {
		t.Fatalf("conversation turn carried the source chat binding %+v", binding)
	}
}

type cancelBlockingRunner struct {
	started chan struct{}
	once    sync.Once
}

type largeOutputRunner struct {
	done chan struct{}
	err  error
}

func (r *largeOutputRunner) Chat(context.Context, []ai.Message, agentruntime.MessageContent) <-chan agentruntime.Event {
	stream := make(chan agentruntime.Event)
	go func() {
		defer close(r.done)
		defer close(stream)
		for range 100 {
			stream <- agentruntime.Event{Text: strings.Repeat("x", 1_024)}
		}
		stream <- agentruntime.Event{Err: r.err}
	}()
	return stream
}

func (r *largeOutputRunner) Alive() bool             { return true }
func (r *largeOutputRunner) Busy() bool              { return false }
func (r *largeOutputRunner) LastActivity() time.Time { return time.Now() }
func (r *largeOutputRunner) SystemPrompt() string    { return "" }
func (r *largeOutputRunner) PluginContext() agentruntime.PluginContext {
	return agentruntime.PluginContext{}
}
func (r *largeOutputRunner) Close() error { return nil }

func TestServiceDelegateBoundsLargeOutputAndPreservesTerminalError(t *testing.T) {
	wantErr := errors.New("terminal delegate error")
	runner := &largeOutputRunner{done: make(chan struct{}), err: wantErr}
	svc := newBoundaryTestService(t, memorytest.New(), runner)
	ctx := memory.WithSessionID(authz.WithAgentID(authz.WithUserID(context.Background(), "u1"), "agent1"), "source-chat")

	result, err := svc.Delegate(ctx, agent.DelegateRequest{UserID: "u1", AgentID: "agent1", Task: "work"})
	if !errors.Is(err, wantErr) {
		t.Fatalf("delegate error=%v, want %v", err, wantErr)
	}
	if len(result.Output) > session.MaxSynchronousOutputBytes || !result.OutputTruncated || result.Complete {
		t.Fatalf("delegate result bytes=%d truncated=%v complete=%v", len(result.Output), result.OutputTruncated, result.Complete)
	}
	select {
	case <-runner.done:
	case <-time.After(time.Second):
		t.Fatal("delegate producer remained blocked after large output")
	}
}

func (r *cancelBlockingRunner) Chat(ctx context.Context, _ []ai.Message, _ agentruntime.MessageContent) <-chan agentruntime.Event {
	r.once.Do(func() { close(r.started) })
	stream := make(chan agentruntime.Event)
	go func() {
		<-ctx.Done()
		close(stream)
	}()
	return stream
}

func (r *cancelBlockingRunner) Alive() bool             { return true }
func (r *cancelBlockingRunner) Busy() bool              { return false }
func (r *cancelBlockingRunner) LastActivity() time.Time { return time.Now() }
func (r *cancelBlockingRunner) SystemPrompt() string    { return "" }
func (r *cancelBlockingRunner) PluginContext() agentruntime.PluginContext {
	return agentruntime.PluginContext{}
}
func (r *cancelBlockingRunner) Close() error { return nil }

func TestServiceDelegatePreservesPostAdmissionCancellation(t *testing.T) {
	runner := &cancelBlockingRunner{started: make(chan struct{})}
	svc := newBoundaryTestService(t, memorytest.New(), runner)
	ctx := memory.WithSessionID(authz.WithAgentID(authz.WithUserID(context.Background(), "u1"), "agent1"), "source-chat")
	ctx, err := agentctx.EnterSessionCall(ctx, "source-chat", "")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	type delegateOutcome struct {
		result agent.DelegateResult
		err    error
	}
	done := make(chan delegateOutcome, 1)
	go func() {
		result, runErr := svc.Delegate(ctx, agent.DelegateRequest{UserID: "u1", AgentID: "agent1", Task: "work"})
		done <- delegateOutcome{result: result, err: runErr}
	}()
	select {
	case <-runner.started:
	case <-time.After(time.Second):
		t.Fatal("delegate turn was not admitted")
	}
	cancel()
	select {
	case outcome := <-done:
		if !errors.Is(outcome.err, context.Canceled) || outcome.result.Complete {
			t.Fatalf("delegate outcome=%+v err=%v, want incomplete cancellation", outcome.result, outcome.err)
		}
	case <-time.After(time.Second):
		t.Fatal("delegate cancellation did not return promptly")
	}
}

type bufferedCancellationRunner struct {
	stream  chan agentruntime.Event
	started chan struct{}
	once    sync.Once
}

func newBufferedCancellationRunner() *bufferedCancellationRunner {
	const eventCount = 400
	runner := &bufferedCancellationRunner{
		stream:  make(chan agentruntime.Event, eventCount),
		started: make(chan struct{}),
	}
	for range eventCount {
		runner.stream <- agentruntime.Event{Text: "partial"}
	}
	return runner
}

func (r *bufferedCancellationRunner) Chat(ctx context.Context, _ []ai.Message, _ agentruntime.MessageContent) <-chan agentruntime.Event {
	r.once.Do(func() { close(r.started) })
	go func() {
		<-ctx.Done()
		close(r.stream)
	}()
	return r.stream
}

func (r *bufferedCancellationRunner) Alive() bool             { return true }
func (r *bufferedCancellationRunner) Busy() bool              { return false }
func (r *bufferedCancellationRunner) LastActivity() time.Time { return time.Now() }
func (r *bufferedCancellationRunner) SystemPrompt() string    { return "" }
func (r *bufferedCancellationRunner) PluginContext() agentruntime.PluginContext {
	return agentruntime.PluginContext{}
}
func (r *bufferedCancellationRunner) Close() error { return nil }

func TestServiceConversationKeepsCancellationWhenOutputBufferIsFull(t *testing.T) {
	runner := newBufferedCancellationRunner()
	svc := newBoundaryTestService(t, memorytest.New(), runner)
	target := session.Info{ID: "target-chat", UserID: "u1", AgentID: "agent1", Kind: string(session.KindChat)}
	ctx := memory.WithSessionID(authz.WithAgentID(authz.WithUserID(context.Background(), "u1"), "agent1"), "source-chat")
	ctx, err := agentctx.EnterSessionCall(ctx, "source-chat", target.ID)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stream := svc.RunConversationSession(ctx, target, "work")

	select {
	case <-runner.started:
	case <-time.After(time.Second):
		t.Fatal("conversation turn was not admitted")
	}
	deadline := time.Now().Add(time.Second)
	for len(runner.stream) > 100 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if len(runner.stream) > 100 {
		t.Fatalf("output pipeline did not fill: runner still has %d events", len(runner.stream))
	}
	cancel()

	var terminalErr error
	for event := range stream {
		if event.Err != nil {
			terminalErr = event.Err
		}
	}
	if !errors.Is(terminalErr, context.Canceled) {
		t.Fatalf("terminal error=%v, want cancellation after buffered partial output", terminalErr)
	}
}

type blockingCompactionMemory struct {
	*memorytest.Fake
	mu      sync.Mutex
	needed  bool
	started chan struct{}
}

func (m *blockingCompactionMemory) NeedsCompaction(context.Context, memory.Session, float64) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.needed
}

func (m *blockingCompactionMemory) Compact(ctx context.Context, _ memory.Session, _ memory.CompactionMode) (*memory.CompactionResult, error) {
	m.mu.Lock()
	m.needed = false
	close(m.started)
	m.mu.Unlock()
	<-ctx.Done()
	return nil, ctx.Err()
}

type cancellationAwareRunner struct{}

func (cancellationAwareRunner) Chat(ctx context.Context, _ []ai.Message, _ agentruntime.MessageContent) <-chan agentruntime.Event {
	stream := make(chan agentruntime.Event, 1)
	if ctx.Err() == nil {
		stream <- agentruntime.Event{Text: "done"}
	}
	close(stream)
	return stream
}

func (cancellationAwareRunner) Alive() bool             { return true }
func (cancellationAwareRunner) Busy() bool              { return false }
func (cancellationAwareRunner) LastActivity() time.Time { return time.Now() }
func (cancellationAwareRunner) SystemPrompt() string    { return "" }
func (cancellationAwareRunner) PluginContext() agentruntime.PluginContext {
	return agentruntime.PluginContext{}
}
func (cancellationAwareRunner) Close() error { return nil }

func TestServiceConversationCancellationStopsNestedCompactionAndReleasesTarget(t *testing.T) {
	mem := &blockingCompactionMemory{Fake: memorytest.New(), needed: true, started: make(chan struct{})}
	svc := newBoundaryTestService(t, mem, cancellationAwareRunner{})
	target := session.Info{ID: "target-chat", UserID: "u1", AgentID: "agent1", Kind: string(session.KindChat)}
	base := memory.WithSessionID(authz.WithAgentID(authz.WithUserID(context.Background(), "u1"), "agent1"), "source-chat")
	firstCtx, err := agentctx.EnterSessionCall(base, "source-chat", target.ID)
	if err != nil {
		t.Fatal(err)
	}
	firstCtx, cancelFirst := context.WithCancel(firstCtx)
	first := svc.RunConversationSession(firstCtx, target, "first")
	select {
	case <-mem.started:
	case <-time.After(time.Second):
		cancelFirst()
		t.Fatal("nested auto-compaction did not start")
	}
	cancelFirst()
	firstDone := make(chan error, 1)
	go func() {
		var terminalErr error
		for event := range first {
			if event.Err != nil {
				terminalErr = event.Err
			}
		}
		firstDone <- terminalErr
	}()
	select {
	case terminalErr := <-firstDone:
		if !errors.Is(terminalErr, context.Canceled) {
			t.Fatalf("first turn error=%v, want cancellation", terminalErr)
		}
	case <-time.After(time.Second):
		t.Fatal("source cancellation did not stop nested compaction promptly")
	}

	secondBase, cancelSecond := context.WithTimeout(base, time.Second)
	defer cancelSecond()
	secondCtx, err := agentctx.EnterSessionCall(secondBase, "source-chat", target.ID)
	if err != nil {
		t.Fatal(err)
	}
	var output string
	for event := range svc.RunConversationSession(secondCtx, target, "second") {
		if event.Err != nil {
			t.Fatalf("second turn after cancellation: %v", event.Err)
		}
		output += event.Text
	}
	if output != "done" {
		t.Fatalf("second turn output=%q, want target released with done", output)
	}
}
