package agent

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	agentruntime "github.com/CherryHQ/stella/internal/agent/runtime"
	"github.com/CherryHQ/stella/internal/agent/session"
	sessioninbox "github.com/CherryHQ/stella/internal/agent/session/inbox"
	"github.com/CherryHQ/stella/internal/agent/session/turnqueue"
	"github.com/CherryHQ/stella/internal/eventlog"
	"github.com/CherryHQ/stella/internal/memory"
	"github.com/CherryHQ/stella/internal/memory/memorytest"
	"github.com/CherryHQ/stella/pkg/ai"
)

type recordingSessionInbox struct {
	mu          sync.Mutex
	seq         int64
	inputs      []sessioninbox.Input
	failures    []sessioninbox.ErrorCode
	enqueueErr  error
	failApplied bool
	failErr     error
}

func (f *recordingSessionInbox) Enqueue(_ context.Context, input sessioninbox.Input) (sessioninbox.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq++
	f.inputs = append(f.inputs, input)
	return sessioninbox.Message{ID: fmt.Sprintf("00000000-0000-4000-8000-%012d", f.seq), EnqueueSeq: f.seq}, f.enqueueErr
}

func (f *recordingSessionInbox) FailPending(_ context.Context, _ string, code sessioninbox.ErrorCode) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failures = append(f.failures, code)
	return f.failApplied, f.failErr
}

func (f *recordingSessionInbox) failureCodes() []sessioninbox.ErrorCode {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]sessioninbox.ErrorCode(nil), f.failures...)
}

type gateRunner struct {
	started chan struct{}
	release chan struct{}
	calls   atomic.Int64
}

func (r *gateRunner) Chat(ctx context.Context, _ []ai.Message, _ agentruntime.MessageContent) <-chan agentruntime.Event {
	r.calls.Add(1)
	select {
	case r.started <- struct{}{}:
	default:
	}
	out := make(chan agentruntime.Event, 1)
	go func() {
		defer close(out)
		select {
		case <-r.release:
			out <- agentruntime.Event{Text: "done"}
		case <-ctx.Done():
			out <- agentruntime.Event{Err: ctx.Err()}
		}
	}()
	return out
}

func (*gateRunner) Alive() bool                  { return true }
func (*gateRunner) Busy() bool                   { return false }
func (*gateRunner) LastActivity() time.Time      { return time.Now() }
func (*gateRunner) SystemPrompt() string         { return "" }
func (*gateRunner) PluginContext() PluginContext { return PluginContext{} }
func (*gateRunner) Close() error                 { return nil }

type failingInboxMemory struct {
	*memorytest.Fake
	err error
}

func (m *failingInboxMemory) AppendInboxInput(context.Context, memory.Session, string, ai.Message) error {
	return m.err
}

func newSessionInboxTestService(t *testing.T, mem memory.Provider, runner agentruntime.Runner, queue *turnqueue.Queue, inbox *recordingSessionInbox) *Service {
	t.Helper()
	rt, err := agentruntime.New(agentruntime.Config{
		Memory: mem,
		NewRunner: func(context.Context, agentruntime.RunnerParams) (agentruntime.Runner, error) {
			return runner, nil
		},
	})
	if err != nil {
		t.Fatalf("runtime.New: %v", err)
	}
	svc := &Service{Runtime: rt, SessionInbox: inbox, AgentID: "agent-1"}
	svc.turnQueueOnce.Do(func() { svc.turnQueue = queue })
	return svc
}

func runInboxTestTurn(ctx context.Context, svc *Service, target session.Info) error {
	actor := eventlog.MessageActor{Type: eventlog.ActorAgent, ID: target.AgentID, SourceSessionID: "source-session"}
	return svc.runQueuedTurn(ctx, target, "hello", actor, []agentruntime.Option{
		agentruntime.WithInputActor(actor),
	}, func(stream <-chan Event) error {
		var terminalErr error
		for event := range stream {
			if terminalErr == nil && event.Err != nil {
				terminalErr = event.Err
			}
		}
		return terminalErr
	})
}

func inboxTestTarget() session.Info {
	return session.Info{ID: "target-session", UserID: "user-1", AgentID: "agent-1", Kind: string(session.KindChat)}
}

