package runtime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/CherryHQ/stella/internal/agent/session"
	"github.com/CherryHQ/stella/internal/authz"
	"github.com/CherryHQ/stella/internal/core/agentctx"
	"github.com/CherryHQ/stella/internal/memory"
	"github.com/CherryHQ/stella/internal/plugin"
	"github.com/CherryHQ/stella/internal/sessionmedia"
	"github.com/CherryHQ/stella/pkg/ai"
	"github.com/CherryHQ/stella/pkg/hooks"
	pkgplugins "github.com/CherryHQ/stella/pkg/plugins"
)

type sessionImagesFunc func(context.Context, sessionmedia.Owner, string, []ai.ContentBlock) ([]ai.ContentBlock, error)

func (f sessionImagesFunc) Enrich(ctx context.Context, owner sessionmedia.Owner, agentID string, blocks []ai.ContentBlock) ([]ai.ContentBlock, error) {
	return f(ctx, owner, agentID, blocks)
}

// A session's user ID is an auth_user UUID in production; media ownership is
// derived from it, so the fixtures have to be real UUIDs.
var imageTestUserID = uuid.NewString()

type recordingMemory struct {
	mu             sync.Mutex
	messages       []ai.Message
	appendSessions []memory.Session
	commits        []int64
	appendError    error
	assembleError  error
}

type snapshotRecordingMemory struct {
	*recordingMemory
	snapshot memory.SessionSnapshot
}

type blockingSnapshotMemory struct {
	*snapshotRecordingMemory
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (m *blockingSnapshotMemory) Assemble(context.Context, memory.Session, int, int) ([]ai.Message, error) {
	m.once.Do(func() {
		close(m.started)
		<-m.release
	})
	return nil, nil
}

func (m *snapshotRecordingMemory) GetOrCreateSessionSnapshot(context.Context, string, string, string) (memory.SessionSnapshot, error) {
	return m.snapshot, nil
}

func (*snapshotRecordingMemory) AdvanceSessionSnapshot(context.Context, string, string, string) error {
	return nil
}

func (m *recordingMemory) Name() string { return "recording" }

func (m *recordingMemory) Bootstrap(context.Context, memory.Session) error { return nil }

func (m *recordingMemory) Append(_ context.Context, session memory.Session, msgs ...ai.Message) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.appendError != nil {
		return m.appendError
	}
	m.appendSessions = append(m.appendSessions, session)
	m.messages = append(m.messages, msgs...)
	return nil
}

func (m *recordingMemory) Assemble(context.Context, memory.Session, int, int) ([]ai.Message, error) {
	return nil, m.assembleError
}

func (m *recordingMemory) Stats(context.Context, memory.Session) (memory.SessionStats, error) {
	return memory.SessionStats{}, nil
}

func (m *recordingMemory) Close() error { return nil }

func (m *recordingMemory) CommitGroupCursor(_ context.Context, _ memory.Session, seq int64) error {
	m.mu.Lock()
	m.commits = append(m.commits, seq)
	m.mu.Unlock()
	return nil
}

type chatFakeRunner struct {
	events        []Event
	system        string
	messages      *[]MessageContent
	ctx           *context.Context
	pluginContext PluginContext
}

func (r chatFakeRunner) Chat(ctx context.Context, _ []ai.Message, msg MessageContent) <-chan Event {
	if r.ctx != nil {
		*r.ctx = ctx
	}
	if r.messages != nil {
		*r.messages = append(*r.messages, msg)
	}
	ch := make(chan Event, len(r.events))
	for _, evt := range r.events {
		ch <- evt
	}
	close(ch)
	return ch
}

func (r chatFakeRunner) Alive() bool                  { return true }
func (r chatFakeRunner) Busy() bool                   { return false }
func (r chatFakeRunner) LastActivity() time.Time      { return time.Now() }
func (r chatFakeRunner) SystemPrompt() string         { return r.system }
func (r chatFakeRunner) PluginContext() PluginContext { return r.pluginContext }
func (r chatFakeRunner) Close() error                 { return nil }

