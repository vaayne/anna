package plugins

import (
	"log/slog"

	"github.com/CherryHQ/stella/pkg/channel"
)

// Host is the flat registration surface exposed to plugins.
// Platform services are provided only through capability-specific contexts.
type Host interface {
	SetInfo(PluginInfo)
	AddAdmin(AdminSpec)
	AddTool(ToolSpec)
	AddChannel(ChannelSpec)
	AddHook(HookSpec)
	AddRuntime(RuntimeSpec)
	AddPromptInventory(PromptInventorySpec)
	AddSystemPrompt(SystemPromptSpec)
	AddBeforeRun(BeforeRunSpec)
	AddBeforeToolCall(BeforeToolCallSpec)
	AddAfterToolResult(AfterToolResultSpec)
	AddSessionEnv(SessionEnvSpec)
}

// Platform is the plugin-scoped service surface available during build/runtime
// work. It is capability-gated: each accessor returns its scoped service only if
// the plugin declared the matching Capability in PluginInfo.RequiredCapabilities.
// An accessor for an undeclared capability returns nil (fail-closed) — there is
// no ambient, all-capabilities Platform.
type Platform interface {
	Logger() *slog.Logger
	ConfigStore() ConfigStore
	StateStore() StateStore
	Notifier() Notifier
	Auth() Auth
	RuntimeLookup() RuntimeLookup
	ChannelPlatform() ChannelPlatform
	AccountEnrollment() channel.AccountEnroller
}
