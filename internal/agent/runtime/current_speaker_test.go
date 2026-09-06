package runtime

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/CherryHQ/stella/internal/agent/session"
	"github.com/CherryHQ/stella/internal/memory"
	"github.com/CherryHQ/stella/pkg/ai"
)

type recordingChatRunner struct {
	system  string
	events  []Event
	history []ai.Message
	message MessageContent
}

func (r *recordingChatRunner) Chat(_ context.Context, history []ai.Message, message MessageContent) <-chan Event {
	r.history = append([]ai.Message(nil), history...)
	r.message = message
	ch := make(chan Event, len(r.events))
	for _, evt := range r.events {
		ch <- evt
	}
	close(ch)
	return ch
}

func (r *recordingChatRunner) Alive() bool                  { return true }
func (r *recordingChatRunner) Busy() bool                   { return false }
func (r *recordingChatRunner) LastActivity() time.Time      { return time.Now() }
func (r *recordingChatRunner) SystemPrompt() string         { return r.system }
func (r *recordingChatRunner) PluginContext() PluginContext { return PluginContext{} }
func (r *recordingChatRunner) Close() error                 { return nil }

func TestRuntimeChatInjectsCurrentSpeakerIntoGroupTurnMessage(t *testing.T) {
	mem := &recordingMemory{}
	runner := &recordingChatRunner{system: "stable group system", events: []Event{{Text: "ok"}}}
	var beforeRunSystems []string

	rt, err := New(Config{
		Memory: mem,
		NewRunner: func(context.Context, RunnerParams) (Runner, error) {
			return runner, nil
		},
		BeforeRun: func(_ context.Context, _ session.Info, _, _, system string, _ []ai.Message, _ PluginContext) (string, error) {
			beforeRunSystems = append(beforeRunSystems, system)
			return system, nil
		},
	})
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}

	info := session.Info{ID: "sess-1", UserID: "11111111-1111-4111-8111-111111111111", AgentID: "agent-1", GroupID: "11111111-1111-4111-8111-111111111111"}
	out := make(chan Event, 10)
	rt.chat(
		memory.WithGroupSeq(context.Background(), 7),
		out,
		info,
		"hello everyone",
		chatOptions{currentSpeaker: memory.CurrentSpeaker{DisplayName: "Alice", UserID: "alice-user"}, hasSpeaker: true},
	)
	for range out { // drain
	}

	if len(beforeRunSystems) != 1 || beforeRunSystems[0] != "stable group system" {
		t.Fatalf("before-run systems = %q, want stable base system", beforeRunSystems)
	}
	got := flattenRuntimeUserMessage(runner.message.(ai.UserMessage))
	for _, want := range []string{"<current_speaker>", "Name: Alice", "Linked Stella user: yes", "hello everyone"} {
		if !strings.Contains(got, want) {
			t.Fatalf("model message missing %q:\n%s", want, got)
		}
	}
	if len(mem.messages) == 0 {
		t.Fatal("expected persisted group user message")
	}
	if persisted := flattenRuntimeUserMessage(mem.messages[0]); persisted != "hello everyone" {
		t.Fatalf("persisted group user message = %q, want original user text only", persisted)
	}
}

func TestWithCurrentSpeakerContextSupportsMultimodalContent(t *testing.T) {
	msg := []ai.ContentBlock{
		ai.ImageContent{Data: "abc", MimeType: "image/png"},
		ai.TextContent{Text: "what is this?"},
	}
	got, ok := withCurrentSpeakerContext(msg, memory.CurrentSpeaker{DisplayName: "Bob"}).([]ai.ContentBlock)
	if !ok {
		t.Fatalf("speaker context content = %T, want []ai.ContentBlock", got)
	}
	if len(got) != 3 {
		t.Fatalf("blocks = %d, want speaker prefix + original 2 blocks", len(got))
	}
	prefix, ok := got[0].(ai.TextContent)
	if !ok || !strings.Contains(prefix.Text, "Name: Bob") || !strings.Contains(prefix.Text, "Linked Stella user: no") {
		t.Fatalf("prefix block = %#v", got[0])
	}
	if got[1] != msg[0] || got[2] != msg[1] {
		t.Fatalf("original blocks not preserved: %#v", got)
	}
}

func TestWakeBlockRendersReasonAndHeldUpTo(t *testing.T) {
	got, ok := withGroupWakeContext("[seq:4 Alice]: ship it?", memory.GroupWake{Reason: "mentioned"}).(string)
	if !ok || !strings.Contains(got, "<wake>") || !strings.Contains(got, "mentioned") {
		t.Fatalf("wake prefix = %q", got)
	}
	if strings.Contains(got, "held") {
		t.Fatalf("a first attempt must not claim it was held: %q", got)
	}
	if !strings.HasSuffix(got, "[seq:4 Alice]: ship it?") {
		t.Fatalf("wake prefix dropped the trigger: %q", got)
	}

	held, _ := withGroupWakeContext("[seq:9 Alice]: and now?", memory.GroupWake{Reason: "open_floor", HeldUpToSeq: 7}).(string)
	if !strings.Contains(held, "seq 7") {
		t.Fatalf("held wake = %q, want the seq peers reached", held)
	}
}

func TestNoWakeReasonLeavesTriggerUntouched(t *testing.T) {
	msg := "[seq:1 Alice]: hi"
	if got := withGroupWakeContext(msg, memory.GroupWake{}); got != msg {
		t.Fatalf("unset wake changed the trigger: %v", got)
	}
}

// The block is the only place the model is told what a group turn may do with
// private memory, and memory routing makes that non-negotiable: a group turn
// reaches the public lane before it decodes a ref, so a profile read is refused
// no matter who asked.
//
// The assertion is over the whole rendered block, not over the instruction
// constant. The contradiction this test exists to catch was not in the
// instruction at all: the instruction refused profile reads while the linked
// label two lines below offered them "by explicit request", and a test that
// read only the constant saw nothing wrong.
func TestCurrentSpeakerBlockRefusesPrivateProfileReadsThroughout(t *testing.T) {
	for _, speaker := range []memory.CurrentSpeaker{
		{DisplayName: "Alice", UserID: "alice-user"},
		{DisplayName: "Alice"},
	} {
		got := currentSpeakerContextText(speaker)
		for _, want := range []string{
			"cannot be read in a group turn at all",
			"one-to-one session",
		} {
			if !strings.Contains(got, want) {
				t.Errorf("speaker block lost %q:\n%s", want, got)
			}
		}
		// Every phrasing that has stood for "you can get the profile if you ask".
		for _, banned := range []string{"unless", "by explicit request", "on request", "profile available"} {
			if strings.Contains(got, banned) {
				t.Errorf("speaker block still offers a profile read via %q:\n%s", banned, got)
			}
		}
	}
}
