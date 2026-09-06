package runtime

import (
	"context"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/CherryHQ/stella/internal/agent/session"
	"github.com/CherryHQ/stella/internal/memory"
	"github.com/CherryHQ/stella/pkg/ai"
)

type panicRunner struct{}

func (panicRunner) Chat(_ context.Context, _ []ai.Message, _ MessageContent) <-chan Event {
	panic("boom")
}
func (panicRunner) Alive() bool                  { return true }
func (panicRunner) Busy() bool                   { return false }
func (panicRunner) LastActivity() time.Time      { return time.Now() }
func (panicRunner) SystemPrompt() string         { return "" }
func (panicRunner) PluginContext() PluginContext { return PluginContext{} }
func (panicRunner) Close() error                 { return nil }

// A panic inside the turn must not wedge the session: the caller's channel
// still closes, the busy guard clears, and the hub stops reporting the session
// as live (otherwise SSE watchers would poll a never-closing stream forever).
func TestChat_PanicRecovers_FreesSessionAndHub(t *testing.T) {
	mem := &activityRecordingMemory{}
	rt, _ := New(Config{
		NewRunner: func(_ context.Context, _ RunnerParams) (Runner, error) {
			return panicRunner{}, nil
		},
		Memory: mem,
	})

	info := session.Info{ID: "sess-1", UserID: "u1", AgentID: "a1"}
	ch := rt.Chat(context.Background(), info, "hi")
	for range ch { //nolint:revive // drain until the forwarder closes out
	}

	waitSessionFree(t, rt, info.ID)
	if rt.SessionLive(info.ID) {
		t.Fatal("session stuck live after a panicking turn")
	}
	if got := mem.activitySnapshot(); !reflect.DeepEqual(got, []string{"started", "completed:error"}) {
		t.Fatalf("panicked turn activity = %v, want [started completed:error]", got)
	}
}

// panicOnNthAppendMemory panics on the nth Append call. Used to drive a panic
// from inside streamEvents (which persists the assistant message) rather than
// before it — the user message is appended first, in rt.chat.
type panicOnNthAppendMemory struct {
	recordingMemory
	n     int
	mu    sync.Mutex
	count int
}

func (m *panicOnNthAppendMemory) Append(_ context.Context, _ memory.Session, _ ...ai.Message) error {
	m.mu.Lock()
	m.count++
	c := m.count
	m.mu.Unlock()
	if c == m.n {
		panic("append boom")
	}
	return nil
}

// A panic raised inside streamEvents unwinds through its defer, which closes
// inner. The Chat wrapper's recover must NOT blindly close inner again — a
// second close of the same channel panics in a goroutine and crashes the
// process. This guards that the recovery path tolerates an already-closed
// channel while still freeing the session and hub.
func TestChat_PanicInStreamEvents_NoDoubleClose(t *testing.T) {
	mem := &panicOnNthAppendMemory{n: 2} // 1: user message in rt.chat; 2: assistant flush in streamEvents
	rt, _ := New(Config{
		Memory: mem,
		NewRunner: func(_ context.Context, _ RunnerParams) (Runner, error) {
			return chatFakeRunner{events: []Event{{Text: "hi"}}}, nil
		},
	})

	info := session.Info{ID: "sess-1", UserID: "u1", AgentID: "a1"}
	ch := rt.Chat(context.Background(), info, "hi")
	for range ch { //nolint:revive // drain until the forwarder closes out
	}

	waitSessionFree(t, rt, info.ID)
	if rt.SessionLive(info.ID) {
		t.Fatal("session stuck live after a panic inside streamEvents")
	}
}
