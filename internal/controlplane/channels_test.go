package controlplane

import (
	"context"
	"testing"

	"github.com/CherryHQ/stella/internal/authz"
	"github.com/CherryHQ/stella/internal/platform/config"
)

type channelIdentityStore struct {
	config.Store
	channels map[string]config.Channel
}

func (s *channelIdentityStore) GetChannel(_ context.Context, id string) (config.Channel, error) {
	ch, ok := s.channels[id]
	if !ok {
		return config.Channel{}, config.ErrChannelNotFound
	}
	return ch, nil
}

func TestManageChannelPinsExactID(t *testing.T) {
	store := &channelIdentityStore{channels: map[string]config.Channel{
		"weixin-a": {ID: "weixin-a", Type: "weixin"},
		"weixin-b": {ID: "weixin-b", Type: "weixin"},
	}}
	svc := NewService(store, nil, nil, nil, nil, nil)
	authority, err := authz.NewUserAuthority("admin", true)
	if err != nil {
		t.Fatal(err)
	}
	access, err := svc.Begin(context.Background(), authority)
	if err != nil {
		t.Fatal(err)
	}
	operation, err := access.ManageChannel("weixin-a")
	if err != nil {
		t.Fatal(err)
	}
	if got := operation.ID(); got != "weixin-a" {
		t.Fatalf("operation id = %q, want weixin-a", got)
	}
	if _, err := operation.Channel(context.Background(), "weixin-b"); err == nil {
		t.Fatal("cross-instance channel read unexpectedly succeeded")
	}
	if _, err := operation.Channel(context.Background(), "weixin-a"); err != nil {
		t.Fatalf("exact channel read: %v", err)
	}
	if _, err := operation.Save(context.Background(), config.Channel{ID: "weixin-b", Type: "weixin"}, nil, false); err == nil {
		t.Fatal("cross-instance channel save unexpectedly succeeded")
	}
}
