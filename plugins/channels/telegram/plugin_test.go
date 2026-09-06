package telegram

import (
	"context"
	"testing"

	internalnotify "github.com/CherryHQ/stella/internal/notify"
	"github.com/CherryHQ/stella/internal/platform/config"
	pluginhost "github.com/CherryHQ/stella/internal/plugin/host"
	pkgchannel "github.com/CherryHQ/stella/pkg/channel"
	pkgplugins "github.com/CherryHQ/stella/pkg/plugins"
)

func TestSelfRegisteredTelegramPluginIsComplete(t *testing.T) {
	host := pluginhost.New(newStubStore())
	services := pluginhost.NewChannelRuntimeServices()
	host.SetAccountEnrollment(testAccountEnroller{})
	services.Set(context.Background(), fakeChannelHandler{}, internalnotify.NewDispatcher(), nil)
	host.SetChannelRuntimeServices(services)

	if err := host.LoadDefaultCatalog(); err != nil {
		t.Fatalf("LoadDefaultCatalog: %v", err)
	}
	if !host.HasRuntime(PluginID) || !host.HasConfig(PluginID) || !host.HasStatus(PluginID) {
		t.Fatal("expected telegram runtime/config/status registrations")
	}
	metas := host.ListRegisteredPlugins()
	found := false
	for _, meta := range metas {
		if meta.ID != PluginID {
			continue
		}
		found = true
		if meta.Kind != "channel" || meta.Name != pkgchannel.PlatformTelegram || !meta.Managed || !meta.AdminVisible {
			t.Fatalf("unexpected telegram metadata: %#v", meta)
		}
	}
	if !found {
		t.Fatal("expected telegram metadata")
	}
}

func TestSelfRegisteredTelegramPluginAppliesAndReportsStatus(t *testing.T) {
	store := newStubStore()
	store.plugins[PluginID] = config.Plugin{ID: PluginID, Kind: "channel", Name: pkgchannel.PlatformTelegram, Enabled: true, Config: map[string]any{"token": "tg-token", "enable_notify": true}}
	store.channels = []config.Channel{{ID: "telegram-test", Type: pkgchannel.PlatformTelegram, Enabled: true, Config: `{"token":"tg-token"}`}}

	host := pluginhost.New(store, pluginhost.WithListenerCap(func(context.Context, string, string) (bool, error) {
		return true, nil
	}))
	services := pluginhost.NewChannelRuntimeServices()
	dispatcher := internalnotify.NewDispatcher()
	host.SetAccountEnrollment(testAccountEnroller{})
	services.Set(context.Background(), fakeChannelHandler{}, dispatcher, nil)
	host.SetChannelRuntimeServices(services)

	reset := SetRuntimeFactoryForTesting(func(pkgplugins.Platform) (pkgplugins.Runtime, error) {
		return NewManagedRuntime(RuntimeDeps{
			Parent:        context.Background(),
			Handler:       fakeChannelHandler{},
			Notifications: dispatcher,
			NewChannel: func(cfg TelegramConfig, handler pkgchannel.Handler) (pkgchannel.Channel, error) {
				return newTestChannel(), nil
			},
		}), nil
	})
	defer reset()

	if err := host.LoadDefaultCatalog(); err != nil {
		t.Fatalf("LoadDefaultCatalog: %v", err)
	}
	ctx := context.Background()
	if err := host.ApplyPlugin(ctx, PluginID); err != nil {
		t.Fatalf("ApplyPlugin: %v", err)
	}
	handle, ok := host.Runtime().Get(ctx, "telegram-test", RuntimeName)
	if !ok {
		t.Fatal("missing exact telegram channel runtime")
	}
	status, err := handle.Snapshot(ctx)
	if err != nil {
		t.Fatalf("runtime snapshot: %v", err)
	}
	if status.State != pkgplugins.RuntimeStateRunning {
		t.Fatalf("state = %#v, want %q", status.State, pkgplugins.RuntimeStateRunning)
	}
	if got := dispatcher.Channels(); len(got) != 1 || got[0] != pkgchannel.PlatformTelegram {
		t.Fatalf("dispatcher channels = %v", got)
	}
}

type testAccountEnroller struct{}

func (testAccountEnroller) EnrollAccount(context.Context, string, pkgchannel.EnrollmentRequest) error {
	return nil
}

type stubStore struct {
	plugins  map[string]config.Plugin
	channels []config.Channel
}

func newStubStore() *stubStore { return &stubStore{plugins: map[string]config.Plugin{}} }

