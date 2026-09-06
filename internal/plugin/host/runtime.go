package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/CherryHQ/stella/internal/platform/config"
	pkgchannel "github.com/CherryHQ/stella/pkg/channel"
	pkgplugins "github.com/CherryHQ/stella/pkg/plugins"
)

// runtimeKey identifies a managed runtime instance.
// Pair (RuntimeID, RuntimeName) — RuntimeID is the channel.ID for managed
// channel runtimes and the plugin ID for other plugin-scoped runtimes.
type runtimeKey struct {
	RuntimeID   string
	RuntimeName string
}

type runtimeEntry struct {
	reg     pkgplugins.RuntimeSpec
	managed pkgplugins.Runtime
	applyMu sync.Mutex
}

// RuntimeHost owns the map of managed runtime instances.
type RuntimeHost struct {
	host *Host
	mu   sync.RWMutex
	rt   map[runtimeKey]*runtimeEntry

	// applyMu serializes lifecycle state transitions and map membership. A
	// per-entry mutex serializes Build/Apply with drain-time Quiesce.
	applyMu  sync.Mutex
	quiesced bool

	// channelLocks serialize the durable read and runtime apply for one
	// channel ID. Keeping this boundary per instance lets unrelated channels
	// reconcile concurrently while preventing an old apply from racing a CRUD
	// follow-up for the same row.
	channelLocksMu sync.Mutex
	channelLocks   map[string]*sync.Mutex
}

func NewRuntimeHost(host *Host) *RuntimeHost {
	return &RuntimeHost{host: host, rt: map[runtimeKey]*runtimeEntry{}, channelLocks: map[string]*sync.Mutex{}}
}

func configMapFromJSON(raw string) map[string]any {
	if raw == "" {
		return map[string]any{}
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(raw), &out); err != nil || out == nil {
		return map[string]any{}
	}
	return out
}

// Get resolves a managed runtime by its exact runtime identity. For channel
// runtimes, runtimeID is the persisted channel instance ID, never the platform
// plugin ID, because multiple instances may share one platform.
func (h *RuntimeHost) Get(_ context.Context, runtimeID string, runtimeName string) (pkgplugins.RuntimeHandle, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if entry := h.rt[runtimeKey{RuntimeID: runtimeID, RuntimeName: runtimeName}]; entry != nil && entry.managed != nil {
		return runtimeHandle{entry: entry}, true
	}
	return nil, false
}

// Lookup is an alias for Get to match the pkgplugins.RuntimeLookup interface.
func (h *RuntimeHost) Lookup(ctx context.Context, runtimeID string, runtimeName string) (pkgplugins.RuntimeHandle, bool) {
	return h.Get(ctx, runtimeID, runtimeName)
}

// ListChannelChats reads the optional joined-chat capability from a running
// channel instance. Runtime selection stays in the plugin host so transport
// code never imports a concrete channel plugin.
func (h *RuntimeHost) ListChannelChats(ctx context.Context, channelID string, pageSize int, pageToken string) (pkgchannel.JoinedChatPage, error) {
	h.mu.RLock()
	var managed pkgplugins.Runtime
	for key, entry := range h.rt {
		if key.RuntimeID == channelID && entry.managed != nil {
			managed = entry.managed
			break
		}
	}
	h.mu.RUnlock()

	lister, ok := managed.(pkgchannel.JoinedChatLister)
	if !ok {
		return pkgchannel.JoinedChatPage{}, pkgchannel.ErrJoinedChatListingUnavailable
	}
	return lister.ListJoinedChats(ctx, pageSize, pageToken)
}

func (h *RuntimeHost) ApplyPlugin(ctx context.Context, pluginID string) error {
	desired, err := h.host.DesiredState(ctx, pluginID)
	if err != nil {
		return err
	}
	if channelType, ok := strings.CutPrefix(pluginID, config.PluginKindChannel+"/"); ok {
		channels, err := h.host.store.ListChannelsByType(ctx, channelType)
		if err != nil {
			return fmt.Errorf("list channel instances for %s: %w", channelType, err)
		}
		for _, channel := range channels {
			if err := h.ReconcileChannel(ctx, channel.ID); err != nil {
				return err
			}
		}
		// A channel plugin has no platform-wide runtime. An empty inventory is a
		// valid no-op and must never fabricate a hidden instance.
		return nil
	}
	regs := h.registrations(pluginID)
	for _, reg := range regs {
		if err := h.applyOne(ctx, reg, desired); err != nil {
			return err
		}
	}
	return nil
}