func TestRunQueuedTurnEnqueueAcknowledgementFailureTerminalizesPossibleRow(t *testing.T) {
	enqueueErr := errors.New("insert acknowledgement lost")
	inbox := &recordingSessionInbox{enqueueErr: enqueueErr, failApplied: true}
	runner := &gateRunner{started: make(chan struct{}, 1), release: make(chan struct{})}
	svc := newSessionInboxTestService(t, memorytest.New(), runner, turnqueue.NewWithLimits(1, time.Second, time.Second), inbox)

	err := runInboxTestTurn(t.Context(), svc, inboxTestTarget())
	if !errors.Is(err, enqueueErr) {
		t.Fatalf("runQueuedTurn error = %v, want enqueue error", err)
	}
	if got := inbox.failureCodes(); len(got) != 1 || got[0] != sessioninbox.ErrorLiveFailed {
		t.Fatalf("failure codes = %v, want [live_failed]", got)
	}
	if runner.calls.Load() != 0 {
		t.Fatalf("runner calls = %d, want 0", runner.calls.Load())
	}
}

func TestRunQueuedTurnQueueFullTerminalizesPendingInbox(t *testing.T) {
	inbox := &recordingSessionInbox{failApplied: true}
	runner := &gateRunner{started: make(chan struct{}, 1), release: make(chan struct{})}
	svc := newSessionInboxTestService(t, memorytest.New(), runner, turnqueue.NewWithLimits(0, time.Second, time.Second), inbox)

	err := runInboxTestTurn(t.Context(), svc, inboxTestTarget())
	if !errors.Is(err, turnqueue.ErrFull) {
		t.Fatalf("runQueuedTurn error = %v, want ErrFull", err)
	}
	if got := inbox.failureCodes(); len(got) != 1 || got[0] != sessioninbox.ErrorQueueFull {
		t.Fatalf("failure codes = %v, want [queue_full]", got)
	}
	if runner.calls.Load() != 0 {
		t.Fatalf("runner calls = %d, want 0", runner.calls.Load())
	}
}

func TestRunQueuedTurnTimeoutAndCancellationTerminalizeBeforeAdmission(t *testing.T) {
	for _, tc := range []struct {
		name string
		stop func(context.CancelFunc)
		want error
		code sessioninbox.ErrorCode
	}{
		{name: "timeout", stop: func(context.CancelFunc) {}, want: turnqueue.ErrTimeout, code: sessioninbox.ErrorTimeout},
		{name: "canceled", stop: func(cancel context.CancelFunc) { cancel() }, want: context.Canceled, code: sessioninbox.ErrorCanceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inbox := &recordingSessionInbox{failApplied: true}
			runner := &gateRunner{started: make(chan struct{}, 1), release: make(chan struct{})}
			hold := 30 * time.Millisecond
			if tc.name == "canceled" {
				hold = time.Second
			}
			svc := newSessionInboxTestService(t, memorytest.New(), runner, turnqueue.NewWithLimits(1, hold, time.Second), inbox)
			firstDone := make(chan error, 1)
			go func() { firstDone <- runInboxTestTurn(context.Background(), svc, inboxTestTarget()) }()
			select {
			case <-runner.started:
			case <-time.After(time.Second):
				t.Fatal("first turn did not start")
			}

			ctx, cancel := context.WithCancel(context.Background())
			secondDone := make(chan error, 1)
			go func() { secondDone <- runInboxTestTurn(ctx, svc, inboxTestTarget()) }()
			time.Sleep(10 * time.Millisecond)
			tc.stop(cancel)
			err := <-secondDone
			cancel()
			if !errors.Is(err, tc.want) {
				t.Fatalf("second turn error = %v, want %v", err, tc.want)
			}
			close(runner.release)
			if err := <-firstDone; err != nil {
				t.Fatalf("first turn: %v", err)
			}
			codes := inbox.failureCodes()
			if len(codes) != 1 || codes[0] != tc.code {
				t.Fatalf("failure codes = %v, want [%s]", codes, tc.code)
			}
			if runner.calls.Load() != 1 {
				t.Fatalf("runner calls = %d, want 1", runner.calls.Load())
			}
		})
	}
}