func TestTelemetryChannelDoesNotChangeDurableConversationChannel(t *testing.T) {
	mem := &recordingMemory{}
	rt, err := New(Config{
		Memory: mem,
		NewRunner: func(context.Context, RunnerParams) (Runner, error) {
			return chatFakeRunner{events: []Event{{Text: "ok"}}}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	info := session.Info{ID: "durable-channel", UserID: "user", AgentID: "agent", Kind: string(session.KindChat), Channel: "agent:user:telegram:private"}
	for event := range rt.Chat(context.Background(), info, "hello", WithTelemetryChannel("telegram", "agent:user:telegram:private")) {
		if event.Err != nil {
			t.Fatal(event.Err)
		}
	}
	if len(mem.appendSessions) == 0 {
		t.Fatal("chat did not append a durable conversation message")
	}
	if got := mem.appendSessions[0].Channel; got != info.Channel {
		t.Fatalf("durable channel = %q, want %q", got, info.Channel)
	}
}

// TestRuntimeChatUnionsAncestorExcludedTools prevents a goal worker from
// restoring a control-plane tool by delegating. The per-call exclusions model a
// delegate preset that whitelists "goal" (and therefore excludes every other
// registry tool); the ancestor exclusion remains authoritative.
func TestRuntimeChatUnionsAncestorExcludedTools(t *testing.T) {
	mem := &recordingMemory{}
	var runnerCtx context.Context
	rt, err := New(Config{
		Memory: mem,
		NewRunner: func(context.Context, RunnerParams) (Runner, error) {
			return chatFakeRunner{ctx: &runnerCtx, events: []Event{{Text: "ok"}}}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	parent := agentctx.WithExcludedTools(context.Background(), "goal", "scheduler", "workflow")
	for event := range rt.Chat(parent, session.Info{ID: "delegate-1", UserID: "user-1", AgentID: "agent-1"}, "work",
		WithExcludedTools("scheduler", "workflow", "delegate", "read_file"),
	) {
		if event.Err != nil {
			t.Fatalf("chat: %v", event.Err)
		}
	}

	want := []string{"goal", "scheduler", "workflow", "delegate", "read_file"}
	if got := agentctx.ExcludedToolsFromContext(runnerCtx); !slices.Equal(got, want) {
		t.Fatalf("child excluded tools = %v, want %v", got, want)
	}
}

func TestGuestChatCarriesGuestIdentityWithoutUserIdentity(t *testing.T) {
	const guestID = "11111111-1111-4111-8111-111111111111"
	mem := &recordingMemory{}
	var runnerCtx context.Context
	var params RunnerParams
	rt, err := New(Config{
		Memory: mem,
		NewRunner: func(_ context.Context, p RunnerParams) (Runner, error) {
			params = p
			return chatFakeRunner{ctx: &runnerCtx, events: []Event{{Text: "ok"}}}, nil
		},
		BeforeRun: func(context.Context, session.Info, string, string, string, []ai.Message, PluginContext) (string, error) {
			t.Fatal("guest must not run before-run hooks")
			return "", nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	info := session.Info{ID: "guest-session", UserID: guestID, GuestID: guestID, AgentID: "agent-1", Kind: "chat"}
	for evt := range rt.Chat(context.Background(), info, "hello") {
		if evt.Err != nil {
			t.Fatalf("chat: %v", evt.Err)
		}
	}
	if params.GuestID != guestID {
		t.Fatalf("runner GuestID = %q, want %q", params.GuestID, guestID)
	}
	if got := authz.UserIDFromContext(runnerCtx); got != "" {
		t.Fatalf("runtime UserID = %q, want empty", got)
	}
	if got := authz.GuestIDFromContext(runnerCtx); got != guestID {
		t.Fatalf("runtime GuestID = %q, want %q", got, guestID)
	}
	if len(mem.messages) < 1 {
		t.Fatal("guest conversation was not appended")
	}
}

func TestChatRebuildsSnapshotPromptAtVersionZero(t *testing.T) {
	mem := &snapshotRecordingMemory{
		recordingMemory: &recordingMemory{},
		snapshot:        memory.SessionSnapshot{Version: 0},
	}
	var promptCalls int
	var beforeRunSystem string
	rt, err := New(Config{
		Memory: mem,
		NewRunner: func(context.Context, RunnerParams) (Runner, error) {
			return chatFakeRunner{system: "live base prompt", events: []Event{{Text: "ok"}}}, nil
		},
		SnapshotPrompt: func(_ context.Context, _ session.Info, snap memory.SessionSnapshot, _ PluginContext) (string, error) {
			promptCalls++
			if snap.Version != 0 {
				t.Fatalf("snapshot version = %d, want 0", snap.Version)
			}
			return "frozen snapshot prompt", nil
		},
		BeforeRun: func(_ context.Context, _ session.Info, _, _, system string, _ []ai.Message, _ PluginContext) (string, error) {
			beforeRunSystem = system
			return "", nil
		},
	})
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}

	out := rt.Chat(context.Background(), session.Info{ID: "sess-1", UserID: imageTestUserID, AgentID: "agent-1"}, "hello")
	for evt := range out {
		if evt.Err != nil {
			t.Fatalf("chat: %v", evt.Err)
		}
	}

	if promptCalls != 1 {
		t.Fatalf("snapshot prompt calls = %d, want 1", promptCalls)
	}
	if beforeRunSystem != "frozen snapshot prompt" {
		t.Fatalf("before-run system = %q, want frozen snapshot prompt", beforeRunSystem)
	}
}

func TestAdmittedChatKeepsPromptBuilderSnapshot(t *testing.T) {
	mem := &blockingSnapshotMemory{
		snapshotRecordingMemory: &snapshotRecordingMemory{
			recordingMemory: &recordingMemory{},
			snapshot:        memory.SessionSnapshot{Version: 1},
		},
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	systemsSeen := make(chan string, 2)
	pluginContext := NewPluginContext(plugin.Snapshot{}, pkgplugins.SessionPluginView{RegisteredPluginIDs: []string{"plugin/a"}, ExposedPluginIDs: []string{"plugin/a"}})
	rt, err := New(Config{
		Memory: mem,
		NewRunner: func(context.Context, RunnerParams) (Runner, error) {
			return chatFakeRunner{system: "runner prompt", events: []Event{{Text: "ok"}}, pluginContext: pluginContext}, nil
		},
		SnapshotPrompt: func(_ context.Context, _ session.Info, _ memory.SessionSnapshot, got PluginContext) (string, error) {
			view := got.SessionPluginView()
			if len(view.ExposedPluginIDs) != 1 || view.ExposedPluginIDs[0] != "plugin/a" {
				t.Fatalf("snapshot prompt plugin context = %#v", view)
			}
			return "admitted prompt", nil
		},
		BeforeRun: func(_ context.Context, _ session.Info, _, _, system string, _ []ai.Message, _ PluginContext) (string, error) {
			systemsSeen <- system
			return "", nil
		},
	})
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}
	first, err := rt.ChatAdmitted(context.Background(), session.Info{ID: "sess-1", UserID: imageTestUserID, AgentID: "agent-1"}, "hello")
	if err != nil {
		t.Fatalf("admit first chat: %v", err)
	}
	<-mem.started
	rt.SetPromptBuilders(
		func(_ context.Context, _ session.Info, _, _, system string, _ []ai.Message, _ PluginContext) (string, error) {
			systemsSeen <- system
			return "", nil
		},
		func(context.Context, session.Info, memory.SessionSnapshot, PluginContext) (string, error) {
			return "refreshed prompt", nil
		},
	)
	close(mem.release)
	for evt := range first {
		if evt.Err != nil {
			t.Fatalf("first chat: %v", evt.Err)
		}
	}
	if got := <-systemsSeen; got != "admitted prompt" {
		t.Fatalf("admitted chat prompt = %q, want admitted prompt", got)
	}

	for evt := range rt.Chat(context.Background(), session.Info{ID: "sess-2", UserID: "user-1", AgentID: "agent-1"}, "hello") {
		if evt.Err != nil {
			t.Fatalf("second chat: %v", evt.Err)
		}
	}
	if got := <-systemsSeen; got != "refreshed prompt" {
		t.Fatalf("next chat prompt = %q, want refreshed prompt", got)
	}
}

type countingChatRunner struct{ calls *int }

func (r countingChatRunner) Chat(context.Context, []ai.Message, MessageContent) <-chan Event {
	*r.calls++
	ch := make(chan Event)
	close(ch)
	return ch
}
func (countingChatRunner) Alive() bool                  { return true }
func (countingChatRunner) Busy() bool                   { return false }
func (countingChatRunner) LastActivity() time.Time      { return time.Now() }
func (countingChatRunner) SystemPrompt() string         { return "base" }
func (countingChatRunner) PluginContext() PluginContext { return PluginContext{} }
func (countingChatRunner) Close() error                 { return nil }

func TestChatSnapshotPromptErrorIsTerminalBeforeRunnerChat(t *testing.T) {
	mem := &snapshotRecordingMemory{recordingMemory: &recordingMemory{}, snapshot: memory.SessionSnapshot{Version: 1}}
	var calls int
	want := errors.New("Home unavailable")
	rt, err := New(Config{
		Memory:    mem,
		NewRunner: func(context.Context, RunnerParams) (Runner, error) { return countingChatRunner{calls: &calls}, nil },
		SnapshotPrompt: func(context.Context, session.Info, memory.SessionSnapshot, PluginContext) (string, error) {
			return "", want
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var events []Event
	for event := range rt.Chat(context.Background(), session.Info{ID: "s", UserID: "u", AgentID: "a"}, "hello") {
		events = append(events, event)
	}
	if len(events) != 1 || !errors.Is(events[0].Err, want) {
		t.Fatalf("events = %#v, want one terminal snapshot error", events)
	}
	if calls != 0 {
		t.Fatalf("runner Chat calls = %d, want 0", calls)
	}
}

func TestRuntimeChatEnrichesAndCanonicallyAppendsOrdinaryImages(t *testing.T) {
	mem := &recordingMemory{}
	var received []MessageContent
	ref := ai.ImageRefContent{MediaID: "media-1"}
	rt, err := New(Config{
		Memory: mem,
		SessionImages: sessionImagesFunc(func(_ context.Context, owner sessionmedia.Owner, agentID string, blocks []ai.ContentBlock) ([]ai.ContentBlock, error) {
			if owner != sessionmedia.UserOwner(uuid.MustParse(imageTestUserID)) || agentID != "agent-1" || !ai.HasImage(blocks) {
				t.Fatalf("unexpected enrich input: owner=%#v agent=%q blocks=%#v", owner, agentID, blocks)
			}
			return []ai.ContentBlock{ref}, nil
		}),
		NewRunner: func(context.Context, RunnerParams) (Runner, error) {
			return chatFakeRunner{messages: &received}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	out := rt.Chat(context.Background(), session.Info{ID: "sess-1", UserID: imageTestUserID, AgentID: "agent-1"}, []ai.ContentBlock{ai.ImageContent{Data: "raw", MimeType: "image/png"}})
	for evt := range out {
		if evt.Err != nil {
			t.Fatalf("chat: %v", evt.Err)
		}
	}
	receivedCanonical := false
	if len(received) == 1 {
		if receivedMsg, ok := received[0].(ai.UserMessage); ok {
			receivedCanonical = containsRuntimeRef(receivedMsg)
		}
	}
	if len(mem.messages) != 1 || !containsRuntimeRef(mem.messages[0]) || !receivedCanonical {
		t.Fatalf("canonical persistence = messages:%#v received:%#v", mem.messages, received)
	}
}

func TestRuntimeChatEnrichesSingularImageBeforeCanonicalAppend(t *testing.T) {
	mem := &recordingMemory{}
	ref := ai.ImageRefContent{MediaID: "media-1"}
	enrichCalls := 0
	rt, err := New(Config{
		Memory: mem,
		SessionImages: sessionImagesFunc(func(_ context.Context, _ sessionmedia.Owner, _ string, blocks []ai.ContentBlock) ([]ai.ContentBlock, error) {
			enrichCalls++
			if len(blocks) != 1 {
				t.Fatalf("singular image blocks = %#v", blocks)
			}
			if _, ok := blocks[0].(ai.ImageContent); !ok {
				t.Fatalf("enricher received %T, want raw ImageContent", blocks[0])
			}
			return []ai.ContentBlock{ref}, nil
		}),
		NewRunner: func(context.Context, RunnerParams) (Runner, error) { return chatFakeRunner{}, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	for evt := range rt.Chat(context.Background(), session.Info{ID: "sess-1", UserID: imageTestUserID, AgentID: "agent-1"}, ai.ImageContent{Data: "raw", MimeType: "image/png"}) {
		if evt.Err != nil {
			t.Fatalf("chat: %v", evt.Err)
		}
	}
	if enrichCalls != 1 || len(mem.messages) != 1 || !containsRuntimeRef(mem.messages[0]) {
		t.Fatalf("singular image bypassed canonical pipeline: enrich=%d messages=%#v", enrichCalls, mem.messages)
	}
}

func TestRuntimeChatPassesCanonicalImageRefWithoutEnrichment(t *testing.T) {
	mem := &recordingMemory{}
	ref := ai.ImageRefContent{MediaID: "media-1"}
	rt, err := New(Config{
		Memory: mem,
		SessionImages: sessionImagesFunc(func(context.Context, sessionmedia.Owner, string, []ai.ContentBlock) ([]ai.ContentBlock, error) {
			t.Fatal("canonical ImageRef input must not be re-enriched")
			return nil, nil
		}),
		NewRunner: func(context.Context, RunnerParams) (Runner, error) { return chatFakeRunner{}, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	for evt := range rt.Chat(context.Background(), session.Info{ID: "sess-1", UserID: imageTestUserID, AgentID: "agent-1"}, ref) {
		if evt.Err != nil {
			t.Fatalf("chat: %v", evt.Err)
		}
	}
	if len(mem.messages) != 1 || !containsRuntimeRef(mem.messages[0]) {
		t.Fatalf("canonical ImageRef did not persist safely: %#v", mem.messages)
	}
}

// A group trigger arrives already canonical: the event log stored a reference
// when the message was ingested, so the runtime passes it through untouched
// rather than enriching it a second time under the wrong owner.
func TestRuntimeGroupImageRefPassesThroughWithoutEnrichment(t *testing.T) {
	mem := &recordingMemory{}
	rt, err := New(Config{
		Memory: mem,
		SessionImages: sessionImagesFunc(func(context.Context, sessionmedia.Owner, string, []ai.ContentBlock) ([]ai.ContentBlock, error) {
			t.Fatal("a group trigger is canonical before the runtime sees it")
			return nil, nil
		}),
		NewRunner: func(context.Context, RunnerParams) (Runner, error) { return chatFakeRunner{}, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	groupID := uuid.NewString()
	info := session.Info{ID: "sess-1", UserID: groupID, AgentID: "agent-1", GroupID: groupID}
	blocks := []ai.ContentBlock{ai.TextContent{Text: "[seq:1 Ann]:"}, ai.ImageRefContent{MediaID: "media-1"}}
	for evt := range rt.Chat(context.Background(), info, blocks) {
		if evt.Err != nil {
			t.Fatalf("chat: %v", evt.Err)
		}
	}
	if len(mem.messages) != 1 || !containsRuntimeRef(mem.messages[0]) {
		t.Fatalf("group canonical trigger did not reach durable history: %#v", mem.messages)
	}
}

func TestRuntimeChatFailsClosedWhenCanonicalImageAppendFails(t *testing.T) {
	mem := &recordingMemory{appendError: errors.New("write failed")}
	rt, err := New(Config{
		Memory: mem,
		SessionImages: sessionImagesFunc(func(context.Context, sessionmedia.Owner, string, []ai.ContentBlock) ([]ai.ContentBlock, error) {
			return []ai.ContentBlock{ai.ImageRefContent{MediaID: "media-1"}}, nil
		}),
		NewRunner: func(context.Context, RunnerParams) (Runner, error) { return chatFakeRunner{}, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	out := rt.Chat(context.Background(), session.Info{ID: "sess-1", UserID: imageTestUserID, AgentID: "agent-1"}, []ai.ContentBlock{ai.ImageContent{Data: "raw", MimeType: "image/png"}})
	var got error
	for evt := range out {
		if evt.Err != nil {
			got = evt.Err
		}
	}
	if got == nil || len(mem.messages) != 0 {
		t.Fatalf("expected closed image-ref write, err=%v messages=%#v", got, mem.messages)
	}
}

func containsRuntimeRef(msg ai.Message) bool {
	for _, block := range runtimeTestMessageBlocks(msg) {
		if _, ok := block.(ai.ImageRefContent); ok {
			return true
		}
	}
	return false
}

func runtimeTestMessageBlocks(msg ai.Message) []ai.ContentBlock {
	switch msg := msg.(type) {
	case ai.UserMessage:
		blocks, _ := msg.Content.([]ai.ContentBlock)
		return blocks
	case ai.AssistantMessage:
		return msg.Content
	case ai.ToolResultMessage:
		return msg.Content
	default:
		return nil
	}
}

func TestRuntimeChatCommitsGroupCursorAfterSuccessfulGroupTurn(t *testing.T) {
	mem := &recordingMemory{}
	rt, err := New(Config{
		Memory: mem,
		NewRunner: func(context.Context, RunnerParams) (Runner, error) {
			return chatFakeRunner{events: []Event{{Text: "ok"}}}, nil
		},
	})
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}
	ctx := memory.WithGroupSeq(context.Background(), 42)
	out := make(chan Event, 10)
	rt.chat(ctx, out, session.Info{ID: "sess-1", UserID: "11111111-1111-4111-8111-111111111111", AgentID: "agent-1", GroupID: "11111111-1111-4111-8111-111111111111"}, "hello", chatOptions{})
	for range out {
	}
	if len(mem.commits) != 1 || mem.commits[0] != 42 {
		t.Fatalf("commits = %v, want [42]", mem.commits)
	}
	if len(mem.messages) != 2 {
		t.Fatalf("messages = %d, want user + assistant", len(mem.messages))
	}
	if got := flattenRuntimeUserMessage(mem.messages[0]); got != "hello" {
		t.Fatalf("persisted user = %q", got)
	}
	if _, ok := mem.messages[1].(ai.AssistantMessage); !ok {
		t.Fatalf("second persisted message = %T, want assistant", mem.messages[1])
	}
}

func TestRuntimeChatDoesNotCommitGroupCursorOnChatError(t *testing.T) {
	mem := &recordingMemory{}
	boom := errors.New("boom")
	rt, err := New(Config{
		Memory: mem,
		NewRunner: func(context.Context, RunnerParams) (Runner, error) {
			return chatFakeRunner{events: []Event{{Err: boom}}}, nil
		},
	})
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}
	ctx := memory.WithGroupSeq(context.Background(), 42)
	out := make(chan Event, 10)
	rt.chat(ctx, out, session.Info{ID: "sess-1", UserID: "11111111-1111-4111-8111-111111111111", AgentID: "agent-1", GroupID: "11111111-1111-4111-8111-111111111111"}, "hello", chatOptions{})
	for range out {
	}
	if len(mem.commits) != 0 {
		t.Fatalf("commits = %v, want none", mem.commits)
	}
	if len(mem.messages) != 0 {
		t.Fatalf("messages = %d, want none on failed group turn", len(mem.messages))
	}
}

func TestRuntimeChatDoesNotCommitGroupCursorWhenContextCanceled(t *testing.T) {
	mem := &recordingMemory{}
	rt, err := New(Config{
		Memory: mem,
		NewRunner: func(context.Context, RunnerParams) (Runner, error) {
			return chatFakeRunner{events: []Event{{Text: "ok"}}}, nil
		},
	})
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}
	ctx, cancel := context.WithCancel(memory.WithGroupSeq(context.Background(), 42))
	cancel()
	out := make(chan Event, 10)
	rt.chat(ctx, out, session.Info{ID: "sess-1", UserID: "11111111-1111-4111-8111-111111111111", AgentID: "agent-1", GroupID: "11111111-1111-4111-8111-111111111111"}, "hello", chatOptions{})
	for range out {
	}
	if len(mem.commits) != 0 {
		t.Fatalf("commits = %v, want none", mem.commits)
	}
}

func TestRuntimeChatDoesNotPersistGroupPartialOnTimeout(t *testing.T) {
	mem := &recordingMemory{}
	rt, err := New(Config{
		Memory: mem,
		NewRunner: func(context.Context, RunnerParams) (Runner, error) {
			return chatFakeRunner{events: []Event{{Text: "partial"}, {Err: ErrChatTimeout}}}, nil
		},
	})
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}
	ctx := memory.WithGroupSeq(context.Background(), 42)
	out := make(chan Event, 10)
	rt.chat(ctx, out, session.Info{ID: "sess-1", UserID: "11111111-1111-4111-8111-111111111111", AgentID: "agent-1", GroupID: "11111111-1111-4111-8111-111111111111"}, "hello", chatOptions{})
	for range out {
	}
	if len(mem.commits) != 0 {
		t.Fatalf("commits = %v, want none", mem.commits)
	}
	if len(mem.messages) != 0 {
		t.Fatalf("messages = %d, want none on timeout", len(mem.messages))
	}
}

func TestRuntimeChatDoesNotPersistGroupStoreBeforeLaterError(t *testing.T) {
	mem := &recordingMemory{}
	boom := errors.New("boom")
	rt, err := New(Config{
		Memory: mem,
		NewRunner: func(context.Context, RunnerParams) (Runner, error) {
			return chatFakeRunner{events: []Event{{Store: ai.AssistantMessage{Content: []ai.ContentBlock{ai.TextContent{Text: "stored"}}}}, {Err: boom}}}, nil
		},
	})
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}
	ctx := memory.WithGroupSeq(context.Background(), 42)
	out := make(chan Event, 10)
	rt.chat(ctx, out, session.Info{ID: "sess-1", UserID: "11111111-1111-4111-8111-111111111111", AgentID: "agent-1", GroupID: "11111111-1111-4111-8111-111111111111"}, "hello", chatOptions{})
	for range out {
	}
	if len(mem.commits) != 0 {
		t.Fatalf("commits = %v, want none", mem.commits)
	}
	if len(mem.messages) != 0 {
		t.Fatalf("messages = %d, want none after store then error", len(mem.messages))
	}
}

func TestRuntimeChatDoesNotCommitGroupCursorWhenStoreFails(t *testing.T) {
	mem := &recordingMemory{appendError: errors.New("append failed")}
	rt, err := New(Config{
		Memory: mem,
		NewRunner: func(context.Context, RunnerParams) (Runner, error) {
			return chatFakeRunner{events: []Event{{Text: "ok"}}}, nil
		},
	})
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}
	ctx := memory.WithGroupSeq(context.Background(), 42)
	out := make(chan Event, 10)
	rt.chat(ctx, out, session.Info{ID: "sess-1", UserID: "11111111-1111-4111-8111-111111111111", AgentID: "agent-1", GroupID: "11111111-1111-4111-8111-111111111111"}, "hello", chatOptions{})
	for range out {
	}
	if len(mem.commits) != 0 {
		t.Fatalf("commits = %v, want none", mem.commits)
	}
}

func TestRuntimeChatDoesNotCommitGroupCursorWhenAssembleFails(t *testing.T) {
	assembleErr := errors.New("assemble failed")
	mem := &recordingMemory{assembleError: assembleErr}
	rt, err := New(Config{
		Memory: mem,
		NewRunner: func(context.Context, RunnerParams) (Runner, error) {
			return chatFakeRunner{events: []Event{{Text: "ok"}}}, nil
		},
	})
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}
	ctx := memory.WithGroupSeq(context.Background(), 42)
	out := make(chan Event, 10)
	rt.chat(ctx, out, session.Info{ID: "sess-1", UserID: "11111111-1111-4111-8111-111111111111", AgentID: "agent-1", GroupID: "11111111-1111-4111-8111-111111111111"}, "hello", chatOptions{})
	var gotErr bool
	for evt := range out {
		if errors.Is(evt.Err, assembleErr) {
			gotErr = true
		}
	}
	if !gotErr {
		t.Fatal("expected assemble error event")
	}
	if len(mem.commits) != 0 {
		t.Fatalf("commits = %v, want none", mem.commits)
	}
	if len(mem.messages) != 0 {
		t.Fatalf("messages = %d, want none on assemble failure", len(mem.messages))
	}
}

func TestSinkDeliversExactlyOneResultOnEveryExit(t *testing.T) {
	group := "11111111-1111-4111-8111-111111111111"
	info := session.Info{ID: "sink-session", UserID: group, AgentID: "agent-1", GroupID: group}
	boom := errors.New("boom")
	for _, tc := range []struct {
		name      string
		mem       *recordingMemory
		beforeRun BeforeRunFunc
		events    []Event
	}{
		{name: "assemble failure", mem: &recordingMemory{assembleError: boom}},
		{name: "before run failure", mem: &recordingMemory{}, beforeRun: func(context.Context, session.Info, string, string, string, []ai.Message, PluginContext) (string, error) {
			return "", boom
		}},
		{name: "runner error", mem: &recordingMemory{}, events: []Event{{Err: boom}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rt, err := New(Config{
				Memory:    tc.mem,
				BeforeRun: tc.beforeRun,
				NewRunner: func(context.Context, RunnerParams) (Runner, error) { return chatFakeRunner{events: tc.events}, nil },
			})
			if err != nil {
				t.Fatal(err)
			}
			sink := memory.NewGroupTurnSink()
			for range rt.Chat(memory.WithGroupTurnSink(context.Background(), sink), info, "hello") {
			}
			turn, delivered := sink.Result()
			if !delivered {
				t.Fatal("sink did not deliver a result")
			}
			if turn.Complete {
				t.Fatal("incomplete exit delivered Complete=true")
			}
			sink.Deliver(memory.DeferredGroupTurn{Complete: true})
			if got, ok := sink.Result(); !ok || got.Complete {
				t.Fatalf("second delivery replaced result: %+v, %v", got, ok)
			}
		})
	}

	t.Run("runner construction panic", func(t *testing.T) {
		rt, err := New(Config{
			Memory:    &recordingMemory{},
			NewRunner: func(context.Context, RunnerParams) (Runner, error) { return panicRunner{}, nil },
		})
		if err != nil {
			t.Fatal(err)
		}
		sink := memory.NewGroupTurnSink()
		for range rt.Chat(memory.WithGroupTurnSink(context.Background(), sink), info, "hello") {
		}
		turn, delivered := sink.Result()
		if !delivered {
			t.Fatal("sink did not deliver a result")
		}
		if turn.Complete {
			t.Fatal("panic delivered Complete=true")
		}
	})
}

func TestNoSinkGroupTurnKeepsInlineAssemblerAppendAndCursor(t *testing.T) {
	mem := &recordingMemory{}
	rt, err := New(Config{
		Memory: mem,
		NewRunner: func(context.Context, RunnerParams) (Runner, error) {
			return chatFakeRunner{events: []Event{{Text: "ok"}}}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	group := "11111111-1111-4111-8111-111111111111"
	out := make(chan Event, 10)
	rt.chat(memory.WithGroupSeq(context.Background(), 7), out, session.Info{ID: "legacy-sink-session", UserID: group, AgentID: "agent-1", GroupID: group}, "hello", chatOptions{})
	for range out {
	}
	if len(mem.messages) != 2 || len(mem.commits) != 1 || mem.commits[0] != 7 {
		t.Fatalf("legacy group writes = messages:%d commits:%v, want user+assistant and [7]", len(mem.messages), mem.commits)
	}
}

func TestSinkGroupTurnDefersRowsAndCursor(t *testing.T) {
	mem := &recordingMemory{}
	rt, err := New(Config{
		Memory: mem,
		NewRunner: func(context.Context, RunnerParams) (Runner, error) {
			return chatFakeRunner{events: []Event{{Text: "ok"}}}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	group := "11111111-1111-4111-8111-111111111111"
	sink := memory.NewGroupTurnSink()
	ctx := memory.WithGroupTurnSink(memory.WithGroupSeq(context.Background(), 8), sink)
	for range rt.Chat(ctx, session.Info{ID: "deferred-sink-session", UserID: group, AgentID: "agent-1", GroupID: group}, "hello") {
	}
	turn, delivered := sink.Result()
	if !delivered {
		t.Fatal("sink did not deliver a result")
	}
	if !turn.Complete || turn.TriggerSeq != 8 || len(turn.OwnRows) != 2 {
		t.Fatalf("deferred turn = %+v", turn)
	}
	if len(mem.messages) != 0 || len(mem.commits) != 0 {
		t.Fatalf("sink group writes = messages:%d commits:%v, want none", len(mem.messages), mem.commits)
	}
}

func flattenRuntimeUserMessage(msg ai.Message) string {
	um, ok := msg.(ai.UserMessage)
	if !ok {
		return ""
	}
	switch c := um.Content.(type) {
	case string:
		return c
	case []ai.ContentBlock:
		return ai.FlattenText(c)
	default:
		return fmt.Sprintf("%v", c)
	}
}

func TestStreamEventsDoesNotDuplicateBufferedAssistantStore(t *testing.T) {
	mem := &recordingMemory{}
	rt := &Runtime{mem: mem, log: slog.Default()}

	stream := make(chan Event, 3)
	out := make(chan Event, 3)
	stream <- Event{Reasoning: "thinking"}
	stream <- Event{Text: "answer"}
	stream <- Event{Store: ai.AssistantMessage{Content: []ai.ContentBlock{
		ai.ThinkingContent{Thinking: "thinking"},
		ai.TextContent{Text: "answer"},
		ai.ToolCall{ID: "tool-1", Name: "search", Arguments: map[string]any{"q": "x"}},
	}}}
	close(stream)

	if err := rt.streamEventsClosing(context.Background(), "session-1", memory.Session{ID: "session-1"}, stream, out, hooks.NewHookSet(nil), hooks.HookMeta{}, time.Now()); err != nil {
		t.Fatalf("stream events: %v", err)
	}
	for range out {
	}

	if len(mem.messages) != 1 {
		t.Fatalf("expected one persisted assistant message, got %d", len(mem.messages))
	}
	msg, ok := mem.messages[0].(ai.AssistantMessage)
	if !ok {
		t.Fatalf("expected assistant message, got %T", mem.messages[0])
	}
	if len(msg.Content) != 3 {
		t.Fatalf("expected 3 content blocks, got %d", len(msg.Content))
	}
	if got := msg.Content[0].(ai.ThinkingContent).Thinking; got != "thinking" {
		t.Fatalf("thinking = %q", got)
	}
	if got := msg.Content[1].(ai.TextContent).Text; got != "answer" {
		t.Fatalf("text = %q", got)
	}
}

func TestStreamEventsPersistsAtomicToolCompletionBeforeCancelledForward(t *testing.T) {
	mem := &recordingMemory{}
	rt := &Runtime{mem: mem, log: slog.Default()}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	stream := make(chan Event, 1)
	stream <- Event{
		ToolUse: &ToolUseEvent{ID: "call-1", Tool: "task", Status: "done"},
		Store:   ai.ToolResultMessage{ToolCallID: "call-1", ToolName: "task", Content: []ai.ContentBlock{ai.TextContent{Text: "committed"}}},
	}
	close(stream)
	out := make(chan Event, 1)
	err := rt.streamEventsClosing(ctx, "session-1", memory.Session{ID: "session-1"}, stream, out, hooks.NewHookSet(nil), hooks.HookMeta{}, time.Now())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("stream error = %v, want context cancellation", err)
	}
	if len(mem.messages) != 1 {
		t.Fatalf("persisted messages = %#v, want completed tool result", mem.messages)
	}
	if _, ok := mem.messages[0].(ai.ToolResultMessage); !ok {
		t.Fatalf("persisted message = %T, want ai.ToolResultMessage", mem.messages[0])
	}
	if len(out) != 0 {
		t.Fatalf("cancelled completion should not leak a partial tool event: %#v", <-out)
	}
}

func TestStreamEvents_TimeoutDoesNotForwardError(t *testing.T) {
	mem := &recordingMemory{}
	rt := &Runtime{mem: mem, log: slog.Default()}

	stream := make(chan Event, 3)
	out := make(chan Event, 10)
	stream <- Event{Text: "partial"}
	stream <- Event{Err: ErrChatTimeout}
	close(stream)

	if err := rt.streamEventsClosing(context.Background(), "sess-1", memory.Session{ID: "sess-1"}, stream, out, hooks.NewHookSet(nil), hooks.HookMeta{}, time.Now()); !errors.Is(err, ErrChatTimeout) {
		t.Fatalf("stream events error = %v, want timeout", err)
	}

	var events []Event
	for evt := range out {
		events = append(events, evt)
	}

	// Should have: text "partial", then the timeout notice text.
	// Should NOT have an Err event.
	for _, evt := range events {
		if evt.Err != nil {
			t.Fatalf("timeout should not forward error to caller, got: %v", evt.Err)
		}
	}
	if len(events) < 2 {
		t.Fatalf("expected at least 2 events (partial + notice), got %d", len(events))
	}
}

func TestStreamEvents_NonTimeoutErrorForwarded(t *testing.T) {
	mem := &recordingMemory{}
	rt := &Runtime{mem: mem, log: slog.Default()}

	stream := make(chan Event, 2)
	out := make(chan Event, 10)
	realErr := fmt.Errorf("provider error")
	stream <- Event{Err: realErr}
	close(stream)

	if err := rt.streamEventsClosing(context.Background(), "sess-1", memory.Session{ID: "sess-1"}, stream, out, hooks.NewHookSet(nil), hooks.HookMeta{}, time.Now()); !errors.Is(err, realErr) {
		t.Fatalf("stream events error = %v, want provider error", err)
	}

	var gotErr bool
	for evt := range out {
		if evt.Err != nil && errors.Is(evt.Err, realErr) {
			gotErr = true
		}
	}
	if !gotErr {
		t.Fatal("non-timeout errors should be forwarded to caller")
	}
}

// streamEventsClosing exercises streamEvents the way chatWithRunner does: the
// caller owns out and closes it when the stream is finished.
func (rt *Runtime) streamEventsClosing(
	ctx context.Context,
	sessionID string,
	memSess memory.Session,
	stream <-chan Event,
	out chan<- Event,
	hs *hooks.HookSet,
	hookMeta hooks.HookMeta,
	chatStart time.Time,
	storePrefix ...ai.Message,
) error {
	defer close(out)
	_, err := rt.streamEvents(ctx, sessionID, memSess, stream, out, hs, hookMeta, chatStart, storePrefix...)
	return err
}
