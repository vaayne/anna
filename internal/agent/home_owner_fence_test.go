package agent

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	agentruntime "github.com/CherryHQ/stella/internal/agent/runtime"
	"github.com/CherryHQ/stella/internal/agent/session"
	"github.com/CherryHQ/stella/internal/db/dbtest"
	"github.com/CherryHQ/stella/internal/memory/memorytest"
	"github.com/CherryHQ/stella/internal/platform/home"
	"github.com/CherryHQ/stella/pkg/ai"
	"github.com/CherryHQ/stella/pkg/db/sqlc"
)

type ownerFenceRunner struct{}

func (*ownerFenceRunner) Chat(context.Context, []ai.Message, agentruntime.MessageContent) <-chan agentruntime.Event {
	out := make(chan agentruntime.Event)
	close(out)
	return out
}
func (*ownerFenceRunner) Alive() bool                  { return true }
func (*ownerFenceRunner) Busy() bool                   { return false }
func (*ownerFenceRunner) LastActivity() time.Time      { return time.Now() }
func (*ownerFenceRunner) SystemPrompt() string         { return "" }
func (*ownerFenceRunner) PluginContext() PluginContext { return PluginContext{} }
func (*ownerFenceRunner) Close() error                 { return nil }

type signalingFenceAcquirer struct {
	delegate home.OwnerFenceAcquirer
	entered  chan struct{}
	once     sync.Once
}

func (f *signalingFenceAcquirer) AcquireHomeOwnerFence(ctx context.Context, kind home.OwnerKind, id string) (home.OwnerFenceLease, error) {
	f.once.Do(func() { close(f.entered) })
	return f.delegate.AcquireHomeOwnerFence(ctx, kind, id)
}

func TestHomeOwnerDeletionWaitsForWorkspaceAdmissionWithoutDeadlock(t *testing.T) {
	for _, kind := range []home.OwnerKind{home.OwnerUser, home.OwnerGroup, home.OwnerAgent} {
		t.Run(string(kind), func(t *testing.T) {
			ctx := t.Context()
			db := dbtest.New(t)
			q := sqlc.New(db)
			userID, agentID := uuid.NewString(), uuid.NewString()
			if _, err := db.Exec(ctx, "INSERT INTO auth_user (id,email) VALUES ($1,$2)", userID, userID+"@test.invalid"); err != nil {
				t.Fatal(err)
			}
			if err := q.SeedAgent(ctx, sqlc.SeedAgentParams{ID: agentID, Name: "agent", Model: "test", Sandbox: []byte(`{}`), Scope: "system", Enabled: true}); err != nil {
				t.Fatal(err)
			}
			if kind == home.OwnerGroup {
				if _, err := q.CreateGroupState(ctx, sqlc.CreateGroupStateParams{ID: userID, Platform: "test", PlatformGroupID: userID, GroupName: "group"}); err != nil {
					t.Fatal(err)
				}
			}
			manager, err := home.NewWorkspaceManager(db, t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = manager.Close() })
			factoryEntered, releaseFactory := make(chan struct{}), make(chan struct{})
			var once sync.Once
			rt, err := agentruntime.New(agentruntime.Config{Memory: memorytest.New(), NewRunner: func(ctx context.Context, _ agentruntime.RunnerParams) (agentruntime.Runner, error) {
				once.Do(func() { close(factoryEntered); <-releaseFactory })
				req := home.WorkspaceRequest{UserID: userID, AgentID: agentID}
				if kind == home.OwnerGroup {
					req.GroupID = userID
				}
				if _, err := manager.WorkspaceView(ctx, req); err != nil {
					return nil, err
				}
				return &ownerFenceRunner{}, nil
			}})
			if err != nil {
				t.Fatal(err)
			}
			pm := NewPoolManager(nil, memorytest.New())
			svc := &Service{Runtime: rt, AgentID: agentID, lifecycle: pm.lifecycle}
			pm.services[agentID] = svc
			fencer := &signalingFenceAcquirer{delegate: pm, entered: make(chan struct{})}
			deletion, err := home.NewOwnerDeletion(db, manager, fencer)
			if err != nil {
				t.Fatal(err)
			}
			info := session.Info{ID: uuid.NewString(), UserID: userID, AgentID: agentID, Kind: string(session.KindChat), Channel: string(session.ChannelWeb)}
			if kind == home.OwnerGroup {
				info.GroupID = userID
			}
			admitDone := make(chan error, 1)
			go func() { _, err := svc.admit(ctx, info, "turn"); admitDone <- err }()
			<-factoryEntered
			deleteDone := make(chan error, 1)
			go func() {
				switch kind {
				case home.OwnerUser:
					deleteDone <- deletion.DeleteUser(ctx, userID, "operator")
				case home.OwnerGroup:
					deleteDone <- deletion.DeleteGroup(ctx, userID, "operator")
				case home.OwnerAgent:
					deleteDone <- deletion.DeleteAgent(ctx, agentID, "operator")
				}
			}()
			<-fencer.entered
			close(releaseFactory)
			for label, done := range map[string]<-chan error{"admission": admitDone, "deletion": deleteDone} {
				select {
				case err := <-done:
					if err != nil {
						t.Fatalf("%s: %v", label, err)
					}
				case <-time.After(2 * time.Second):
					t.Fatalf("%s deadlocked", label)
				}
			}
			if kind == home.OwnerAgent && pm.GetService(agentID) != nil {
				t.Fatal("Agent service remained published after commit")
			}
			if _, err := manager.WorkspaceView(ctx, home.WorkspaceRequest{UserID: userID, GroupID: info.GroupID, AgentID: agentID}); err == nil {
				t.Fatal("WorkspaceView admitted after owner deletion")
			}
		})
	}
}

