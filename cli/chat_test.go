package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vaayne/pibot/agent"
)

func writeMockPi(t *testing.T, events []map[string]interface{}) string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "mock-pi")

	// Script reads one line from stdin (the prompt command), then writes events to stdout.
	var lines []string
	for _, evt := range events {
		data, _ := json.Marshal(evt)
		lines = append(lines, fmt.Sprintf("echo '%s'", string(data)))
	}

	script := "#!/bin/sh\nread line\n" + strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

func TestRunChatQuit(t *testing.T) {
	// Mock Pi that responds with a text delta then agent_end.
	bin := writeMockPi(t, []map[string]interface{}{
		{"type": "message_update", "assistantMessageEvent": map[string]string{"type": "text_delta", "delta": "Hi there"}},
		{"type": "agent_end"},
	})

	sm := agent.NewSessionManager(bin, t.TempDir(), 10*time.Minute)
	defer sm.StopAll()

	// Pipe: send "hello", then "/quit".
	input := "hello\n/quit\n"

	// Replace os.Stdin for the test.
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}

	oldStdin := os.Stdin
	os.Stdin = r
	defer func() { os.Stdin = oldStdin }()

	// Capture stdout.
	oldStdout := os.Stdout
	outR, outW, _ := os.Pipe()
	os.Stdout = outW
	defer func() { os.Stdout = oldStdout }()

	go func() {
		io.WriteString(w, input)
		w.Close()
	}()

	ctx := context.Background()
	runErr := RunChat(ctx, sm)

	outW.Close()
	var buf bytes.Buffer
	io.Copy(&buf, outR)
	os.Stdout = oldStdout

	if runErr != nil {
		t.Fatalf("RunChat: %v", runErr)
	}

	output := buf.String()
	if !strings.Contains(output, "pibot") {
		t.Errorf("output missing welcome message: %s", output)
	}
}

func TestRunChatExit(t *testing.T) {
	bin := writeMockPi(t, []map[string]interface{}{
		{"type": "agent_end"},
	})

	sm := agent.NewSessionManager(bin, t.TempDir(), 10*time.Minute)
	defer sm.StopAll()

	r, w, _ := os.Pipe()
	oldStdin := os.Stdin
	os.Stdin = r
	defer func() { os.Stdin = oldStdin }()

	// Capture stdout.
	oldStdout := os.Stdout
	_, outW, _ := os.Pipe()
	os.Stdout = outW
	defer func() { os.Stdout = oldStdout }()

	go func() {
		io.WriteString(w, "/exit\n")
		w.Close()
	}()

	err := RunChat(context.Background(), sm)
	outW.Close()
	os.Stdout = oldStdout

	if err != nil {
		t.Fatalf("RunChat: %v", err)
	}
}

func TestRunChatEOF(t *testing.T) {
	bin := writeMockPi(t, []map[string]interface{}{
		{"type": "agent_end"},
	})

	sm := agent.NewSessionManager(bin, t.TempDir(), 10*time.Minute)
	defer sm.StopAll()

	r, w, _ := os.Pipe()
	oldStdin := os.Stdin
	os.Stdin = r
	defer func() { os.Stdin = oldStdin }()

	oldStdout := os.Stdout
	_, outW, _ := os.Pipe()
	os.Stdout = outW
	defer func() { os.Stdout = oldStdout }()

	// Close immediately — EOF.
	w.Close()

	err := RunChat(context.Background(), sm)
	outW.Close()
	os.Stdout = oldStdout

	if err != nil {
		t.Fatalf("RunChat: %v", err)
	}
}

func TestRunChatSkipsEmpty(t *testing.T) {
	bin := writeMockPi(t, []map[string]interface{}{
		{"type": "agent_end"},
	})

	sm := agent.NewSessionManager(bin, t.TempDir(), 10*time.Minute)
	defer sm.StopAll()

	r, w, _ := os.Pipe()
	oldStdin := os.Stdin
	os.Stdin = r
	defer func() { os.Stdin = oldStdin }()

	oldStdout := os.Stdout
	_, outW, _ := os.Pipe()
	os.Stdout = outW
	defer func() { os.Stdout = oldStdout }()

	go func() {
		// Empty lines followed by quit.
		io.WriteString(w, "\n\n\n/quit\n")
		w.Close()
	}()

	err := RunChat(context.Background(), sm)
	outW.Close()
	os.Stdout = oldStdout

	if err != nil {
		t.Fatalf("RunChat: %v", err)
	}
}
