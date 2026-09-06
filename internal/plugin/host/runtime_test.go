package host

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/CherryHQ/stella/internal/platform/config"
	pkgchannel "github.com/CherryHQ/stella/pkg/channel"
	pkgplugins "github.com/CherryHQ/stella/pkg/plugins"
)

func allowAllListenerCap(context.Context, string, string) (bool, error) { return true, nil }

func TestConfigMapFromJSON(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want map[string]any
	}{
		{"empty string", "", map[string]any{}},
		{"valid json", `{"key":"val"}`, map[string]any{"key": "val"}},
		{"invalid json", `not json`, map[string]any{}},
		{"null json", `null`, map[string]any{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := configMapFromJSON(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("configMapFromJSON(%q): got %v, want %v", tc.in, got, tc.want)
			}
			for k, wv := range tc.want {
				if gv, ok := got[k]; !ok || gv != wv {
					t.Fatalf("configMapFromJSON(%q)[%q] = %v, want %v", tc.in, k, gv, wv)
				}
			}
		})
	}
}

func TestRuntimeLookup(t *testing.T) {
	store := &stubStore{plugins: map[string]config.Plugin{"tool/test": {ID: "tool/test", Enabled: true}}}
	host := New(store, WithListenerCap(allowAllListenerCap))
	host.RegisterPluginID("tool/test")
	host.AddRuntime(pkgplugins.RuntimeSpec{PluginID: "tool/test", Name: "main", Build: func(ctx pkgplugins.RuntimeContext) (pkgplugins.Runtime, error) {
		return runtimeStub{apply: func(context.Context, pkgplugins.PluginState) error { return nil }}, nil
	}})
	ctx := t.Context()
	if err := host.ApplyPlugin(ctx, "tool/test"); err != nil {
		t.Fatal(err)
	}
	handle, ok := host.Runtime().Get(ctx, "tool/test", "main")
	if !ok {
		t.Fatal("expected runtime handle")
	}
	snap, err := handle.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if snap.State != pkgplugins.RuntimeStateRunning {
		t.Fatalf("unexpected state: %s", snap.State)
	}
}

func TestRuntimeLookupKeepsChannelInstancesDistinct(t *testing.T) {
	store := &stubStore{plugins: map[string]config.Plugin{
		"channel/feishu": {ID: "channel/feishu", Kind: config.PluginKindChannel, Name: "feishu", Enabled: true},
	}}
	host := New(store, WithListenerCap(allowAllListenerCap))
	host.RegisterPluginID("channel/feishu")
	instances := map[string]*channelInstanceRuntime{}
	builds := map[string]int{}
	host.AddRuntime(pkgplugins.RuntimeSpec{
		PluginID: "channel/feishu",
		Name:     "bot",
		Build: func(rc pkgplugins.RuntimeContext) (pkgplugins.Runtime, error) {
			instanceID := rc.State.ID
			builds[instanceID]++
			runtime := &channelInstanceRuntime{id: instanceID}
			instances[instanceID] = runtime
			return runtime, nil
		},
	})

	ctx := t.Context()
	first := config.Channel{
		ID: "feishu-first", Type: "feishu", AgentID: "agent-first", Enabled: true,
		Config: `{"app_id":"app-first","app_secret":"secret-first"}`,
	}
	second := config.Channel{
		ID: "feishu-second", Type: "feishu", AgentID: "agent-second", Enabled: true,
		Config: `{"app_id":"app-second","app_secret":"secret-second"}`,
	}
	store.channels = map[string][]config.Channel{"feishu": {first, second}}
	if err := host.ApplyChannel(ctx, first); err != nil {
		t.Fatalf("ApplyChannel(first): %v", err)
	}
	if err := host.ApplyChannel(ctx, second); err != nil {
		t.Fatalf("ApplyChannel(second): %v", err)
	}

	firstHandle, ok := host.Runtime().Get(ctx, first.ID, "bot")
	if !ok {
		t.Fatal("missing first channel runtime")
	}
	secondHandle, ok := host.Runtime().Lookup(ctx, second.ID, "bot")
	if !ok {
		t.Fatal("missing second channel runtime")
	}
	assertChannelRuntimeSnapshot(t, ctx, firstHandle, first.ID, "app-first")
	assertChannelRuntimeSnapshot(t, ctx, secondHandle, second.ID, "app-second")
	assertChannelRuntimeCredentials(t, instances[first.ID], first.ID, "app-first", "secret-first")
	assertChannelRuntimeCredentials(t, instances[second.ID], second.ID, "app-second", "secret-second")
	if _, ok := host.Runtime().Get(ctx, "channel/feishu", "bot"); ok {
		t.Fatal("platform plugin ID must not select an arbitrary channel instance")
	}
	if _, ok := host.Runtime().Lookup(ctx, "channel/feishu", "bot"); ok {
		t.Fatal("platform plugin ID must not select an arbitrary channel instance")
	}

	first.Config = `{"app_id":"app-first-updated","app_secret":"secret-first-updated"}`
	store.channels["feishu"][0] = first
	if err := host.ApplyChannel(ctx, first); err != nil {
		t.Fatalf("ApplyChannel(first update): %v", err)
	}
	if builds[first.ID] != 1 || builds[second.ID] != 1 {
		t.Fatalf("runtime builds = %#v, want one build per instance", builds)
	}
	if instances[first.ID].applies != 2 {
		t.Fatalf("first runtime applies = %d, want 2", instances[first.ID].applies)
	}
	if instances[second.ID].applies != 1 {
		t.Fatalf("second runtime applies = %d, want 1 after first update", instances[second.ID].applies)
	}
	assertChannelRuntimeCredentials(t, instances[first.ID], first.ID, "app-first-updated", "secret-first-updated")
	assertChannelRuntimeCredentials(t, instances[second.ID], second.ID, "app-second", "secret-second")
	assertChannelRuntimeSnapshot(t, ctx, secondHandle, second.ID, "app-second")
}

