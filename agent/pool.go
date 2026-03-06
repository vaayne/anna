package agent

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/vaayne/anna/agent/runner"
	"github.com/vaayne/anna/agent/store"
)

// Pool manages a set of sessions, each with its own history and runner.
// It is the only type channels interact with.
type Pool struct {
	factory     runner.NewRunnerFunc
	sessions    map[string]*Session
	store       store.Store
	mu          sync.Mutex
	idleTimeout time.Duration
	log         *slog.Logger
}

// PoolOption configures a Pool.
type PoolOption func(*Pool)

// WithIdleTimeout sets the idle timeout for reaping runners.
func WithIdleTimeout(d time.Duration) PoolOption {
	return func(p *Pool) {
		p.idleTimeout = d
	}
}

// WithStore sets the persistent store for session history.
func WithStore(s store.Store) PoolOption {
	return func(p *Pool) {
		p.store = s
	}
}

// NewPool creates a new Pool with the given runner factory.
func NewPool(factory runner.NewRunnerFunc, opts ...PoolOption) *Pool {
	p := &Pool{
		factory:     factory,
		sessions:    make(map[string]*Session),
		idleTimeout: 10 * time.Minute,
		log:         slog.With("component", "pool"),
	}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// Chat sends a message in a session and streams back events.
// Internally: gets/creates runner, passes history, collects events,
// appends to session log, streams to caller.
func (p *Pool) Chat(ctx context.Context, sessionID string, message string) <-chan runner.Event {
	out := make(chan runner.Event, 100)

	sess, r, err := p.getOrCreateRunner(ctx, sessionID)
	if err != nil {
		go func() {
			out <- runner.Event{Err: fmt.Errorf("get runner: %w", err)}
			close(out)
		}()
		return out
	}

	p.log.Debug("chat started", "session_id", sessionID, "history_len", len(sess.Events), "message_len", len(message))

	// Store user message so stateless runners can reconstruct the conversation.
	userEvt := runner.RPCEvent{Type: "user_message", Summary: message}
	p.mu.Lock()
	sess.Events = append(sess.Events, userEvt)
	p.mu.Unlock()
	p.persist(sessionID, userEvt)

	stream := r.Chat(ctx, sess.Events, message)

	go func() {
		defer close(out)
		var textBuf strings.Builder
		for evt := range stream {
			if evt.Err != nil {
				// Persist any buffered text before returning on error.
				if textBuf.Len() > 0 {
					p.persist(sessionID, runner.AssistantMessageToRPCEvent(textBuf.String()))
				}
				out <- evt
				return
			}

			// Store events emitted by runners (tool calls, tool results, text deltas).
			if evt.Store != nil {
				// Flush buffered text before storing a non-text event.
				if textBuf.Len() > 0 {
					p.persist(sessionID, runner.AssistantMessageToRPCEvent(textBuf.String()))
					textBuf.Reset()
				}
				p.mu.Lock()
				sess.Events = append(sess.Events, *evt.Store)
				p.mu.Unlock()
				p.persist(sessionID, *evt.Store)
			}

			// Tool-use events pass through without history storage.
			if evt.ToolUse != nil {
				out <- evt
				continue
			}

			// Text delta: store in memory for the runner, buffer for persistence.
			if evt.Text != "" {
				rpcEvt := runner.TextDeltaToRPCEvent(evt.Text)
				p.mu.Lock()
				sess.Events = append(sess.Events, rpcEvt)
				p.mu.Unlock()
				textBuf.WriteString(evt.Text)
			}

			out <- evt
		}
		// Stream ended normally — persist the complete assistant message.
		if textBuf.Len() > 0 {
			p.persist(sessionID, runner.AssistantMessageToRPCEvent(textBuf.String()))
		}
	}()

	return out
}

// Reset clears session history and closes the current runner.
func (p *Pool) Reset(sessionID string) error {
	p.mu.Lock()
	sess, ok := p.sessions[sessionID]
	if !ok {
		p.mu.Unlock()
		if p.store != nil {
			return p.store.Delete(sessionID)
		}
		return nil
	}
	r := sess.Runner
	delete(p.sessions, sessionID)
	p.mu.Unlock()

	if p.store != nil {
		if err := p.store.Delete(sessionID); err != nil {
			p.log.Warn("failed to delete persisted session", "session_id", sessionID, "error", err)
		}
	}

	if closer, ok := r.(io.Closer); ok {
		return closer.Close()
	}
	return nil
}

// SetFactory replaces the runner factory used for new runners.
// Existing runners are not affected until their session is reset.
func (p *Pool) SetFactory(factory runner.NewRunnerFunc) {
	p.mu.Lock()
	p.factory = factory
	p.mu.Unlock()
}

// Close shuts down all sessions and runners.
func (p *Pool) Close() error {
	p.mu.Lock()
	sessions := p.sessions
	p.sessions = make(map[string]*Session)
	p.mu.Unlock()

	var lastErr error
	for id, sess := range sessions {
		p.log.Info("closing session", "session_id", id)
		if closer, ok := sess.Runner.(io.Closer); ok {
			if err := closer.Close(); err != nil {
				lastErr = err
			}
		}
	}
	return lastErr
}

// StartReaper runs a background goroutine that periodically checks for
// idle or dead runners. It returns when ctx is cancelled.
func (p *Pool) StartReaper(ctx context.Context) {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.reap()
		}
	}
}

