package mcp

import (
	"bytes"
	"context"
	"fmt"
	"reflect"
	"sync/atomic"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/CherryHQ/stella/internal/authz"
	"github.com/CherryHQ/stella/internal/plugin"
	vaultpkg "github.com/CherryHQ/stella/internal/vault"
)

var errTypedConfigMutationRequired = fmt.Errorf("%w: mcp: connection identity changes require the typed credential mutation", plugin.ErrInvalidConfig)

type configMutationPermitKey struct{}

type configMutationPermit struct {
	tx          pgx.Tx
	configID    string
	pluginID    string
	revision    int64
	kind        plugin.MutationKind
	forceRevoke bool
	used        atomic.Bool
}

// The permit exists only for one synchronous common write inside a typed MCP
// transaction. Callbacks receiving Access do not inherit this capability.
func permittedConfigMutation(ctx context.Context, tx pgx.Tx, before plugin.Config, kind plugin.MutationKind, forceRevoke bool) context.Context {
	return context.WithValue(ctx, configMutationPermitKey{}, &configMutationPermit{
		tx: tx, configID: before.ID, pluginID: before.PluginID, revision: before.Revision, kind: kind, forceRevoke: forceRevoke,
	})
}

func NewMCPBackendPolicy(endpointPolicy EndpointPolicy) plugin.BackendPolicy {
	return plugin.BackendPolicy{
		Validate:   NewMCPPayloadValidator(endpointPolicy),
		Transition: transitionMCPConfig,
	}
}

// This runs after a successful row CAS, before commit. The row lock also fences
// refresh; any failure rolls back the config and its credential changes.
func transitionMCPConfig(ctx context.Context, tx pgx.Tx, authority authz.Authority, kind plugin.MutationKind, def plugin.Definition, before, after *plugin.Config) error {
	if tx == nil || !authority.Valid() || def.Backend != plugin.BackendMCP {
		return authz.ErrForbidden
	}
	if kind == plugin.MutationDelete {
		if before == nil || after != nil {
			return authz.ErrForbidden
		}
		id, err := uuid.Parse(before.ID)
		if err != nil {
			return authz.ErrForbidden
		}
		return vaultpkg.DeleteMCPConfigCredentialsTx(ctx, tx, id)
	}
	if after == nil {
		return authz.ErrForbidden
	}
	if before == nil {
		// Typed creation starts with a validated auth-none row, then installs the
		// final refs and secret in this same transaction.
		if len(after.Payload) == 0 {
			return nil
		}
		payload, err := mergedMCPMovePayload(def, *after)
		if err != nil {
			return err
		}
		if payload.AuthType == AuthTypeBearer {
			return errTypedConfigMutationRequired
		}
		return nil
	}
	permit, hasPermit := ctx.Value(configMutationPermitKey{}).(*configMutationPermit)
	forceRevoke := hasPermit && permit.forceRevoke
	// Closing an obsolete config must remain possible without decoding it.
	if !forceRevoke && before.Scope == after.Scope && before.UserID == after.UserID && before.AgentID == after.AgentID && bytes.Equal(before.Payload, after.Payload) && bytes.Equal(before.CredentialRefs, after.CredentialRefs) {
		return nil
	}
	oldIdentity, err := mcpExecutionIdentity(def, *before)
	if err != nil {
		return err
	}
	newIdentity, err := mcpExecutionIdentity(def, *after)
	if err != nil {
		return err
	}
	if !forceRevoke && oldIdentity == newIdentity {
		return nil
	}
	if !hasPermit || !reflect.ValueOf(tx).Comparable() || !reflect.ValueOf(permit.tx).Comparable() || permit.tx != tx || permit.configID != before.ID || permit.pluginID != before.PluginID || permit.revision != before.Revision || permit.kind != kind || !permit.used.CompareAndSwap(false, true) {
		return errTypedConfigMutationRequired
	}
	id, err := uuid.Parse(before.ID)
	if err != nil {
		return authz.ErrForbidden
	}
	return vaultpkg.DeleteMCPConfigCredentialsTx(ctx, tx, id)
}

type mcpConnectionIdentity struct {
	Scope                                                                       plugin.Scope
	UserID, AgentID                                                             string
	URL, Transport, AuthType, CredentialMode, ClientID, TokenEndpointAuthMethod string
	Refs                                                                        [2]string
}

func mcpExecutionIdentity(def plugin.Definition, cfg plugin.Config) (mcpConnectionIdentity, error) {
	identity := mcpConnectionIdentity{Scope: cfg.Scope, UserID: cfg.UserID, AgentID: cfg.AgentID}
	if len(cfg.Payload) == 0 {
		return identity, nil
	}
	payload, err := mergedMCPMovePayload(def, cfg)
	if err != nil {
		return identity, err
	}
	identity.URL, identity.Transport, identity.AuthType, identity.CredentialMode = payload.URL, payload.Transport, payload.AuthType, payload.CredentialMode
	identity.ClientID = metadataOAuthClientID(payload.Metadata)
	identity.TokenEndpointAuthMethod = oauthMetadataTokenEndpointAuthMethod(payload.Metadata)
	bearer, _, clientSecret, err := decodeMCPPluginCredentialRefs(cfg.CredentialRefs, cfg, payload.AuthType, payload.CredentialMode)
	if err != nil {
		return identity, err
	}
	identity.Refs = [2]string{bearer, clientSecret}

	return identity, nil
}

func updateCredentialConfig(ctx context.Context, access *plugin.Access, tx pgx.Tx, current plugin.Config, forceRevoke bool, patch plugin.ConfigPatch) (plugin.Config, error) {
	return access.UpdateConfig(permittedConfigMutation(ctx, tx, current, plugin.MutationUpdate, forceRevoke), current.PluginID, current.ID, current.Revision, patch)
}