func TestApplyPluginChannelUsesExactInstances(t *testing.T) {
	store := &stubStore{
		plugins: map[string]config.Plugin{
			"channel/feishu": {ID: "channel/feishu", Kind: config.PluginKindChannel, Name: "feishu", Enabled: true},
		},
		channels: map[string][]config.Channel{
			"feishu": {
				{ID: "feishu-first", Type: "feishu", Enabled: true, Config: `{"app_id":"first"}`},
				{ID: "feishu-second", Type: "feishu", Enabled: true, Config: `{"app_id":"second"}`},
			},
		},
	}
	host := New(store, WithListenerCap(allowAllListenerCap))
	host.RegisterPluginID("channel/feishu")
	builds := map[string]int{}
	host.AddRuntime(pkgplugins.RuntimeSpec{
		PluginID: "channel/feishu",
		Name:     "bot",
		Build: func(rc pkgplugins.RuntimeContext) (pkgplugins.Runtime, error) {
			builds[rc.State.ID]++
			return &channelInstanceRuntime{id: rc.State.ID}, nil
		},
	})
	if err := host.ApplyPlugin(t.Context(), "channel/feishu"); err != nil {
		t.Fatal(err)
	}
	if builds["feishu-first"] != 1 || builds["feishu-second"] != 1 || len(builds) != 2 {
		t.Fatalf("channel runtime builds = %#v, want one per exact instance", builds)
	}
	if _, ok := host.Runtime().Get(t.Context(), "channel/feishu", "bot"); ok {
		t.Fatal("platform plugin ID must not create a hidden runtime")
	}
	for _, id := range []string{"feishu-first", "feishu-second"} {
		if _, ok := host.Runtime().Get(t.Context(), id, "bot"); !ok {
			t.Fatalf("missing runtime for channel %s", id)
		}
	}
}