// getOrCreateRunner returns the session and its runner, creating both if needed.
// If the session is not in memory but exists on disk, its history is restored.
func (p *Pool) getOrCreateRunner(ctx context.Context, sessionID string) (*Session, runner.Runner, error) {
	p.mu.Lock()
	sess, ok := p.sessions[sessionID]
	if ok && sess.Runner != nil {
		// Check if the runner is still alive (for runners that support liveness).
		if aliver, isAliver := sess.Runner.(runner.Aliver); isAliver && !aliver.Alive() {
			p.log.Warn("replacing dead runner", "session_id", sessionID)
			if closer, isCloser := sess.Runner.(io.Closer); isCloser {
				_ = closer.Close()
			}
			sess.Runner = nil
		}
	}
	if ok && sess.Runner != nil {
		p.mu.Unlock()
		return sess, sess.Runner, nil
	}
	if !ok {
		sess = &Session{}
		p.sessions[sessionID] = sess

		// Restore history from disk if available.
		if p.store != nil {
			p.mu.Unlock()
			events, err := p.store.Load(sessionID)
			p.mu.Lock()
			if err != nil {
				p.log.Warn("failed to load persisted session", "session_id", sessionID, "error", err)
			} else if len(events) > 0 {
				sess.Events = events
				p.log.Info("restored session from disk", "session_id", sessionID, "events", len(events))
			}
		}
	}
	p.mu.Unlock()

	r, err := p.factory(ctx)
	if err != nil {
		return nil, nil, err
	}

	p.mu.Lock()
	sess.Runner = r
	p.mu.Unlock()

	p.log.Info("created runner", "session_id", sessionID)
	return sess, r, nil
}

// persist appends events to the store if one is configured.
func (p *Pool) persist(sessionID string, events ...runner.RPCEvent) {
	if p.store == nil {
		return
	}
	if err := p.store.Append(sessionID, events...); err != nil {
		p.log.Warn("failed to persist event", "session_id", sessionID, "error", err)
	}
}

// reap checks all sessions and closes runners that are idle or dead.
func (p *Pool) reap() {
	p.mu.Lock()
	defer p.mu.Unlock()

	now := time.Now()
	for id, sess := range p.sessions {
		if sess.Runner == nil {
			continue
		}

		aliver, isAliver := sess.Runner.(runner.Aliver)
		tracker, isTracker := sess.Runner.(runner.ActivityTracker)

		if !isAliver {
			continue
		}

		if !aliver.Alive() {
			p.log.Warn("removing dead runner", "session_id", id)
			sess.Runner = nil
			continue
		}

		if isTracker && now.Sub(tracker.LastActivity()) > p.idleTimeout {
			p.log.Info("reaping idle runner", "session_id", id, "idle_duration", now.Sub(tracker.LastActivity()).Round(time.Second))
			if closer, isCloser := sess.Runner.(io.Closer); isCloser {
				_ = closer.Close()
			}
			sess.Runner = nil
		}
	}
}
