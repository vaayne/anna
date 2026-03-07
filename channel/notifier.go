package channel

import (
	"context"
	"fmt"
	"sync"
)

// Notification represents a message to push to a chat or channel.
type Notification struct {
	ChatID string // target chat/channel ID
	Text   string // markdown content
	Silent bool   // send without notification sound
}

// Notifier can push messages to users proactively.
type Notifier interface {
	Notify(ctx context.Context, n Notification) error
}

// NotifierProxy is a Notifier whose backend can be set after construction.
// This solves ordering issues where the notifier tool must be created before
// the actual Notifier implementation (e.g., the Telegram bot) exists.
type NotifierProxy struct {
	mu       sync.Mutex
	notifier Notifier
}

// Set installs the real notifier backend.
func (p *NotifierProxy) Set(n Notifier) {
	p.mu.Lock()
	p.notifier = n
	p.mu.Unlock()
}

// Notify delegates to the installed backend. Returns an error if no backend is set.
func (p *NotifierProxy) Notify(ctx context.Context, n Notification) error {
	p.mu.Lock()
	notifier := p.notifier
	p.mu.Unlock()
	if notifier == nil {
		return fmt.Errorf("no notifier configured")
	}
	return notifier.Notify(ctx, n)
}
