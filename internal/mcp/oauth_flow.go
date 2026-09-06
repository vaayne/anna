package mcp

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"golang.org/x/oauth2"

	"github.com/CherryHQ/stella/internal/authz"
)

// StartOAuth runs discovery, resolves (or registers) the client, persists a
// one-shot flow row, and returns the authorization URL the user's browser must
// open. The flow binds the initiating user, so the unauthenticated callback can
// re-identify safely.
func (s *Service) StartOAuth(ctx context.Context, reg Registration, userID, callback string) (string, string, time.Time, error) {
	return s.startOAuth(ctx, reg, userID, callback, authz.Authority{})
}

// StartOAuthForAuthority carries the already verified request authority into
// DCR persistence. The authority never comes from registration fields or the
// callback URL; it is supplied by MCP Access after its PEP checks.
func (s *Service) StartOAuthForAuthority(ctx context.Context, reg Registration, authority authz.Authority, callback string) (string, string, time.Time, error) {
	if !authority.Valid() {
		return "", "", time.Time{}, authz.ErrForbidden
	}
	return s.startOAuth(ctx, reg, string(authority.UserID()), callback, authority)
}

func (s *Service) startOAuth(ctx context.Context, reg Registration, userID, callback string, authority authz.Authority) (string, string, time.Time, error) {
	if authority.Valid() {
		ctx = withOAuthAuthority(ctx, authority)
	}
	if reg.Transport != TransportStreamableHTTP {
		return "", "", time.Time{}, fmt.Errorf("mcp: auth_type %q requires the streamable_http transport", AuthTypeOAuth)
	}
	if err := s.validatePluginConfigRegistration(ctx, reg); err != nil {
		return "", "", time.Time{}, err
	}
	challenges, err := fetchChallenge(ctx, reg, s.endpoints)
	if err != nil {
		return "", "", time.Time{}, err
	}
	prm, err := protectedResourceMetadata(ctx, reg, challenges, s.endpoints)
	if err != nil {
		return "", "", time.Time{}, err
	}
	asm, err := authServerMetadata(ctx, prm.AuthorizationServers[0], s.endpoints)
	if err != nil {
		return "", "", time.Time{}, err
	}
	reg, clientID, clientSecret, authStyle, err := s.resolveOAuthClient(ctx, reg, asm, callback)
	if err != nil {
		return "", "", time.Time{}, err
	}

	scopes := scopesFromChallenges(challenges)
	if len(scopes) == 0 {
		scopes = prm.ScopesSupported
	}
	verifier := oauth2.GenerateVerifier()
	flowID := uuid.Must(uuid.NewV7()).String()
	expiresAt := time.Now().UTC().Add(oauthFlowTTL)
	secretRef := ""
	if reg.OAuthClientSecretRef != "" {
		secretRef = reg.OAuthClientSecretRef
	} else if clientSecret != "" {
		secretRef = oauthClientSecretName(reg.ID)
	}
	configRaw, err := oauthFlowConfig{
		ClientID: clientID, ClientSecretRef: secretRef,
		TokenEndpoint: asm.TokenEndpoint, AuthStyle: int(authStyle),
		Resource: prm.Resource, Scopes: scopes, RedirectURI: callback,
		PluginID: reg.PluginID, Namespace: reg.Namespace, ConfigRevision: reg.ConfigRevision,
		ConfigScope: reg.Scope, ConfigUserID: reg.UserID, ConfigAgentID: reg.AgentID,
		CredentialMode: reg.CredentialMode,
		Endpoint:       reg.URL, Transport: reg.Transport, RegistrationName: reg.Name,
	}.marshal()
	if err != nil {
		return "", "", time.Time{}, err
	}
	if _, err := s.db.CreateMCPOAuthFlow(ctx, flowParams(flowID, reg, userID, verifier, configRaw, expiresAt)); err != nil {
		return "", "", time.Time{}, fmt.Errorf("mcp: persist oauth flow: %w", err)
	}
	authURL := (&oauth2.Config{
		ClientID: clientID, ClientSecret: clientSecret,
		Endpoint:    oauth2.Endpoint{AuthURL: asm.AuthorizationEndpoint, TokenURL: asm.TokenEndpoint},
		RedirectURL: callback, Scopes: scopes,
	}).AuthCodeURL(flowID,
		oauth2.S256ChallengeOption(verifier),
		oauth2.SetAuthURLParam("resource", prm.Resource))
	return authURL, flowID, expiresAt, nil
}

