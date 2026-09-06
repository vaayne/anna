package vault

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"filippo.io/age"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/CherryHQ/stella/internal/authz"
	oauth "github.com/CherryHQ/stella/internal/connections/oauth"
	agentaccess "github.com/CherryHQ/stella/internal/core/access"
	"github.com/CherryHQ/stella/pkg/db/pgnull"
	"github.com/CherryHQ/stella/pkg/db/sqlc"
)

const (
	ScopeUser        = "user"
	ScopeUserAgent   = "user_agent"
	ScopeSystem      = "system"
	ScopeSystemAgent = "system_agent"
)

// DB is the minimal database interface the vault Service requires.
type DB interface {
	GetVaultUser(ctx context.Context, id string) (sqlc.VaultUser, error)
	GetVaultEntryByScope(ctx context.Context, arg sqlc.GetVaultEntryByScopeParams) (sqlc.VaultEntry, error)
	ListVaultEntriesByScope(ctx context.Context, arg sqlc.ListVaultEntriesByScopeParams) ([]sqlc.VaultEntry, error)
	ListVaultEntriesForRuntime(ctx context.Context, arg sqlc.ListVaultEntriesForRuntimeParams) ([]sqlc.VaultEntry, error)
	UpsertVaultEntryByScope(ctx context.Context, arg sqlc.UpsertVaultEntryByScopeParams) (sqlc.VaultEntry, error)
	DeleteVaultEntryByScope(ctx context.Context, arg sqlc.DeleteVaultEntryByScopeParams) error
}

// Service provides vault operations: storing, retrieving, and decrypting
// secrets using user-level or system-level age encryption.
type Service struct {
	db              DB
	queries         *sqlc.Queries
	masterIdentity  *age.X25519Identity
	masterRecipient *age.X25519Recipient

	// agents powers the ResourceVault access rules (see access.go). It may be nil
	// for trusted-only Service instances that never open an Access (e.g. tests of
	// the raw crypto/persistence methods).
	agents *agentaccess.Service

	systemManagedMu       sync.RWMutex
	systemManagedNames    map[string]struct{}
	systemManagedPrefixes []string
}

// NewService creates a vault Service. masterIdentityStr is the raw age secret
// key string (typically from the STELLA_VAULT_KEY environment variable). agents
// backs the ResourceVault access rules; pass nil only for trusted-only instances.
func NewService(db DB, masterIdentityStr string, agents *agentaccess.Service) (*Service, error) {
	id, recipient, err := ParseMasterIdentity(masterIdentityStr)
	if err != nil {
		return nil, fmt.Errorf("vault: new service: %w", err)
	}
	return &Service{
		db:                    db,
		masterIdentity:        id,
		masterRecipient:       recipient,
		agents:                agents,
		systemManagedNames:    defaultSystemManagedNames(),
		systemManagedPrefixes: []string{"OAUTH_", "MCP_TOKEN_", "MCP_OAUTH_"},
	}, nil
}

// NewServiceForPool creates a vault Service backed by the given connection
// pool, owning construction of its sqlc query set. masterIdentityStr is the raw
// age secret key string (typically from the STELLA_VAULT_KEY environment
// variable).
func NewServiceForPool(pool *pgxpool.Pool, masterIdentityStr string, agents *agentaccess.Service) (*Service, error) {
	queries := sqlc.New(pool)
	svc, err := NewService(queries, masterIdentityStr, agents)
	if err != nil {
		return nil, err
	}
	svc.queries = queries
	return svc, nil
}

// WithTx returns a vault writer bound to the caller's transaction. It is used
// only by domain composition such as MCP registration, where the credential and
// its metadata row must commit or roll back together.
func (s *Service) WithTx(tx pgx.Tx) *Service {
	if s == nil || s.queries == nil || tx == nil {
		return nil
	}
	queries := s.queries.WithTx(tx)
	return &Service{
		db:                    queries,
		queries:               queries,
		masterIdentity:        s.masterIdentity,
		masterRecipient:       s.masterRecipient,
		agents:                s.agents,
		systemManagedNames:    s.systemManagedNames,
		systemManagedPrefixes: s.systemManagedPrefixes,
	}
}