func TestApplyPluginChannelWithNoInstancesDoesNotStartRuntime(t *testing.T) {
	store := &stubStore{
		plugins:  map[string]config.Plugin{"channel/feishu": {ID: "channel/feishu", Enabled: true}},
		channels: map[string][]config.Channel{"feishu": {}},
	}
	host := New(store, WithListenerCap(allowAllListenerCap))
	host.RegisterPluginID("channel/feishu")
	builds := 0
	host.AddRuntime(pkgplugins.RuntimeSpec{
		PluginID: "channel/feishu",
		Name:     "bot",
		Build: func(pkgplugins.RuntimeContext) (pkgplugins.Runtime, error) {
			builds++
			return &channelInstanceRuntime{}, nil
		},
	})
	if err := host.ApplyPlugin(t.Context(), "channel/feishu"); err != nil {
		t.Fatal(err)
	}
	if builds != 0 {
		t.Fatalf("runtime builds = %d, want 0 for an empty channel inventory", builds)
	}
	if _, ok := host.Runtime().Get(t.Context(), "channel/feishu", "bot"); ok {
		t.Fatal("empty channel inventory must not create a hidden runtime")
	}
}

func TestListenerCapIsScopedToExactChannelInstance(t *testing.T) {
	store := &stubStore{plugins: map[string]config.Plugin{
		"channel/feishu": {ID: "channel/feishu", Kind: config.PluginKindChannel, Enabled: true},
	}, channels: map[string][]config.Channel{"feishu": {}}}
	var calls []string
	host := New(store, WithListenerCap(func(_ context.Context, pluginID, agentID string) (bool, error) {
		calls = append(calls, pluginID+"/"+agentID)
		return agentID != "agent-blocked", nil
	}))
	host.RegisterPluginID("channel/feishu")
	instances := map[string]*channelInstanceRuntime{}
	host.AddRuntime(pkgplugins.RuntimeSpec{
		PluginID: "channel/feishu",
		Name:     "bot",
		Build: func(rc pkgplugins.RuntimeContext) (pkgplugins.Runtime, error) {
			runtime := &channelInstanceRuntime{id: rc.State.ID}
			instances[rc.State.ID] = runtime
			return runtime, nil
		},
	})

	first := config.Channel{ID: "feishu-first", Type: "feishu", AgentID: "agent-first", Enabled: true}
	blocked := config.Channel{ID: "feishu-blocked", Type: "feishu", AgentID: "agent-blocked", Enabled: true}
	store.channels["feishu"] = []config.Channel{first, blocked}
	if err := host.ApplyChannel(t.Context(), first); err != nil {
		t.Fatalf("ApplyChannel(first): %v", err)
	}
	if err := host.ApplyChannel(t.Context(), blocked); err != nil {
		t.Fatalf("ApplyChannel(blocked): %v", err)
	}

	if got, want := strings.Join(calls, ","), "channel/feishu/agent-first,channel/feishu/agent-blocked"; got != want {
		t.Fatalf("listener cap calls = %q, want %q", got, want)
	}
	if !instances[first.ID].state.Enabled {
		t.Fatal("allowed channel instance was disabled by another instance's cap")
	}
	if instances[blocked.ID].state.Enabled {
		t.Fatal("blocked channel instance remained enabled")
	}
}

func TestListenerCapErrorLeavesExistingRuntimeUntouched(t *testing.T) {
	store := &stubStore{plugins: map[string]config.Plugin{
		"channel/feishu": {ID: "channel/feishu", Kind: config.PluginKindChannel, Enabled: true},
	}, channels: map[string][]config.Channel{"feishu": {{ID: "feishu-first", Type: "feishu", AgentID: "agent-first", Enabled: true, Config: `{"app_id":"old"}`}}}}
	capErr := errors.New("cap unavailable")
	host := New(store, WithListenerCap(func(context.Context, string, string) (bool, error) {
		return true, nil
	}))
	host.RegisterPluginID("channel/feishu")
	instance := &channelInstanceRuntime{id: "feishu-first"}
	host.AddRuntime(pkgplugins.RuntimeSpec{
		PluginID: "channel/feishu",
		Name:     "bot",
		Build:    func(pkgplugins.RuntimeContext) (pkgplugins.Runtime, error) { return instance, nil },
	})
	channel := config.Channel{ID: "feishu-first", Type: "feishu", AgentID: "agent-first", Enabled: true, Config: `{"app_id":"old"}`}
	if err := host.ApplyChannel(t.Context(), channel); err != nil {
		t.Fatalf("initial ApplyChannel: %v", err)
	}
	host.SetListenerCap(func(context.Context, string, string) (bool, error) { return false, capErr })
	channel.Config = `{"app_id":"new"}`
	store.channels["feishu"][0] = channel
	if err := host.ApplyChannel(t.Context(), channel); !errors.Is(err, capErr) {
		t.Fatalf("ApplyChannel error = %v, want %v", err, capErr)
	}
	if instance.applies != 1 || instance.state.Config["app_id"] != "old" || !instance.state.Enabled {
		t.Fatalf("runtime changed after cap error: applies=%d state=%#v", instance.applies, instance.state)
	}
}

