package telegram

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vaayne/pibot/agent"
)

func TestSplitMessageShort(t *testing.T) {
	chunks := splitMessage("hello")
	if len(chunks) != 1 || chunks[0] != "hello" {
		t.Errorf("chunks = %v, want [hello]", chunks)
	}
}

func TestSplitMessageExactLimit(t *testing.T) {
	msg := strings.Repeat("a", telegramMaxMessageLen)
	chunks := splitMessage(msg)
	if len(chunks) != 1 {
		t.Errorf("len(chunks) = %d, want 1", len(chunks))
	}
}

func TestSplitMessageLong(t *testing.T) {
	// Create a message just over the limit.
	msg := strings.Repeat("a", telegramMaxMessageLen+100)
	chunks := splitMessage(msg)
	if len(chunks) != 2 {
		t.Errorf("len(chunks) = %d, want 2", len(chunks))
	}
	if len(chunks[0]) != telegramMaxMessageLen {
		t.Errorf("chunk[0] len = %d, want %d", len(chunks[0]), telegramMaxMessageLen)
	}
	if len(chunks[1]) != 100 {
		t.Errorf("chunk[1] len = %d, want 100", len(chunks[1]))
	}
}

func TestSplitMessageAtNewline(t *testing.T) {
	// Build a message with a newline before the limit.
	part1 := strings.Repeat("a", 3000)
	part2 := strings.Repeat("b", 2000)
	msg := part1 + "\n" + part2

	chunks := splitMessage(msg)
	if len(chunks) != 2 {
		t.Fatalf("len(chunks) = %d, want 2", len(chunks))
	}
	// First chunk should include up to and including the newline.
	if chunks[0] != part1+"\n" {
		t.Errorf("chunk[0] = %q..., want split at newline", chunks[0][:20])
	}
	if chunks[1] != part2 {
		t.Errorf("chunk[1] len = %d, want %d", len(chunks[1]), len(part2))
	}
}

func TestSplitMessageEmpty(t *testing.T) {
	chunks := splitMessage("")
	if len(chunks) != 1 || chunks[0] != "" {
		t.Errorf("chunks = %v, want [\"\"]", chunks)
	}
}

func TestSplitMessageMultipleChunks(t *testing.T) {
	msg := strings.Repeat("x", telegramMaxMessageLen*3+500)
	chunks := splitMessage(msg)
	if len(chunks) != 4 {
		t.Errorf("len(chunks) = %d, want 4", len(chunks))
	}
	// Reconstruct and verify.
	var rebuilt strings.Builder
	for _, c := range chunks {
		rebuilt.WriteString(c)
	}
	if rebuilt.String() != msg {
		t.Error("chunks do not reconstruct to original message")
	}
}

func TestGetUpdates(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/getUpdates") {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		resp := telegramGetUpdatesResponse{
			OK: true,
			Result: []telegramUpdate{
				{
					UpdateID: 100,
					Message: &telegramMessage{
						Chat: telegramChat{ID: 42},
						Text: "hello",
					},
				},
			},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	updates, err := getUpdates(context.Background(), server.Client(), server.URL, 0)
	if err != nil {
		t.Fatalf("getUpdates: %v", err)
	}
	if len(updates) != 1 {
		t.Fatalf("len(updates) = %d, want 1", len(updates))
	}
	if updates[0].Message.Text != "hello" {
		t.Errorf("text = %q, want %q", updates[0].Message.Text, "hello")
	}
	if updates[0].Message.Chat.ID != 42 {
		t.Errorf("chat ID = %d, want 42", updates[0].Message.Chat.ID)
	}
}

func TestGetUpdatesError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("internal error"))
	}))
	defer server.Close()

	_, err := getUpdates(context.Background(), server.Client(), server.URL, 0)
	if err == nil {
		t.Fatal("expected error for 500 response")
	}
}

func TestGetUpdatesNotOK(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := telegramGetUpdatesResponse{OK: false}
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	_, err := getUpdates(context.Background(), server.Client(), server.URL, 0)
	if err == nil {
		t.Fatal("expected error for ok=false")
	}
}

func TestSendMessage(t *testing.T) {
	var received telegramSendMessageRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/sendMessage") {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		json.NewDecoder(r.Body).Decode(&received)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()

	err := sendMessage(context.Background(), server.Client(), server.URL, 42, "hi there")
	if err != nil {
		t.Fatalf("sendMessage: %v", err)
	}
	if received.ChatID != 42 {
		t.Errorf("chat_id = %d, want 42", received.ChatID)
	}
	if received.Text != "hi there" {
		t.Errorf("text = %q, want %q", received.Text, "hi there")
	}
}

func TestSendMessageError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte("bad request"))
	}))
	defer server.Close()

	err := sendMessage(context.Background(), server.Client(), server.URL, 42, "hi")
	if err == nil {
		t.Fatal("expected error for 400 response")
	}
}