// MasterRecipient returns the master public key recipient.
// It is used when generating new user key pairs.
func (s *Service) MasterRecipient() *age.X25519Recipient {
	return s.masterRecipient
}

// EntryMeta holds non-sensitive metadata for a vault entry.
type EntryMeta struct {
	Scope       string
	UserID      string
	AgentID     string
	Name        string
	Description string
	CreatedAt   string
	UpdatedAt   string
}

type SetOptions struct {
	Description *string
}

// ScopeRequest / ResolvedScope / ResolveScope compute the durable owner columns
// for a vault-style scope. They are NOT the ResourceVault access rules (see
// access.go) — vault entries are authorized there. They remain here as a pure
// utility because unrelated resources that reuse the vault scope vocabulary
// (agent tool overrides, MCP connection tokens) still resolve their own columns
// this way. Their admin gate is a coarse structural check, not a policy decision.
type ScopeRequest struct {
	Scope        string
	UserID       string
	AgentID      string
	IsAdmin      bool
	AgentScoped  bool
	BoundAgentID string
}

type ResolvedScope struct {
	Scope   string
	UserID  string
	AgentID string
}

func ResolveScope(req ScopeRequest) (ResolvedScope, error) {
	scope := req.Scope
	if scope == "" {
		scope = ScopeUser
	}
	agentID := req.AgentID
	if req.AgentScoped {
		switch scope {
		case ScopeUser:
		case ScopeUserAgent:
			if agentID != req.BoundAgentID {
				return ResolvedScope{}, fmt.Errorf("vault: agent-scoped identity cannot access another agent's vault: %w", authz.ErrForbidden)
			}
		default:
			return ResolvedScope{}, fmt.Errorf("vault: system-scoped secrets are managed by operators: %w", authz.ErrForbidden)
		}
	}
	out := ResolvedScope{Scope: scope}
	userID := req.UserID
	switch scope {
	case ScopeUser:
		out.UserID = userID
	case ScopeUserAgent:
		if agentID == "" {
			return ResolvedScope{}, fmt.Errorf("vault: agent_id is required for user_agent scope")
		}
		out.UserID = userID
		out.AgentID = agentID
	case ScopeSystem:
		if !req.IsAdmin {
			return ResolvedScope{}, fmt.Errorf("vault: admin access required: %w", authz.ErrForbidden)
		}
	case ScopeSystemAgent:
		if !req.IsAdmin {
			return ResolvedScope{}, fmt.Errorf("vault: admin access required: %w", authz.ErrForbidden)
		}
		if agentID == "" {
			return ResolvedScope{}, fmt.Errorf("vault: agent_id is required for system_agent scope")
		}
		out.AgentID = agentID
	default:
		return ResolvedScope{}, fmt.Errorf("vault: invalid scope %q", scope)
	}
	if err := validateScope(out.Scope, out.UserID, out.AgentID); err != nil {
		return ResolvedScope{}, err
	}
	return out, nil
}

// EncryptSystem encrypts plaintext with the master key for system-level storage
// (not tied to any user).
func (s *Service) EncryptSystem(plaintext string) (string, error) {
	return encryptArmored(s.masterRecipient, plaintext)
}

// DecryptSystem decrypts ciphertext that was produced by EncryptSystem.
func (s *Service) DecryptSystem(ciphertext string) (string, error) {
	return decryptArmored(s.masterIdentity, ciphertext)
}

// Set validates name, encrypts plaintext with the user's public key, and
// upserts the user-level vault entry. The user must already have age keys provisioned.
func (s *Service) Set(ctx context.Context, userID string, name string, plaintext string) error {
	return s.set(ctx, ScopeUser, userID, "", name, plaintext, true, SetOptions{})
}

