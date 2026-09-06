package host

import (
	"context"
	"testing"

	"github.com/CherryHQ/stella/internal/platform/config"
	pkgplugins "github.com/CherryHQ/stella/pkg/plugins"
)

// TestApplyChannelHonoursOnlyExplicitPlatformOverride pins the kill-switch
// semantics: a channel runs unless an admin stored an explicit "off" row for its
// platform. An absent row must not read as a veto — that would mean creating one
// channel required a deployment-wide plugin write first.
func TestApplyChannelHonoursOnlyExplicitPlatformOverride(t *testing.T) {
	for _, tc := range []struct {
		name        string
		overrides   map[string]config.Plugin
		wantEnabled bool
	}{
		{
			name:        "no plugin row runs the channel",
			overrides:   map[string]config.Plugin{},
			wantEnabled: true,
		},
		{
			name: "explicit disabled row stops the channel",
			overrides: map[string]config.Plugin{
				"channel/telegram": {ID: "channel/telegram", Kind: "channel", Name: "telegram", Enabled: false},
			},
			wantEnabled: false,
		},
		{
			name: "explicit enabled row runs the channel",
			overrides: map[string]config.Plugin{
				"channel/telegram": {ID: "channel/telegram", Kind: "channel", Name: "telegram", Enabled: true},
			},
			wantEnabled: true,
		},
		{
			name: "another platform's off row is not this one's",
			overrides: map[string]config.Plugin{
				"channel/discord": {ID: "channel/discord", Kind: "channel", Name: "discord", Enabled: false},
			},
			wantEnabled: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &stubStore{plugins: tc.overrides, channels: map[string][]config.Channel{
				"telegram": {{ID: "telegram-team", Type: "telegram", Enabled: true}},
			}}
			host := New(store, WithListenerCap(allowAllListenerCap))
			host.RegisterPluginID("channel/telegram")

			var got []pkgplugins.PluginState
			host.AddRuntime(pkgplugins.RuntimeSpec{
				PluginID: "channel/telegram",
				Name:     "main",
				Build: func(pkgplugins.RuntimeContext) (pkgplugins.Runtime, error) {
					return runtimeStub{apply: func(_ context.Context, desired pkgplugins.PluginState) error {
						got = append(got, desired)
						return nil
					}}, nil
				},
			})

			channel := config.Channel{ID: "telegram-team", Type: "telegram", Enabled: true}
			if err := host.ApplyChannel(context.Background(), channel); err != nil {
				t.Fatalf("apply channel: %v", err)
			}
			if len(got) != 1 {
				t.Fatalf("apply calls = %d, want 1", len(got))
			}
			if got[0].ID != channel.ID {
				t.Fatalf("applied id = %q, want %q", got[0].ID, channel.ID)
			}
			if got[0].Enabled != tc.wantEnabled {
				t.Fatalf("applied enabled = %v, want %v", got[0].Enabled, tc.wantEnabled)
			}
		})
	}
}