// channelPlatformDisabled reports whether an admin has switched a whole channel
// platform off. Only a stored override row counts: the builtin default for a
// channel plugin is "disabled" because a platform with no channel has nothing to
// run, and reading that default as a veto would mean every channel instance
// needed its plugin row flipped on first — a deployment-wide write as the price
// of creating one channel. A channel instance is governed by its own Enabled
// column; this is the kill switch above it.
func (h *RuntimeHost) channelPlatformDisabled(ctx context.Context, channelType string) (bool, error) {
	overrides, err := h.host.store.ListPluginOverrides(ctx)
	if err != nil {
		return false, err
	}
	pluginID := config.PluginID(config.PluginKindChannel, channelType)
	for _, override := range overrides {
		if override.ID == pluginID {
			return !override.Enabled, nil
		}
	}
	return false, nil
}

// ReconcileChannel reads the committed channel row by its exact ID and then
// applies that fresh value while holding the instance reconcile lock. A
// missing row is a committed deletion, so every runtime for that ID is
// stopped instead of applying a caller's stale struct.
func (h *RuntimeHost) ReconcileChannel(ctx context.Context, channelID string) error {
	if channelID == "" {
		return errors.New("channel ID is required")
	}
	lock := h.channelLock(channelID)
	lock.Lock()
	defer lock.Unlock()

	channel, err := h.host.store.GetChannel(ctx, channelID)
	if err != nil {
		if errors.Is(err, config.ErrChannelNotFound) {
			return h.stopChannel(ctx, channelID)
		}
		return fmt.Errorf("get channel %q for reconciliation: %w", channelID, err)
	}
	if channel.ID == "" {
		return fmt.Errorf("get channel %q for reconciliation: empty channel ID", channelID)
	}
	return h.applyChannel(ctx, channel)
}

func (h *RuntimeHost) channelLock(channelID string) *sync.Mutex {
	h.channelLocksMu.Lock()
	defer h.channelLocksMu.Unlock()
	lock := h.channelLocks[channelID]
	if lock == nil {
		lock = &sync.Mutex{}
		h.channelLocks[channelID] = lock
	}
	return lock
}

// ApplyChannel is the compatibility entry point for callers that already have
// a channel-shaped value. Only its ID is trusted; the durable row is reread by
// ReconcileChannel before any runtime sees credentials or enablement.
func (h *RuntimeHost) ApplyChannel(ctx context.Context, channel config.Channel) error {
	return h.ReconcileChannel(ctx, channel.ID)
}

func (h *RuntimeHost) applyChannel(ctx context.Context, channel config.Channel) error {
	if channel.Type == "" {
		channel.Type = channel.ID
	}
	pluginID := config.PluginID(config.PluginKindChannel, channel.Type)
	off, err := h.channelPlatformDisabled(ctx, channel.Type)
	if err != nil {
		return err
	}
	if off {
		channel.Enabled = false
	}
	allowed, err := h.host.listenerAllowed(ctx, pluginID, channel.AgentID)
	if err != nil {
		// The gate is checked before touching the managed runtime. This preserves
		// an already-running shared listener when policy storage is unavailable.
		return err
	}
	if !allowed {
		channel.Enabled = false
	}
	regs := h.registrations(pluginID)
	desired := pkgplugins.PluginState{
		ID:      channel.ID,
		Enabled: channel.Enabled,
		Config:  configMapFromJSON(channel.Config),
	}
	for _, reg := range regs {
		if err := h.applyOneWithKey(ctx, reg, channel.ID, desired); err != nil {
			return err
		}
	}
	return nil
}

