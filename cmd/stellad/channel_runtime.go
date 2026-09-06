package main

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/CherryHQ/stella/internal/platform/config"
)

type managedChannelRuntimeHost interface {
	ListChannels(context.Context) ([]config.Channel, error)
	ReconcileChannel(context.Context, string) error
	ChannelInstanceConfigured(config.Channel) bool
}

type managedChannelRuntimeSummary struct {
	Registered int
	Configured int
	Started    int
}

func applyManagedChannelPlugins(ctx context.Context, host managedChannelRuntimeHost) (managedChannelRuntimeSummary, error) {
	if host == nil {
		return managedChannelRuntimeSummary{}, nil
	}

	var summary managedChannelRuntimeSummary
	channels, err := host.ListChannels(ctx)
	if err != nil {
		return summary, fmt.Errorf("list channel instances: %w", err)
	}

	summary.Registered = len(channels)
	for _, ch := range channels {
		if err := ctx.Err(); err != nil {
			return summary, err
		}
		configured := host.ChannelInstanceConfigured(ch)
		if configured {
			summary.Configured++
		}
		if err := host.ReconcileChannel(ctx, ch.ID); err != nil {
			slog.Warn("managed channel runtime failed to start", "channel_id", ch.ID, "channel_type", ch.Type, "error", err)
			continue
		}
		if configured {
			summary.Started++
		}
	}

	return summary, nil
}
