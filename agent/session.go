package agent

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// SessionManager manages a pool of Agent instances keyed by session ID.
type SessionManager struct {
	binary      string
	sessionsDir string
	idleTimeout time.Duration
	agents      map[string]*Agent
	mu          sync.Mutex
}

// NewSessionManager creates a new SessionManager.
func NewSessionManager(binary, sessionsDir string, idleTimeout time.Duration) *SessionManager {
	return &SessionManager{
		binary:      binary,
		sessionsDir: sessionsDir,
		idleTimeout: idleTimeout,
		agents:      make(map[string]*Agent),
	}
}

// GetOrCreate returns an existing live agent for the given session ID,
// or creates and starts a new one. The session directory is created if needed.
func (sm *SessionManager) GetOrCreate(ctx context.Context, sessionID string) (*Agent, error) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	if ag, ok := sm.agents[sessionID]; ok {
		if ag.Alive() {
			return ag, nil
		}
		// Dead agent — clean up and fall through to create a new one.
		_ = ag.Stop()
		delete(sm.agents, sessionID)
	}

	sessionPath := filepath.Join(sm.sessionsDir, sessionID)
	if err := os.MkdirAll(sessionPath, 0o755); err != nil {
		return nil, fmt.Errorf("create session dir: %w", err)
	}

	ag := NewAgent(sm.binary, sessionPath)
	if err := ag.Start(ctx); err != nil {
		return nil, fmt.Errorf("start agent for session %q: %w", sessionID, err)
	}

	sm.agents[sessionID] = ag
	return ag, nil
}

// StopAll stops every managed agent.
func (sm *SessionManager) StopAll() {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	for id, ag := range sm.agents {
		log.Printf("session: stopping agent %q", id)
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
			log.Printf("session: removing dead agent %q", id)
			delete(sm.agents, id)
			continue
		}
		if now.Sub(ag.LastActivity()) > sm.idleTimeout {
			log.Printf("session: reaping idle agent %q (idle %s)", id, now.Sub(ag.LastActivity()).Round(time.Second))
			_ = ag.Stop()
			delete(sm.agents, id)
		}
	}
}