// stopChannel evicts and stops every runtime belonging to one durable channel
// ID. Runtime apply code may observe the eviction and perform its own cleanup;
// Stop is therefore deliberately idempotent at this boundary.
func (h *RuntimeHost) stopChannel(ctx context.Context, channelID string) error {
	h.applyMu.Lock()
	h.mu.Lock()
	entries := make([]*runtimeEntry, 0)
	for key, entry := range h.rt {
		if key.RuntimeID == channelID {
			delete(h.rt, key)
			entries = append(entries, entry)
		}
	}
	h.mu.Unlock()
	h.applyMu.Unlock()

	var failures []error
	for _, entry := range entries {
		entry.applyMu.Lock()
		managed := entry.managed
		if managed != nil {
			if err := managed.Stop(ctx); err != nil {
				failures = append(failures, err)
			}
		}
		entry.applyMu.Unlock()
	}
	return errors.Join(failures...)
}

func (h *RuntimeHost) registrations(pluginID string) []pkgplugins.RuntimeSpec {
	h.host.mu.RLock()
	defer h.host.mu.RUnlock()
	regs := make([]pkgplugins.RuntimeSpec, 0, len(h.host.runtimeRegs))
	for _, reg := range h.host.runtimeRegs {
		if reg.PluginID == pluginID {
			regs = append(regs, reg)
		}
	}
	sort.Slice(regs, func(i, j int) bool { return regs[i].Name < regs[j].Name })
	return regs
}

func (h *RuntimeHost) applyOne(ctx context.Context, reg pkgplugins.RuntimeSpec, desired pkgplugins.PluginState) error {
	return h.applyOneWithKey(ctx, reg, reg.PluginID, desired)
}

func (h *RuntimeHost) applyOneWithKey(ctx context.Context, reg pkgplugins.RuntimeSpec, runtimeID string, desired pkgplugins.PluginState) error {
	key := runtimeKey{RuntimeID: runtimeID, RuntimeName: reg.Name}
	h.applyMu.Lock()
	if h.quiesced {
		h.applyMu.Unlock()
		return fmt.Errorf("runtime host is quiescing")
	}
	h.mu.Lock()
	entry := h.rt[key]
	if entry == nil {
		entry = &runtimeEntry{reg: reg}
		h.rt[key] = entry
	}
	h.mu.Unlock()
	h.applyMu.Unlock()

	entry.applyMu.Lock()
	defer entry.applyMu.Unlock()

	// Shutdown can evict the entry before this apply generation begins.
	h.applyMu.Lock()
	h.mu.RLock()
	current := !h.quiesced && h.rt[key] == entry
	h.mu.RUnlock()
	h.applyMu.Unlock()
	if !current {
		return fmt.Errorf("runtime %s/%s was revoked while starting", runtimeID, reg.Name)
	}

	h.mu.RLock()
	managed := entry.managed
	h.mu.RUnlock()
	if managed == nil {
		if err := h.host.verifyRuntimeCapabilities(reg.PluginID); err != nil {
			h.host.log.Error("refusing to start managed runtime: capability unavailable",
				"plugin", reg.PluginID, "runtime", reg.Name, "error", err)
			return fmt.Errorf("start runtime %s/%s: %w", runtimeID, reg.Name, err)
		}
		build := reg.Build
		if build == nil {
			return fmt.Errorf("runtime %s/%s has no builder", runtimeID, reg.Name)
		}
		created, err := build(pkgplugins.RuntimeContext{
			Platform: h.host.platform(reg.PluginID),
			State:    desired.Clone(),
		})
		if err != nil {
			return fmt.Errorf("create runtime %s/%s: %w", runtimeID, reg.Name, err)
		}
		managed = created
		h.mu.Lock()
		entry.managed = created
		h.mu.Unlock()
	}

	// Quiesce serializes through entry.applyMu; Stop evicts the generation before
	// teardown so a slow platform handshake cannot admit a runtime after shutdown.
	h.applyMu.Lock()
	h.mu.RLock()
	current = !h.quiesced && h.rt[key] == entry
	h.mu.RUnlock()
	h.applyMu.Unlock()
	if !current {
		stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := managed.Stop(stopCtx); err != nil {
			return fmt.Errorf("stop revoked runtime %s/%s: %w", runtimeID, reg.Name, err)
		}
		return fmt.Errorf("runtime %s/%s was revoked while starting", runtimeID, reg.Name)
	}
	if err := managed.Apply(ctx, desired.Clone()); err != nil {
		return fmt.Errorf("apply runtime %s/%s: %w", runtimeID, reg.Name, err)
	}

	// Stop may replace the runtime table while Apply performs platform I/O.
	// Recheck membership so a late-started poller cannot escape shutdown.
	h.applyMu.Lock()
	h.mu.RLock()
	current = !h.quiesced && h.rt[key] == entry
	h.mu.RUnlock()
	h.applyMu.Unlock()
	if !current {
		stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := managed.Stop(stopCtx); err != nil {
			return fmt.Errorf("stop revoked runtime %s/%s: %w", runtimeID, reg.Name, err)
		}
		return fmt.Errorf("runtime %s/%s was revoked while starting", runtimeID, reg.Name)
	}
	return nil
}

