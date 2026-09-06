package plugins

import (
	"context"
	"encoding/json"

	"github.com/CherryHQ/stella/pkg/channel"
	"github.com/CherryHQ/stella/pkg/hooks"
	"github.com/CherryHQ/stella/pkg/tools"
)

// BuiltinSkillAsset is an explicit release asset declaration. SourceRoot is
// relative to the plugins tree; LogicalRoot is the stable bundle path.
type BuiltinSkillAsset struct {
	Name          string
	SourceRoot    string
	LogicalRoot   string
	OwnerPluginID string
}

// ToolSpec declares a tool capability owned by a plugin.
type ToolSpec struct {
	PluginID    string
	Name        string
	Description string
	Required    bool
	Build       func(ctx ToolContext) (tools.Tool, error)
}

// ChannelSpec declares a channel capability owned by a plugin.
type ChannelSpec struct {
	PluginID    string
	Name        string
	Configured  func(raw map[string]any) bool
	GuestPolicy channel.GuestPolicyDecoder
	Build       func(ctx ChannelContext) (channel.Channel, error)
}

// HookSpec declares a hook capability owned by a plugin.
type HookSpec struct {
	PluginID string
	Name     string
	Build    func(ctx HookContext) (hooks.HookPlugin, error)
}

// RuntimeSpec declares a managed runtime capability owned by a plugin.
type RuntimeSpec struct {
	PluginID string
	Name     string
	Build    func(ctx RuntimeContext) (Runtime, error)
}

// AdminSpec declares plugin-owned admin behavior: config defaults, schema, validation,
// redaction, and status.
type AdminSpec struct {
	PluginID      string
	DefaultConfig func() map[string]any
	Schema        map[string]any
	Validate      func(raw map[string]any) error
	Redact        func(raw map[string]any) map[string]any
	Status        func(ctx context.Context, build AdminContext) (any, error)
}

// Defaults returns a defensive copy of the registered default config.
func (r AdminSpec) Defaults() map[string]any {
	if r.DefaultConfig == nil {
		return map[string]any{}
	}
	return cloneMap(r.DefaultConfig())
}

// Redacted returns a redacted copy of raw config, or a cloned copy when no redactor is set.
func (r AdminSpec) Redacted(raw map[string]any) map[string]any {
	if r.Redact == nil {
		return cloneMap(raw)
	}
	return cloneMap(r.Redact(cloneMap(raw)))
}

// SchemaDefinition returns a defensive deep copy of the registered config schema.
func (r AdminSpec) SchemaDefinition() map[string]any {
	if len(r.Schema) == 0 {
		return map[string]any{}
	}
	b, err := json.Marshal(r.Schema)
	if err != nil {
		return cloneMap(r.Schema)
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		return cloneMap(r.Schema)
	}
	return out
}

// PromptInventorySpec declares structured tool inventory contribution.
type PromptInventorySpec struct {
	PluginID string
	Name     string
	GetTools func(ctx context.Context, build PromptInventoryContext) ([]PromptToolInfo, error)
}

// SystemPromptSpec declares prompt contribution owned by a plugin.
type SystemPromptSpec struct {
	PluginID string
	Name     string
	Required bool
	Build    func(ctx context.Context, build SystemPromptContext) (SystemPromptSection, error)
}

// BeforeRunSpec declares a dynamic per-run lifecycle hook owned by a plugin.
type BeforeRunSpec struct {
	PluginID string
	Name     string
	Order    int
	Required bool
	Run      func(ctx context.Context, build BeforeRunContext) (BeforeRunResult, error)
}

// BeforeToolCallSpec declares a pre-tool lifecycle hook owned by a plugin.
type BeforeToolCallSpec struct {
	PluginID string
	Name     string
	Order    int
	Required bool
	Run      func(ctx context.Context, build BeforeToolCallContext) (BeforeToolCallResult, error)
}

// AfterToolResultSpec declares a post-tool lifecycle hook owned by a plugin.
type AfterToolResultSpec struct {
	PluginID string
	Name     string
	Order    int
	Required bool
	Run      func(ctx context.Context, build AfterToolResultContext) (AfterToolResult, error)
}

// SessionEnvSource identifies how a session env var is produced.
type SessionEnvSource string

const (
	SessionEnvSourceStatic SessionEnvSource = "static"
)

// SessionEnvSpec declares one env var contributed to sandbox sessions.
// Sources are metadata-driven so plugins can declare what they need without
// depending on runner-owned services such as TokenManager.
type SessionEnvSpec struct {
	PluginID        string
	ConfigID        string
	Scope           string
	Revision        int64
	EnvVar          string
	Source          SessionEnvSource
	Value           string // used only when Source == SessionEnvSourceStatic
	Required        bool   // if true, session creation fails when this env cannot be resolved
	OAuthProviderID string // set when source is oauth.*; identifies which provider bundle to load
}