// SetScoped stores a user-owned secret in the requested vault scope.
func (s *Service) SetScoped(ctx context.Context, scope string, userID string, agentID string, name string, plaintext string) error {
	return s.SetScopedWithOptions(ctx, scope, userID, agentID, name, plaintext, SetOptions{})
}

func (s *Service) SetScopedWithOptions(ctx context.Context, scope string, userID string, agentID string, name string, plaintext string, opts SetOptions) error {
	if isSystemScope(scope) {
		return fmt.Errorf("vault: system scope requires privileged caller")
	}
	return s.set(ctx, scope, userID, agentID, name, plaintext, true, opts)
}

// SetSystemScoped stores an admin-managed secret for trusted host-side callers.
func (s *Service) SetSystemScoped(ctx context.Context, scope string, agentID string, name string, plaintext string) error {
	return s.SetSystemScopedWithOptions(ctx, scope, agentID, name, plaintext, SetOptions{})
}

func (s *Service) SetSystemScopedWithOptions(ctx context.Context, scope string, agentID string, name string, plaintext string, opts SetOptions) error {
	if !isSystemScope(scope) {
		return fmt.Errorf("vault: scope %q is not system-managed", scope)
	}
	return s.set(ctx, scope, "", agentID, name, plaintext, true, opts)
}

func (s *Service) set(ctx context.Context, scope string, userID string, agentID string, name string, plaintext string, validate bool, opts SetOptions) error {
	if validate {
		if err := ValidateName(name); err != nil {
			return err
		}
	}
	if err := validateScope(scope, userID, agentID); err != nil {
		return err
	}

	ciphertext, err := s.encryptForScope(ctx, scope, userID, plaintext)
	if err != nil {
		return fmt.Errorf("vault: set %q: %w", name, err)
	}

	description := pgtype.Text{}
	if opts.Description != nil {
		description = pgnull.Text(*opts.Description)
	}
	_, err = s.db.UpsertVaultEntryByScope(ctx, sqlc.UpsertVaultEntryByScopeParams{
		ID:          uuid.Must(uuid.NewV7()).String(),
		Scope:       scope,
		UserID:      pgnull.Text(userID),
		AgentID:     pgnull.Text(agentID),
		Name:        name,
		Ciphertext:  ciphertext,
		Description: description,
	})
	if err != nil {
		return fmt.Errorf("vault: set %q: upsert: %w", name, err)
	}
	return nil
}

func (s *Service) encryptForScope(ctx context.Context, scope string, userID string, plaintext string) (string, error) {
	if scope == ScopeSystem || scope == ScopeSystemAgent {
		ciphertext, err := s.EncryptSystem(plaintext)
		if err != nil {
			return "", fmt.Errorf("encrypt system: %w", err)
		}
		return ciphertext, nil
	}

	user, err := s.db.GetVaultUser(ctx, userID)
	if err != nil {
		return "", fmt.Errorf("get user: %w", err)
	}
	if user.AgePublicKey == "" {
		return "", fmt.Errorf("user %s has no age public key provisioned", userID)
	}

	ciphertext, err := Encrypt(user.AgePublicKey, plaintext)
	if err != nil {
		return "", fmt.Errorf("encrypt: %w", err)
	}
	return ciphertext, nil
}

// Delete removes a user-level vault entry by name for the given user.
func (s *Service) Delete(ctx context.Context, userID string, name string) error {
	return s.DeleteScoped(ctx, ScopeUser, userID, "", name)
}

// DeleteScoped removes a user-owned vault entry by name and scope.
func (s *Service) DeleteScoped(ctx context.Context, scope string, userID string, agentID string, name string) error {
	if isSystemScope(scope) {
		return fmt.Errorf("vault: system scope requires privileged caller")
	}
	return s.deleteScoped(ctx, scope, userID, agentID, name)
}

// DeleteSystemScoped removes an admin-managed vault entry by name and scope.
func (s *Service) DeleteSystemScoped(ctx context.Context, scope string, agentID string, name string) error {
	if !isSystemScope(scope) {
		return fmt.Errorf("vault: scope %q is not system-managed", scope)
	}
	return s.deleteScoped(ctx, scope, "", agentID, name)
}

