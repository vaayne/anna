package host

import (
	"fmt"
	"sort"
	"strings"

	pkgplugins "github.com/CherryHQ/stella/pkg/plugins"
)

func (h *Host) ListRegisteredPlugins() []pkgplugins.PluginInfo {
	h.mu.RLock()
	metas := make([]pkgplugins.PluginInfo, 0, len(h.metadataRegs))
	for _, meta := range h.metadataRegs {
		metas = append(metas, meta.Clone())
	}
	h.mu.RUnlock()
	sort.Slice(metas, func(i, j int) bool {
		if metas[i].Kind != metas[j].Kind {
			return metas[i].Kind < metas[j].Kind
		}
		if metas[i].Name != metas[j].Name {
			return metas[i].Name < metas[j].Name
		}
		return metas[i].ID < metas[j].ID
	})
	return metas
}

func (h *Host) ValidateRegistrations() error {
	h.mu.RLock()
	defer h.mu.RUnlock()
	envOwners := map[string]string{}
	for pluginID, specs := range h.sessionEnvRegs {
		for _, spec := range specs {
			if spec.EnvVar == "" {
				return fmt.Errorf("pluginhost: session env registration for %q missing env var", pluginID)
			}
			switch {
			case spec.Source == pkgplugins.SessionEnvSourceStatic:
			case strings.HasPrefix(string(spec.Source), "oauth."):
			default:
				return fmt.Errorf("pluginhost: session env %q for %q has unknown source %q", spec.EnvVar, pluginID, spec.Source)
			}
			if prev, ok := envOwners[spec.EnvVar]; ok && prev != pluginID {
				return fmt.Errorf("pluginhost: session env %q registered by both %q and %q", spec.EnvVar, prev, pluginID)
			}
			envOwners[spec.EnvVar] = pluginID
		}
	}
	for _, meta := range h.metadataRegs {
		if meta.Managed && !hasRuntimeLocked(h.runtimeRegs, meta.ID) {
			return fmt.Errorf("pluginhost: metadata for %q declares managed runtime but no runtime is registered", meta.ID)
		}
		if meta.HasConfig {
			if _, ok := h.configRegs[meta.ID]; !ok {
				return fmt.Errorf("pluginhost: metadata for %q declares config but no config is registered", meta.ID)
			}
		}
		if meta.HasStatus {
			if _, ok := h.statusRegs[meta.ID]; !ok {
				return fmt.Errorf("pluginhost: metadata for %q declares status but no status is registered", meta.ID)
			}
		}
		for _, capability := range meta.Capabilities {
			switch capability {
			case pkgplugins.CapabilityChannel:
				if !hasChannelLocked(h.channelRegs, meta.ID) {
					return fmt.Errorf("pluginhost: metadata for %q declares channel capability but no channel is registered", meta.ID)
				}
			case pkgplugins.CapabilityLifecycle:
				if !hasLifecycleLocked(h.beforeRunRegs, h.beforeToolRegs, h.afterToolRegs, meta.ID) {
					return fmt.Errorf("pluginhost: metadata for %q declares lifecycle capability but no lifecycle hook is registered", meta.ID)
				}
			case pkgplugins.CapabilityRuntime:
				if !hasRuntimeLocked(h.runtimeRegs, meta.ID) {
					return fmt.Errorf("pluginhost: metadata for %q declares runtime capability but no runtime is registered", meta.ID)
				}
			case pkgplugins.CapabilityConfig:
				if _, ok := h.configRegs[meta.ID]; !ok {
					return fmt.Errorf("pluginhost: metadata for %q declares config capability but no config is registered", meta.ID)
				}
			case pkgplugins.CapabilityStatus:
				if _, ok := h.statusRegs[meta.ID]; !ok {
					return fmt.Errorf("pluginhost: metadata for %q declares status capability but no status is registered", meta.ID)
				}
			case pkgplugins.CapabilityTool:
				if !hasToolLocked(h.toolRegs, meta.ID) {
					return fmt.Errorf("pluginhost: metadata for %q declares tool capability but no tool is registered", meta.ID)
				}
			case pkgplugins.CapabilityPrompt:
				if !hasPromptLocked(h.promptRegs, h.systemPromptRegs, h.beforeRunRegs, h.manifestPrompts, meta.ID) {
					return fmt.Errorf("pluginhost: metadata for %q declares prompt capability but no prompt contribution is registered", meta.ID)
				}
			case pkgplugins.CapabilityHook:
				if !hasHookLocked(h.hookRegs, meta.ID) {
					return fmt.Errorf("pluginhost: metadata for %q declares hook capability but no hook is registered", meta.ID)
				}
			}
		}
		for _, capability := range meta.RequiredCapabilities {
			if capability == pkgplugins.CapabilityAccountEnrollment {
				namespaces := channelEnrollmentNamespacesLocked(h.channelRegs, meta.ID)
				if len(namespaces) != 1 {
					return fmt.Errorf("pluginhost: metadata for %q requires exactly one channel enrollment port, got %d", meta.ID, len(namespaces))
				}
			}
		}
	}
	return nil
}

// validateCapabilityBackings checks host services at the composition boundary.
// Static catalog inspection deliberately does not run this runtime check.
func (h *Host) validateCapabilityBackings() error {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, meta := range h.metadataRegs {
		for _, capability := range meta.RequiredCapabilities {
			if err := h.capabilityBackedLocked(capability); err != nil {
				return fmt.Errorf("pluginhost: metadata for %q declares Platform capability %q but %w", meta.ID, capability, err)
			}
		}
	}
	return nil
}

