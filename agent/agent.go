package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

// RPCCommand is sent to Pi's stdin as NDJSON.
type RPCCommand struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Message string `json:"message,omitempty"`
}

// RPCEvent is received from Pi's stdout as NDJSON.
type RPCEvent struct {
	Type                  string          `json:"type"`
	AssistantMessageEvent json.RawMessage `json:"assistantMessageEvent,omitempty"`
	ID                    string          `json:"id,omitempty"`
	Result                json.RawMessage `json:"result,omitempty"`
	Error                 string          `json:"error,omitempty"`
	Tool                  string          `json:"tool,omitempty"`
	Summary               string          `json:"summary,omitempty"`
}

// assistantMessageEvent represents the inner event for text deltas.
type assistantMessageEvent struct {
	Type  string `json:"type"`
	Delta string `json:"delta"`
}

// StreamEvent is emitted to consumers of SendPrompt.
type StreamEvent struct {
	Text string
	Err  error
}

// Agent manages a single Pi RPC process.
type Agent struct {
	binary      string
	model       string
	sessionFile string

	cmd     *exec.Cmd
	stdin   io.WriteCloser
	decoder *json.Decoder
	events  chan *RPCEvent
	done    chan struct{}
	mu      sync.Mutex
	reqID   atomic.Int64

	// For RPC request-response matching.
	pending   map[string]chan *RPCEvent
	pendingMu sync.Mutex

	lastActivity time.Time
}

// NewAgent creates a new Agent but does not start the process.
func NewAgent(binary string, model string, sessionFile string) *Agent {
	return &Agent{
		binary:       binary,
		model:        model,
		sessionFile:  sessionFile,
		events:       make(chan *RPCEvent, 100),
		done:         make(chan struct{}),
		pending:      make(map[string]chan *RPCEvent),
		lastActivity: time.Now(),
	}
}

// Start spawns the Pi process and begins reading stdout/stderr.
func (a *Agent) Start(ctx context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	args := []string{"--mode", "rpc", "--session", a.sessionFile}
	if a.model != "" {
		args = append(args, "--model", a.model)
	}
	a.cmd = exec.CommandContext(ctx, a.binary, args...)
	a.cmd.Env = append(os.Environ(), "PI_CODING_AGENT_DIR="+filepath.Join(".", ".agents", "pi"))

	var err error
	a.stdin, err = a.cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("stdin pipe: %w", err)
	}

	stdout, err := a.cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}

	stderr, err := a.cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("stderr pipe: %w", err)
	}

	if err := a.cmd.Start(); err != nil {
		return fmt.Errorf("start process: %w", err)
	}

	a.decoder = json.NewDecoder(stdout)

	go a.readStdout()
	go a.readStderr(stderr)
	go a.waitProcess()

	return nil
}

// readStdout decodes NDJSON events from stdout.
// "response" events are routed to the pending request channel;
// all other events are sent to the shared events channel.
func (a *Agent) readStdout() {
	defer close(a.events)

	for {
		var evt RPCEvent
		if err := a.decoder.Decode(&evt); err != nil {
			return
		}

		if evt.Type == "response" && evt.ID != "" {
			a.pendingMu.Lock()
			ch, ok := a.pending[evt.ID]
			if ok {
				delete(a.pending, evt.ID)
			}
			a.pendingMu.Unlock()

			if ok {
				ch <- &evt
				close(ch)
			}
			continue
		}

		a.events <- &evt
	}
}

// readStderr drains stderr to prevent pipe deadlock.
func (a *Agent) readStderr(stderr io.Reader) {
	buf := make([]byte, 4096)
	for {
		_, err := stderr.Read(buf)
		if err != nil {
			return
		}
	}
}

// waitProcess waits for the process to exit, then signals done.
func (a *Agent) waitProcess() {
	_ = a.cmd.Wait()
	close(a.done)
}

// SendPrompt sends a prompt to Pi and returns a channel of StreamEvents.
// The channel is closed when the agent_end event is received or an error occurs.
func (a *Agent) SendPrompt(ctx context.Context, message string) <-chan StreamEvent {
	out := make(chan StreamEvent, 100)

	a.mu.Lock()
	a.lastActivity = time.Now()
	a.mu.Unlock()

	id := fmt.Sprintf("%d", a.reqID.Add(1))
	cmd := RPCCommand{
		ID:      id,
		Type:    "prompt",
		Message: message,
	}

	if err := a.sendCommand(cmd); err != nil {
		go func() {
			out <- StreamEvent{Err: fmt.Errorf("send prompt: %w", err)}
			close(out)
		}()
		return out
	}

	go func() {
		defer close(out)
		for {
			select {
			case <-ctx.Done():
				out <- StreamEvent{Err: ctx.Err()}
				return
			case <-a.done:
				out <- StreamEvent{Err: fmt.Errorf("agent process exited")}
				return
			case evt, ok := <-a.events:
				if !ok {
					out <- StreamEvent{Err: fmt.Errorf("event channel closed")}
					return
				}

				switch evt.Type {
				case "message_update":
					if len(evt.AssistantMessageEvent) > 0 {
						var ame assistantMessageEvent
						if err := json.Unmarshal(evt.AssistantMessageEvent, &ame); err != nil {
							out <- StreamEvent{Err: fmt.Errorf("parse assistant event: %w", err)}
							return
						}
						if ame.Type == "text_delta" && ame.Delta != "" {
							out <- StreamEvent{Text: ame.Delta}
						}
					}
				case "error":
					out <- StreamEvent{Err: fmt.Errorf("pi error: %s", evt.Error)}
					return
				case "agent_end":
					return
				}
			}
		}
	}()

	return out
}

// sendCommand writes a JSON command followed by a newline to stdin.
func (a *Agent) sendCommand(cmd RPCCommand) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	data, err := json.Marshal(cmd)
	if err != nil {
		return fmt.Errorf("marshal command: %w", err)
	}

	data = append(data, '\n')
	if _, err := a.stdin.Write(data); err != nil {
		return fmt.Errorf("write stdin: %w", err)
	}

	return nil
}

// Stop gracefully shuts down the Pi process.
// It closes stdin and waits up to 5 seconds for the process to exit,
// then force-kills it if necessary.
func (a *Agent) Stop() error {
	a.mu.Lock()
	if a.stdin != nil {
		_ = a.stdin.Close()
	}
	a.mu.Unlock()

	select {
	case <-a.done:
		return nil
	case <-time.After(5 * time.Second):
		if a.cmd != nil && a.cmd.Process != nil {
			return a.cmd.Process.Kill()
		}
		return nil
	}
}

// Alive reports whether the Pi process is still running.
func (a *Agent) Alive() bool {
	select {
	case <-a.done:
		return false
	default:
		return true
	}
}

// LastActivity returns the time of the last SendPrompt call.
func (a *Agent) LastActivity() time.Time {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.lastActivity
}
