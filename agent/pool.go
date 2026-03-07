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
	factory      runner.NewRunnerFunc
	sessions     map[string]*Session
	store        store.Store
	mu           sync.Mutex
	idleTimeout  time.Duration
	compaction   CompactionConfig
	defaultModel string // default model ID for new runners
	fastModel    string // model ID used for compaction / fast tasks
	log          *slog.Logger
}

// PoolOption configures a Pool.
type PoolOption func(*Pool)

// WithIdleTimeout sets the idle timeout for reaping runners.
func WithIdleTimeout(d time.Duration) PoolOption {
	return func(p *Pool) {
		p.idleTimeout = d
	}
}

// WithCompaction sets the compaction configuration.
func WithCompaction(cfg CompactionConfig) PoolOption {
	return func(p *Pool) {
		p.compaction = cfg
	}
}

// WithDefaultModel sets the default model ID for new runners.
func WithDefaultModel(model string) PoolOption {
	return func(p *Pool) {
		p.defaultModel = model
	}
}

// WithFastModel sets the model ID used for compaction and other fast tasks.
func WithFastModel(model string) PoolOption {
	return func(p *Pool) {
		p.fastModel = model
	}
}

// WithStore sets the persistent store for session history.
func WithStore(s store.Store) PoolOption {
	return func(p *Pool) {
		p.store = s
	}
}

// ChatOption configures a single Chat call.
type ChatOption func(*chatOptions)

type chatOptions struct {
	model string
}

// WithModel overrides the model for this Chat call. If the session already
// has a runner with a different model, the runner is replaced.
func WithModel(model string) ChatOption {
	return func(o *chatOptions) {
		o.model = model
	}
}