// CompleteOAuth consumes the flow exactly once, exchanges the code with the
// stored verifier, writes the bundle to the flow's credential tuple, resets the
// status, and re-probes with the fresh credential.
func (s *Service) CompleteOAuth(ctx context.Context, flowID, code string) (Registration, error) {
	flow, err := s.db.ConsumeMCPOAuthFlow(ctx, flowID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Registration{}, fmt.Errorf("mcp: oauth flow is unknown, expired, or already used")
	}
	if err != nil {
		return Registration{}, fmt.Errorf("mcp: consume oauth flow: %w", err)
	}
	cfg, err := decodeOAuthFlowConfig(flow.OauthConfig)
	if err != nil {
		return Registration{}, err
	}
	owner := CredentialOwner{Scope: flow.CredentialScope, UserID: textOrEmpty(flow.CredentialUserID), AgentID: textOrEmpty(flow.CredentialAgentID)}
	if cfg.PluginID == "" || cfg.ConfigRevision < 1 {
		return Registration{}, errPluginConfigIdentity
	}
	reg, err := commonOAuthRegistration(flow, cfg)
	if err != nil {
		return Registration{}, err
	}
	if err := s.validatePluginConfigRegistration(ctx, reg); err != nil {
		return Registration{}, err
	}
	expectedOwner := s.CredentialOwner(reg, flow.UserID)
	if owner != expectedOwner {
		return Registration{}, fmt.Errorf("mcp: oauth flow credential owner does not match registration")
	}
	return s.completeCommonOAuth(ctx, flow, cfg, reg, owner, code)
}

func commonOAuthRegistration(flow McpOauthFlow, cfg oauthFlowConfig) (Registration, error) {
	if cfg.PluginID == "" || cfg.Namespace == "" || cfg.ConfigRevision < 1 ||
		cfg.ConfigScope == "" || cfg.Endpoint == "" || cfg.Transport == "" || cfg.CredentialMode == "" {
		return Registration{}, fmt.Errorf("mcp: oauth flow common plugin identity is incomplete")
	}
	if !ValidCredentialMode(cfg.CredentialMode) || !ValidScope(cfg.ConfigScope) {
		return Registration{}, fmt.Errorf("mcp: oauth flow common plugin identity is invalid")
	}
	return Registration{
		ID: flow.ServerID, PluginID: cfg.PluginID, Namespace: cfg.Namespace,
		ConfigRevision: cfg.ConfigRevision, Scope: cfg.ConfigScope,
		UserID: cfg.ConfigUserID, AgentID: cfg.ConfigAgentID,
		Name: cfg.RegistrationName, URL: cfg.Endpoint, Transport: cfg.Transport,
		AuthType: AuthTypeOAuth, Enabled: true, CredentialMode: cfg.CredentialMode,
		OAuthClientID: cfg.ClientID, OAuthClientSecretRef: cfg.ClientSecretRef,
	}, nil
}

