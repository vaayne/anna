package channel

import (
	"context"
	"errors"
	"testing"
)

// mockBackend is a test Backend that records calls.
type mockBackend struct {
	name  string
	calls []Notification
	err   error
}

func (m *mockBackend) Name() string { return m.name }
func (m *mockBackend) Notify(_ context.Context, n Notification) error {
	m.calls = append(m.calls, n)
	return m.err
}

func TestDispatcherBroadcast(t *testing.T) {
	d := NewDispatcher()
	tg := &mockBackend{name: "telegram"}
	slack := &mockBackend{name: "slack"}
	d.Register(tg, "tg-chat")
	d.Register(slack, "slack-channel")

	err := d.Notify(context.Background(), Notification{Text: "hello"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(tg.calls) != 1 {
		t.Fatalf("telegram got %d calls, want 1", len(tg.calls))
	}
	if tg.calls[0].ChatID != "tg-chat" {
		t.Errorf("telegram ChatID = %q, want %q", tg.calls[0].ChatID, "tg-chat")
	}
	if len(slack.calls) != 1 {
		t.Fatalf("slack got %d calls, want 1", len(slack.calls))
	}
	if slack.calls[0].ChatID != "slack-channel" {
		t.Errorf("slack ChatID = %q, want %q", slack.calls[0].ChatID, "slack-channel")
	}
}

func TestDispatcherRouteToSpecific(t *testing.T) {
	d := NewDispatcher()
	tg := &mockBackend{name: "telegram"}
	slack := &mockBackend{name: "slack"}
	d.Register(tg, "tg-chat")
	d.Register(slack, "slack-channel")

	err := d.Notify(context.Background(), Notification{
		Channel: "slack",
		Text:    "only slack",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(tg.calls) != 0 {
		t.Errorf("telegram got %d calls, want 0", len(tg.calls))
	}
	if len(slack.calls) != 1 {
		t.Fatalf("slack got %d calls, want 1", len(slack.calls))
	}
	if slack.calls[0].ChatID != "slack-channel" {
		t.Errorf("slack ChatID = %q, want default %q", slack.calls[0].ChatID, "slack-channel")
	}
}

func TestDispatcherExplicitChatID(t *testing.T) {
	d := NewDispatcher()
	tg := &mockBackend{name: "telegram"}
	d.Register(tg, "default-chat")

	err := d.Notify(context.Background(), Notification{
		Channel: "telegram",
		ChatID:  "override-chat",
		Text:    "test",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tg.calls[0].ChatID != "override-chat" {
		t.Errorf("ChatID = %q, want %q", tg.calls[0].ChatID, "override-chat")
	}
}

func TestDispatcherUnknownChannel(t *testing.T) {
	d := NewDispatcher()
	d.Register(&mockBackend{name: "telegram"}, "chat")

	err := d.Notify(context.Background(), Notification{Channel: "discord", Text: "test"})
	if err == nil {
		t.Fatal("expected error for unknown channel")
	}
}

func TestDispatcherNoBackends(t *testing.T) {
	d := NewDispatcher()
	err := d.Notify(context.Background(), Notification{Text: "test"})
	if err == nil {
		t.Fatal("expected error with no backends")
	}
}

func TestDispatcherPartialFailure(t *testing.T) {
	d := NewDispatcher()
	good := &mockBackend{name: "telegram"}
	bad := &mockBackend{name: "slack", err: errors.New("slack down")}
	d.Register(good, "chat")
	d.Register(bad, "channel")

	err := d.Notify(context.Background(), Notification{Text: "test"})
	if err == nil {
		t.Fatal("expected error on partial failure")
	}
	// Good backend should still have been called.
	if len(good.calls) != 1 {
		t.Errorf("good backend got %d calls, want 1", len(good.calls))
	}
}

func TestDispatcherBackends(t *testing.T) {
	d := NewDispatcher()
	d.Register(&mockBackend{name: "telegram"}, "")
	d.Register(&mockBackend{name: "slack"}, "")

	names := d.Backends()
	if len(names) != 2 {
		t.Fatalf("len(Backends()) = %d, want 2", len(names))
	}
	if names[0] != "telegram" || names[1] != "slack" {
		t.Errorf("Backends() = %v, want [telegram slack]", names)
	}
}
