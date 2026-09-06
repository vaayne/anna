package main

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/CherryHQ/stella/internal/platform/config"
	pkgplugins "github.com/CherryHQ/stella/pkg/plugins"
)

type fakeManagedChannelRuntimeHost struct {
	metas                 []pkgplugins.PluginInfo
	channels              []config.Channel
	listErr               error
	configured            map[string]bool
	applyErrs             map[string]error
	applyCalls            []string
	reconcileChannelCalls []string
}

func (h *fakeManagedChannelRuntimeHost) ListRegisteredPlugins() []pkgplugins.PluginInfo {
	return append([]pkgplugins.PluginInfo(nil), h.metas...)
}

func (h *fakeManagedChannelRuntimeHost) ApplyPlugin(_ context.Context, pluginID string) error {
	h.applyCalls = append(h.applyCalls, pluginID)
	if h.applyErrs == nil {
		return nil
	}
	return h.applyErrs[pluginID]
}

func (h *fakeManagedChannelRuntimeHost) ListChannels(context.Context) ([]config.Channel, error) {
	return h.channels, h.listErr
}

func (h *fakeManagedChannelRuntimeHost) ReconcileChannel(_ context.Context, channelID string) error {
	h.reconcileChannelCalls = append(h.reconcileChannelCalls, channelID)
	if h.applyErrs == nil {
		return nil
	}
	return h.applyErrs[channelID]
}

func (h *fakeManagedChannelRuntimeHost) ChannelInstanceConfigured(channel config.Channel) bool {
	return h.configured[channel.ID]
}

func (h *fakeManagedChannelRuntimeHost) ChannelConfigured(_ context.Context, name string) bool {
	if h.configured == nil {
		return false
	}
	return h.configured[name]
}

func TestApplyManagedChannelPluginsContinuesAfterStartupError(t *testing.T) {
	host := &fakeManagedChannelRuntimeHost{
		channels: []config.Channel{
			{ID: "telegram-instance", Type: "telegram"},
			{ID: "qq-instance", Type: "qq"},
		},
		configured: map[string]bool{
			"telegram-instance": true,
			"qq-instance":       true,
		},
		applyErrs: map[string]error{
			"telegram-instance": errors.New("EOF"),
		},
	}

	summary, err := applyManagedChannelPlugins(context.Background(), host)
	if err != nil {
		t.Fatalf("applyManagedChannelPlugins: %v", err)
	}

	if summary.Registered != 2 {
		t.Fatalf("Registered = %d, want 2", summary.Registered)
	}
	if summary.Configured != 2 {
		t.Fatalf("Configured = %d, want 2", summary.Configured)
	}
	if summary.Started != 1 {
		t.Fatalf("Started = %d, want 1", summary.Started)
	}

	wantCalls := []string{"telegram-instance", "qq-instance"}
	if !reflect.DeepEqual(host.reconcileChannelCalls, wantCalls) {
		t.Fatalf("ReconcileChannel calls = %v, want %v", host.reconcileChannelCalls, wantCalls)
	}
}

func TestApplyManagedChannelPluginsCountsOnlyConfiguredSuccessfulChannels(t *testing.T) {
	host := &fakeManagedChannelRuntimeHost{
		channels: []config.Channel{
			{ID: "telegram-instance", Type: "telegram"},
			{ID: "feishu-instance", Type: "feishu"},
		},
		configured: map[string]bool{
			"telegram-instance": true,
			"feishu-instance":   false,
		},
	}

	summary, err := applyManagedChannelPlugins(context.Background(), host)
	if err != nil {
		t.Fatalf("applyManagedChannelPlugins: %v", err)
	}

	if summary.Registered != 2 {
		t.Fatalf("Registered = %d, want 2", summary.Registered)
	}
	if summary.Configured != 1 {
		t.Fatalf("Configured = %d, want 1", summary.Configured)
	}
	if summary.Started != 1 {
		t.Fatalf("Started = %d, want 1", summary.Started)
	}
}

func TestApplyManagedChannelPluginsDoesNotStartHiddenPluginRuntimeWhenNoChannels(t *testing.T) {
	host := &fakeManagedChannelRuntimeHost{
		metas: []pkgplugins.PluginInfo{
			{ID: "channel/telegram", Kind: "channel", Name: "telegram"},
			{ID: "channel/feishu", Kind: "channel", Name: "feishu"},
		},
		configured: map[string]bool{
			"telegram": true,
			"feishu":   true,
		},
	}

	summary, err := applyManagedChannelPlugins(context.Background(), host)
	if err != nil {
		t.Fatalf("applyManagedChannelPlugins: %v", err)
	}
	if summary != (managedChannelRuntimeSummary{}) {
		t.Fatalf("summary = %#v, want empty summary", summary)
	}
	if len(host.applyCalls) != 0 || len(host.reconcileChannelCalls) != 0 {
		t.Fatalf("runtime startup calls = plugin %v, channel %v; want none", host.applyCalls, host.reconcileChannelCalls)
	}
}

func TestApplyManagedChannelPluginsReturnsChannelListError(t *testing.T) {
	listErr := errors.New("database unavailable")
	host := &fakeManagedChannelRuntimeHost{
		metas:   []pkgplugins.PluginInfo{{ID: "channel/feishu", Kind: "channel", Name: "feishu"}},
		listErr: listErr,
	}

	summary, err := applyManagedChannelPlugins(context.Background(), host)
	if !errors.Is(err, listErr) {
		t.Fatalf("error = %v, want wrapped list error", err)
	}
	if summary != (managedChannelRuntimeSummary{}) {
		t.Fatalf("summary = %#v, want empty summary", summary)
	}
	if len(host.applyCalls) != 0 || len(host.reconcileChannelCalls) != 0 {
		t.Fatalf("runtime startup calls = plugin %v, channel %v; want none", host.applyCalls, host.reconcileChannelCalls)
	}
}
