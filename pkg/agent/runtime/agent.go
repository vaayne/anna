package runtime

import (
	"context"
	"errors"
	"sync"

	"github.com/vaayne/anna/pkg/agent/core"
	agenttypes "github.com/vaayne/anna/pkg/agent/types"
	aitypes "github.com/vaayne/anna/pkg/ai/types"
)

// Agent is a stateful runtime wrapper around core.Engine.
type Agent struct {
	mu      sync.RWMutex
	history []aitypes.Message
	events  chan agenttypes.Event
	done    chan struct{}
	abort   chan struct{}
	engine  *core.Engine
	cfg     agenttypes.Config
	waitErr error
}

// New creates an agent runtime.
func New(engine *core.Engine, cfg agenttypes.Config) *Agent {
	if cfg.Tools == nil {
		cfg.Tools = agenttypes.ToolSet{}
	}
	abort := make(chan struct{})
	cfg.Interrupt = abort
	return &Agent{
		history: make([]aitypes.Message, 0, 16),
		events:  make(chan agenttypes.Event, 64),
		done:    make(chan struct{}),
		abort:   abort,
		engine:  engine,
		cfg:     cfg,
	}
}

// Subscribe returns runtime event stream.
func (a *Agent) Subscribe() <-chan agenttypes.Event { return a.events }

// Prompt appends a user prompt and runs loop.
func (a *Agent) Prompt(ctx context.Context, prompt string) error {
	a.mu.Lock()
	a.history = append(a.history, aitypes.UserMessage{Content: prompt})
	history := append([]aitypes.Message(nil), a.history...)
	a.mu.Unlock()

	next, err := a.engine.Run(ctx, a.cfg, history, a.emit)
	a.mu.Lock()
	defer a.mu.Unlock()
	if err != nil {
		a.waitErr = err
		close(a.done)
		close(a.events)
		return err
	}
	a.history = next
	close(a.done)
	close(a.events)
	return nil
}

// Continue continues from current history.
func (a *Agent) Continue(ctx context.Context) error {
	a.mu.RLock()
	history := append([]aitypes.Message(nil), a.history...)
	a.mu.RUnlock()

	next, err := a.engine.Continue(ctx, a.cfg, history, a.emit)
	a.mu.Lock()
	defer a.mu.Unlock()
	if err != nil {
		a.waitErr = err
		return err
	}
	a.history = next
	return nil
}

// Abort requests interruption.
func (a *Agent) Abort() { close(a.abort) }

// Wait waits for prompt completion.
func (a *Agent) Wait() error {
	<-a.done
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.waitErr
}

// History returns a snapshot of transcript.
func (a *Agent) History() []aitypes.Message {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return append([]aitypes.Message(nil), a.history...)
}

func (a *Agent) emit(event agenttypes.Event) {
	select {
	case a.events <- event:
	default:
	}
}

// Validate ensures runtime has required dependencies.
func (a *Agent) Validate() error {
	if a.engine == nil {
		return errors.New("engine required")
	}
	if a.cfg.Model.API == "" {
		return errors.New("model api required")
	}
	return nil
}
