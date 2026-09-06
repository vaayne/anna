package mcp

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
	"golang.org/x/oauth2"
)

// oauthSession implements mcpauth.OAuthHandler for one (registration, owner)
// pair. It is never interactive: a 401/403 challenge is surfaced as a durable
// needs_auth status and an error the model can act on, while valid bundles are
// injected per request and refreshed singleflight-style just before expiry.
type oauthSession struct {
	svc   *Service
	reg   Registration
	owner CredentialOwner
}

// TokenSource is called per outgoing request by the streamable transport. The
// returned source loads the bundle fresh each call and refreshes through the
// singleflight lock, so a restart or a reconnect in another process is picked
// up immediately.
func (h *oauthSession) TokenSource(ctx context.Context) (oauth2.TokenSource, error) {
	return &oauthRefreshSource{svc: h.svc, reg: h.reg, owner: h.owner}, nil
}

// Authorize handles an eligible 401/403. It never starts an interactive flow
// from a tool call: the status flips to needs_auth and the caller gets a fixed
// recovery hint. Always returning non-nil prevents the transport's retry from
// looping (#1196).
func (h *oauthSession) Authorize(ctx context.Context, req *http.Request, resp *http.Response) error {
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	reason := "credential rejected by server"
	if challenges, err := oauthex.ParseWWWAuthenticate(resp.Header[http.CanonicalHeaderKey("WWW-Authenticate")]); err == nil {
		if code := challengeError(challenges); code != "" {
			reason = "credential rejected by server: " + code
		}
	}
	_ = h.svc.setStatusForRegistration(ctx, h.reg, h.owner, StatusNeedsAuth, reason)
	return fmt.Errorf("mcp: %s", credentialRejectedHint)
}

// challengeError extracts the RFC 6750 error code from a challenge. Only the
// three codes the RFC defines are persisted: the header is remote-controlled
// text and must not reach status_error verbatim.
func challengeError(challenges []oauthex.Challenge) string {
	for _, c := range challenges {
		switch code := c.Params["error"]; code {
		case "invalid_request", "invalid_token", "insufficient_scope":
			return code
		}
	}
	return ""
}

// oauthRefreshSource is a stateless TokenSource over the vault bundle: every
// Token() call re-reads the bundle, so refreshes persist across sessions and
// processes.
type oauthRefreshSource struct {
	svc   *Service
	reg   Registration
	owner CredentialOwner
}

func (ts *oauthRefreshSource) Token() (*oauth2.Token, error) {
	unlock := ts.svc.lockRefresh(ts.reg.ID, ts.owner)
	defer unlock()

	// Bind the refresh round trip to the SSRF-safe client (test-hook aware);
	// oauth2 would otherwise dial with http.DefaultClient.
	ctx, cancel := context.WithTimeout(oauth2Context(context.Background(), ts.svc.endpoints), oauthExchangeTimeout)
	defer cancel()
	snapshot, err := ts.svc.loadCredentialSnapshot(ctx, ts.reg, ts.owner)
	if err != nil {
		return nil, err
	}
	bundle, expectedRaw := snapshot.Bundle, snapshot.BundleRaw
	if bundle == nil || bundle.AccessToken == "" {
		return nil, fmt.Errorf("mcp: %s", credentialRejectedHint)
	}
	tok := &oauth2.Token{
		AccessToken:  bundle.AccessToken,
		TokenType:    "Bearer",
		Expiry:       bundle.AccessExpiresAt,
		RefreshToken: bundle.RefreshToken,
	}
	if !tok.Expiry.IsZero() && time.Until(tok.Expiry) > oauthRefreshSlop {
		return tok, nil
	}
	if tok.RefreshToken == "" {
		if tok.Expiry.IsZero() || time.Until(tok.Expiry) > 0 {
			return tok, nil // non-expiring or still-valid token without a refresh grant
		}
		return nil, fmt.Errorf("mcp: %s", credentialRejectedHint)
	}
	clientSecret, err := ts.svc.oauthClientSecret(ctx, ts.reg)
	if err != nil {
		return nil, err
	}
	refreshed, err := (&oauth2.Config{
		ClientID:     bundle.ClientID,
		ClientSecret: clientSecret,
		Endpoint:     oauth2.Endpoint{TokenURL: bundle.TokenEndpoint, AuthStyle: oauth2.AuthStyle(bundle.AuthStyle)},
	}).TokenSource(ctx, tok).Token()
	if err != nil {
		// Refresh failure is terminal for this credential. Mark it before
		// returning so the provider cannot repeatedly offer a dead token.
		_ = ts.svc.setStatusForRegistration(ctx, ts.reg, ts.owner, StatusNeedsAuth, credentialRejectedHint)
		return nil, fmt.Errorf("mcp: refresh oauth token: %w", err)
	}
	bundle.AccessToken = refreshed.AccessToken
	bundle.AccessExpiresAt = refreshed.Expiry
	if refreshed.RefreshToken != "" {
		bundle.RefreshToken = refreshed.RefreshToken
	}
	if err := ts.svc.storeBundleCAS(ctx, ts.reg, ts.owner, *bundle, expectedRaw); err != nil {
		return nil, err
	}
	return refreshed, nil
}

// lockRefresh serializes refreshes per (registration, owner). The lock guards
// only the load-refresh-store cycle; reads of a consistent bundle are safe.
func (s *Service) lockRefresh(regID string, owner CredentialOwner) func() {
	key := regID + "|" + owner.Scope + "|" + owner.UserID + "|" + owner.AgentID
	entry, _ := s.refreshLocks.LoadOrStore(key, &sync.Mutex{})
	mu := entry.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// oauthClientSecret reads the confidential client secret, which always lives
// at the registration's own scope: the client is registered once per
// registration even when token bundles are per user.

func (s *Service) oauthClientSecret(ctx context.Context, reg Registration) (string, error) {
	return s.loadOAuthClientSecret(ctx, reg)
}

// flowParams shapes the flow row insert.
func flowParams(flowID string, reg Registration, userID, verifier string, configRaw []byte, expiresAt time.Time) CreateMCPOAuthFlowParams {
	owner := credentialOwnerFor(reg, userID)
	return CreateMCPOAuthFlowParams{
		ID: flowID, ServerID: reg.ID, UserID: userID,
		CredentialScope: owner.Scope, CredentialUserID: textNarg(owner.UserID), CredentialAgentID: textNarg(owner.AgentID),
		PkceVerifier: verifier, OauthConfig: configRaw, ExpiresAt: expiresAt,
	}
}

func textNarg(v string) pgtype.Text { return pgtype.Text{String: v, Valid: v != ""} }

// tokenScope reads the raw granted-scope string the provider echoed back.
func tokenScope(tok *oauth2.Token) string {
	if tok == nil {
		return ""
	}
	if raw, ok := tok.Extra("scope").(string); ok {
		return raw
	}
	return ""
}

// scopesFromChallenges collects space-separated scope values from Bearer
// challenges (mirrors the SDK's own extraction).
func scopesFromChallenges(challenges []oauthex.Challenge) []string {
	var out []string
	for _, c := range challenges {
		if c.Scheme != "bearer" {
			continue
		}
		for scope := range strings.FieldsSeq(c.Params["scope"]) {
			if scope != "" {
				out = append(out, scope)
			}
		}
	}
	return out
}
