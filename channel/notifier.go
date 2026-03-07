package channel

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// Notification represents a message to push to a user or channel.
type Notification struct {
	Channel string // optional: route to a specific backend ("telegram", "slack")
	ChatID  string // target chat/channel within the backend
	Text    string // markdown content
	Silent  bool   // send without notification sound
}

// Notifier can push notifications. Both Dispatcher and individual backends
// satisfy this interface, so consumers don't need to know the routing layer.
type Notifier interface {
	Notify(ctx context.Context, n Notification) error
}

// Backend is a named notification backend (telegram, slack, discord, etc.).
type Backend interface {
	Notifier
	Name() string
}

type backendEntry struct {
	backend     Backend
	defaultChat string
}

// Dispatcher routes notifications to one or more registered backends.
// It implements Notifier so it can be passed to tools and cron wiring.
type Dispatcher struct {
	mu       sync.RWMutex
	backends []backendEntry
}

// NewDispatcher creates an empty dispatcher. Register backends before use.
func NewDispatcher() *Dispatcher {
	return &Dispatcher{}
}

// Register adds a backend with its default chat/channel target.
func (d *Dispatcher) Register(b Backend, defaultChat string) {
	d.mu.Lock()
	d.backends = append(d.backends, backendEntry{backend: b, defaultChat: defaultChat})
	d.mu.Unlock()
}

// Notify routes a notification to backends. If Notification.Channel is set,
// only that backend receives it. Otherwise all registered backends receive it.
func (d *Dispatcher) Notify(ctx context.Context, n Notification) error {
	d.mu.RLock()
	entries := make([]backendEntry, len(d.backends))
	copy(entries, d.backends)
	d.mu.RUnlock()

	if len(entries) == 0 {
		return fmt.Errorf("no notification backends registered")
	}

	// Route to a specific backend.
	if n.Channel != "" {
		for _, e := range entries {
			if e.backend.Name() == n.Channel {
				if n.ChatID == "" {
					n.ChatID = e.defaultChat
				}
				return e.backend.Notify(ctx, n)
			}
		}
		return fmt.Errorf("unknown notification channel %q", n.Channel)
	}

	// Broadcast to all backends.
	var errs []error
	for _, e := range entries {
		nn := n
		if nn.ChatID == "" {
			nn.ChatID = e.defaultChat
		}
		if err := e.backend.Notify(ctx, nn); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", e.backend.Name(), err))
		}
	}
	return errors.Join(errs...)
}

// Backends returns the names of all registered backends.
func (d *Dispatcher) Backends() []string {
	d.mu.RLock()
	defer d.mu.RUnlock()
	names := make([]string, len(d.backends))
	for i, e := range d.backends {
		names[i] = e.backend.Name()
	}
	return names
}