func TestRunQueuedTurnInboxAppendFailureDoesNotInvokeModel(t *testing.T) {
	appendErr := errors.New("append unavailable")
	mem := &failingInboxMemory{Fake: memorytest.New(), err: appendErr}
	inbox := &recordingSessionInbox{failApplied: true}
	runner := &gateRunner{started: make(chan struct{}, 1), release: make(chan struct{})}
	svc := newSessionInboxTestService(t, mem, runner, turnqueue.NewWithLimits(1, time.Second, time.Second), inbox)

	err := runInboxTestTurn(t.Context(), svc, inboxTestTarget())
	if !errors.Is(err, appendErr) {
		t.Fatalf("runQueuedTurn error = %v, want append error", err)
	}
	if runner.calls.Load() != 0 {
		t.Fatalf("runner calls = %d, want 0", runner.calls.Load())
	}
	if got := inbox.failureCodes(); len(got) != 1 || got[0] != sessioninbox.ErrorLiveFailed {
		t.Fatalf("failure codes = %v, want [live_failed]", got)
	}
}

func TestRunQueuedTurnAmbiguousAppendCommitReportsOutcomeUnknown(t *testing.T) {
	commitErr := fmt.Errorf("%w: connection reset", memory.ErrInboxAppendOutcomeUnknown)
	mem := &failingInboxMemory{Fake: memorytest.New(), err: commitErr}
	inbox := &recordingSessionInbox{failApplied: false}
	runner := &gateRunner{started: make(chan struct{}, 1), release: make(chan struct{})}
	svc := newSessionInboxTestService(t, mem, runner, turnqueue.NewWithLimits(1, time.Second, time.Second), inbox)

	err := runInboxTestTurn(t.Context(), svc, inboxTestTarget())
	if !errors.Is(err, memory.ErrInboxAppendOutcomeUnknown) || !errors.Is(err, sessioninbox.ErrOutcomeUnknown) {
		t.Fatalf("runQueuedTurn error = %v, want append + delivery outcome unknown", err)
	}
	if runner.calls.Load() != 0 {
		t.Fatalf("runner calls = %d, want 0", runner.calls.Load())
	}
}

func TestRunQueuedTurnCancellationAfterDeliveryKeepsTranscriptInput(t *testing.T) {
	mem := memorytest.New()
	inbox := &recordingSessionInbox{failApplied: false}
	runner := &gateRunner{started: make(chan struct{}, 1), release: make(chan struct{})}
	svc := newSessionInboxTestService(t, mem, runner, turnqueue.NewWithLimits(1, time.Second, time.Second), inbox)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runInboxTestTurn(ctx, svc, inboxTestTarget()) }()
	select {
	case <-runner.started:
	case <-time.After(time.Second):
		t.Fatal("turn did not reach the model")
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("runQueuedTurn error = %v, want context.Canceled", err)
	}
	history, err := mem.LoadHistory(t.Context(), inboxTestTarget().ID)
	if err != nil {
		t.Fatalf("LoadHistory: %v", err)
	}
	if len(history) != 1 || memory.MessageText(history[0]) != "hello" {
		t.Fatalf("history = %#v, want one delivered input", history)
	}
	if got := inbox.failureCodes(); len(got) != 1 || got[0] != sessioninbox.ErrorCanceled {
		t.Fatalf("failure CAS attempts = %v, want [canceled]", got)
	}
}

func TestRunQueuedTurnFinalizationFailureIsOutcomeUnknown(t *testing.T) {
	finalizeErr := errors.New("database unavailable")
	inbox := &recordingSessionInbox{failErr: finalizeErr}
	runner := &gateRunner{started: make(chan struct{}, 1), release: make(chan struct{})}
	svc := newSessionInboxTestService(t, memorytest.New(), runner, turnqueue.NewWithLimits(0, time.Second, time.Second), inbox)

	err := runInboxTestTurn(t.Context(), svc, inboxTestTarget())
	if !errors.Is(err, turnqueue.ErrFull) || !errors.Is(err, sessioninbox.ErrOutcomeUnknown) {
		t.Fatalf("runQueuedTurn error = %v, want ErrFull + ErrOutcomeUnknown", err)
	}
}
