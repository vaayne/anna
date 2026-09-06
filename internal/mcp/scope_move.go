package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"

	"github.com/jackc/pgx/v5"

	"github.com/CherryHQ/stella/internal/authz"
	"github.com/CherryHQ/stella/internal/plugin"
)

var (
	// ErrScopeMoveBearerReplacement makes bearer moves fail closed. Copying an
	// old token into a new owner tuple would silently extend its authority.
	ErrScopeMoveBearerReplacement = errors.New("mcp: bearer scope move requires a replacement token")
	ErrScopeMoveOAuth             = errors.New("mcp: OAuth scope moves are unsupported")
	ErrScopeMoveCombinedUpdate    = errors.New("mcp: move scope separately from other updates")
)

// ScopeMoveRequest is the narrow MCP adapter seam for moving one common
// config. Target user identity is intentionally absent: Access derives it
// from the trusted authority and checks the target AgentPEP.
type ScopeMoveRequest struct {
	PluginID         string
	ConfigID         string
	ExpectedRevision int64
	TargetScope      plugin.Scope
	TargetAgentID    string
	// Patch carries the optional complete next payload/credential refs needed
	// by a backend validator. Scope moves still preserve auth type and mode.
	Patch       plugin.ConfigPatch
	Replacement *string
}

// MoveConfigScope moves one MCP config through common Access and returns its
// metadata adapter. Auth-none moves carry no vault work. Bearer moves delete
// the old exact vault tuple and store the replacement at the CAS-selected
// target tuple in the same transaction. OAuth moves are rejected before any
// config or credential write.
func (s *Service) MoveConfigScope(ctx context.Context, authority authz.Authority, req ScopeMoveRequest) (Registration, error) {
	if s == nil || s.pool == nil || s.plugins == nil || !authority.Valid() || authority.Kind() != authz.ActorUser {
		return Registration{}, authz.ErrForbidden
	}
	if req.PluginID == "" || req.ConfigID == "" || req.ExpectedRevision < 1 {
		return Registration{}, plugin.ErrConflict
	}

	var result Registration
	err := s.plugins.WithMutationTx(ctx, authority, func(mutationCtx context.Context, access *plugin.Access, tx pgx.Tx) error {
		current, err := access.GetConfig(mutationCtx, req.PluginID, req.ConfigID)
		if err != nil {
			return err
		}
		def, err := access.GetDefinition(mutationCtx, current.PluginID)
		if err != nil {
			return err
		}

		currentPayload, err := mergedMCPMovePayload(def, current)
		if err != nil {
			return err
		}
		candidatePayload, err := applyMCPMovePayload(current.Payload, req.Patch)
		if err != nil {
			return err
		}
		candidatePayload, err = mergeMCPJSONObjects(def.Spec, candidatePayload)
		if err != nil {
			return err
		}
		candidate, err := decodeMCPPluginPayload(candidatePayload)
		if err != nil {
			return err
		}
		if candidate.AuthType != currentPayload.AuthType || candidate.CredentialMode != currentPayload.CredentialMode {
			return errors.New("mcp: scope move cannot change auth type or credential mode")
		}

		switch currentPayload.AuthType {
		case AuthTypeNone:
			if req.Replacement != nil || req.Patch.CredentialRefsSet && len(req.Patch.CredentialRefs) != 0 {
				return authz.ErrForbidden
			}
			req.Patch.CredentialRefsSet = true
			req.Patch.CredentialRefs = json.RawMessage(`{}`)
		case AuthTypeBearer:
			if req.Replacement == nil || *req.Replacement == "" {
				return ErrScopeMoveBearerReplacement
			}
			if s.bindVault == nil {
				return errPluginCredentialsUnavailable
			}
			if req.Patch.CredentialRefsSet {
				return authz.ErrForbidden
			}
			req.Patch.CredentialRefsSet = true
			req.Patch.CredentialRefs = bearerMoveRefs(req.ConfigID, req.TargetScope, req.TargetAgentID, authority)

		case AuthTypeOAuth:
			return ErrScopeMoveOAuth
		default:
			return errors.New("mcp: unsupported auth type for scope move")
		}
		if req.Patch.BinaryVersionsSet {
			return authz.ErrForbidden
		}

		moved, err := access.MoveConfig(permittedConfigMutation(mutationCtx, tx, current, plugin.MutationMove, false), req.PluginID, req.ConfigID, req.ExpectedRevision, req.TargetScope, req.TargetAgentID, req.Patch)
		if err != nil {
			return err
		}
		newOwner := CredentialOwner{Scope: string(moved.Scope), UserID: moved.UserID, AgentID: moved.AgentID}
		var vault Vault
		if currentPayload.AuthType == AuthTypeBearer {
			vault = s.bindVault(tx)
			if vault == nil {
				return errPluginCredentialsUnavailable
			}
		}
		mutation := CredentialMutation{tx: tx, config: moved, owner: newOwner, vault: vault, configManaged: true}
		if currentPayload.AuthType == AuthTypeBearer {
			if err := mutation.StoreBearer(mutationCtx, *req.Replacement); err != nil {
				return err
			}
		}

		effective, err := plugin.Resolve(def, []plugin.Config{moved}, moved.UserID, moved.AgentID)
		if err != nil {
			return err
		}
		result, err = RegistrationFromPluginConfig(def, moved, effective, PluginMCPObservation{ConfigRevision: moved.Revision}, authority)
		return err
	})
	return result, err
}

func mergedMCPMovePayload(def plugin.Definition, config plugin.Config) (mcpPluginPayload, error) {
	merged, err := mergeMCPJSONObjects(def.Spec, config.Payload)
	if err != nil {
		return mcpPluginPayload{}, err
	}
	return decodeMCPPluginPayload(merged)
}

// applyMCPMovePayload mirrors common ConfigPatch payload semantics locally so
// the adapter can classify auth before invoking the mutating Access method.
func applyMCPMovePayload(current json.RawMessage, patch plugin.ConfigPatch) (json.RawMessage, error) {
	if !patch.PayloadSet && len(patch.ResetFields) == 0 {
		return current, nil
	}
	if len(current) == 0 {
		current = json.RawMessage(`{}`)
	}
	var owned map[string]json.RawMessage
	if err := json.Unmarshal(current, &owned); err != nil || owned == nil {
		return nil, plugin.ErrInvalidConfig
	}
	fields := map[string]json.RawMessage{}
	if patch.PayloadSet {
		if err := json.Unmarshal(patch.Payload, &fields); err != nil || fields == nil {
			return nil, plugin.ErrInvalidConfig
		}
		maps.Copy(owned, fields)
	}
	for _, key := range patch.ResetFields {
		if key == "" {
			return nil, plugin.ErrInvalidConfig
		}
		if _, supplied := fields[key]; supplied {
			return nil, fmt.Errorf("%w: patch and reset overlap", plugin.ErrInvalidConfig)
		}
		delete(owned, key)
	}
	return json.Marshal(owned)
}

func bearerMoveRefs(configID string, targetScope plugin.Scope, targetAgentID string, authority authz.Authority) json.RawMessage {
	userID := ""
	if targetScope == plugin.ScopeUser || targetScope == plugin.ScopeUserAgent {
		userID = string(authority.UserID())
	}
	ref := map[string]string{
		"name": credentialName(configID), "scope": string(targetScope),
		"user_id": userID, "agent_id": targetAgentID,
	}
	raw, _ := json.Marshal(map[string]any{"bearer": ref})
	return raw
}
