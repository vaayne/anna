package agent

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// SessionManager manages a pool of Agent instances keyed by session ID.
type SessionManager struct {
	binary      string
	model       string
	sessionsDir string
	idleTimeout time.Duration
	agents      map[string]*Agent
	mu          sync.Mutex
	log         *slog.Logger
}

// NewSessionManager creates a new SessionManager.
func NewSessionManager(binary, model, sessionsDir string, idleTimeout time.Duration) *SessionManager {
	return &SessionManager{
		binary:      binary,
		model:       model,
		sessionsDir: sessionsDir,
		idleTimeout: idleTimeout,
		agents:      make(map[string]*Agent),
		log:         slog.With("component", "session"),
	}
}

// sessionFile returns the path to the session file for the given session ID.
func (sm *SessionManager) sessionFile(sessionID string) string {
	return filepath.Join(sm.sessionsDir, fmt.Sprintf("%s.jsonl", sessionID))
}

// GetOrCreate returns an existing live agent for the given session ID,
// or creates and starts a new one. The session directory is created if needed.
func (sm *SessionManager) GetOrCreate(ctx context.Context, sessionID string) (*Agent, error) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	if ag, ok := sm.agents[sessionID]; ok {
		if ag.Alive() {
			sm.log.Debug("returning existing agent", "session_id", sessionID)
			return ag, nil
		}
		// Dead agent — clean up and fall through to create a new one.
		sm.log.Warn("replacing dead agent", "session_id", sessionID)
		_ = ag.Stop()
		delete(sm.agents, sessionID)
	}

	sessionFile := sm.sessionFile(sessionID)
	if err := os.MkdirAll(filepath.Dir(sessionFile), 0o755); err != nil {
		return nil, fmt.Errorf("create session dir: %w", err)
	}

	ag := NewAgent(sm.binary, sm.model, sessionFile)
	if err := ag.Start(ctx); err != nil {
		return nil, fmt.Errorf("start agent for session %q: %w", sessionID, err)
	}

	sm.log.Info("created new agent", "session_id", sessionID)
	sm.agents[sessionID] = ag
	return ag, nil
}

// NewSession stops the current agent for the given session ID, backs up the
// existing session file to a timestamped name, and removes the agent from the
// pool so the next GetOrCreate call starts a fresh session.
func (sm *SessionManager) NewSession(sessionID string) error {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	if ag, ok := sm.agents[sessionID]; ok {
		_ = ag.Stop()
		delete(sm.agents, sessionID)
	}

	sessionFile := sm.sessionFile(sessionID)
	if _, err := os.Stat(sessionFile); err == nil {
		backup := filepath.Join(
			filepath.Dir(sessionFile),
			time.Now().Format("20060102-150405")+".jsonl",
		)
		if err := os.Rename(sessionFile, backup); err != nil {
			return fmt.Errorf("backup session file: %w", err)
		}
		sm.log.Info("backed up session", "session_id", sessionID, "from", sessionFile, "to", backup)
	}

	return nil
}

// StopAll stops every managed agent.
func (sm *SessionManager) StopAll() {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	for id, ag := range sm.agents {
		sm.log.Info("stopping agent", "session_id", id)
		_ = ag.Stop()
	}
	sm.agents = make(map[string]*Agent)
}

// StartReaper runs a background goroutine that periodically stops agents
// that have been idle longer than the configured timeout.
// It returns when ctx is cancelled.
func (sm *SessionManager) StartReaper(ctx context.Context) {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sm.reap()
		}
	}
}

// reap checks all agents and stops those that have exceeded the idle timeout.
func (sm *SessionManager) reap() {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	now := time.Now()
	for id, ag := range sm.agents {
		if !ag.Alive() {
			sm.log.Warn("removing dead agent", "session_id", id)
			delete(sm.agents, id)
			continue
		}
		if now.Sub(ag.LastActivity()) > sm.idleTimeout {
			sm.log.Info("reaping idle agent", "session_id", id, "idle_duration", now.Sub(ag.LastActivity()).Round(time.Second))
			_ = ag.Stop()
			delete(sm.agents, id)
		}
	}
}