func (s *Service) deleteScoped(ctx context.Context, scope string, userID string, agentID string, name string) error {
	if err := validateScope(scope, userID, agentID); err != nil {
		return err
	}
	if err := s.db.DeleteVaultEntryByScope(ctx, sqlc.DeleteVaultEntryByScopeParams{
		Scope:   scope,
		UserID:  pgnull.Text(userID),
		AgentID: pgnull.Text(agentID),
		Name:    name,
	}); err != nil {
		return fmt.Errorf("vault: delete %q: %w", name, err)
	}
	return nil
}

// Get decrypts and returns the plaintext value of a single user-level vault entry by name.
func (s *Service) Get(ctx context.Context, userID string, name string) (string, error) {
	return s.GetScoped(ctx, ScopeUser, userID, "", name)
}

// GetScoped decrypts and returns one scoped vault entry by name.
func (s *Service) GetScoped(ctx context.Context, scope string, userID string, agentID string, name string) (string, error) {
	if err := validateScope(scope, userID, agentID); err != nil {
		return "", err
	}
	entry, err := s.db.GetVaultEntryByScope(ctx, sqlc.GetVaultEntryByScopeParams{
		Scope:   scope,
		UserID:  pgnull.Text(userID),
		AgentID: pgnull.Text(agentID),
		Name:    name,
	})
	if err != nil {
		return "", fmt.Errorf("vault: get %q: %w", name, err)
	}
	return s.decryptEntry(ctx, entry)
}

// GetScopedMeta returns non-sensitive metadata for a single scoped vault entry by name.
func (s *Service) GetScopedMeta(ctx context.Context, scope string, userID string, agentID string, name string) (EntryMeta, error) {
	entry, err := s.db.GetVaultEntryByScope(ctx, sqlc.GetVaultEntryByScopeParams{
		Scope:   scope,
		UserID:  pgnull.Text(userID),
		AgentID: pgnull.Text(agentID),
		Name:    name,
	})
	if err != nil {
		return EntryMeta{}, fmt.Errorf("vault: get meta %q: %w", name, err)
	}
	return s.metaFromEntry(ctx, entry), nil
}

func (s *Service) List(ctx context.Context, userID string) ([]EntryMeta, error) {
	return s.ListScoped(ctx, ScopeUser, userID, "")
}

// ListScoped returns metadata for all user-owned vault entries in one effective scope.
func (s *Service) ListScoped(ctx context.Context, scope string, userID string, agentID string) ([]EntryMeta, error) {
	if isSystemScope(scope) {
		return nil, fmt.Errorf("vault: system scope requires privileged caller")
	}
	return s.listScoped(ctx, scope, userID, agentID)
}

// ListSystemScoped returns metadata for admin-managed vault entries in one effective scope.
func (s *Service) ListSystemScoped(ctx context.Context, scope string, agentID string) ([]EntryMeta, error) {
	if !isSystemScope(scope) {
		return nil, fmt.Errorf("vault: scope %q is not system-managed", scope)
	}
	return s.listScoped(ctx, scope, "", agentID)
}

func (s *Service) listScoped(ctx context.Context, scope string, userID string, agentID string) ([]EntryMeta, error) {
	if err := validateScope(scope, userID, agentID); err != nil {
		return nil, err
	}
	entries, err := s.db.ListVaultEntriesByScope(ctx, sqlc.ListVaultEntriesByScopeParams{
		Scope:   scope,
		UserID:  pgnull.Text(userID),
		AgentID: pgnull.Text(agentID),
	})
	if err != nil {
		return nil, fmt.Errorf("vault: list: %w", err)
	}
	return s.metasFromEntries(ctx, entries), nil
}