// NewPool creates a new Pool with the given runner factory.
func NewPool(factory runner.NewRunnerFunc, opts ...PoolOption) *Pool {
	p := &Pool{
		factory:      factory,
		sessions:     make(map[string]*Session),
		idleTimeout:  10 * time.Minute,
		log:          slog.With("component", "pool"),
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

// compactionPrompt is sent to the runner to generate a conversation summary
// that replaces old history. Based on the handoff pattern — the summary must
// be self-contained so the runner can continue without the original context.
const compactionPrompt = `Summarize the conversation so far so a fresh context window can continue the work. Use this structure (skip empty sections):

## Goal
[Original objective of this session]

## Progress
- [What was completed]
- [What was partially done]

## Key Decisions
- [Decision and why]

## Files Changed
- ` + "`path/to/file`" + ` — [what changed]

## Current State
[Where things stand — what works, what doesn't]

## Blockers / Gotchas
- [Issues, edge cases, or warnings]

## Next Steps
1. [Concrete next action]
2. [Follow-up action]

Guidelines:
- Be self-contained — the reader has NO access to the previous conversation.
- Be concise — only what's relevant. Skip empty sections.
- Focus on decisions and rationale, not the discussion that led to them.
- List concrete file paths with context, not just paths.
- State next steps as actionable tasks — clear enough to execute immediately.
- Do NOT use tools or ask questions. Just output the summary.`

// CompactSession summarizes the conversation via the runner, rewrites the
// session file with a compaction entry + recent messages, and restarts the
// runner so it picks up the clean context.
//
// It returns the summary text on success.
func (p *Pool) CompactSession(ctx context.Context, sessionID string) (string, error) {
	if p.store == nil {
		return "", fmt.Errorf("compaction requires a persistent store")
	}

	// Remember the session's original model so we can restore it after
	// compaction — the fast model is only for generating the summary.
	p.mu.Lock()
	sess, ok := p.sessions[sessionID]
	origModel := ""
	if ok {
		origModel = sess.Model
	}
	p.mu.Unlock()

	sess, r, err := p.getOrCreateRunner(ctx, sessionID, p.fastModel)
	if err != nil {
		return "", fmt.Errorf("get runner: %w", err)
	}

	// If the session was new (no prior model), fall back to the pool default.
	if origModel == "" {
		origModel = p.defaultModel
	}

	p.mu.Lock()
	events := make([]runner.RPCEvent, len(sess.Events))
	copy(events, sess.Events)
	p.mu.Unlock()

	p.log.Info("compaction started", "session_id", sessionID, "events", len(events))

	// Ask the runner to summarize the conversation.
	summary, err := p.collectFullResponse(ctx, r, events, compactionPrompt)
	if err != nil {
		return "", fmt.Errorf("generate summary: %w", err)
	}

	// Rewrite the session file with compaction.
	keepTail := p.compaction.KeepTail
	if keepTail == 0 {
		keepTail = 20
	}
	newEvents, err := p.store.Compact(sessionID, summary, keepTail)
	if err != nil {
		return "", fmt.Errorf("compact store: %w", err)
	}

	// Replace in-memory events.
	p.mu.Lock()
	sess.Events = newEvents
	p.mu.Unlock()

	// Kill the runner so it restarts with clean context on next Chat() —
	// unless the runner is stateful (maintains its own in-process context),
	// in which case killing it would lose context for no benefit. The
	// compacted history is persisted to disk for crash recovery either way.
	if sf, ok := r.(runner.Stateful); !ok || !sf.Stateful() {
		if closer, ok := r.(io.Closer); ok {
			_ = closer.Close()
		}
		p.mu.Lock()
		sess.Runner = nil
		sess.Model = origModel // restore so next Chat uses the original model
		p.mu.Unlock()
	}

	p.log.Info("compaction complete", "session_id", sessionID,
		"summary_len", len(summary), "new_events", len(newEvents))

	return summary, nil
}

// collectFullResponse sends a message to a runner and collects the complete
// text response, blocking until the stream ends.
func (p *Pool) collectFullResponse(ctx context.Context, r runner.Runner, history []runner.RPCEvent, message string) (string, error) {
	stream := r.Chat(ctx, history, message)
	var buf strings.Builder
	for evt := range stream {
		if evt.Err != nil {
			return buf.String(), evt.Err
		}
		if evt.Text != "" {
			buf.WriteString(evt.Text)
		}
	}
	if buf.Len() == 0 {
		return "", fmt.Errorf("empty summary response")
	}
	return buf.String(), nil
}

// NeedsCompaction reports whether a session's estimated token count exceeds
// the compaction threshold. Returns false if compaction is disabled or no
// store is set.
func (p *Pool) NeedsCompaction(sessionID string) bool {
	if p.store == nil || p.compaction.MaxTokens <= 0 {
		return false
	}
	tokens, err := p.store.EstimateTokens(sessionID)
	if err != nil {
		p.log.Warn("failed to estimate tokens", "session_id", sessionID, "error", err)
		return false
	}
	return tokens > p.compaction.MaxTokens
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
func (p *Pool) Chat(ctx context.Context, sessionID string, message string, opts ...ChatOption) <-chan runner.Event {
	out := make(chan runner.Event, 100)

	var co chatOptions
	for _, o := range opts {
		o(&co)
	}

	sess, r, err := p.getOrCreateRunner(ctx, sessionID, co.model)
	if err != nil {
		go func() {
			out <- runner.Event{Err: fmt.Errorf("get runner: %w", err)}
			close(out)
		}()
		return out
	}

	p.log.Debug("chat started", "session_id", sessionID, "history_len", len(sess.Events), "message_len", len(message))

	// Auto-compact if the session has grown too large.
	if p.NeedsCompaction(sessionID) {
		p.log.Info("auto-compaction triggered", "session_id", sessionID)
		if summary, err := p.CompactSession(ctx, sessionID); err != nil {
			p.log.Warn("auto-compaction failed, continuing with full history",
				"session_id", sessionID, "error", err)
		} else {
			p.log.Info("auto-compaction succeeded", "session_id", sessionID,
				"summary_len", len(summary))
			// Re-acquire session and runner after compaction (runner was restarted).
			sess, r, err = p.getOrCreateRunner(ctx, sessionID, co.model)
			if err != nil {
				go func() {
					out <- runner.Event{Err: fmt.Errorf("get runner after compaction: %w", err)}
					close(out)
				}()
				return out
			}
		}
	}

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

// SetDefaultModel updates the default model used for new runners.
// Call this alongside SetFactory when the user switches models at runtime.
func (p *Pool) SetDefaultModel(model string) {
	p.mu.Lock()
	p.defaultModel = model
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
// If model is non-empty and differs from the session's current model, the
// existing runner is replaced.
func (p *Pool) getOrCreateRunner(ctx context.Context, sessionID string, model string) (*Session, runner.Runner, error) {
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
		// If a specific model was requested and it differs from the session's
		// current model, replace the runner.
		if model != "" && sess.Model != model {
			p.log.Info("switching model", "session_id", sessionID, "from", sess.Model, "to", model)
			if closer, isCloser := sess.Runner.(io.Closer); isCloser {
				_ = closer.Close()
			}
			sess.Runner = nil
		} else {
			p.mu.Unlock()
			return sess, sess.Runner, nil
		}
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

	// Resolve the model: explicit > session's current > pool default.
	effectiveModel := model
	if effectiveModel == "" {
		effectiveModel = sess.Model
	}
	if effectiveModel == "" {
		effectiveModel = p.defaultModel
	}

	r, err := p.factory(ctx, effectiveModel)
	if err != nil {
		return nil, nil, err
	}

	p.mu.Lock()
	sess.Runner = r
	sess.Model = effectiveModel
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
