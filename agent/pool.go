package agent

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
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

// CreateSession creates a new session with a generated ID and persists its metadata.
func (p *Pool) CreateSession() (SessionInfo, error) {
	now := time.Now()
	info := SessionInfo{
		ID:         uuid.New().String()[:8],
		CreatedAt:  now,
		LastActive: now,
	}

	p.mu.Lock()
	p.sessions[info.ID] = &Session{Info: info}
	p.mu.Unlock()

	if p.store != nil {
		if err := p.store.SaveInfo(info); err != nil {
			return info, fmt.Errorf("persist session info: %w", err)
		}
	}

	p.log.Info("session created", "session_id", info.ID)
	return info, nil
}

// GetSession returns metadata for a session.
func (p *Pool) GetSession(sessionID string) (SessionInfo, error) {
	p.mu.Lock()
	sess, ok := p.sessions[sessionID]
	p.mu.Unlock()
	if ok {
		return sess.Info, nil
	}

	if p.store != nil {
		si, err := p.store.LoadInfo(sessionID)
		if err == nil {
			return si, nil
		}
	}
	return SessionInfo{}, fmt.Errorf("session %q not found", sessionID)
}

// ListSessions returns metadata for all sessions.
func (p *Pool) ListSessions(includeArchived bool) ([]SessionInfo, error) {
	if p.store == nil {
		p.mu.Lock()
		defer p.mu.Unlock()
		result := make([]SessionInfo, 0, len(p.sessions))
		for _, sess := range p.sessions {
			if !includeArchived && sess.Info.Archived {
				continue
			}
			result = append(result, sess.Info)
		}
		return result, nil
	}

	items, err := p.store.ListInfo(includeArchived)
	if err != nil {
		return nil, err
	}
	result := make([]SessionInfo, len(items))
	for i, si := range items {
		result[i] = si
	}
	return result, nil
}

// ArchiveSession marks a session as archived, closes its runner, but keeps history on disk.
// The session is removed from the in-memory map; its metadata persists in the index.
func (p *Pool) ArchiveSession(sessionID string) error {
	p.mu.Lock()
	sess, ok := p.sessions[sessionID]
	var r runner.Runner
	if ok {
		r = sess.Runner
		delete(p.sessions, sessionID)
	}
	p.mu.Unlock()

	if p.store != nil {
		info, err := p.store.LoadInfo(sessionID)
		if err == nil {
			info.Archived = true
			if err := p.store.SaveInfo(info); err != nil {
				p.log.Warn("failed to persist archive", "session_id", sessionID, "error", err)
			}
		}
	}

	if r != nil {
		if closer, ok := r.(io.Closer); ok {
			return closer.Close()
		}
	}

	p.log.Info("session archived", "session_id", sessionID)
	return nil
}

// History returns the event log for a session, loading from disk if needed.
// Returns nil if the session has no history.
func (p *Pool) History(sessionID string) []runner.RPCEvent {
	p.mu.Lock()
	sess, ok := p.sessions[sessionID]
	if ok && len(sess.Events) > 0 {
		events := make([]runner.RPCEvent, len(sess.Events))
		copy(events, sess.Events)
		p.mu.Unlock()
		return events
	}
	p.mu.Unlock()

	if p.store != nil {
		events, err := p.store.Load(sessionID)
		if err == nil && len(events) > 0 {
			return events
		}
	}
	return nil
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

	// Update last active timestamp.
	now := time.Now()
	p.mu.Lock()
	sess.Info.LastActive = now
	p.mu.Unlock()
	p.touchLastActive(sessionID, now)

	// Store user message so stateless runners can reconstruct the conversation.
	userEvt := runner.RPCEvent{Type: "user_message", Summary: message}
	p.mu.Lock()
	sess.Events = append(sess.Events, userEvt)
	// Auto-title: use the first user message as the session title.
	if sess.Info.Title == "" && len(message) > 0 {
		title := message
		if len(title) > 60 {
			// Truncate at word boundary.
			if idx := strings.LastIndex(title[:60], " "); idx > 20 {
				title = title[:idx] + "…"
			} else {
				title = title[:60] + "…"
			}
		}
		sess.Info.Title = title
		p.saveInfo(sess.Info)
	}
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

// Reset archives the session and removes it from memory.
// For backward compatibility — prefer ArchiveSession for new code.
func (p *Pool) Reset(sessionID string) error {
	return p.ArchiveSession(sessionID)
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

		// Restore metadata from index if available.
		if p.store != nil {
			if info, err := p.store.LoadInfo(sessionID); err == nil {
				sess.Info = SessionInfo(info)
			} else {
				sess.Info = SessionInfo{ID: sessionID, CreatedAt: time.Now(), LastActive: time.Now()}
			}
		} else {
			sess.Info = SessionInfo{ID: sessionID, CreatedAt: time.Now(), LastActive: time.Now()}
		}

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

// saveInfo persists session metadata. Caller must hold p.mu.
func (p *Pool) saveInfo(info SessionInfo) {
	if p.store == nil {
		return
	}
	if err := p.store.SaveInfo(info); err != nil {
		p.log.Warn("failed to persist session info", "session_id", info.ID, "error", err)
	}
}

// touchLastActive updates the last active timestamp in the index.
func (p *Pool) touchLastActive(sessionID string, t time.Time) {
	if p.store == nil {
		return
	}
	info, err := p.store.LoadInfo(sessionID)
	if err != nil {
		return
	}
	info.LastActive = t
	if err := p.store.SaveInfo(info); err != nil {
		p.log.Warn("failed to update last active", "session_id", sessionID, "error", err)
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