// Lookup decrypts one user-level vault entry by name.
func (s *Service) Lookup(ctx context.Context, userID string, name string) (string, bool, error) {
	entry, err := s.db.GetVaultEntryByScope(ctx, sqlc.GetVaultEntryByScopeParams{
		Scope:   ScopeUser,
		UserID:  pgnull.Text(userID),
		AgentID: pgnull.Text(""),
		Name:    name,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", false, nil
		}
		return "", false, err
	}
	plaintext, err := s.decryptEntry(ctx, entry)
	if err != nil {
		return "", false, err
	}
	return plaintext, true, nil
}

// LoadEnvForAgent resolves runtime env in the SQL precedence order;
// later scopes override earlier scopes. System-managed names stay internal-only.
func (s *Service) LoadEnvForAgent(ctx context.Context, userID string, agentID string) (map[string]string, error) {
	entries, err := s.db.ListVaultEntriesForRuntime(ctx, sqlc.ListVaultEntriesForRuntimeParams{
		UserID:         pgnull.Text(userID),
		RuntimeAgentID: pgnull.Text(agentID),
	})
	if err != nil {
		return nil, fmt.Errorf("vault: load env: list entries: %w", err)
	}

	env := make(map[string]string, len(entries))
	userCache := make(map[string]sqlc.VaultUser)
	for _, e := range entries {
		if !s.isAmbientSecretName(e.Name) {
			continue
		}
		plaintext, err := s.decryptEntryWithUserCache(ctx, e, userCache)
		if err != nil {
			slog.Warn("vault env entry skipped",
				"component", "vault",
				"scope", e.Scope,
				"name", e.Name,
				"error", err,
			)
			continue
		}
		env[e.Name] = plaintext
	}
	return env, nil
}

// AmbientSecretMeta is prompt-safe ambient secret metadata: the env var name a
// tool reads plus the user's hint about what it holds. Never carries values.
type AmbientSecretMeta struct {
	Name        string
	Description string
}

// ListAmbientSecretMetas returns prompt-safe metadata for ambient secrets only.
func (s *Service) ListAmbientSecretMetas(ctx context.Context, userID string, agentID string) ([]AmbientSecretMeta, error) {
	entries, err := s.db.ListVaultEntriesForRuntime(ctx, sqlc.ListVaultEntriesForRuntimeParams{
		UserID:         pgnull.Text(userID),
		RuntimeAgentID: pgnull.Text(agentID),
	})
	if err != nil {
		return nil, fmt.Errorf("vault: list ambient secret metas: list entries: %w", err)
	}

	byName := make(map[string]AmbientSecretMeta, len(entries))
	for _, e := range entries {
		if !s.isAmbientSecretName(e.Name) {
			continue
		}
		byName[e.Name] = AmbientSecretMeta{Name: e.Name, Description: stringFromNull(e.Description)}
	}

	names := make([]string, 0, len(byName))
	for name := range byName {
		names = append(names, name)
	}
	sort.Strings(names)
	metas := make([]AmbientSecretMeta, 0, len(names))
	for _, name := range names {
		metas = append(metas, byName[name])
	}
	return metas, nil
}

func (s *Service) decryptEntry(ctx context.Context, entry sqlc.VaultEntry) (string, error) {
	return s.decryptEntryWithUserCache(ctx, entry, nil)
}

func (s *Service) decryptEntryWithUserCache(ctx context.Context, entry sqlc.VaultEntry, userCache map[string]sqlc.VaultUser) (string, error) {
	if entry.Scope == ScopeSystem || entry.Scope == ScopeSystemAgent {
		return s.DecryptSystem(entry.Ciphertext)
	}
	if !entry.UserID.Valid || entry.UserID.String == "" {
		return "", fmt.Errorf("user-scoped entry %q has no user", entry.Name)
	}
	userID := entry.UserID.String
	user, ok := userCache[userID]
	if !ok {
		var err error
		user, err = s.db.GetVaultUser(ctx, userID)
		if err != nil {
			return "", fmt.Errorf("get user: %w", err)
		}
		if userCache != nil {
			userCache[userID] = user
		}
	}
	if user.AgePrivateKey == "" {
		return "", fmt.Errorf("user %s has no age private key provisioned", userID)
	}
	plaintext, err := Decrypt(s.masterIdentity, user.AgePrivateKey, entry.Ciphertext)
	if err != nil {
		return "", fmt.Errorf("decrypt: %w", err)
	}
	return plaintext, nil
}

