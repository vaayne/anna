package agent

import (
	"context"
	"fmt"

	"github.com/CherryHQ/stella/internal/agent/sandbox"
)

// ToolIdentity is the durable identity of a model-facing tool. Core tools use
// CoreToolName; plugin tools use the trusted plugin/local pair. Exported names
// are a runtime projection and never participate in this identity.
type ToolIdentity struct {
	CoreToolName  string `json:"core_tool_name,omitempty"`
	PluginID      string `json:"plugin_id,omitempty"`
	LocalToolName string `json:"local_tool_name,omitempty"`
}

// Validate enforces the storage shape without trying to mint ownership from a
// string. Catalog composition validates that a plugin/local pair belongs to a
// definition before an identity reaches this package.
func (id ToolIdentity) Validate() error {
	core := id.CoreToolName != ""
	plugin := id.PluginID != "" || id.LocalToolName != ""
	if core == plugin {
		return fmt.Errorf("tool identity must be exactly core or plugin")
	}
	if plugin && (id.PluginID == "" || id.LocalToolName == "") {
		return fmt.Errorf("plugin tool identity requires plugin_id and local_tool_name")
	}
	return nil
}

func (id ToolIdentity) isPlugin() bool { return id.PluginID != "" }

const (
	ToolOverrideScopeSystem      = "system"
	ToolOverrideScopeSystemAgent = "system_agent"
	ToolOverrideScopeUser        = "user"
	ToolOverrideScopeUserAgent   = "user_agent"

	ToolOverrideOriginDefault = "default"
)

// ToolOverride is the runner-facing view of a persisted tool visibility row.
type ToolOverride struct {
	Identity ToolIdentity
	Scope    string
	Enabled  bool
}

type ToolOverrideDecision struct {
	Enabled bool
	Origin  string
}

type ToolOverrideFetcher func(ctx context.Context, userID, agentID string) ([]ToolOverride, error)

var coreToolNames = func() map[string]struct{} {
	m := make(map[string]struct{}, 5)
	for _, d := range sandbox.ReservedToolDefinitions() {
		m[d.Name] = struct{}{}
	}
	return m
}()

func IsCoreToolName(name string) bool {
	_, ok := coreToolNames[name]
	return ok
}

// ResolveToolOverride applies the scope ceilings to one trusted identity.
func ResolveToolOverride(defaultEnabled bool, identity ToolIdentity, rows []ToolOverride) ToolOverrideDecision {
	if identity.Validate() != nil {
		return ToolOverrideDecision{Enabled: defaultEnabled, Origin: ToolOverrideOriginDefault}
	}
	if IsCoreToolName(identity.CoreToolName) {
		return ToolOverrideDecision{Enabled: true, Origin: ToolOverrideOriginDefault}
	}

	admin, hasAdmin := adminOverride(identity, rows)
	if hasAdmin && !admin.Enabled {
		return ToolOverrideDecision{Enabled: false, Origin: admin.Scope}
	}

	user, hasUser := mostSpecificOverride(identity, rows, ToolOverrideScopeUserAgent, ToolOverrideScopeUser)
	if hasUser {
		return ToolOverrideDecision{Enabled: user.Enabled, Origin: user.Scope}
	}
	if hasAdmin {
		return ToolOverrideDecision{Enabled: true, Origin: admin.Scope}
	}
	return ToolOverrideDecision{Enabled: defaultEnabled, Origin: ToolOverrideOriginDefault}
}

// adminOverride applies both administrator ceilings before choosing the
// narrower enabled origin. A system disable applies to every agent, so a
// system-agent enable must never mask it; the same rule applies independently
// to a system-agent disable for that agent.
func adminOverride(identity ToolIdentity, rows []ToolOverride) (ToolOverride, bool) {
	var winner ToolOverride
	hasWinner := false
	for _, scope := range []string{ToolOverrideScopeSystemAgent, ToolOverrideScopeSystem} {
		row, ok := mostSpecificOverride(identity, rows, scope)
		if !ok {
			continue
		}
		if !row.Enabled {
			return row, true
		}
		if !hasWinner {
			winner, hasWinner = row, true
		}
	}
	return winner, hasWinner
}

func mostSpecificOverride(identity ToolIdentity, rows []ToolOverride, scopes ...string) (ToolOverride, bool) {
	for _, scope := range scopes {
		for _, row := range rows {
			rowIdentity, ok := row.toolIdentity()
			if ok && rowIdentity == identity && row.Scope == scope {
				return row, true
			}
		}
	}
	return ToolOverride{}, false
}

func FilterToolEnabled(defaultEnabled bool, identity ToolIdentity, rows []ToolOverride) bool {
	return ResolveToolOverride(defaultEnabled, identity, rows).Enabled
}

func (o ToolOverride) toolIdentity() (ToolIdentity, bool) {
	return o.Identity, o.Identity.Validate() == nil
}
