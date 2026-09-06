package controlplane

import (
	"context"
	"errors"
	"testing"

	"github.com/CherryHQ/stella/internal/authz"
	"github.com/CherryHQ/stella/internal/platform/config"
	pluginhost "github.com/CherryHQ/stella/internal/plugin/host"
)

// catalogFakeStore implements only the read methods the catalog reads use; the
// rest of config.Store is embedded nil (unused here). reads counts every store
// call so a test can prove a denied authority never reaches the store.
type catalogFakeStore struct {
	config.Store
	providers []config.Provider
	cached    []config.CachedModel
	channels  []config.Channel
	plugins   []config.Plugin
	reads     int
}

func (f *catalogFakeStore) ListProviders(context.Context) ([]config.Provider, error) {
	f.reads++
	return f.providers, nil
}

func (f *catalogFakeStore) ListCachedModels(context.Context) ([]config.CachedModel, error) {
	f.reads++
	return f.cached, nil
}

func (f *catalogFakeStore) ListChannels(context.Context) ([]config.Channel, error) {
	f.reads++
	return f.channels, nil
}

func (f *catalogFakeStore) ListPluginOverrides(context.Context) ([]config.Plugin, error) {
	f.reads++
	return f.plugins, nil
}

func catalogUser(t *testing.T) authz.Authority {
	t.Helper()
	a, err := authz.NewUserAuthority(authz.UserID("user-1"), false)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestListEnabledModelsFiltersDedupesAndSorts(t *testing.T) {
	store := &catalogFakeStore{
		providers: []config.Provider{
			{ID: "openai", Name: "OpenAI", Enabled: true, Models: map[string]config.ProviderModelOverride{
				"gpt":      {Enabled: config.ValuePtr(true)},
				"disabled": {Enabled: config.ValuePtr(false)},
			}},
			{ID: "off", Name: "Off", Enabled: false, Models: map[string]config.ProviderModelOverride{
				"hidden": {Enabled: config.ValuePtr(true)},
			}},
		},
		cached: []config.CachedModel{
			{Provider: "openai", Model: "gpt"},      // dup of the enabled provider model
			{Provider: "openai", Model: "fetched"},  // new, provider enabled -> included
			{Provider: "openai", Model: "disabled"}, // model disabled -> excluded
			{Provider: "off", Model: "hidden"},      // provider disabled -> excluded
		},
	}
	got, err := NewService(store, nil, nil, nil, nil, nil).ListEnabledModels(context.Background(), catalogUser(t))
	if err != nil {
		t.Fatalf("ListEnabledModels: %v", err)
	}
	keys := make([]string, 0, len(got))
	for _, m := range got {
		keys = append(keys, m.Provider+"/"+m.Model)
	}
	want := []string{"openai/fetched", "openai/gpt"} // sorted, deduped, disabled+off excluded
	if len(keys) != len(want) {
		t.Fatalf("models = %v, want %v", keys, want)
	}
	for i := range want {
		if keys[i] != want[i] {
			t.Fatalf("models = %v, want %v (sorted)", keys, want)
		}
	}
}

func TestPublicChannelsProjectsWithoutSecrets(t *testing.T) {
	store := &catalogFakeStore{channels: []config.Channel{
		{ID: "tg1", Type: "telegram", AgentID: "a1", Enabled: true, Config: `{"token":"SECRET"}`},
		{ID: "weixin", Type: "", AgentID: "a2", Enabled: false, Config: `{"secret":"S"}`}, // type falls back to id
	}}
	host := pluginhost.New(store, pluginhost.WithListenerCap(func(context.Context, string, string) (bool, error) {
		return true, nil
	}))
	got, err := NewService(store, host, nil, nil, nil, nil).PublicChannels(context.Background(), catalogUser(t))
	if err != nil {
		t.Fatalf("PublicChannels: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("channels = %d, want 2", len(got))
	}
	// PublicChannel is a struct with no Config field — this is a compile-time
	// guarantee that channel credentials cannot leak through the public list.
	if got[0] != (PublicChannel{ID: "tg1", Type: "telegram", AgentID: "a1", Enabled: true}) {
		t.Fatalf("channel[0] = %+v, want secret-free telegram projection", got[0])
	}
	if got[1].Type != "weixin" {
		t.Fatalf("channel[1].Type = %q, want effective type 'weixin' (fell back to id)", got[1].Type)
	}
}

func TestPublicChannelsHonoursExactListenerCap(t *testing.T) {
	store := &catalogFakeStore{channels: []config.Channel{
		{ID: "tg-a", Type: "telegram", AgentID: "agent-a", Enabled: true},
		{ID: "tg-b", Type: "telegram", AgentID: "agent-b", Enabled: true},
	}}
	host := pluginhost.New(store, pluginhost.WithListenerCap(func(_ context.Context, pluginID, agentID string) (bool, error) {
		if pluginID != "channel/telegram" {
			t.Fatalf("listener cap pluginID = %q, want channel/telegram", pluginID)
		}
		return agentID == "agent-a", nil
	}))
	got, err := NewService(store, host, nil, nil, nil, nil).PublicChannels(context.Background(), catalogUser(t))
	if err != nil {
		t.Fatalf("PublicChannels: %v", err)
	}
	if len(got) != 1 || got[0].ID != "tg-a" {
		t.Fatalf("channels = %+v, want only tg-a", got)
	}
}

// TestDisabledChannelTypes pins the default: only an explicitly stored, disabled
// plugin row switches a platform off. A platform with no row at all (discord
// here) is usable, so creating a channel never needs a plugin row.
func TestDisabledChannelTypes(t *testing.T) {
	store := &catalogFakeStore{plugins: []config.Plugin{
		{ID: "channel/telegram", Kind: config.PluginKindChannel, Name: "telegram", Enabled: true},
		{ID: "channel/qq", Kind: config.PluginKindChannel, Name: "qq", Enabled: false},
		{ID: "channel/feishu", Kind: config.PluginKindChannel, Name: "feishu", Enabled: true},
	}}
	got, err := NewService(store, nil, nil, nil, nil, nil).DisabledChannelTypes(context.Background(), catalogUser(t))
	if err != nil {
		t.Fatalf("DisabledChannelTypes: %v", err)
	}
	if !got["qq"] || got["telegram"] || got["feishu"] || got["discord"] {
		t.Fatalf("disabled types = %v, want qq only", got)
	}
}

// TestCatalogReadsRejectInvalidAuthorityBeforeStore proves the F1 contract: an
// invalid (unauthenticated) authority is denied and the store is never touched.
func TestCatalogReadsRejectInvalidAuthorityBeforeStore(t *testing.T) {
	ctx := context.Background()
	invalid := authz.Authority{}

	store := &catalogFakeStore{}
	svc := NewService(store, nil, nil, nil, nil, nil)
	if _, err := svc.ListEnabledModels(ctx, invalid); !errors.Is(err, authz.ErrForbidden) {
		t.Fatalf("ListEnabledModels(invalid) = %v, want ErrForbidden", err)
	}
	if _, err := svc.PublicChannels(ctx, invalid); !errors.Is(err, authz.ErrForbidden) {
		t.Fatalf("PublicChannels(invalid) = %v, want ErrForbidden", err)
	}
	if _, err := svc.DisabledChannelTypes(ctx, invalid); !errors.Is(err, authz.ErrForbidden) {
		t.Fatalf("DisabledChannelTypes(invalid) = %v, want ErrForbidden", err)
	}
	if store.reads != 0 {
		t.Fatalf("store was read %d times for a denied authority, want 0 (fail closed before read)", store.reads)
	}
}

// TestCatalogReadsRejectNonUserActor proves a valid but non-user actor (e.g. a
// system component) is also denied — the catalog gate is user/admin only.
func TestCatalogReadsRejectNonUserActor(t *testing.T) {
	ctx := context.Background()
	sys, err := authz.NewSystemAuthority(authz.Component("test"))
	if err != nil {
		t.Fatal(err)
	}
	store := &catalogFakeStore{}
	if _, err := NewService(store, nil, nil, nil, nil, nil).PublicChannels(ctx, sys); !errors.Is(err, authz.ErrForbidden) {
		t.Fatalf("PublicChannels(system) = %v, want ErrForbidden", err)
	}
	if store.reads != 0 {
		t.Fatalf("store read %d times for a non-user actor, want 0", store.reads)
	}
}

func TestCatalogReadsFailClosedOnNilService(t *testing.T) {
	ctx := context.Background()
	var nilSvc *Service
	if _, err := nilSvc.ListEnabledModels(ctx, catalogUser(t)); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("nil ListEnabledModels = %v, want ErrUnavailable", err)
	}
	if _, err := nilSvc.PublicChannels(ctx, catalogUser(t)); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("nil PublicChannels = %v, want ErrUnavailable", err)
	}
	if _, err := nilSvc.DisabledChannelTypes(ctx, catalogUser(t)); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("nil DisabledChannelTypes = %v, want ErrUnavailable", err)
	}
}

func TestPublicChannelsFailsClosedWithoutListenerCap(t *testing.T) {
	store := &catalogFakeStore{}
	svc := NewService(store, pluginhost.New(store), nil, nil, nil, nil)
	if _, err := svc.PublicChannels(context.Background(), catalogUser(t)); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("PublicChannels without listener cap = %v, want ErrUnavailable", err)
	}
	if store.reads != 0 {
		t.Fatalf("store reads = %d, want 0 when listener cap is unavailable", store.reads)
	}
}