func (s *Service) completeCommonOAuth(ctx context.Context, flow McpOauthFlow, cfg oauthFlowConfig, reg Registration, owner CredentialOwner, code string) (Registration, error) {
	// Fence immediately before the network exchange. A concurrent config edit
	// must invalidate this authorization attempt rather than minting a token for
	// the stale endpoint/config revision.
	if err := s.validatePluginConfigRegistration(ctx, reg); err != nil {
		return Registration{}, err
	}
	exchangeCtx, cancel := context.WithTimeout(oauth2Context(ctx, s.endpoints), oauthExchangeTimeout)
	defer cancel()
	clientSecret := ""
	if cfg.ClientSecretRef != "" {
		var secretErr error
		clientSecret, secretErr = s.oauthClientSecret(ctx, reg)
		if secretErr != nil {
			return Registration{}, secretErr
		}
	}
	tok, err := (&oauth2.Config{
		ClientID: cfg.ClientID, ClientSecret: clientSecret,
		Endpoint:    oauth2.Endpoint{TokenURL: cfg.TokenEndpoint, AuthStyle: oauth2.AuthStyle(cfg.AuthStyle)},
		RedirectURL: cfg.RedirectURI, Scopes: cfg.Scopes,
	}).Exchange(exchangeCtx, code,
		oauth2.VerifierOption(flow.PkceVerifier),
		oauth2.SetAuthURLParam("resource", cfg.Resource))
	if err != nil {
		return Registration{}, fmt.Errorf("mcp: exchange authorization code: %w", err)
	}
	// storeBundle takes the config-row lock and validates the captured revision
	// again before writing the Vault bundle. The callback intentionally does not
	// update legacy mcp_server status or probe columns.
	bundle := OAuthBundle{
		Version: 1, ClientID: cfg.ClientID, TokenEndpoint: cfg.TokenEndpoint,
		AuthStyle: cfg.AuthStyle, Resource: cfg.Resource,
		AccessToken: tok.AccessToken, RefreshToken: tok.RefreshToken,
		AccessExpiresAt: tok.Expiry, GrantedScope: tokenScope(tok),
	}
	if err := s.storeBundle(ctx, reg, owner, bundle); err != nil {
		return Registration{}, err
	}
	// Refresh the exact credential owner's observation after the new bundle is
	// committed. Probe writes only mcp_connection_state and fences the captured
	// config revision, so a concurrent config edit cannot publish stale tools.
	return s.Probe(ctx, reg, owner)
}

// Disconnect removes the caller-appropriate bundle and marks the server
// needs_auth, so subsequent tool calls fail closed until a reconnect.
func (s *Service) Disconnect(ctx context.Context, reg Registration, userID string) (Registration, error) {
	owner := s.CredentialOwner(reg, userID)
	if err := s.withCredentialVault(ctx, reg, owner, func(vault Vault) error {
		return deleteOAuthBundle(ctx, vault, owner, reg.ID)
	}); err != nil {
		return Registration{}, fmt.Errorf("mcp: delete oauth bundle: %w", err)
	}
	if err := s.persistCommonStatus(ctx, reg, owner, StatusNeedsAuth, credentialRejectedHint); err != nil {
		return Registration{}, err
	}
	reg.Status, reg.StatusError, reg.Tools = StatusNeedsAuth, credentialRejectedHint, nil
	return reg, nil
}

// GetMCPServerForOwner re-reads a registration by id, unmapped by scope —
// the callers have already passed the PEP for this exact row.
func (s *Service) GetMCPServerForOwner(ctx context.Context, id string) (Registration, error) {
	return Registration{}, fmt.Errorf("mcp: common registration resolution requires an immutable plugin snapshot")
}

// HasUserCredential reports whether the given user has a credential to use
// for this registration. per_user registrations need their own bundle; shared
// registrations answer for every user. False for per_user means the tools
// list should show mcp_needs_auth for exactly this user.
func (s *Service) HasUserCredential(ctx context.Context, reg Registration, userID string) bool {
	if reg.CredentialMode != CredentialModePerUser {
		return true
	}
	bundle, err := s.loadBundle(ctx, reg, s.CredentialOwner(reg, userID))
	return err == nil && bundle != nil && bundle.AccessToken != ""
}

// OAuthState is the user-facing OAuth view of one registration, computed for
// the calling user (per_user) or the registration owner (shared). No token
// material ever reaches it.
type OAuthState struct {
	Connected        bool
	AccessExpiresAt  time.Time
	NeedsReconnect   bool
	ClientRegistered bool
}

// OAuthState resolves the API projection for one registration and user.
func (s *Service) OAuthState(ctx context.Context, reg Registration, userID string) OAuthState {
	state := OAuthState{ClientRegistered: reg.OAuthClientID != ""}
	if reg.AuthType != AuthTypeOAuth {
		return state
	}
	bundle, err := s.loadBundle(ctx, reg, s.CredentialOwner(reg, userID))
	if err != nil || bundle == nil || bundle.AccessToken == "" {
		return state
	}
	state.Connected = true
	state.AccessExpiresAt = bundle.AccessExpiresAt
	state.NeedsReconnect = bundle.RefreshToken == "" && time.Now().After(bundle.AccessExpiresAt)
	return state
}
