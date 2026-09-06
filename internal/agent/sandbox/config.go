// Package sandbox resolves an agent's sandbox config into a live session and
// provides that session's agent-facing tool projections (bash, read, write, edit).
package sandbox

import (
	"context"
	"time"

	oauth "github.com/CherryHQ/stella/internal/connections/oauth"
	"github.com/CherryHQ/stella/internal/platform/config"
	"github.com/CherryHQ/stella/internal/plugin/manifest"
	"github.com/CherryHQ/stella/internal/vault"
	pkgplugins "github.com/CherryHQ/stella/pkg/plugins"
	"github.com/CherryHQ/stella/plugins/core"
)

// VaultEnvLoader is the vault surface an agent session needs.
// Implemented by *vault.Service.
type VaultEnvLoader interface {
	// LoadEnvForAgent returns the ambient env for a user's agent session.
	LoadEnvForAgent(ctx context.Context, userID string, agentID string) (map[string]string, error)
	// ListAmbientSecretMetas returns prompt-safe ambient secret metadata.
	ListAmbientSecretMetas(ctx context.Context, userID string, agentID string) ([]vault.AmbientSecretMeta, error)
}

// Config is passed to sandbox operations.
// It is constructed from the runner config in the parent agent package.
type Config struct {
	SandboxConfig     config.SandboxConfig
	SandboxBackendFn  func(ctx context.Context) string
	Backends          *BackendRegistry
	Paths             Paths
	UserID            string
	GroupID           string // non-empty for group sessions; vault/env use group principal
	AgentID           string
	SessionID         string
	ProjectID         string
	SessionEnvSpecs   []pkgplugins.SessionEnvSpec
	BinarySpecs       []pkgplugins.PluginBinarySpec
	ContextBinaryPlan *manifest.BinaryInstallPlan
	UserBinaryPlan    *manifest.BinaryInstallPlan
	CoreRuntimePlan   *core.RuntimePlan
	// ManagedBinaryRoot is used only by the short preparation session. The final
	// session receives UserBinaryPlan and never mounts this private tree.
	ManagedBinaryRoot   string
	VaultEnvLoader      VaultEnvLoader
	SessionSecretValues *SessionSecretValues
	TokenManager        *oauth.TokenManager
	// OAuthEnvBindings records which sandbox env vars were injected from an OAuth
	// bundle at session creation, so a later live refresh reloads only those and
	// never overwrites an explicit vault override of the same name. Shared by
	// pointer with the retained runner config so RefreshSessionEnv sees what
	// buildSandboxEnv recorded.
	OAuthEnvBindings *OAuthEnvBindings
	// ChatTimeout is the wall-clock budget for one chat turn. OAuth-derived env
	// is refreshed to stay valid for at least this long plus a safety margin so a
	// token injected at turn start outlives the turn (#722). Zero uses the
	// runner's default turn budget.
	ChatTimeout time.Duration
}