func TestSendChatAction(t *testing.T) {
	var received telegramSendChatActionRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&received)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()

	err := sendChatAction(context.Background(), server.Client(), server.URL, 42, "typing")
	if err != nil {
		t.Fatalf("sendChatAction: %v", err)
	}
	if received.ChatID != 42 {
		t.Errorf("chat_id = %d, want 42", received.ChatID)
	}
	if received.Action != "typing" {
		t.Errorf("action = %q, want %q", received.Action, "typing")
	}
}

func TestGetUpdatesCancelled(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Block forever — context cancel should abort.
		<-r.Context().Done()
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately.

	_, err := getUpdates(ctx, server.Client(), server.URL, 0)
	if err == nil {
		t.Fatal("expected error for cancelled context")
	}
}

func TestRunCancelledImmediately(t *testing.T) {
	bin := writeMockPiBinary(t)
	sm := agent.NewSessionManager(bin, "", t.TempDir(), 10*time.Minute)
	defer sm.StopAll()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel before starting.

	err := Run(ctx, "fake-token", sm)
	if err != context.Canceled {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

func writeMockPiBinary(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "mock-pi")
	// Reads one line (the prompt), emits text_delta + agent_end, then exits.
	script := `#!/bin/sh
read line
echo '{"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":"mock reply"}}'
echo '{"type":"agent_end"}'
# Keep running for further prompts
while read line; do
  echo '{"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":"another reply"}}'
  echo '{"type":"agent_end"}'
done
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

func TestRunTelegramLoop(t *testing.T) {
	bin := writeMockPiBinary(t)
	sm := agent.NewSessionManager(bin, "", t.TempDir(), 10*time.Minute)
	defer sm.StopAll()

	var callCount atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/getUpdates"):
			n := callCount.Add(1)
			if n == 1 {
				// First call: return a message.
				resp := telegramGetUpdatesResponse{
					OK: true,
					Result: []telegramUpdate{
						{
							UpdateID: 1,
							Message: &telegramMessage{
								Chat: telegramChat{ID: 100},
								Text: "hello bot",
							},
						},
					},
				}
				json.NewEncoder(w).Encode(resp)
			} else {
				// Subsequent calls: block until context cancelled.
				<-r.Context().Done()
			}
		case strings.Contains(r.URL.Path, "/sendMessage"):
			var req telegramSendMessageRequest
			json.NewDecoder(r.Body).Decode(&req)
			if req.ChatID != 100 {
				t.Errorf("sendMessage chat_id = %d, want 100", req.ChatID)
			}
			if !strings.Contains(req.Text, "mock reply") {
				t.Errorf("sendMessage text = %q, want contains 'mock reply'", req.Text)
			}
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, `{"ok":true}`)
		case strings.Contains(r.URL.Path, "/sendChatAction"):
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, `{"ok":true}`)
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := runTelegramLoop(ctx, server.URL, server.Client(), sm)
	if err != nil && err != context.DeadlineExceeded && err != context.Canceled {
		t.Fatalf("runTelegramLoop: %v", err)
	}

	if callCount.Load() < 1 {
		t.Error("expected at least 1 getUpdates call")
	}
}

func TestRunTelegramLoopSkipsEmptyMessage(t *testing.T) {
	bin := writeMockPiBinary(t)
	sm := agent.NewSessionManager(bin, "", t.TempDir(), 10*time.Minute)
	defer sm.StopAll()

	var sendCalled atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/getUpdates"):
			resp := telegramGetUpdatesResponse{
				OK: true,
				Result: []telegramUpdate{
					{UpdateID: 1, Message: nil},                                                      // nil message
					{UpdateID: 2, Message: &telegramMessage{Chat: telegramChat{ID: 1}, Text: "  "}}, // whitespace only
				},
			}
			json.NewEncoder(w).Encode(resp)
			// Second call blocks.
			<-r.Context().Done()
		case strings.Contains(r.URL.Path, "/sendMessage"):
			sendCalled.Add(1)
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, `{"ok":true}`)
		case strings.Contains(r.URL.Path, "/sendChatAction"):
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, `{"ok":true}`)
		}
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	runTelegramLoop(ctx, server.URL, server.Client(), sm)

	if sendCalled.Load() != 0 {
		t.Errorf("sendMessage called %d times, want 0 for empty messages", sendCalled.Load())
	}
}

func TestRunTelegramLoopRetryOnError(t *testing.T) {
	bin := writeMockPiBinary(t)
	sm := agent.NewSessionManager(bin, "", t.TempDir(), 10*time.Minute)
	defer sm.StopAll()

	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if n == 1 {
			// First call fails.
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		// Second call blocks until cancel.
		<-r.Context().Done()
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	runTelegramLoop(ctx, server.URL, server.Client(), sm)

	if calls.Load() < 2 {
		t.Errorf("expected at least 2 calls (1 error + 1 retry), got %d", calls.Load())
	}
}
