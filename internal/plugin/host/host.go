package host

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"

	"github.com/CherryHQ/stella/internal/plugin"

	"github.com/CherryHQ/stella/internal/platform/config"
	"github.com/CherryHQ/stella/internal/plugin/manifest"
	"github.com/CherryHQ/stella/pkg/ai"
	pkgchannel "github.com/CherryHQ/stella/pkg/channel"
	pkgplugins "github.com/CherryHQ/stella/pkg/plugins"
)

type Option func(*Host)

// ListenerCap decides whether a channel instance may accept new ingress.
// pluginID identifies the platform plugin and agentID identifies the bound
// channel owner. The gate is required for channel runtime admission.
type ListenerCap func(context.Context, string, string) (bool, error)

// ErrListenerCapUnavailable means the common listener policy was not wired.
// Channel runtimes fail closed when this gate is absent.
var ErrListenerCapUnavailable = errors.New("pluginhost: listener capability unavailable")

type Host struct {
	store    config.Store
	log      *slog.Logger
	config   *configService
	runtimes *RuntimeHost
	mu       sync.RWMutex
	// sealed is set by Seal() after all static registrations and capability
	// bindings are complete. Once sealed, the static composition surface
	// (LoadCatalog and the Set* capability binders) refuses further changes,
	// while the dynamic desired-state surface (ApplyPlugin/ApplyChannel/
	// SetEnabled/Stop/RegisterManifestPlugins) stays available.
	sealed             bool
	pluginIDs          map[string]struct{}
	manifestIDs        map[string]struct{}
	manifestEnabledIDs map[string]struct{}
	manifestOwnedIDs   map[string]struct{}
	metadataRegs       map[string]pkgplugins.PluginInfo
	notifications      pkgplugins.Notifier
	stateStore         StateStoreBackend
	authService        pkgplugins.Auth
	enrollment         AccountEnrollmentBackend
	channelRuntime     pkgplugins.ChannelPlatform
	listenerCap        ListenerCap
	toolRegs           map[string]pkgplugins.ToolSpec
	hookRegs           map[string]pkgplugins.HookSpec
	beforeRunRegs      map[string]pkgplugins.BeforeRunSpec
	beforeToolRegs     map[string]pkgplugins.BeforeToolCallSpec
	afterToolRegs      map[string]pkgplugins.AfterToolResultSpec
	channelRegs        map[string]pkgplugins.ChannelSpec
	runtimeRegs        map[string]pkgplugins.RuntimeSpec
	configRegs         map[string]pkgplugins.AdminSpec
	statusRegs         map[string]pkgplugins.AdminSpec
	promptRegs         map[string]pkgplugins.PromptInventorySpec
	systemPromptRegs   map[string]pkgplugins.SystemPromptSpec
	manifestPrompts    map[string]pkgplugins.SystemPromptSection
	sessionEnvRegs     map[string][]pkgplugins.SessionEnvSpec
}

func New(store config.Store, opts ...Option) *Host {
	h := &Host{
		store:              store,
		log:                slog.With("component", "plugin_host"),
		pluginIDs:          map[string]struct{}{},
		manifestIDs:        map[string]struct{}{},
		manifestEnabledIDs: map[string]struct{}{},
		manifestOwnedIDs:   map[string]struct{}{},
		metadataRegs:       map[string]pkgplugins.PluginInfo{},
		toolRegs:           map[string]pkgplugins.ToolSpec{},
		hookRegs:           map[string]pkgplugins.HookSpec{},
		beforeRunRegs:      map[string]pkgplugins.BeforeRunSpec{},
		beforeToolRegs:     map[string]pkgplugins.BeforeToolCallSpec{},
		afterToolRegs:      map[string]pkgplugins.AfterToolResultSpec{},
		channelRegs:        map[string]pkgplugins.ChannelSpec{},
		runtimeRegs:        map[string]pkgplugins.RuntimeSpec{},
		configRegs:         map[string]pkgplugins.AdminSpec{},
		statusRegs:         map[string]pkgplugins.AdminSpec{},
		promptRegs:         map[string]pkgplugins.PromptInventorySpec{},
		systemPromptRegs:   map[string]pkgplugins.SystemPromptSpec{},
		manifestPrompts:    map[string]pkgplugins.SystemPromptSection{},
		sessionEnvRegs:     map[string][]pkgplugins.SessionEnvSpec{},
	}
	h.config = &configService{store: store}
	h.runtimes = NewRuntimeHost(h)
	for _, opt := range opts {
		opt(h)
	}
	return h
}

