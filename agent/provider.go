package agent

import "context"

// SessionProvider is the interface that channels use to interact with agent sessions.
type SessionProvider interface {
	GetOrCreate(ctx context.Context, sessionID string) (*Agent, error)
	NewSession(sessionID string) error
}