func (s *stubStore) ListProviders(context.Context) ([]config.Provider, error) { return nil, nil }
func (s *stubStore) GetProvider(context.Context, string) (config.Provider, error) {
	return config.Provider{}, nil
}
func (s *stubStore) CreateProvider(context.Context, config.Provider) error { return nil }
func (s *stubStore) UpdateProvider(context.Context, config.Provider) error { return nil }
func (s *stubStore) DeleteProvider(context.Context, string) error          { return nil }
func (s *stubStore) ListCachedModels(context.Context) ([]config.CachedModel, error) {
	return nil, nil
}
func (s *stubStore) ReplaceCachedModels(context.Context, string, []string) error { return nil }
func (s *stubStore) Seed(context.Context) error                                  { return nil }
func (s *stubStore) ListAgents(context.Context) ([]config.Agent, error)          { return nil, nil }
func (s *stubStore) ListEnabledAgents(context.Context) ([]config.Agent, error)   { return nil, nil }
func (s *stubStore) GetAgent(context.Context, string) (config.Agent, error) {
	return config.Agent{}, nil
}
func (s *stubStore) CreateAgent(context.Context, config.Agent) error        { return nil }
func (s *stubStore) UpdateAgent(context.Context, config.Agent) error        { return nil }
func (s *stubStore) DeleteAgent(context.Context, string) error              { return nil }
func (s *stubStore) ListChannels(context.Context) ([]config.Channel, error) { return nil, nil }
func (s *stubStore) ListChannelsByType(context.Context, string) ([]config.Channel, error) {
	return s.channels, nil
}

func (s *stubStore) GetChannel(_ context.Context, id string) (config.Channel, error) {
	for _, channel := range s.channels {
		if channel.ID == id {
			return channel, nil
		}
	}
	return config.Channel{}, config.ErrChannelNotFound
}
func (s *stubStore) CreateChannel(context.Context, config.Channel) error { return nil }
func (s *stubStore) UpdateChannel(context.Context, config.Channel) error { return nil }
func (s *stubStore) DeleteChannel(context.Context, string) error         { return nil }
func (s *stubStore) ListPlugins(context.Context) ([]config.Plugin, error) {
	plugins := make([]config.Plugin, 0, len(s.plugins))
	for _, plugin := range s.plugins {
		plugins = append(plugins, plugin)
	}
	return plugins, nil
}

func (s *stubStore) ListPluginOverrides(ctx context.Context) ([]config.Plugin, error) {
	return s.ListPlugins(ctx)
}

func (s *stubStore) ListEnabledPlugins(context.Context) ([]config.Plugin, error) { return nil, nil }
func (s *stubStore) GetPlugin(_ context.Context, id string) (config.Plugin, error) {
	return s.plugins[id], nil
}

func (s *stubStore) UpsertPlugin(_ context.Context, plugin config.Plugin) error {
	s.plugins[plugin.ID] = plugin
	return nil
}

func (s *stubStore) SetPluginEnabled(_ context.Context, id string, enabled bool) error {
	p := s.plugins[id]
	p.Enabled = enabled
	s.plugins[id] = p
	return nil
}

func (s *stubStore) SetPluginConfig(_ context.Context, id string, cfg map[string]any) error {
	p := s.plugins[id]
	p.Config = cfg
	s.plugins[id] = p
	return nil
}

// The real store writes only the config column here; the stub mirrors that by
// leaving Enabled alone, and creating a missing row enabled.
func (s *stubStore) SetChannelPluginConfig(_ context.Context, id, kind, name string, cfg map[string]any) error {
	p, ok := s.plugins[id]
	if !ok {
		p = config.Plugin{ID: id, Kind: kind, Name: name, Enabled: true}
	}
	p.Config = cfg
	s.plugins[id] = p
	return nil
}

func (s *stubStore) DeletePlugin(context.Context, string) error { return nil }
func (s *stubStore) GetManifestPluginOverride(context.Context, string) (config.ManifestPluginOverride, bool, error) {
	return config.ManifestPluginOverride{}, false, nil
}

func (s *stubStore) ListManifestPluginOverrides(context.Context) ([]config.ManifestPluginOverride, error) {
	return nil, nil
}

func (s *stubStore) UpsertManifestPluginOverride(context.Context, config.ManifestPluginOverride) error {
	return nil
}
func (s *stubStore) DeleteManifestPluginOverride(context.Context, string) error { return nil }
func (s *stubStore) GetChatAgent(context.Context, string, string, string) (string, error) {
	return "", nil
}
func (s *stubStore) SetChatAgent(context.Context, string, string, string, string) error { return nil }
func (s *stubStore) DeleteChatAgent(context.Context, string, string, string) error      { return nil }
func (s *stubStore) GetSetting(context.Context, string) (string, error)                 { return "", nil }
func (s *stubStore) SetSetting(context.Context, string, string) error                   { return nil }
func (s *stubStore) Snapshot(context.Context, string) (*config.Snapshot, error)         { return nil, nil }

type testChannel struct{ stop chan struct{} }

func newTestChannel() *testChannel                                         { return &testChannel{stop: make(chan struct{})} }
func (*testChannel) Name() string                                          { return pkgchannel.PlatformTelegram }
func (c *testChannel) Start(ctx context.Context) error                     { <-ctx.Done(); close(c.stop); return ctx.Err() }
func (*testChannel) Stop()                                                 {}
func (*testChannel) Notify(context.Context, pkgchannel.Notification) error { return nil }
