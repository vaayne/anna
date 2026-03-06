package stream

import (
	"sync"

	"github.com/vaayne/anna/pkg/ai/types"
)

// AssistantEventStream provides ordered assistant events.
type AssistantEventStream interface {
	Events() <-chan types.AssistantEvent
	Close() error
	Wait() error
}

// ChannelEventStream is a channel-backed AssistantEventStream.
type ChannelEventStream struct {
	events chan types.AssistantEvent
	once   sync.Once
	errMu  sync.RWMutex
	err    error
}

// NewChannelEventStream returns a writable channel stream.
func NewChannelEventStream(buffer int) *ChannelEventStream {
	return &ChannelEventStream{events: make(chan types.AssistantEvent, buffer)}
}

// Events returns read-only event channel.
func (s *ChannelEventStream) Events() <-chan types.AssistantEvent {
	return s.events
}

// Emit sends one event to subscribers.
func (s *ChannelEventStream) Emit(event types.AssistantEvent) {
	s.events <- event
}

// Finish closes stream and stores terminal error if any.
func (s *ChannelEventStream) Finish(err error) {
	s.errMu.Lock()
	s.err = err
	s.errMu.Unlock()
	s.once.Do(func() {
		close(s.events)
	})
}

// Close closes the event stream.
func (s *ChannelEventStream) Close() error {
	s.once.Do(func() {
		close(s.events)
	})
	return nil
}

// Wait returns terminal stream error.
func (s *ChannelEventStream) Wait() error {
	s.errMu.RLock()
	defer s.errMu.RUnlock()
	return s.err
}