func (h *Host) Logger(pluginID string) *slog.Logger { return h.log.With("plugin", pluginID) }
func (h *Host) Config() ConfigBackend               { return h.config }
func (h *Host) Runtime() pkgplugins.RuntimeLookup   { return h.runtimes }

// ListChannelChats returns the group chats currently joined by a channel bot.
func (h *Host) ListChannelChats(ctx context.Context, channelID string, pageSize int, pageToken string) (pkgchannel.JoinedChatPage, error) {
	return h.runtimes.ListChannelChats(ctx, channelID, pageSize, pageToken)
}

func (h *Host) RegisterPluginID(id string) {
	if id == "" {
		panic("pluginhost: empty plugin id")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, exists := h.pluginIDs[id]; exists {
		panic(fmt.Sprintf("pluginhost: duplicate plugin id %q", id))
	}
	h.pluginIDs[id] = struct{}{}
}

// Seal validates every static registration and capability binding, then marks
// the host sealed so no further static composition can occur. Missing capability
// registrations fail here; duplicate static registrations already fail eagerly
// at registration time (registerUnique). After Seal, LoadCatalog and the Set*
// capability binders refuse late changes, while the dynamic desired-state
// surface (ApplyPlugin/ApplyChannel/SetEnabled/RegisterManifestPlugins/Stop)
// remains available. Seal is one-shot.
func (h *Host) Seal() error {
	if err := h.ValidateRegistrations(); err != nil {
		return err
	}
	if err := h.validateCapabilityBackings(); err != nil {
		return err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.sealed {
		return errors.New("pluginhost: already sealed")
	}
	h.sealed = true
	return nil
}

// requireUnsealedLocked panics if the host is sealed. Callers must already hold
// h.mu. A late static registration is a composition bug, mirroring the eager
// panic registerUnique raises for duplicates.
func (h *Host) requireUnsealedLocked(op string) {
	if h.sealed {
		panic("pluginhost: " + op + " after Seal (static registration is sealed)")
	}
}

func (h *Host) LoadCatalog(catalog *pkgplugins.Catalog) error {
	if catalog == nil {
		return nil
	}
	h.mu.RLock()
	sealed := h.sealed
	h.mu.RUnlock()
	if sealed {
		return errors.New("pluginhost: LoadCatalog after Seal")
	}
	for _, id := range catalog.Names() {
		plugin, ok := catalog.Get(id)
		if !ok {
			continue
		}
		h.RegisterPluginID(id)
		plugin.Register(h)
	}
	if err := h.ValidateRegistrations(); err != nil {
		return err
	}
	return nil
}

func (h *Host) LoadDefaultCatalog() error { return h.LoadCatalog(defaultCatalog()) }

// RegisterManifestPlugins registers plugins declared in a manifest. For each
// enabled plugin:
//   - New plugins (not already Go-registered) are fully registered with ID, info,
//     and session envs.
//   - Existing plugins (already Go-registered) get manifest session envs registered
//     so the manifest is the single source of truth for env injection. Go code
//     should no longer call AddSessionEnv for manifest-declared env vars.
func (h *Host) RegisterManifestPlugins(m *manifest.Manifest) {
	if m == nil {
		return
	}

	// Collect which IDs are already registered without holding the lock across
	// the public method calls (RegisterPluginID / SetInfo / AddSessionEnv each
	// acquire the lock themselves). Also clear prior manifest registrations so
	// admin UI manifest edits can be applied without accumulating stale env specs.
	type toRegister struct {
		plugin    manifest.ManifestPlugin
		alreadyGo bool
	}
	var entries []toRegister

	h.mu.Lock()
	for id := range h.manifestEnabledIDs {
		delete(h.sessionEnvRegs, id)
		delete(h.manifestPrompts, id)
	}
	for id := range h.manifestOwnedIDs {
		delete(h.pluginIDs, id)
		delete(h.metadataRegs, id)
		delete(h.sessionEnvRegs, id)
	}
	h.manifestIDs = map[string]struct{}{}
	h.manifestEnabledIDs = map[string]struct{}{}
	h.manifestOwnedIDs = map[string]struct{}{}

	for _, p := range m.Plugins {
		h.manifestIDs[p.ID] = struct{}{}
		if !p.Enabled {
			continue
		}
		_, alreadyGo := h.pluginIDs[p.ID]
		h.manifestEnabledIDs[p.ID] = struct{}{}
		if !alreadyGo {
			h.manifestOwnedIDs[p.ID] = struct{}{}
		}
		entries = append(entries, toRegister{plugin: p, alreadyGo: alreadyGo})
	}
	h.mu.Unlock()

	for _, e := range entries {
		p := e.plugin
		if !e.alreadyGo {
			name := p.Name
			if name == "" {
				name = p.ID
			}
			displayName := p.DisplayName
			if displayName == "" {
				displayName = name
			}
			var caps []string
			if p.Prompt != "" {
				caps = append(caps, pkgplugins.CapabilityPrompt)
			}
			h.RegisterPluginID(p.ID)
			h.SetInfo(pkgplugins.PluginInfo{
				ID:           p.ID,
				Kind:         p.Kind,
				Name:         name,
				DisplayName:  displayName,
				Description:  p.Description,
				AdminVisible: true,
				Capabilities: caps,
			})
		}

		// Register manifest session_envs for all enabled plugins, including
		// Go-registered ones. The manifest is the source of truth.
		for _, se := range p.SessionEnvs {
			h.AddSessionEnv(pkgplugins.SessionEnvSpec{
				PluginID:        p.ID,
				EnvVar:          se.EnvVar,
				Source:          pkgplugins.SessionEnvSource(se.Source),
				Value:           se.Value,
				Required:        se.Required,
				OAuthProviderID: p.OAuthProvider,
			})
		}

		if p.Prompt != "" {
			promptName := p.Name
			if promptName == "" {
				promptName = p.ID
			}
			h.mu.Lock()
			h.manifestPrompts[p.ID] = pkgplugins.SystemPromptSection{Title: promptName, Content: p.Prompt}
			h.mu.Unlock()
		}
	}
}

func (h *Host) SetInfo(info pkgplugins.PluginInfo) {
	info = normalizeMetadata(info)
	validateMetadataShape(info)

	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.pluginIDs[info.ID]; !ok {
		panic(fmt.Sprintf("pluginhost: metadata registered for unknown plugin id %q", info.ID))
	}
	registerUnique(h.metadataRegs, info.ID, info, "metadata")
}

func (h *Host) AddAdmin(reg pkgplugins.AdminSpec) {
	h.mu.Lock()
	if reg.DefaultConfig != nil || len(reg.Schema) > 0 || reg.Validate != nil || reg.Redact != nil {
		registerUnique(h.configRegs, reg.PluginID, reg, "config")
	}
	if reg.Status != nil {
		registerUnique(h.statusRegs, reg.PluginID, reg, "status")
	}
	h.mu.Unlock()
}

func (h *Host) AddTool(reg pkgplugins.ToolSpec) {
	h.mu.Lock()
	defer h.mu.Unlock()
	registerUnique(h.toolRegs, reg.Name, reg, "tool")
}

func (h *Host) AddChannel(reg pkgplugins.ChannelSpec) {
	h.mu.Lock()
	defer h.mu.Unlock()
	registerUnique(h.channelRegs, reg.Name, reg, "channel")
}

func (h *Host) AddHook(reg pkgplugins.HookSpec) {
	h.mu.Lock()
	defer h.mu.Unlock()
	registerUnique(h.hookRegs, reg.Name, reg, "hook")
}

func (h *Host) AddBeforeRun(reg pkgplugins.BeforeRunSpec) {
	h.mu.Lock()
	defer h.mu.Unlock()
	registerUnique(h.beforeRunRegs, promptKey(reg.PluginID, reg.Name), reg, "before run")
}

func (h *Host) AddBeforeToolCall(reg pkgplugins.BeforeToolCallSpec) {
	h.mu.Lock()
	defer h.mu.Unlock()
	registerUnique(h.beforeToolRegs, promptKey(reg.PluginID, reg.Name), reg, "before tool call")
}

func (h *Host) AddAfterToolResult(reg pkgplugins.AfterToolResultSpec) {
	h.mu.Lock()
	defer h.mu.Unlock()
	registerUnique(h.afterToolRegs, promptKey(reg.PluginID, reg.Name), reg, "after tool result")
}

func (h *Host) AddRuntime(reg pkgplugins.RuntimeSpec) {
	h.mu.Lock()
	defer h.mu.Unlock()
	registerUnique(h.runtimeRegs, runtimeRegKey(reg.PluginID, reg.Name), reg, "runtime")
}

func (h *Host) AddPromptInventory(reg pkgplugins.PromptInventorySpec) {
	h.mu.Lock()
	defer h.mu.Unlock()
	registerUnique(h.promptRegs, promptKey(reg.PluginID, reg.Name), reg, "prompt inventory")
}

func (h *Host) AddSystemPrompt(reg pkgplugins.SystemPromptSpec) {
	h.mu.Lock()
	defer h.mu.Unlock()
	registerUnique(h.systemPromptRegs, promptKey(reg.PluginID, reg.Name), reg, "system prompt")
}

func (h *Host) AddSessionEnv(spec pkgplugins.SessionEnvSpec) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.sessionEnvRegs[spec.PluginID] = append(h.sessionEnvRegs[spec.PluginID], spec)
}

func (h *Host) SessionEnvSpecs(pluginID string) []pkgplugins.SessionEnvSpec {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return append([]pkgplugins.SessionEnvSpec(nil), h.sessionEnvRegs[pluginID]...)
}

func (h *Host) AllSessionEnvSpecs() []pkgplugins.SessionEnvSpec {
	h.mu.RLock()
	defer h.mu.RUnlock()
	var out []pkgplugins.SessionEnvSpec
	for _, specs := range h.sessionEnvRegs {
		out = append(out, specs...)
	}
	return out
}

func registerUnique[T any](m map[string]T, key string, reg T, kind string) {
	if key == "" {
		panic("pluginhost: empty " + kind + " key")
	}
	if _, exists := m[key]; exists {
		panic(fmt.Sprintf("pluginhost: duplicate %s registration %q", kind, key))
	}
	m[key] = reg
}

func runtimeRegKey(pluginID, name string) string { return pluginID + "/" + name }
func promptKey(pluginID, name string) string     { return pluginID + "/" + name }

func (h *Host) SetEnabled(ctx context.Context, pluginID string, enabled bool) error {
	return h.config.SetEnabled(ctx, pluginID, enabled)
}

func (h *Host) Status(ctx context.Context, pluginID string) (any, error) {
	h.mu.RLock()
	reg, ok := h.statusRegs[pluginID]
	h.mu.RUnlock()
	if !ok || reg.Status == nil {
		return map[string]any{}, nil
	}
	state, err := h.DesiredState(ctx, pluginID)
	if err != nil {
		return nil, err
	}
	return reg.Status(ctx, pkgplugins.AdminContext{Platform: h.platform(pluginID), State: state})
}

func (h *Host) ValidateConfig(pluginID string, raw map[string]any) error {
	h.mu.RLock()
	reg, ok := h.configRegs[pluginID]
	h.mu.RUnlock()
	if !ok || reg.Validate == nil {
		return nil
	}
	return reg.Validate(raw)
}

// IsConfigurable reports whether the plugin has an admin config schema
// registered. Used by the admin API to reject writes to plugin IDs that
// no longer exist in code (orphan plugin rows from old installs)
// or to typo'd IDs, instead of silently accepting them.
func (h *Host) IsConfigurable(pluginID string) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	_, ok := h.configRegs[pluginID]
	return ok
}

