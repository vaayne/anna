package agent_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/CherryHQ/stella/internal/agent"
	agentruntime "github.com/CherryHQ/stella/internal/agent/runtime"
	"github.com/CherryHQ/stella/internal/agent/session"
	sessioninbox "github.com/CherryHQ/stella/internal/agent/session/inbox"
	"github.com/CherryHQ/stella/internal/authz"
	"github.com/CherryHQ/stella/internal/eventlog"
	"github.com/CherryHQ/stella/internal/memory"
	"github.com/CherryHQ/stella/internal/memory/memorytest"
	"github.com/CherryHQ/stella/pkg/ai"
)

// --- fake runner for service tests ------------------------------------------

type fakeRunnerSvc struct {
	events []agentruntime.Event
}

type fakeSessionInbox struct {
	mu  sync.Mutex
	seq int64
}

func (f *fakeSessionInbox) Enqueue(_ context.Context, input sessioninbox.Input) (sessioninbox.Message, error) {
	if input.SourceSessionID == "" || input.Actor.SourceSessionID != input.SourceSessionID {
		return sessioninbox.Message{}, errors.New("test inbox requires source Session provenance")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq++
	return sessioninbox.Message{ID: fmt.Sprintf("00000000-0000-4000-8000-%012d", f.seq), EnqueueSeq: f.seq}, nil
}

func (*fakeSessionInbox) FailPending(context.Context, string, sessioninbox.ErrorCode) (bool, error) {
	return true, nil
}

type groupActorMemory struct {
	*memorytest.Fake
	mu     sync.Mutex
	actors []eventlog.MessageActor
}

func (m *groupActorMemory) Append(ctx context.Context, sess memory.Session, msgs ...ai.Message) error {
	actor, _ := eventlog.MessageActorFromContext(ctx)
	m.mu.Lock()
	m.actors = append(m.actors, actor)
	m.mu.Unlock()
	return m.Fake.Append(ctx, sess, msgs...)
}

type inputRecordingRunner struct {
	fakeRunnerSvc
	mu    sync.Mutex
	input agentruntime.MessageContent
}

func (r *inputRecordingRunner) Chat(_ context.Context, _ []ai.Message, input agentruntime.MessageContent) <-chan agentruntime.Event {
	r.mu.Lock()
	r.input = input
	r.mu.Unlock()
	return r.fakeRunnerSvc.Chat(context.Background(), nil, nil)
}

type fakeSessionAccessSvc struct{ reg *session.Registry }

type fakeSessionAccess struct{ reg *session.Registry }

func (s fakeSessionAccessSvc) Begin(context.Context, authz.Authority) (agent.SessionAccess, error) {
	return fakeSessionAccess(s), nil
}

func (a fakeSessionAccess) Create(ctx context.Context, userID, agentID, projectID string, kind session.Kind, channel session.Channel) (session.Info, error) {
	return a.reg.Ensure(ctx, session.Request{UserID: userID, AgentID: agentID, ProjectID: projectID, Kind: kind, Channel: channel, CreateIfMissing: true})
}

func (a fakeSessionAccess) ResolveMain(ctx context.Context, userID, agentID string) (session.Info, error) {
	return a.reg.ResolveMain(ctx, session.MainRequest{UserID: userID, AgentID: agentID})
}

func (a fakeSessionAccess) RotateMain(ctx context.Context, userID, agentID, expectedSessionID string) (session.Info, error) {
	return a.reg.RotateMain(ctx, session.MainRequest{UserID: userID, AgentID: agentID, ExpectedSessionID: expectedSessionID})
}

func (a fakeSessionAccess) ResolveChatChannel(ctx context.Context, req session.ChannelRequest) (session.Info, error) {
	return a.reg.ResolveChatChannel(ctx, req)
}

func (a fakeSessionAccess) RotateChannel(ctx context.Context, req session.ChannelRequest) (session.Info, error) {
	return a.reg.RotateChannel(ctx, req)
}

func (a fakeSessionAccess) Use(ctx context.Context, agentID, sessionID string) (session.Info, error) {
	return a.reg.Get(ctx, session.Scope{AgentID: agentID, System: true}, sessionID)
}

func (a fakeSessionAccess) EnsureRead(ctx context.Context, req session.Request) (session.Info, error) {
	return a.reg.Ensure(ctx, req)
}

func (a fakeSessionAccess) EnsureUse(ctx context.Context, req session.Request) (session.Info, error) {
	return a.reg.Ensure(ctx, req)
}

func (a fakeSessionAccess) Delete(ctx context.Context, agentID, sessionID string) (session.Info, error) {
	return a.reg.Get(ctx, session.Scope{AgentID: agentID}, sessionID)
}

func (a fakeSessionAccess) Archive(ctx context.Context, info session.Info) error {
	return a.reg.Archive(ctx, session.Scope{UserID: info.UserID, AgentID: info.AgentID}, info.ID)
}

func (r *fakeRunnerSvc) Chat(_ context.Context, _ []ai.Message, _ agentruntime.MessageContent) <-chan agentruntime.Event {
	ch := make(chan agentruntime.Event, len(r.events)+1)
	for _, e := range r.events {
		ch <- e
	}
	close(ch)
	return ch
}
func (r *fakeRunnerSvc) Alive() bool             { return true }
func (r *fakeRunnerSvc) Busy() bool              { return false }
func (r *fakeRunnerSvc) LastActivity() time.Time { return time.Now() }
func (r *fakeRunnerSvc) SystemPrompt() string    { return "" }
func (r *fakeRunnerSvc) PluginContext() agentruntime.PluginContext {
	return agentruntime.PluginContext{}
}
func (r *fakeRunnerSvc) Close() error { return nil }

// newTestService builds a Service backed by memorytest.Fake and a fake runner.
func newTestService(t *testing.T, events []agentruntime.Event) (*agent.Service, *memorytest.Fake) {
	t.Helper()

	mem := memorytest.New()
	factory := func(_ context.Context, _ agentruntime.RunnerParams) (agentruntime.Runner, error) {
		return &fakeRunnerSvc{events: events}, nil
	}
	rt, err := agentruntime.New(agentruntime.Config{
		NewRunner: factory,
		Memory:    mem,
	})
	if err != nil {
		t.Fatalf("agentruntime.New: %v", err)
	}

	reg, err := session.NewRegistry(mem, "agent1")
	if err != nil {
		t.Fatalf("session.NewRegistry: %v", err)
	}

	svc := &agent.Service{
		Sessions:      reg,
		Runtime:       rt,
		SessionAccess: fakeSessionAccessSvc{reg: reg},
		SessionInbox:  &fakeSessionInbox{},
		AgentID:       "agent1",
	}
	return svc, mem
}

// TestService_Chat_SessionEnsuredBeforeRuntime verifies that Chat resolves a
// session through the registry before dispatching to runtime.
func TestService_Chat_SessionEnsuredBeforeRuntime(t *testing.T) {
	svc, mem := newTestService(t, nil)

	stream := svc.Chat(context.Background(), agent.ChatRequest{
		UserID:  "u1",
		AgentID: "agent1",
		Channel: session.ChannelWeb,
		Message: "hello",
	})
	for range stream {
	}

	// A session must exist in the store.
	ctx := authz.WithUserID(context.Background(), "u1")
	ctx = authz.WithAgentID(ctx, "agent1")
	infos, err := mem.ListInfo(ctx, memory.ListOptions{UserID: "u1", AgentID: "agent1"})
	if err != nil {
		t.Fatalf("ListInfo: %v", err)
	}
	if len(infos) == 0 {
		t.Error("expected at least one session in store")
	}
}

// TestService_Chat_PropagatesEvents verifies events reach the caller.
func TestService_Chat_PropagatesEvents(t *testing.T) {
	events := []agentruntime.Event{{Text: "hello"}, {Text: " world"}}
	svc, _ := newTestService(t, events)

	stream := svc.Chat(context.Background(), agent.ChatRequest{
		UserID:  "u1",
		AgentID: "agent1",
		Message: "hi",
	})

	var got string
	for ev := range stream {
		got += ev.Text
	}
	if got != "hello world" {
		t.Errorf("got %q, want %q", got, "hello world")
	}
}

func TestServiceGroupTurnPersistsHumanSpeakerAndRendersUnwrapped(t *testing.T) {
	const groupID = "11111111-1111-4111-8111-111111111111"
	mem := &groupActorMemory{Fake: memorytest.New()}
	runner := &inputRecordingRunner{}
	rt, err := agentruntime.New(agentruntime.Config{
		NewRunner: func(context.Context, agentruntime.RunnerParams) (agentruntime.Runner, error) { return runner, nil },
		Memory:    mem,
	})
	if err != nil {
		t.Fatal(err)
	}
	reg, err := session.NewRegistry(mem, "agent1")
	if err != nil {
		t.Fatal(err)
	}
	svc := &agent.Service{Sessions: reg, Runtime: rt, SessionAccess: fakeSessionAccessSvc{reg: reg}, AgentID: "agent1"}
	authority, err := authz.NewGroupAgentAuthority(groupID, "agent1")
	if err != nil {
		t.Fatal(err)
	}
	stream := svc.Chat(context.Background(), agent.ChatRequest{
		UserID:         groupID,
		AgentID:        "agent1",
		GroupID:        groupID,
		Kind:           session.KindChat,
		Channel:        session.Channel("group:test"),
		Message:        "human group input",
		CurrentSpeaker: memory.CurrentSpeaker{UserID: "speaker-1", PlatformUserID: "platform-speaker", DisplayName: "Alice"},
		Authority:      authority,
	})
	for event := range stream {
		if event.Err != nil {
			t.Fatalf("group chat: %v", event.Err)
		}
	}

	mem.mu.Lock()
	actors := append([]eventlog.MessageActor(nil), mem.actors...)
	mem.mu.Unlock()
	if len(actors) == 0 || actors[0] != (eventlog.MessageActor{Type: eventlog.ActorHuman, ID: "speaker-1"}) {
		t.Fatalf("persisted group actor=%#v, want human speaker", actors)
	}
	runner.mu.Lock()
	modelInput := fmt.Sprint(runner.input)
	runner.mu.Unlock()
	if strings.Contains(modelInput, "stella_actor") {
		t.Fatalf("human group input was wrapped as injected agent content: %s", modelInput)
	}
}

// TestService_ChatForSchedulerFinalFenceRejectsAfterCapabilityMutation models
// the dispatch race: the scheduler's first capability check has passed, then
// the plugin is disabled before the agent turn starts. The final fence runs
// after Runtime registers the active turn, so it rejects without runner work.
func TestService_ChatForSchedulerFinalFenceRejectsAfterCapabilityMutation(t *testing.T) {
	svc, _ := newTestService(t, []agentruntime.Event{{Text: "must not run"}})
	authority, err := authz.NewAgentAuthority("u1", "agent1")
	if err != nil {
		t.Fatalf("NewAgentAuthority: %v", err)
	}

	firstGatePassed := true
	pluginDisabled := false
	finalFenceCalled := false
	denied := errors.New("system/scheduler disabled")
	stream := svc.ChatForScheduler(context.Background(), agent.SchedulerChatRequest{
		SessionID: "scheduler-race",
		UserID:    "u1",
		AgentID:   "agent1",
		Message:   "must not run",
		Authority: authority,
		BeforeStart: func() error {
			if !firstGatePassed {
				t.Fatal("final fence ran before the initial capability check")
			}
			pluginDisabled = true // stand-in for the committed disable mutation
			finalFenceCalled = true
			if _, err := svc.Runtime.ChatAdmitted(context.Background(), session.Info{
				ID:      "scheduler-race",
				UserID:  "u1",
				AgentID: "agent1",
				Kind:    string(session.KindScheduler),
				Channel: string(session.ChannelScheduler),
			}, "nested"); !errors.Is(err, agentruntime.ErrSessionBusy) {
				t.Fatalf("nested admission error = %v, want session busy", err)
			}
			return denied
		},
	})

	var gotErr error
	var gotText string
	for event := range stream {
		if event.Err != nil {
			gotErr = event.Err
		}
		gotText += event.Text
	}
	if !errors.Is(gotErr, denied) {
		t.Fatalf("scheduler admission error = %v, want %v", gotErr, denied)
	}
	if gotText != "" {
		t.Fatalf("rejected scheduler admission produced runner output %q", gotText)
	}
	if !finalFenceCalled || !pluginDisabled {
		t.Fatalf("final fence state called=%v disabled=%v, want both true", finalFenceCalled, pluginDisabled)
	}
	if svc.SessionLive("scheduler-race") {
		t.Fatal("rejected scheduler admission retained the active turn")
	}
}

// TestService_Chat_MissingUser returns an error event when no UserID is supplied.
func TestService_Chat_MissingUser(t *testing.T) {
	svc, _ := newTestService(t, nil)

	stream := svc.Chat(context.Background(), agent.ChatRequest{
		AgentID: "agent1",
		Message: "hi",
	})
	var gotErr error
	for ev := range stream {
		if ev.Err != nil {
			gotErr = ev.Err
		}
	}
	if gotErr == nil {
		t.Error("expected error event for missing UserID")
	}
}