func TestApplyChannelFailsClosedWhenListenerCapIsMissing(t *testing.T) {
	host := New(&stubStore{channels: map[string][]config.Channel{"feishu": {{ID: "feishu-a", Type: "feishu", Enabled: true}}}})
	host.RegisterPluginID("channel/feishu")
	builds := 0
	host.AddRuntime(pkgplugins.RuntimeSpec{
		PluginID: "channel/feishu",
		Name:     "bot",
		Build: func(pkgplugins.RuntimeContext) (pkgplugins.Runtime, error) {
			builds++
			return &channelInstanceRuntime{}, nil
		},
	})

	err := host.ApplyChannel(t.Context(), config.Channel{ID: "feishu-a", Type: "feishu", Enabled: true})
	if !errors.Is(err, ErrListenerCapUnavailable) {
		t.Fatalf("ApplyChannel error = %v, want ErrListenerCapUnavailable", err)
	}
	if builds != 0 {
		t.Fatalf("runtime builds = %d, want 0 when listener cap is missing", builds)
	}
}

func TestReconcileChannelsAttemptsEveryCommittedInstance(t *testing.T) {
	store := &channelInventoryStore{
		stubStore: &stubStore{plugins: map[string]config.Plugin{
			"channel/feishu": {ID: "channel/feishu", Kind: config.PluginKindChannel, Enabled: true},
		}},
		channels: []config.Channel{
			{ID: "feishu-a", Type: "feishu", Enabled: true},
			{ID: "feishu-b", Type: "feishu", Enabled: true},
		},
	}
	host := New(store, WithListenerCap(allowAllListenerCap))
	host.RegisterPluginID("channel/feishu")
	var applied []string
	host.AddRuntime(pkgplugins.RuntimeSpec{
		PluginID: "channel/feishu",
		Name:     "bot",
		Build: func(rc pkgplugins.RuntimeContext) (pkgplugins.Runtime, error) {
			return runtimeStub{apply: func(_ context.Context, desired pkgplugins.PluginState) error {
				applied = append(applied, desired.ID)
				if desired.ID == "feishu-a" {
					return errors.New("listener close failed")
				}
				return nil
			}}, nil
		},
	})

	err := host.ReconcileChannels(t.Context())
	if err == nil || !strings.Contains(err.Error(), "feishu-a") {
		t.Fatalf("ReconcileChannels error = %v, want feishu-a failure", err)
	}
	if got, want := strings.Join(applied, ","), "feishu-a,feishu-b"; got != want {
		t.Fatalf("reconciled instances = %q, want %q", got, want)
	}
}