func (h *Host) RedactConfig(pluginID string, raw map[string]any) map[string]any {
	h.mu.RLock()
	reg, ok := h.configRegs[pluginID]
	h.mu.RUnlock()
	if !ok {
		return cloneMap(raw)
	}
	return reg.Redacted(raw)
}

func (h *Host) ConfigSchema(pluginID string) map[string]any {
	h.mu.RLock()
	reg, ok := h.configRegs[pluginID]
	h.mu.RUnlock()
	if !ok {
		return map[string]any{}
	}
	return reg.SchemaDefinition()
}

func (h *Host) DesiredState(ctx context.Context, pluginID string) (pkgplugins.PluginState, error) {
	return h.config.Get(ctx, pluginID)
}

func (h *Host) ApplyPlugin(ctx context.Context, pluginID string) error {
	return h.runtimes.ApplyPlugin(ctx, pluginID)
}

func (h *Host) ApplyChannel(ctx context.Context, channel config.Channel) error {
	return h.runtimes.ApplyChannel(ctx, channel)
}

// ReconcileChannel reapplies one committed channel instance by exact ID.
func (h *Host) ReconcileChannel(ctx context.Context, channelID string) error {
	return h.runtimes.ReconcileChannel(ctx, channelID)
}

func (h *Host) listenerAllowed(ctx context.Context, pluginID, agentID string) (bool, error) {
	h.mu.RLock()
	cap := h.listenerCap
	h.mu.RUnlock()
	if cap == nil {
		return false, ErrListenerCapUnavailable
	}
	allowed, err := cap(ctx, pluginID, agentID)
	if err != nil {
		return false, fmt.Errorf("listener capability for %s/%s: %w", pluginID, agentID, err)
	}
	return allowed, nil
}

