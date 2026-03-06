package pi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vaayne/anna/agent/runner"
)

// Runner spawns Pi as a local process with --no-session and communicates
// via NDJSON over stdin/stdout. It implements the runner.Runner interface.
type Runner struct {
	binary string
	model  string

	cmd     *exec.Cmd
	stdin   io.WriteCloser
	decoder *json.Decoder
	events  chan *runner.RPCEvent
	done    chan struct{}
	mu      sync.Mutex
	reqID   atomic.Int64

	// For RPC request-response matching.
	pending   map[string]chan *runner.RPCEvent
	pendingMu sync.Mutex

	lastActivity time.Time
	log          *slog.Logger
}

// New creates and starts a Pi process runner.
func New(ctx context.Context, binary, model string) (*Runner, error) {
	r := &Runner{
		binary:       binary,
		model:        model,
		events:       make(chan *runner.RPCEvent, 100),
		done:         make(chan struct{}),
		pending:      make(map[string]chan *runner.RPCEvent),
		lastActivity: time.Now(),
		log:          slog.With("component", "pi_runner"),
	}
	if err := r.start(ctx); err != nil {
		return nil, err
	}
	return r, nil
}

// start spawns the Pi process and begins reading stdout/stderr.
func (r *Runner) start(ctx context.Context) error {
	args := []string{"--mode", "rpc", "--no-session"}
	if r.model != "" {
		args = append(args, "--model", r.model)
	}
	r.cmd = exec.CommandContext(ctx, r.binary, args...)
	r.cmd.Env = append(os.Environ(), "PI_CODING_AGENT_DIR="+filepath.Join(".", ".agents", "pi"))

	var err error
	r.stdin, err = r.cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("stdin pipe: %w", err)
	}

	stdout, err := r.cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}

	stderr, err := r.cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("stderr pipe: %w", err)
	}

	if err := r.cmd.Start(); err != nil {
		return fmt.Errorf("start process: %w", err)
	}

	r.log.Info("process started", "pid", r.cmd.Process.Pid, "binary", r.binary, "model", r.model)

	r.decoder = json.NewDecoder(stdout)

	go r.readStdout()
	go r.readStderr(stderr)
	go r.waitProcess()

	return nil
}

// Chat sends a prompt and streams back events. The history parameter is
// available for future use (e.g. crash recovery replay). Currently Pi
// maintains context in-process.
func (r *Runner) Chat(ctx context.Context, history []runner.RPCEvent, message string) <-chan runner.Event {
	out := make(chan runner.Event, 100)

	r.mu.Lock()
	r.lastActivity = time.Now()
	r.mu.Unlock()

	id := fmt.Sprintf("%d", r.reqID.Add(1))
	cmd := runner.RPCCommand{
		ID:      id,
		Type:    "prompt",
		Message: message,
	}

	r.log.Debug("sending prompt", "request_id", id, "message_len", len(message))

	if err := r.sendCommand(cmd); err != nil {
		go func() {
			out <- runner.Event{Err: fmt.Errorf("send prompt: %w", err)}
			close(out)
		}()
		return out
	}

	go func() {
		defer close(out)
		for {
			select {
			case <-ctx.Done():
				out <- runner.Event{Err: ctx.Err()}
				return
			case <-r.done:
				out <- runner.Event{Err: fmt.Errorf("agent process exited")}
				return
			case evt, ok := <-r.events:
				if !ok {
					out <- runner.Event{Err: fmt.Errorf("event channel closed")}
					return
				}

				switch evt.Type {
				case "message_update":
					if len(evt.AssistantMessageEvent) > 0 {
						var ame runner.AssistantMessageEvent
						if err := json.Unmarshal(evt.AssistantMessageEvent, &ame); err != nil {
							out <- runner.Event{Err: fmt.Errorf("parse assistant event: %w", err)}
							return
						}
						if ame.Type == "text_delta" && ame.Delta != "" {
							out <- runner.Event{Text: ame.Delta}
						}
					}
				case "tool_start":
					out <- runner.Event{ToolUse: &runner.ToolUseEvent{
						Tool:   evt.Tool,
						Status: "running",
						Input:  evt.Summary,
					}}
				case "tool_end":
					status := "done"
					if evt.Error != "" {
						status = "error"
					}
					out <- runner.Event{ToolUse: &runner.ToolUseEvent{
						Tool:   evt.Tool,
						Status: status,
						Input:  evt.Summary,
					}}
				case "error":
					r.log.Error("rpc error", "request_id", id, "error", evt.Error)
					out <- runner.Event{Err: fmt.Errorf("pi error: %s", evt.Error)}
					return
				case "agent_end":
					r.log.Debug("prompt completed", "request_id", id)
					return
				}
			}
		}
	}()

	return out
}

// readStdout decodes NDJSON events from stdout.
func (r *Runner) readStdout() {
	defer close(r.events)

	for {
		var evt runner.RPCEvent
		if err := r.decoder.Decode(&evt); err != nil {
			if err != io.EOF {
				r.log.Warn("stdout decode error", "error", err)
			}
			return
		}

		if evt.Type == "response" && evt.ID != "" {
			r.pendingMu.Lock()
			ch, ok := r.pending[evt.ID]
			if ok {
				delete(r.pending, evt.ID)
			}
			r.pendingMu.Unlock()

			if ok {
				ch <- &evt
				close(ch)
			}
			continue
		}

		r.events <- &evt
	}
}

// readStderr drains stderr and logs any output.
func (r *Runner) readStderr(stderr io.Reader) {
	buf := make([]byte, 4096)
	for {
		n, err := stderr.Read(buf)
		if n > 0 {
			r.log.Warn("stderr output", "text", string(buf[:n]))
		}
		if err != nil {
			return
		}
	}
}

// waitProcess waits for the process to exit, then signals done.
func (r *Runner) waitProcess() {
	err := r.cmd.Wait()
	if err != nil {
		r.log.Warn("process exited with error", "error", err)
	} else {
		r.log.Info("process exited")
	}
	close(r.done)
}

// sendCommand writes a JSON command followed by a newline to stdin.
func (r *Runner) sendCommand(cmd runner.RPCCommand) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	data, err := json.Marshal(cmd)
	if err != nil {
		return fmt.Errorf("marshal command: %w", err)
	}

	data = append(data, '\n')
	if _, err := r.stdin.Write(data); err != nil {
		return fmt.Errorf("write stdin: %w", err)
	}

	return nil
}

// Close gracefully shuts down the Pi process.
func (r *Runner) Close() error {
	r.mu.Lock()
	if r.stdin != nil {
		_ = r.stdin.Close()
	}
	r.mu.Unlock()

	select {
	case <-r.done:
		r.log.Info("runner stopped gracefully")
		return nil
	case <-time.After(5 * time.Second):
		r.log.Warn("runner did not exit in time, force killing")
		if r.cmd != nil && r.cmd.Process != nil {
			return r.cmd.Process.Kill()
		}
		return nil
	}
}

// Alive reports whether the Pi process is still running.
func (r *Runner) Alive() bool {
	select {
	case <-r.done:
		return false
	default:
		return true
	}
}

// LastActivity returns the time of the last Chat call.
func (r *Runner) LastActivity() time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lastActivity
}