func TestReconcileChannelSerializesFreshUpdateAfterOldApply(t *testing.T) {
	channel := config.Channel{ID: "feishu-a", Type: "feishu", Enabled: true, Config: `{"app_id":"old"}`}
	store := newReconcileRaceStore(channel)
	host := New(store, WithListenerCap(allowAllListenerCap))
	host.RegisterPluginID("channel/feishu")
	runtime := newBarrierChannelRuntime()
	host.AddRuntime(pkgplugins.RuntimeSpec{
		PluginID: "channel/feishu",
		Name:     "bot",
		Build:    func(pkgplugins.RuntimeContext) (pkgplugins.Runtime, error) { return runtime, nil },
	})
	if err := host.ReconcileChannel(t.Context(), channel.ID); err != nil {
		t.Fatalf("initial reconcile: %v", err)
	}
	runtime.blockNext.Store(true)
	oldDone := make(chan error, 1)
	go func() { oldDone <- host.ReconcileChannel(t.Context(), channel.ID) }()
	<-runtime.entered
	updated := channel
	updated.Config = `{"app_id":"new"}`
	store.set(updated)
	newDone := make(chan error, 1)
	go func() { newDone <- host.ReconcileChannel(t.Context(), channel.ID) }()
	select {
	case err := <-newDone:
		t.Fatalf("new reconcile completed before old apply released: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(runtime.release)
	if err := <-oldDone; err != nil {
		t.Fatalf("old reconcile: %v", err)
	}
	if err := <-newDone; err != nil {
		t.Fatalf("new reconcile: %v", err)
	}
	if got := runtime.lastConfig(); got != `{"app_id":"new"}` {
		t.Fatalf("final runtime config = %s, want new durable credentials", got)
	}
}

func TestReconcileChannelStopsAfterConcurrentDelete(t *testing.T) {
	channel := config.Channel{ID: "feishu-a", Type: "feishu", Enabled: true, Config: `{"app_id":"old"}`}
	store := newReconcileRaceStore(channel)
	host := New(store, WithListenerCap(allowAllListenerCap))
	host.RegisterPluginID("channel/feishu")
	runtime := newBarrierChannelRuntime()
	host.AddRuntime(pkgplugins.RuntimeSpec{
		PluginID: "channel/feishu",
		Name:     "bot",
		Build:    func(pkgplugins.RuntimeContext) (pkgplugins.Runtime, error) { return runtime, nil },
	})
	if err := host.ReconcileChannel(t.Context(), channel.ID); err != nil {
		t.Fatalf("initial reconcile: %v", err)
	}
	runtime.blockNext.Store(true)
	oldDone := make(chan error, 1)
	go func() { oldDone <- host.ReconcileChannel(t.Context(), channel.ID) }()
	<-runtime.entered
	store.delete()
	deletedDone := make(chan error, 1)
	go func() { deletedDone <- host.ReconcileChannel(t.Context(), channel.ID) }()
	select {
	case err := <-deletedDone:
		t.Fatalf("delete reconcile completed before old apply released: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(runtime.release)
	if err := <-oldDone; err != nil {
		t.Fatalf("old reconcile: %v", err)
	}
	if err := <-deletedDone; err != nil {
		t.Fatalf("delete reconcile: %v", err)
	}
	if _, ok := host.Runtime().Get(t.Context(), channel.ID, "bot"); ok {
		t.Fatal("deleted channel runtime still registered")
	}
	if got := runtime.stops.Load(); got == 0 {
		t.Fatal("deleted channel runtime was not stopped")
	}
}

type reconcileRaceStore struct {
	*stubStore
	mu      sync.Mutex
	channel config.Channel
	deleted bool
}

func newReconcileRaceStore(channel config.Channel) *reconcileRaceStore {
	return &reconcileRaceStore{
		stubStore: &stubStore{plugins: map[string]config.Plugin{
			"channel/feishu": {ID: "channel/feishu", Kind: config.PluginKindChannel, Name: "feishu", Enabled: true},
		}},
		channel: channel,
	}
}

func (s *reconcileRaceStore) GetChannel(context.Context, string) (config.Channel, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.deleted {
		return config.Channel{}, config.ErrChannelNotFound
	}
	return s.channel, nil
}

func (s *reconcileRaceStore) ListChannels(context.Context) ([]config.Channel, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.deleted {
		return nil, nil
	}
	return []config.Channel{s.channel}, nil
}

func (s *reconcileRaceStore) ListChannelsByType(ctx context.Context, _ string) ([]config.Channel, error) {
	channels, err := s.ListChannels(ctx)
	return channels, err
}

func (s *reconcileRaceStore) set(channel config.Channel) {
	s.mu.Lock()
	s.channel = channel
	s.mu.Unlock()
}

func (s *reconcileRaceStore) delete() {
	s.mu.Lock()
	s.deleted = true
	s.mu.Unlock()
}

type barrierChannelRuntime struct {
	mu        sync.Mutex
	states    []pkgplugins.PluginState
	entered   chan struct{}
	release   chan struct{}
	blockNext atomic.Bool
	stops     atomic.Int32
}

func newBarrierChannelRuntime() *barrierChannelRuntime {
	return &barrierChannelRuntime{entered: make(chan struct{}), release: make(chan struct{})}
}

func (r *barrierChannelRuntime) Apply(ctx context.Context, desired pkgplugins.PluginState) error {
	r.mu.Lock()
	r.states = append(r.states, desired.Clone())
	r.mu.Unlock()
	if r.blockNext.Swap(false) {
		close(r.entered)
		select {
		case <-r.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

func (r *barrierChannelRuntime) Start(ctx context.Context, desired pkgplugins.PluginState) error {
	return r.Apply(ctx, desired)
}

func (r *barrierChannelRuntime) Reconcile(ctx context.Context, desired pkgplugins.PluginState) error {
	return r.Apply(ctx, desired)
}

func (r *barrierChannelRuntime) Stop(context.Context) error {
	r.stops.Add(1)
	return nil
}

func (r *barrierChannelRuntime) Snapshot(context.Context) (pkgplugins.RuntimeStatus, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.states) == 0 {
		return pkgplugins.RuntimeStatus{State: pkgplugins.RuntimeStateStopped}, nil
	}
	return pkgplugins.RuntimeStatus{State: pkgplugins.RuntimeStateRunning, Metadata: map[string]any{"config": r.states[len(r.states)-1].Config}}, nil
}

func (r *barrierChannelRuntime) Status(ctx context.Context) (pkgplugins.RuntimeStatus, error) {
	return r.Snapshot(ctx)
}

func (r *barrierChannelRuntime) lastConfig() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.states) == 0 {
		return ""
	}
	raw, _ := json.Marshal(r.states[len(r.states)-1].Config)
	return string(raw)
}

type channelInventoryStore struct {
	*stubStore
	channels []config.Channel
}

func (s *channelInventoryStore) ListChannels(context.Context) ([]config.Channel, error) {
	return s.channels, nil
}

func (s *channelInventoryStore) GetChannel(_ context.Context, id string) (config.Channel, error) {
	for _, channel := range s.channels {
		if channel.ID == id {
			return channel, nil
		}
	}
	return config.Channel{}, config.ErrChannelNotFound
}

func TestApplyPluginChannelReturnsInventoryError(t *testing.T) {
	readErr := errors.New("channel inventory unavailable")
	store := &stubStore{
		plugins:       map[string]config.Plugin{"channel/feishu": {ID: "channel/feishu", Enabled: true}},
		channelsError: readErr,
	}
	host := New(store)
	host.RegisterPluginID("channel/feishu")
	builds := 0
	host.AddRuntime(pkgplugins.RuntimeSpec{
		PluginID: "channel/feishu",
		Name:     "bot",
		Build: func(pkgplugins.RuntimeContext) (pkgplugins.Runtime, error) {
			builds++
			return &channelInstanceRuntime{}, nil
		},
	})
	err := host.ApplyPlugin(t.Context(), "channel/feishu")
	if !errors.Is(err, readErr) {
		t.Fatalf("inventory error = %v", err)
	}
	if builds != 0 {
		t.Fatalf("runtime builds = %d, want 0 after inventory error", builds)
	}
}

func assertChannelRuntimeSnapshot(t *testing.T, ctx context.Context, handle pkgplugins.RuntimeHandle, instanceID, appID string) {
	t.Helper()
	snapshot, err := handle.Snapshot(ctx)
	if err != nil {
		t.Fatalf("Snapshot(%s): %v", instanceID, err)
	}
	if snapshot.Metadata["instance_id"] != instanceID || snapshot.Metadata["app_id"] != appID {
		t.Fatalf("snapshot for %s = %#v, want instance/app %s/%s", instanceID, snapshot.Metadata, instanceID, appID)
	}
}

func assertChannelRuntimeCredentials(t *testing.T, runtime *channelInstanceRuntime, instanceID, appID, appSecret string) {
	t.Helper()
	if runtime.state.Config["app_id"] != appID || runtime.state.Config["app_secret"] != appSecret {
		t.Fatalf("credentials for %s do not match its own applied state", instanceID)
	}
}

type channelInstanceRuntime struct {
	id      string
	state   pkgplugins.PluginState
	applies int
}

func (r *channelInstanceRuntime) Apply(_ context.Context, desired pkgplugins.PluginState) error {
	r.applies++
	r.state = desired.Clone()
	return nil
}

func (r *channelInstanceRuntime) Start(ctx context.Context, desired pkgplugins.PluginState) error {
	return r.Apply(ctx, desired)
}

func (r *channelInstanceRuntime) Reconcile(ctx context.Context, desired pkgplugins.PluginState) error {
	return r.Apply(ctx, desired)
}

func (*channelInstanceRuntime) Stop(context.Context) error { return nil }

func (r *channelInstanceRuntime) Snapshot(context.Context) (pkgplugins.RuntimeStatus, error) {
	return pkgplugins.RuntimeStatus{
		State: pkgplugins.RuntimeStateRunning,
		Metadata: map[string]any{
			"instance_id": r.id,
			"app_id":      r.state.Config["app_id"],
		},
	}, nil
}

func (r *channelInstanceRuntime) Status(ctx context.Context) (pkgplugins.RuntimeStatus, error) {
	return r.Snapshot(ctx)
}

// TestRuntimeMissingKeyReturnsFalse ensures Get returns (nil,false) for
// unknown runtime keys.
func TestRuntimeMissingKeyReturnsFalse(t *testing.T) {
	host := New(&stubStore{plugins: map[string]config.Plugin{}})
	if _, ok := host.Runtime().Get(context.Background(), "tool/test", "main"); ok {
		t.Fatal("expected (nil, false) for unknown runtime key")
	}
}

func TestRuntimeHostListsChannelChatsFromChannelInstance(t *testing.T) {
	host := New(&stubStore{})
	host.runtimes.rt[runtimeKey{RuntimeID: "feishu-work", RuntimeName: "bot"}] = &runtimeEntry{
		managed: joinedChatRuntime{page: pkgchannel.JoinedChatPage{
			Chats: []pkgchannel.JoinedChat{{ID: "oc_1", Name: "Product"}},
		}},
	}

	page, err := host.ListChannelChats(context.Background(), "feishu-work", 100, "")
	if err != nil {
		t.Fatalf("ListChannelChats: %v", err)
	}
	if len(page.Chats) != 1 || page.Chats[0].ID != "oc_1" {
		t.Fatalf("chats = %#v, want oc_1", page.Chats)
	}
}

// TestShutdownReleasesLockBeforeStop ensures Stop() is called outside the
// RuntimeHost mutex; the stub's Stop reads back via Get to verify no deadlock.
func TestStopDuringApplyCleansLateRuntime(t *testing.T) {
	store := &stubStore{plugins: map[string]config.Plugin{"tool/test": {ID: "tool/test", Enabled: true}}}
	host := New(store)
	host.RegisterPluginID("tool/test")
	entered := make(chan struct{})
	release := make(chan struct{})
	var stops atomic.Int32
	host.AddRuntime(pkgplugins.RuntimeSpec{PluginID: "tool/test", Name: "main", Build: func(pkgplugins.RuntimeContext) (pkgplugins.Runtime, error) {
		return &blockingApplyRuntime{entered: entered, release: release, stops: &stops}, nil
	}})

	applyDone := make(chan error, 1)
	go func() { applyDone <- host.ApplyPlugin(context.Background(), "tool/test") }()
	<-entered
	if err := host.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	close(release)
	if err := <-applyDone; err == nil || !strings.Contains(err.Error(), "revoked while starting") {
		t.Fatalf("Apply error=%v, want revoked generation", err)
	}
	if got := stops.Load(); got < 2 {
		t.Fatalf("Stop calls=%d, want initial stop plus late-runtime cleanup", got)
	}
	if _, ok := host.Runtime().Get(context.Background(), "tool/test", "main"); ok {
		t.Fatal("revoked runtime escaped into the active table")
	}
}

type blockingApplyRuntime struct {
	entered chan struct{}
	release chan struct{}
	stops   *atomic.Int32
}

type joinedChatRuntime struct {
	page pkgchannel.JoinedChatPage
}

func (joinedChatRuntime) Apply(context.Context, pkgplugins.PluginState) error { return nil }
func (r joinedChatRuntime) Start(ctx context.Context, state pkgplugins.PluginState) error {
	return r.Apply(ctx, state)
}

func (r joinedChatRuntime) Reconcile(ctx context.Context, state pkgplugins.PluginState) error {
	return r.Apply(ctx, state)
}
func (joinedChatRuntime) Stop(context.Context) error { return nil }
func (joinedChatRuntime) Snapshot(context.Context) (pkgplugins.RuntimeStatus, error) {
	return pkgplugins.RuntimeStatus{State: pkgplugins.RuntimeStateRunning}, nil
}

func (r joinedChatRuntime) Status(ctx context.Context) (pkgplugins.RuntimeStatus, error) {
	return r.Snapshot(ctx)
}

func (r joinedChatRuntime) ListJoinedChats(context.Context, int, string) (pkgchannel.JoinedChatPage, error) {
	return r.page, nil
}

func (r *blockingApplyRuntime) Apply(context.Context, pkgplugins.PluginState) error {
	close(r.entered)
	<-r.release
	return nil
}
func (r *blockingApplyRuntime) Start(context.Context, pkgplugins.PluginState) error     { return nil }
func (r *blockingApplyRuntime) Reconcile(context.Context, pkgplugins.PluginState) error { return nil }
func (r *blockingApplyRuntime) Stop(context.Context) error                              { r.stops.Add(1); return nil }
func (r *blockingApplyRuntime) Snapshot(context.Context) (pkgplugins.RuntimeStatus, error) {
	return pkgplugins.RuntimeStatus{State: pkgplugins.RuntimeStateRunning}, nil
}

func (r *blockingApplyRuntime) Status(ctx context.Context) (pkgplugins.RuntimeStatus, error) {
	return r.Snapshot(ctx)
}

func TestShutdownReleasesLockBeforeStop(t *testing.T) {
	store := &stubStore{plugins: map[string]config.Plugin{"tool/test": {ID: "tool/test", Enabled: true}}}
	host := New(store)
	host.RegisterPluginID("tool/test")
	host.AddRuntime(pkgplugins.RuntimeSpec{PluginID: "tool/test", Name: "main", Build: func(pkgplugins.RuntimeContext) (pkgplugins.Runtime, error) {
		return reentrantRuntime{host: host}, nil
	}})
	ctx := context.Background()
	if err := host.ApplyPlugin(ctx, "tool/test"); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- host.Stop(ctx) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Shutdown: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Shutdown deadlocked: lock held during Stop")
	}
}

// reentrantRuntime's Stop callbacks back into RuntimeHost.Get; if Shutdown held
// the lock during Stop, this would deadlock.
type reentrantRuntime struct{ host *Host }

func (r reentrantRuntime) Apply(context.Context, pkgplugins.PluginState) error     { return nil }
func (r reentrantRuntime) Start(context.Context, pkgplugins.PluginState) error     { return nil }
func (r reentrantRuntime) Reconcile(context.Context, pkgplugins.PluginState) error { return nil }
func (r reentrantRuntime) Stop(ctx context.Context) error {
	// During Stop, Shutdown has already removed the entry; this lookup should
	// return (nil,false) without deadlocking on the RuntimeHost mutex.
	_, _ = r.host.Runtime().Get(ctx, "tool/test", "main")
	return nil
}

func (r reentrantRuntime) Snapshot(context.Context) (pkgplugins.RuntimeStatus, error) {
	return pkgplugins.RuntimeStatus{State: pkgplugins.RuntimeStateRunning}, nil
}

func (r reentrantRuntime) Status(ctx context.Context) (pkgplugins.RuntimeStatus, error) {
	return r.Snapshot(ctx)
}