// Quiesce halts new ingress on managed runtimes (channel pollers) for a graceful
// drain while preserving already-accepted operations and notifier senders. The
// runtime table is left intact so a later Stop can fully tear them down.
func (h *Host) Quiesce(ctx context.Context) { h.runtimes.Quiesce(ctx) }

func (h *Host) Stop(ctx context.Context) error { return h.runtimes.Stop(ctx) }

func (h *Host) PromptTools(ctx context.Context, pluginID string, snapshot plugin.Snapshot) ([]pkgplugins.PromptToolInfo, error) {
	h.mu.RLock()
	regs := make([]pkgplugins.PromptInventorySpec, 0, len(h.promptRegs))
	for _, reg := range h.promptRegs {
		if reg.PluginID == pluginID {
			regs = append(regs, reg)
		}
	}
	h.mu.RUnlock()
	sort.Slice(regs, func(i, j int) bool {
		return promptKey(regs[i].PluginID, regs[i].Name) < promptKey(regs[j].PluginID, regs[j].Name)
	})
	var out []pkgplugins.PromptToolInfo
	for _, reg := range regs {
		if reg.GetTools == nil {
			continue
		}
		state, enabled, err := snapshotState(snapshot, reg.PluginID)
		if err != nil {
			return nil, err
		}
		if !enabled {
			continue
		}
		tools, err := reg.GetTools(ctx, pkgplugins.PromptInventoryContext{Platform: h.platform(reg.PluginID), State: state})
		if err != nil {
			return nil, err
		}
		for _, tool := range tools {
			out = append(out, tool.Clone())
		}
	}
	return out, nil
}

