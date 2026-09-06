package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

var (
	errPluginConfigIdentity         = errors.New("mcp: plugin config identity mismatch")
	errPluginCredentialsUnavailable = errors.New("mcp: plugin credential transaction unavailable")
	errOAuthBundleChanged           = errors.New("mcp: oauth bundle changed during refresh")
)

type credentialSnapshot struct {
	BearerToken  string
	Bundle       *OAuthBundle
	BundleRaw    []byte
	ClientSecret string
}

type pluginConfigIdentity struct {
	ID        string
	PluginID  string
	Namespace string
	Scope     string
	UserID    string
	AgentID   string
	Revision  int64
}

// loadCredentialSnapshot reads the exact common config and its vault entries
// from one repeatable-read transaction. There is no legacy fallback: a
// registration without a common identity is rejected before any secret read.
func (s *Service) loadCredentialSnapshot(ctx context.Context, reg Registration, owner CredentialOwner) (credentialSnapshot, error) {
	if err := validateCredentialOwner(reg, owner); err != nil {
		return credentialSnapshot{}, err
	}
	if s == nil || s.pool == nil || s.bindVault == nil {
		return credentialSnapshot{}, errPluginCredentialsUnavailable
	}
	if _, err := uuid.Parse(reg.ID); err != nil {
		return credentialSnapshot{}, fmt.Errorf("%w: invalid config id", errPluginConfigIdentity)
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return credentialSnapshot{}, fmt.Errorf("mcp: begin credential snapshot: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	identity, err := readPluginConfigIdentity(ctx, tx, reg.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		return credentialSnapshot{}, errPluginConfigIdentity
	}
	if err != nil {
		return credentialSnapshot{}, fmt.Errorf("mcp: read plugin config identity: %w", err)
	}
	if err := validatePluginConfigIdentity(identity, reg); err != nil {
		return credentialSnapshot{}, err
	}
	vault := s.bindVault(tx)
	if vault == nil {
		return credentialSnapshot{}, errPluginCredentialsUnavailable
	}
	snapshot := credentialSnapshot{}
	if reg.AuthType == AuthTypeBearer && reg.CredentialRef != "" {
		snapshot.BearerToken, err = vault.GetScoped(ctx, owner.Scope, owner.UserID, owner.AgentID, reg.CredentialRef)
		if err != nil {
			return credentialSnapshot{}, fmt.Errorf("mcp: read bearer credential: %w", err)
		}
	}
	if reg.AuthType == AuthTypeOAuth {
		var raw string
		raw, snapshot.Bundle, err = loadOAuthBundleFromVault(ctx, vault, owner, reg.ID)
		if err != nil {
			return credentialSnapshot{}, err
		}
		snapshot.BundleRaw = []byte(raw)
		secretOwner := CredentialOwner{Scope: reg.Scope, UserID: reg.UserID, AgentID: reg.AgentID}
		if reg.OAuthClientSecretRef != "" {
			snapshot.ClientSecret, err = vault.GetScoped(ctx, secretOwner.Scope, secretOwner.UserID, secretOwner.AgentID, reg.OAuthClientSecretRef)
			if err != nil {
				return credentialSnapshot{}, fmt.Errorf("mcp: read oauth client secret: %w", err)
			}
		}
	}
	return snapshot, nil
}

func (s *Service) withCredentialVault(ctx context.Context, reg Registration, owner CredentialOwner, fn func(Vault) error) error {
	if err := validateCredentialOwner(reg, owner); err != nil {
		return err
	}
	if s == nil || s.pool == nil || s.bindVault == nil {
		return errPluginCredentialsUnavailable
	}
	if _, err := uuid.Parse(reg.ID); err != nil {
		return fmt.Errorf("%w: invalid config id", errPluginConfigIdentity)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("mcp: begin credential write: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	identity, err := readPluginConfigIdentityForUpdate(ctx, tx, reg.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		return errPluginConfigIdentity
	}
	if err != nil {
		return fmt.Errorf("mcp: lock plugin config identity: %w", err)
	}
	if err := validatePluginConfigIdentity(identity, reg); err != nil {
		return err
	}
	vault := s.bindVault(tx)
	if vault == nil {
		return errPluginCredentialsUnavailable
	}
	if err := fn(vault); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("mcp: commit credential write: %w", err)
	}
	return nil
}

func readPluginConfigIdentity(ctx context.Context, tx pgx.Tx, id string) (pluginConfigIdentity, error) {
	return readPluginConfigIdentityQuery(ctx, tx, id, false)
}

func readPluginConfigIdentityForUpdate(ctx context.Context, tx pgx.Tx, id string) (pluginConfigIdentity, error) {
	return readPluginConfigIdentityQuery(ctx, tx, id, true)
}

func readPluginConfigIdentityQuery(ctx context.Context, tx pgx.Tx, id string, lock bool) (pluginConfigIdentity, error) {
	query := `SELECT id, plugin_id, namespace, scope, user_id, agent_id, revision FROM plugin_config WHERE id = $1::uuid`
	if lock {
		query += " FOR UPDATE"
	}
	var row pluginConfigIdentity
	var userID, agentID pgtype.Text
	err := tx.QueryRow(ctx, query, id).Scan(&row.ID, &row.PluginID, &row.Namespace, &row.Scope, &userID, &agentID, &row.Revision)
	if err != nil {
		return pluginConfigIdentity{}, err
	}
	if userID.Valid {
		row.UserID = userID.String
	}
	if agentID.Valid {
		row.AgentID = agentID.String
	}
	return row, nil
}

func validatePluginConfigIdentity(identity pluginConfigIdentity, reg Registration) error {
	if identity.Revision < 1 || reg.ConfigRevision < 1 ||
		identity.ID != reg.ID || identity.PluginID != reg.PluginID || identity.Namespace != reg.Namespace ||
		identity.Scope != reg.Scope || identity.UserID != reg.UserID || identity.AgentID != reg.AgentID ||
		identity.Revision != reg.ConfigRevision {
		return fmt.Errorf("%w for %q", errPluginConfigIdentity, reg.ID)
	}
	return nil
}

// validatePluginConfigRegistration fences metadata-only OAuth operations before
// they perform network discovery. It intentionally does not read Vault: a
// first authorization flow has no token bundle yet.
func (s *Service) validatePluginConfigRegistration(ctx context.Context, reg Registration) error {
	if s == nil || s.pool == nil || s.bindVault == nil {
		return errPluginCredentialsUnavailable
	}
	if _, err := uuid.Parse(reg.ID); err != nil {
		return fmt.Errorf("%w: invalid config id", errPluginConfigIdentity)
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return fmt.Errorf("mcp: begin plugin config validation: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	identity, err := readPluginConfigIdentity(ctx, tx, reg.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		return errPluginConfigIdentity
	}
	if err != nil {
		return fmt.Errorf("mcp: read plugin config identity: %w", err)
	}
	return validatePluginConfigIdentity(identity, reg)
}

func (s *Service) loadOAuthClientSecret(ctx context.Context, reg Registration) (string, error) {
	if s == nil || s.pool == nil || s.bindVault == nil {
		return "", errPluginCredentialsUnavailable
	}
	if _, err := uuid.Parse(reg.ID); err != nil {
		return "", fmt.Errorf("%w: invalid config id", errPluginConfigIdentity)
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return "", fmt.Errorf("mcp: begin oauth client secret read: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	identity, err := readPluginConfigIdentity(ctx, tx, reg.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", errPluginConfigIdentity
	}
	if err != nil {
		return "", fmt.Errorf("mcp: read plugin config identity: %w", err)
	}
	if err := validatePluginConfigIdentity(identity, reg); err != nil {
		return "", err
	}
	vault := s.bindVault(tx)
	if vault == nil {
		return "", errPluginCredentialsUnavailable
	}
	if reg.OAuthClientSecretRef == "" {
		return "", nil
	}
	secret, err := vault.GetScoped(ctx, reg.Scope, reg.UserID, reg.AgentID, reg.OAuthClientSecretRef)
	if err != nil {
		return "", fmt.Errorf("mcp: read oauth client secret: %w", err)
	}
	return secret, nil
}

func validateCredentialOwner(reg Registration, owner CredentialOwner) error {
	if reg.CredentialMode == CredentialModePerUser {
		if owner.Scope != ScopeUser || owner.UserID == "" || owner.AgentID != "" {
			return fmt.Errorf("mcp: invalid per-user credential owner")
		}
		return nil
	}
	if owner.Scope != reg.Scope || owner.UserID != reg.UserID || owner.AgentID != reg.AgentID {
		return fmt.Errorf("mcp: credential owner does not match registration")
	}
	return nil
}

// setStatusForRegistration writes only the common observation row for the
// exact config revision and credential owner. Legacy status rows are dormant.
func (s *Service) setStatusForRegistration(ctx context.Context, reg Registration, owner CredentialOwner, status, reason string) error {
	if !ValidStatus(status) {
		return fmt.Errorf("mcp: invalid status %q", status)
	}
	return s.persistCommonStatus(ctx, reg, owner, status, reason)
}

func loadOAuthBundleFromVault(ctx context.Context, vault Vault, owner CredentialOwner, configID string) (string, *OAuthBundle, error) {
	raw, err := vault.GetScoped(ctx, owner.Scope, owner.UserID, owner.AgentID, oauthBundleName(configID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", nil, nil
		}
		return "", nil, fmt.Errorf("mcp: read oauth bundle: %w", err)
	}
	if raw == "" {
		return raw, nil, nil
	}
	var bundle OAuthBundle
	if err := json.Unmarshal([]byte(raw), &bundle); err != nil {
		return "", nil, fmt.Errorf("mcp: decode oauth bundle: %w", err)
	}
	return raw, &bundle, nil
}

func bundleDigest(raw []byte) [32]byte { return sha256.Sum256(raw) }

func rawBundleMatches(got, expected []byte) bool {
	return bundleDigest(got) == bundleDigest(expected)
}

func writeOAuthBundle(ctx context.Context, vault Vault, owner CredentialOwner, configID string, bundle OAuthBundle) error {
	raw, err := json.Marshal(bundle)
	if err != nil {
		return fmt.Errorf("mcp: encode oauth bundle: %w", err)
	}
	if IsSystemScope(owner.Scope) {
		return vault.SetSystemScoped(ctx, owner.Scope, owner.AgentID, oauthBundleName(configID), string(raw))
	}
	return vault.SetScoped(ctx, owner.Scope, owner.UserID, owner.AgentID, oauthBundleName(configID), string(raw))
}

func deleteOAuthBundle(ctx context.Context, vault Vault, owner CredentialOwner, configID string) error {
	if IsSystemScope(owner.Scope) {
		return vault.DeleteSystemScoped(ctx, owner.Scope, owner.AgentID, oauthBundleName(configID))
	}
	return vault.DeleteScoped(ctx, owner.Scope, owner.UserID, owner.AgentID, oauthBundleName(configID))
}