// ingressQuiescer is the optional two-phase-drain capability a managed runtime
// may implement. Quiesce stops NEW ingress (e.g. channel polling) while leaving
// already-accepted operations and any downstream registrations (e.g. notifier
// senders) intact until the final Stop. Runtimes that have no ingress to quiesce
// simply do not implement it; RuntimeHost skips them.
type ingressQuiescer interface {
	Quiesce(ctx context.Context)
}

// Quiesce invokes the optional ingress-quiescer on every managed runtime that
// implements it, without clearing the runtime table: the subsequent Stop still
// needs the entries to tear them down fully. This is the drain-phase counterpart
// to Stop — it halts new ingress while preserving accepted work.
func (h *RuntimeHost) Quiesce(ctx context.Context) {
	h.applyMu.Lock()
	if h.quiesced {
		h.applyMu.Unlock()
		return
	}
	h.quiesced = true

	h.mu.RLock()
	entries := make([]*runtimeEntry, 0, len(h.rt))
	for _, entry := range h.rt {
		entries = append(entries, entry)
	}
	h.mu.RUnlock()
	h.applyMu.Unlock()

	for _, entry := range entries {
		// Wait for an in-flight Apply before quiescing it, so it cannot start a
		// poller after the drain has begun.
		entry.applyMu.Lock()
		if entry.managed != nil {
			if q, ok := entry.managed.(ingressQuiescer); ok {
				q.Quiesce(ctx)
			}
		}
		entry.applyMu.Unlock()
	}
}

// Stop tears down every managed runtime permanently. It prevents any later
// Apply, which keeps a graceful drain from admitting new ingress. Entries are
// removed before Stop is called so no lock is held while runtime teardown runs.
func (h *RuntimeHost) Stop(ctx context.Context) error {
	h.applyMu.Lock()
	h.quiesced = true
	h.mu.Lock()
	entries := h.rt
	h.rt = map[runtimeKey]*runtimeEntry{}
	h.mu.Unlock()
	h.applyMu.Unlock()

	var lastErr error
	for _, entry := range entries {
		if entry.managed == nil {
			continue
		}
		if err := entry.managed.Stop(ctx); err != nil {
			lastErr = err
		}
	}
	return lastErr
}

func (h *RuntimeHost) Snapshot(ctx context.Context, runtimeID string, runtimeName string) (pkgplugins.RuntimeStatus, error) {
	handle, ok := h.Get(ctx, runtimeID, runtimeName)
	if !ok {
		return pkgplugins.RuntimeStatus{State: pkgplugins.RuntimeStateStopped, UpdatedAt: time.Now().UTC()}, nil
	}
	return handle.Snapshot(ctx)
}

type runtimeHandle struct{ entry *runtimeEntry }

func (h runtimeHandle) Snapshot(ctx context.Context) (pkgplugins.RuntimeStatus, error) {
	if h.entry == nil || h.entry.managed == nil {
		return pkgplugins.RuntimeStatus{State: pkgplugins.RuntimeStateStopped, UpdatedAt: time.Now().UTC()}, nil
	}
	return h.entry.managed.Snapshot(ctx)
}

func (h runtimeHandle) Status(ctx context.Context) (pkgplugins.RuntimeStatus, error) {
	return h.Snapshot(ctx)
}