func (h *Host) SystemPromptSections(ctx context.Context, build pkgplugins.SystemPromptContext, snapshot plugin.Snapshot) ([]pkgplugins.SystemPromptSection, error) {
	h.mu.RLock()
	regs := make([]pkgplugins.SystemPromptSpec, 0, len(h.systemPromptRegs))
	for _, reg := range h.systemPromptRegs {
		regs = append(regs, reg)
	}
	h.mu.RUnlock()
	sort.Slice(regs, func(i, j int) bool {
		return promptKey(regs[i].PluginID, regs[i].Name) < promptKey(regs[j].PluginID, regs[j].Name)
	})

	var out []pkgplugins.SystemPromptSection
	for _, reg := range regs {
		if reg.Build == nil {
			continue
		}
		state, enabled, err := snapshotState(snapshot, reg.PluginID)
		if err != nil {
			return nil, err
		}
		if !enabled {
			continue
		}
		section, err := reg.Build(ctx, pkgplugins.SystemPromptContext{
			Platform:            h.platform(reg.PluginID),
			State:               state,
			UserID:              build.UserID,
			AgentID:             build.AgentID,
			RegisteredPluginIDs: append([]string(nil), build.RegisteredPluginIDs...),
			EnabledPluginIDs:    append([]string(nil), build.EnabledPluginIDs...),
		})
		if err != nil {
			return nil, err
		}
		if section.Title == "" || section.Content == "" {
			continue
		}
		out = append(out, section)
	}
	for _, definition := range snapshot.Definitions() {
		if definition.Backend != plugin.BackendCLI {
			continue
		}
		resolved, ok := snapshot.Get(definition.ID)
		if !ok || !resolved.Effective.IsEffectivelyEnabled {
			continue
		}
		winner, err := snapshot.ResolveNamespace(definition.Namespace)
		if err != nil {
			return nil, err
		}
		if winner.PluginID != definition.ID || !winner.IsEffectivelyEnabled {
			continue
		}
		if _, err := selectedResourceIdentity(definition, resolved); err != nil {
			return nil, err
		}
		if err := validateResolvedCLIPayload(definition, resolved); err != nil {
			return nil, err
		}
		payload, err := manifest.DecodeCLIPayload(winner.Payload, "prompt CLI payload")
		if err != nil {
			return nil, err
		}
		if payload.Prompt != "" {
			out = append(out, pkgplugins.SystemPromptSection{Title: definition.DisplayName, Content: payload.Prompt, Inline: true})
		}
	}
	return out, nil
}