// capabilityBackedLocked reports whether the host service that backs a declared
// Platform capability is bound. Callers must hold h.mu (read or write). An
// unbacked required capability is a fail-closed error: the plugin must not run
// with a capability the host cannot actually serve.
func (h *Host) capabilityBackedLocked(c pkgplugins.Capability) error {
	backed := false
	switch c {
	case pkgplugins.CapabilityLogger:
		backed = h.log != nil
	case pkgplugins.CapabilityConfigStore:
		backed = h.config != nil
	case pkgplugins.CapabilityStateStore:
		backed = h.stateStore != nil
	case pkgplugins.CapabilityNotifier:
		backed = h.notifications != nil
	case pkgplugins.CapabilityAuth:
		backed = h.authService != nil
	case pkgplugins.CapabilityRuntimeLookup:
		backed = h.runtimes != nil
	case pkgplugins.CapabilityChannelPlatform:
		backed = h.channelRuntime != nil
	case pkgplugins.CapabilityAccountEnrollment:
		backed = h.enrollment != nil
	default:
		return fmt.Errorf("unknown Platform capability")
	}
	if !backed {
		return fmt.Errorf("no host service is bound for it")
	}
	return nil
}

// verifyRuntimeCapabilities checks that every Platform capability the plugin
// declared is granted (declared) and backed (bound), before its managed runtime
// is built and started. A plugin with no registered metadata declares nothing
// and passes. Fail-closed: an unbacked required capability refuses the start.
func (h *Host) verifyRuntimeCapabilities(pluginID string) error {
	h.mu.RLock()
	defer h.mu.RUnlock()
	info, ok := h.metadataRegs[pluginID]
	if !ok {
		return nil
	}
	for _, capability := range info.RequiredCapabilities {
		if err := h.capabilityBackedLocked(capability); err != nil {
			return fmt.Errorf("required Platform capability %q unavailable: %w", capability, err)
		}
	}
	return nil
}

func normalizeMetadata(meta pkgplugins.PluginInfo) pkgplugins.PluginInfo {
	meta = meta.Clone()
	seen := make(map[string]struct{}, len(meta.Capabilities))
	caps := make([]string, 0, len(meta.Capabilities))
	for _, capability := range meta.Capabilities {
		capability = strings.TrimSpace(capability)
		if capability == "" {
			panic("pluginhost: empty metadata capability")
		}
		if _, ok := seen[capability]; ok {
			continue
		}
		seen[capability] = struct{}{}
		caps = append(caps, capability)
	}
	sort.Strings(caps)
	meta.Capabilities = caps
	return meta
}

func validateMetadataShape(meta pkgplugins.PluginInfo) {
	if meta.ID == "" {
		panic("pluginhost: metadata missing plugin id")
	}
	if meta.Kind == "" {
		panic(fmt.Sprintf("pluginhost: metadata for %q missing kind", meta.ID))
	}
	if meta.Name == "" {
		panic(fmt.Sprintf("pluginhost: metadata for %q missing name", meta.ID))
	}
	if meta.DisplayName == "" {
		panic(fmt.Sprintf("pluginhost: metadata for %q missing display name", meta.ID))
	}
}

func hasRuntimeLocked(regs map[string]pkgplugins.RuntimeSpec, pluginID string) bool {
	for _, reg := range regs {
		if reg.PluginID == pluginID {
			return true
		}
	}
	return false
}

func hasToolLocked(regs map[string]pkgplugins.ToolSpec, pluginID string) bool {
	for _, reg := range regs {
		if reg.PluginID == pluginID {
			return true
		}
	}
	return false
}

func hasChannelLocked(regs map[string]pkgplugins.ChannelSpec, pluginID string) bool {
	for _, reg := range regs {
		if reg.PluginID == pluginID {
			return true
		}
	}
	return false
}

func hasHookLocked(regs map[string]pkgplugins.HookSpec, pluginID string) bool {
	for _, reg := range regs {
		if reg.PluginID == pluginID {
			return true
		}
	}
	return false
}

func hasBeforeRunLocked(regs map[string]pkgplugins.BeforeRunSpec, pluginID string) bool {
	for _, reg := range regs {
		if reg.PluginID == pluginID {
			return true
		}
	}
	return false
}

func hasBeforeToolLocked(regs map[string]pkgplugins.BeforeToolCallSpec, pluginID string) bool {
	for _, reg := range regs {
		if reg.PluginID == pluginID {
			return true
		}
	}
	return false
}

func hasAfterToolLocked(regs map[string]pkgplugins.AfterToolResultSpec, pluginID string) bool {
	for _, reg := range regs {
		if reg.PluginID == pluginID {
			return true
		}
	}
	return false
}

func hasLifecycleLocked(beforeRunRegs map[string]pkgplugins.BeforeRunSpec, beforeToolRegs map[string]pkgplugins.BeforeToolCallSpec, afterToolRegs map[string]pkgplugins.AfterToolResultSpec, pluginID string) bool {
	return hasBeforeRunLocked(beforeRunRegs, pluginID) || hasBeforeToolLocked(beforeToolRegs, pluginID) || hasAfterToolLocked(afterToolRegs, pluginID)
}

func hasPromptLocked(promptRegs map[string]pkgplugins.PromptInventorySpec, systemRegs map[string]pkgplugins.SystemPromptSpec, beforeRunRegs map[string]pkgplugins.BeforeRunSpec, manifestPrompts map[string]pkgplugins.SystemPromptSection, pluginID string) bool {
	for _, reg := range promptRegs {
		if reg.PluginID == pluginID {
			return true
		}
	}
	for _, reg := range systemRegs {
		if reg.PluginID == pluginID {
			return true
		}
	}
	for _, reg := range beforeRunRegs {
		if reg.PluginID == pluginID {
			return true
		}
	}
	if _, ok := manifestPrompts[pluginID]; ok {
		return true
	}
	return false
}