func TestAgentOwnerDeletionRollbackKeepsExactServiceFreshAdmissible(t *testing.T) {
	ctx, db := t.Context(), dbtest.New(t)
	q := sqlc.New(db)
	userID, agentID := uuid.NewString(), uuid.NewString()
	_, _ = db.Exec(ctx, "INSERT INTO auth_user (id,email) VALUES ($1,$2)", userID, userID+"@test.invalid")
	if err := q.SeedAgent(ctx, sqlc.SeedAgentParams{ID: agentID, Name: "agent", Model: "test", Sandbox: []byte(`{}`), Scope: "system", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, "INSERT INTO webhook (id,user_id,agent_id,name,provider,wait_timeout_seconds,max_run_timeout_seconds,token_public_id,token_hash,token_last4) VALUES ($1,$2,$3,'x','generic',60,300,'public','hash','1234')", uuid.NewString(), userID, agentID); err != nil {
		t.Fatal(err)
	}
	manager, err := home.NewWorkspaceManager(db, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	rt, _ := agentruntime.New(agentruntime.Config{Memory: memorytest.New(), NewRunner: func(ctx context.Context, _ agentruntime.RunnerParams) (agentruntime.Runner, error) {
		if _, err := manager.WorkspaceView(ctx, home.WorkspaceRequest{UserID: userID, AgentID: agentID}); err != nil {
			return nil, err
		}
		return &ownerFenceRunner{}, nil
	}})
	pm := NewPoolManager(nil, memorytest.New())
	svc := &Service{Runtime: rt, AgentID: agentID, lifecycle: pm.lifecycle}
	pm.services[agentID] = svc
	deletion, _ := home.NewOwnerDeletion(db, manager, pm)
	if err := deletion.DeleteAgent(ctx, agentID, "operator"); err == nil {
		t.Fatal("FK-blocked deletion succeeded")
	}
	if pm.GetService(agentID) != svc {
		t.Fatal("rollback changed published Service")
	}
	if _, err := q.GetAgent(ctx, agentID); err != nil {
		t.Fatalf("owner rolled back: %v", err)
	}
	_, err = svc.admit(ctx, session.Info{ID: uuid.NewString(), UserID: userID, AgentID: agentID, Kind: string(session.KindChat), Channel: string(session.ChannelWeb)}, "fresh")
	if err != nil {
		t.Fatalf("fresh admission after rollback: %v", err)
	}
}

func TestAcquireHomeOwnerFenceCancellationDoesNotLeakLifecycleGate(t *testing.T) {
	pm := NewPoolManager(nil, memorytest.New())
	if err := pm.lifecycle.lockShared(t.Context()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if _, err := pm.AcquireHomeOwnerFence(ctx, home.OwnerUser, "user"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v", err)
	}
	pm.lifecycle.unlockShared()
	lease, err := pm.AcquireHomeOwnerFence(t.Context(), home.OwnerUser, "user")
	if err != nil {
		t.Fatal(err)
	}
	lease.Release()
}