func (h *Host) BeforeRun(ctx context.Context, build pkgplugins.BeforeRunContext, snapshot plugin.Snapshot) (pkgplugins.BeforeRunResult, error) {
	h.mu.RLock()
	regs := make([]pkgplugins.BeforeRunSpec, 0, len(h.beforeRunRegs))
	for _, reg := range h.beforeRunRegs {
		regs = append(regs, reg)
	}
	h.mu.RUnlock()
	sort.Slice(regs, func(i, j int) bool {
		if regs[i].Order != regs[j].Order {
			return regs[i].Order < regs[j].Order
		}
		if regs[i].PluginID != regs[j].PluginID {
			return regs[i].PluginID < regs[j].PluginID
		}
		return regs[i].Name < regs[j].Name
	})

	current := build.SystemPrompt
	for _, reg := range regs {
		if reg.Run == nil {
			continue
		}

		state, enabled, err := snapshotState(snapshot, reg.PluginID)
		if err != nil {
			return pkgplugins.BeforeRunResult{}, err
		}
		if !enabled {
			continue
		}

		result, err := reg.Run(ctx, pkgplugins.BeforeRunContext{
			Platform:     h.platform(reg.PluginID),
			State:        state,
			SessionID:    build.SessionID,
			Channel:      build.Channel,
			UserID:       build.UserID,
			AgentID:      build.AgentID,
			Model:        build.Model,
			MessageText:  build.MessageText,
			SystemPrompt: current,
			History:      append([]ai.Message(nil), build.History...),
		})
		if err != nil {
			return pkgplugins.BeforeRunResult{}, err
		}
		if result.SystemPrompt != "" {
			current = result.SystemPrompt
		}
	}

	return pkgplugins.BeforeRunResult{SystemPrompt: current}, nil
}

func cloneMap(src map[string]any) map[string]any {
	return pkgplugins.PluginState{Config: src}.Clone().Config
}
