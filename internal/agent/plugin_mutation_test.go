package agent

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	agentruntime "github.com/CherryHQ/stella/internal/agent/runtime"
	"github.com/CherryHQ/stella/internal/agent/session"
	"github.com/CherryHQ/stella/internal/memory/memorytest"
	"github.com/CherryHQ/stella/internal/plugin"
	"github.com/CherryHQ/stella/pkg/ai"
)

type pluginMutationRunner struct {
	ownerFenceRunner
	closed   atomic.Int32
	closeErr error
}

func (r *pluginMutationRunner) Close() error { r.closed.Add(1); return r.closeErr }

func TestPluginMutationRetiresAllUsersOnlyAfterPossibleCommit(t *testing.T) {
	rejected := errors.New("transaction rejected")
	for _, tt := range []struct {
		name       string
		result     error
		wantClosed int32
	}{
		{name: "committed", wantClosed: 1},
		{name: "rolled back", result: rejected},
		{name: "unknown commit", result: plugin.ErrCommitOutcomeUnknown, wantClosed: 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			pm := NewPoolManager(nil, memorytest.New())
			var runners []*pluginMutationRunner
			for _, id := range []string{"first", "second"} {
				runner := &pluginMutationRunner{}
				rt, err := agentruntime.New(agentruntime.Config{
					Memory:    memorytest.New(),
					NewRunner: func(context.Context, agentruntime.RunnerParams) (agentruntime.Runner, error) { return runner, nil },
				})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = rt.Close() })
				svc := &Service{AgentID: id, Runtime: rt, lifecycle: pm.lifecycle}
				pm.services[id] = svc
				stream, err := svc.admit(t.Context(), session.Info{ID: id, UserID: id, AgentID: id, Kind: string(session.KindChat), Channel: string(session.ChannelWeb)}, "hello")
				if err != nil {
					t.Fatal(err)
				}
				for range stream {
				}
				runners = append(runners, runner)
			}
			calls := 0
			err := pm.ApplyPluginMutation(t.Context(), func() error {
				calls++
				for _, runner := range runners {
					if runner.closed.Load() != 0 {
						t.Fatal("runner retired before transaction completed")
					}
				}
				ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
				defer cancel()
				if err := pm.lifecycle.lockShared(ctx); !errors.Is(err, context.DeadlineExceeded) {
					if err == nil {
						pm.lifecycle.unlockShared()
					}
					t.Fatalf("admission entered mutation fence: %v", err)
				}
				return tt.result
			})
			if !errors.Is(err, tt.result) || calls != 1 {
				t.Fatalf("mutation = %v, callbacks=%d", err, calls)
			}
			for _, runner := range runners {
				if got := runner.closed.Load(); got != tt.wantClosed {
					t.Fatalf("retired=%d, want %d", got, tt.wantClosed)
				}
			}
			if err := pm.lifecycle.lockShared(t.Context()); err != nil {
				t.Fatal(err)
			}
			pm.lifecycle.unlockShared()
		})
	}
}

func TestPluginMutationCanceledFenceDoesNotWrite(t *testing.T) {
	pm := NewPoolManager(nil, memorytest.New())
	if err := pm.lifecycle.lockShared(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer pm.lifecycle.unlockShared()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	called := false
	err := pm.ApplyPluginMutation(ctx, func() error { called = true; return nil })
	if !errors.Is(err, context.Canceled) || called {
		t.Fatalf("canceled mutation = %v, called=%v", err, called)
	}
}

func TestPluginMutationCloseFailureKeepsCommittedResult(t *testing.T) {
	pm := NewPoolManager(nil, memorytest.New())
	var builds atomic.Int32
	first := &pluginMutationRunner{closeErr: errors.New("close failed")}
	rt, err := agentruntime.New(agentruntime.Config{
		Memory: memorytest.New(),
		NewRunner: func(context.Context, agentruntime.RunnerParams) (agentruntime.Runner, error) {
			if builds.Add(1) == 1 {
				return first, nil
			}
			return &ownerFenceRunner{}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rt.Close() })
	svc := &Service{AgentID: "agent", Runtime: rt, lifecycle: pm.lifecycle}
	pm.services[svc.AgentID] = svc
	info := session.Info{ID: "session", UserID: "user", AgentID: "agent", Kind: string(session.KindChat), Channel: string(session.ChannelWeb)}
	run := func() {
		stream, err := svc.admit(t.Context(), info, "hello")
		if err != nil {
			t.Fatal(err)
		}
		for range stream {
		}
	}
	run()
	if err := pm.ApplyPluginMutation(t.Context(), func() error { return nil }); err != nil {
		t.Fatalf("committed mutation reported a close failure: %v", err)
	}
	run()
	if builds.Load() != 2 || first.closed.Load() != 1 {
		t.Fatalf("builds=%d, retired=%d", builds.Load(), first.closed.Load())
	}
}

type heldPluginMutationRunner struct {
	pluginMutationRunner
	started chan struct{}
	release chan struct{}
	active  atomic.Bool
}

func (r *heldPluginMutationRunner) Busy() bool { return r.active.Load() }

func (r *heldPluginMutationRunner) Chat(ctx context.Context, _ []ai.Message, _ agentruntime.MessageContent) <-chan agentruntime.Event {
	out := make(chan agentruntime.Event)
	r.active.Store(true)
	close(r.started)
	go func() {
		defer close(out)
		select {
		case <-r.release:
		case <-ctx.Done():
		}
		r.active.Store(false)
	}()
	return out
}

func TestPluginMutationKeepsAdmittedTurnAndRebuildsNext(t *testing.T) {
	pm := NewPoolManager(nil, memorytest.New())
	first := &heldPluginMutationRunner{started: make(chan struct{}), release: make(chan struct{})}
	var builds atomic.Int32
	rt, err := agentruntime.New(agentruntime.Config{
		Memory: memorytest.New(),
		NewRunner: func(context.Context, agentruntime.RunnerParams) (agentruntime.Runner, error) {
			if builds.Add(1) == 1 {
				return first, nil
			}
			return &ownerFenceRunner{}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rt.Close() })
	svc := &Service{AgentID: "agent", Runtime: rt, lifecycle: pm.lifecycle}
	pm.services[svc.AgentID] = svc
	info := session.Info{ID: "session", UserID: "user", AgentID: "agent", Kind: string(session.KindChat), Channel: string(session.ChannelWeb)}
	stream, err := svc.admit(t.Context(), info, "hello")
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-first.started:
	case <-t.Context().Done():
		t.Fatal("turn did not start")
	}
	if err := pm.ApplyPluginMutation(t.Context(), func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if first.closed.Load() != 0 || builds.Load() != 1 {
		t.Fatal("mutation interrupted admitted runner")
	}
	close(first.release)
	for range stream {
	}
	next, err := svc.admit(t.Context(), info, "next")
	if err != nil {
		t.Fatal(err)
	}
	for range next {
	}
	if builds.Load() != 2 || first.closed.Load() != 1 {
		t.Fatalf("builds=%d, retired=%d", builds.Load(), first.closed.Load())
	}
}
