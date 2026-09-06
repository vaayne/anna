package host

import (
	"context"
	"errors"
	"fmt"

	"github.com/CherryHQ/stella/internal/platform/config"
	pkgchannel "github.com/CherryHQ/stella/pkg/channel"
	pkgplugins "github.com/CherryHQ/stella/pkg/plugins"
)

// GuestPolicyResolver interprets persisted configuration through its owning plugin.
func (h *Host) GuestPolicyResolver(channelType, rawConfig string) (pkgchannel.GuestConfig, error) {
	h.mu.RLock()
	decoder := h.channelRegs[channelType].GuestPolicy
	h.mu.RUnlock()
	if decoder == nil {
		return pkgchannel.GuestConfig{}, fmt.Errorf("channel %q does not support guest sessions", channelType)
	}
	return decoder(rawConfig)
}

func channelEnrollmentNamespacesLocked(regs map[string]pkgplugins.ChannelSpec, pluginID string) []string {
	namespaces := make([]string, 0, 1)
	for _, reg := range regs {
		if reg.PluginID == pluginID && reg.Name != "" {
			namespaces = append(namespaces, reg.Name)
		}
	}
	return namespaces
}

func (h *Host) ListChannels(ctx context.Context) ([]config.Channel, error) {
	return h.store.ListChannels(ctx)
}

// ReconcileChannels reapplies every durable channel instance after a committed
// plugin mutation. Runtime failures are reported together after all instances
// have been attempted, so one broken credential or listener cannot prevent an
// unrelated instance from receiving the same committed policy.
func (h *Host) ReconcileChannels(ctx context.Context) error {
	channels, err := h.store.ListChannels(ctx)
	if err != nil {
		return fmt.Errorf("list channel instances for reconciliation: %w", err)
	}
	var failures []error
	for _, channel := range channels {
		if err := h.runtimes.ReconcileChannel(ctx, channel.ID); err != nil {
			failures = append(failures, fmt.Errorf("reconcile channel %s: %w", channel.ID, err))
		}
	}
	return errors.Join(failures...)
}

func (h *Host) ChannelInstanceConfigured(channel config.Channel) bool {
	if !channel.Enabled {
		return false
	}
	channelType := channel.Type
	if channelType == "" {
		channelType = channel.ID
	}
	state := pkgplugins.PluginState{
		ID:      channel.ID,
		Enabled: channel.Enabled,
		Config:  configMapFromJSON(channel.Config),
	}
	h.mu.RLock()
	reg, ok := h.channelRegs[channelType]
	h.mu.RUnlock()
	if !ok {
		return false
	}
	if reg.Configured == nil {
		return true
	}
	return reg.Configured(cloneMap(state.Config))
}
