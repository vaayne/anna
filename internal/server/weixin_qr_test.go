package server

import (
	"context"
	"errors"
	"testing"

	"github.com/CherryHQ/stella/internal/authz"
	"github.com/CherryHQ/stella/internal/controlplane"
	"github.com/CherryHQ/stella/internal/platform/config"
)

type failingWeixinChannelStore struct {
	config.Store
	err error
}

func (s failingWeixinChannelStore) GetChannel(context.Context, string) (config.Channel, error) {
	return config.Channel{}, s.err
}

func TestSaveWeixinChannelDoesNotTreatReadFailureAsMissing(t *testing.T) {
	readErr := errors.New("database unavailable")
	cp := controlplane.NewService(failingWeixinChannelStore{err: readErr}, nil, nil, nil, nil, nil)
	authority, err := authz.NewUserAuthority("admin", true)
	if err != nil {
		t.Fatal(err)
	}
	access, err := cp.Begin(context.Background(), authority)
	if err != nil {
		t.Fatal(err)
	}
	operation, err := access.ManageChannel("weixin-a")
	if err != nil {
		t.Fatal(err)
	}
	_, err = (&Server{}).saveWeixinChannel(context.Background(), operation, "", "", false, nil, WeixinQRCodeStatus{})
	if !errors.Is(err, readErr) {
		t.Fatalf("save error = %v, want read failure", err)
	}
}