func (s *Service) metaFromEntry(ctx context.Context, e sqlc.VaultEntry) EntryMeta {
	meta := EntryMeta{
		Scope:       e.Scope,
		UserID:      stringFromNull(e.UserID),
		AgentID:     stringFromNull(e.AgentID),
		Name:        e.Name,
		Description: stringFromNull(e.Description),
		CreatedAt:   e.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt:   e.UpdatedAt.UTC().Format(time.RFC3339),
	}
	return meta
}

func (s *Service) metasFromEntries(ctx context.Context, entries []sqlc.VaultEntry) []EntryMeta {
	meta := make([]EntryMeta, len(entries))
	for i, e := range entries {
		meta[i] = s.metaFromEntry(ctx, e)
	}
	return meta
}

func isSystemScope(scope string) bool {
	return scope == ScopeSystem || scope == ScopeSystemAgent
}

func IsAgentScope(scope string) bool {
	return scope == ScopeUserAgent || scope == ScopeSystemAgent
}

func validateScope(scope string, userID string, agentID string) error {
	switch scope {
	case ScopeUser:
		if userID == "" || agentID != "" {
			return fmt.Errorf("vault: user scope requires user_id only")
		}
	case ScopeUserAgent:
		if userID == "" || agentID == "" {
			return fmt.Errorf("vault: user_agent scope requires user_id and agent_id")
		}
	case ScopeSystem:
		if userID != "" || agentID != "" {
			return fmt.Errorf("vault: system scope cannot include user_id or agent_id")
		}
	case ScopeSystemAgent:
		if userID != "" || agentID == "" {
			return fmt.Errorf("vault: system_agent scope requires agent_id only")
		}
	default:
		return fmt.Errorf("vault: invalid scope %q", scope)
	}
	return nil
}

func defaultSystemManagedNames() map[string]struct{} {
	names := map[string]struct{}{}
	for _, name := range []string{"STELLA_TOKEN", oauth.VaultKeyGitHub} {
		names[name] = struct{}{}
	}
	return names
}

// AddSystemManagedNames reserves host-side credential names so they are never
// exposed as ambient sandbox env or accepted from user-facing vault writes.
// OAuth bundle keys come from the provider registry; hand-enumerating them in
// vault code drifts when manifest providers are added without Go changes (the
// X provider leak was exactly that failure mode). Defaults cover built-ins even
// if startup wiring is missed.
func (s *Service) AddSystemManagedNames(names ...string) {
	s.systemManagedMu.Lock()
	defer s.systemManagedMu.Unlock()
	if s.systemManagedNames == nil {
		s.systemManagedNames = defaultSystemManagedNames()
	}
	for _, name := range names {
		if name == "" {
			continue
		}
		s.systemManagedNames[name] = struct{}{}
	}
}

// ValidateUserFacingName applies core env-var rules plus host-managed credential
// reservations. System writers use Set directly so OAuth and MCP stores can keep
// writing their internal vault rows.
func (s *Service) ValidateUserFacingName(name string) error {
	if s.isSystemManagedName(name) {
		return fmt.Errorf("vault: name %q is reserved for system-managed credentials", name)
	}
	return ValidateName(name)
}

func (s *Service) isAmbientSecretName(name string) bool {
	return !s.isSystemManagedName(name)
}

func (s *Service) isSystemManagedName(name string) bool {
	s.systemManagedMu.RLock()
	defer s.systemManagedMu.RUnlock()
	if _, ok := s.systemManagedNames[name]; ok {
		return true
	}
	for _, prefix := range s.systemManagedPrefixes {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

func stringFromNull(value pgtype.Text) string {
	if !value.Valid {
		return ""
	}
	return value.String
}
