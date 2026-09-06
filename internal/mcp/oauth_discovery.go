package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
	"golang.org/x/oauth2"

	"github.com/CherryHQ/stella/internal/authz"
	"github.com/CherryHQ/stella/internal/plugin"
)

type oauthAuthorityContextKey struct{}

// ErrOAuthClientInitializationRequired tells a non-admin caller that a
// system-scoped config needs one administrator-owned DCR initialization before
// users may authorize their per-user account.
var ErrOAuthClientInitializationRequired = errors.New("mcp: OAuth client initialization requires an administrator")

const (
	oauthTokenEndpointAuthMethodBasic = "client_secret_basic"
	oauthTokenEndpointAuthMethodPost  = "client_secret_post"
	oauthTokenEndpointAuthMethodNone  = "none"
)

// oauthTokenEndpointAuthStyle validates the RFC 7591 token endpoint auth
// method and returns both the x/oauth2 style and its canonical wire value.
// An omitted method has the RFC default, client_secret_basic.
func oauthTokenEndpointAuthStyle(method string) (oauth2.AuthStyle, string, error) {
	if method == "" {
		method = oauthTokenEndpointAuthMethodBasic
	}
	switch method {
	case oauthTokenEndpointAuthMethodBasic:
		return oauth2.AuthStyleInHeader, method, nil
	case oauthTokenEndpointAuthMethodPost:
		return oauth2.AuthStyleInParams, method, nil
	case oauthTokenEndpointAuthMethodNone:
		return oauth2.AuthStyleInParams, method, nil
	default:
		return 0, "", fmt.Errorf("mcp: unsupported OAuth token endpoint auth method %q", method)
	}
}

func oauthMetadataTokenEndpointAuthMethod(metadata map[string]any) string {
	oauthMetadata, _ := metadata["oauth"].(map[string]any)
	method, _ := oauthMetadata["token_endpoint_auth_method"].(string)
	return method
}

func withOAuthAuthority(ctx context.Context, authority authz.Authority) context.Context {
	return context.WithValue(ctx, oauthAuthorityContextKey{}, authority)
}

func oauthAuthority(ctx context.Context) (authz.Authority, bool) {
	authority, ok := ctx.Value(oauthAuthorityContextKey{}).(authz.Authority)
	return authority, ok && authority.Valid()
}

// oauth2Context returns a context whose HTTP client is the SSRF-safe one, so
// x/oauth2 exchange and refresh calls obey the same dial policy as discovery.
func oauth2Context(parent context.Context, policy EndpointPolicy) context.Context {
	return context.WithValue(parent, oauth2.HTTPClient, oauthHTTPClient(policy))
}

// challenge fetch + PRM/AS discovery + client resolution for StartOAuth.
// Every outbound request rides oauthHTTPClient(): the SSRF-safe dialer and
// redirect policy apply to metadata, registration, and token endpoints exactly
// as they do to MCP traffic (#1196).

// fetchChallenge asks the MCP endpoint for a 401 WWW-Authenticate challenge.
// A 2xx answer means the server needs no authorization at all.
func fetchChallenge(ctx context.Context, reg Registration, policy EndpointPolicy) ([]oauthex.Challenge, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reg.URL, nil)
	if err != nil {
		return nil, fmt.Errorf("mcp: invalid endpoint url: malformed URL")
	}
	resp, err := oauthHTTPClient(policy).Do(req)
	if err != nil {
		return nil, fmt.Errorf("mcp: reach endpoint: %w", err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil, fmt.Errorf("mcp: endpoint does not require authorization")
	}
	if resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusForbidden {
		return nil, fmt.Errorf("mcp: endpoint returned status %d", resp.StatusCode)
	}
	challenges, err := oauthex.ParseWWWAuthenticate(resp.Header[http.CanonicalHeaderKey("WWW-Authenticate")])
	if err != nil {
		return nil, fmt.Errorf("mcp: parse WWW-Authenticate: %w", err)
	}
	return challenges, nil
}

// protectedResourceMetadata resolves the PRM: from the challenge's
// resource_metadata URL when present, otherwise the well-known locations under
// the MCP endpoint, and finally the 2025-03-26 fallback that treats the MCP
// server root as the authorization server itself.
func protectedResourceMetadata(ctx context.Context, reg Registration, challenges []oauthex.Challenge, policy EndpointPolicy) (*oauthex.ProtectedResourceMetadata, error) {
	client := oauthHTTPClient(policy)
	for _, candidate := range protectedResourceMetadataURLs(challenges, reg.URL) {
		prm, err := oauthex.GetProtectedResourceMetadata(ctx, candidate.metadataURL, candidate.resource, client)
		if err != nil || prm == nil {
			continue
		}
		if len(prm.AuthorizationServers) == 0 {
			return nil, fmt.Errorf("mcp: protected resource metadata has no authorization servers")
		}
		return prm, nil
	}
	// 2025-03-26 fallback: the MCP server root is the authorization server.
	root, err := url.Parse(reg.URL)
	if err != nil {
		return nil, fmt.Errorf("mcp: parse endpoint url: %w", err)
	}
	root.Path = ""
	return &oauthex.ProtectedResourceMetadata{
		Resource:             reg.URL,
		AuthorizationServers: []string{root.String()},
	}, nil
}

type prmCandidate struct{ metadataURL, resource string }

func protectedResourceMetadataURLs(challenges []oauthex.Challenge, serverURL string) []prmCandidate {
	var out []prmCandidate
	for _, c := range challenges {
		if u := c.Params["resource_metadata"]; u != "" {
			out = append(out, prmCandidate{metadataURL: u, resource: serverURL})
		}
	}
	if u, err := url.Parse(serverURL); err == nil {
		wellKnown := *u
		wellKnown.Path = strings.TrimSuffix(u.Path, "/") + "/.well-known/oauth-protected-resource"
		out = append(out, prmCandidate{metadataURL: wellKnown.String(), resource: serverURL})
		root := *u
		root.Path = ""
		out = append(out, prmCandidate{metadataURL: root.String() + "/.well-known/oauth-protected-resource", resource: serverURL})
	}
	return out
}

// authServerMetadata resolves the AS metadata, falling back to the 2025-03-26
// convention (/authorize /token /register under the issuer) when the issuer
// publishes none.
func authServerMetadata(ctx context.Context, issuer string, policy EndpointPolicy) (*oauthex.AuthServerMeta, error) {
	asm, err := auth.GetAuthServerMetadata(ctx, issuer, oauthHTTPClient(policy))
	if err != nil {
		return nil, fmt.Errorf("mcp: fetch authorization server metadata: %w", err)
	}
	if asm == nil {
		asm = &oauthex.AuthServerMeta{
			Issuer:                issuer,
			AuthorizationEndpoint: strings.TrimRight(issuer, "/") + "/authorize",
			TokenEndpoint:         strings.TrimRight(issuer, "/") + "/token",
			RegistrationEndpoint:  strings.TrimRight(issuer, "/") + "/register",
		}
	}
	if asm.AuthorizationEndpoint == "" || asm.TokenEndpoint == "" {
		return nil, fmt.Errorf("mcp: authorization server metadata is incomplete")
	}
	return asm, nil
}

// resolveOAuthClient returns the client credentials for one registration:
// the pre-registered client from metadata + vault when configured, otherwise
// a DCR registration whose result is persisted so it runs once per
// registration.
func (s *Service) resolveOAuthClient(ctx context.Context, reg Registration, asm *oauthex.AuthServerMeta, callback string) (Registration, string, string, oauth2.AuthStyle, error) {
	if reg.OAuthClientID != "" {
		secret, secretErr := s.oauthClientSecret(ctx, reg)
		if secretErr != nil {
			return Registration{}, "", "", 0, secretErr
		}
		authStyle, _, err := oauthTokenEndpointAuthStyle(oauthMetadataTokenEndpointAuthMethod(reg.Metadata))
		if err != nil {
			return Registration{}, "", "", 0, err
		}
		return reg, reg.OAuthClientID, secret, authStyle, nil
	}
	if asm.RegistrationEndpoint == "" {
		return Registration{}, "", "", 0, fmt.Errorf("mcp: server has no registration endpoint and no pre-registered client is configured")
	}
	if authority, ok := oauthAuthority(ctx); ok && !authority.IsAdmin() && IsSystemScope(reg.Scope) {
		return Registration{}, "", "", 0, ErrOAuthClientInitializationRequired
	}
	resp, err := oauthex.RegisterClient(ctx, asm.RegistrationEndpoint, &oauthex.ClientRegistrationMetadata{
		RedirectURIs:  []string{callback},
		GrantTypes:    []string{"authorization_code", "refresh_token"},
		ResponseTypes: []string{"code"},
		ClientName:    "Stella",
	}, oauthHTTPClient(s.endpoints))
	if err != nil {
		return Registration{}, "", "", 0, fmt.Errorf("mcp: dynamic client registration: %w", err)
	}
	authStyle, authMethod, err := oauthTokenEndpointAuthStyle(resp.TokenEndpointAuthMethod)
	if err != nil {
		return Registration{}, "", "", 0, err
	}
	// Persist the normalized method alongside client_id so a later flow for a
	// pre-registered client uses the same protocol choice. The value is
	// metadata, never a secret, and remains outside safe API projections.
	resp.TokenEndpointAuthMethod = authMethod
	updated, err := s.persistDCRClient(ctx, reg, resp)
	if err != nil {
		return Registration{}, "", "", 0, err
	}
	return updated, resp.ClientID, resp.ClientSecret, authStyle, nil
}

// persistDCRClient writes the issued client id and normalized token endpoint
// auth method into metadata.oauth, and the secret, when the server issued one,
// into the vault.
func (s *Service) persistDCRClient(ctx context.Context, reg Registration, resp *oauthex.ClientRegistrationResponse) (Registration, error) {
	authority, ok := oauthAuthority(ctx)
	if !ok {
		return Registration{}, authz.ErrForbidden
	}
	return s.persistCommonDCRClient(ctx, authority, reg, resp)
}

func (s *Service) persistCommonDCRClient(ctx context.Context, authority authz.Authority, reg Registration, resp *oauthex.ClientRegistrationResponse) (Registration, error) {
	if resp == nil || resp.ClientID == "" {
		return Registration{}, errors.New("mcp: dynamic client registration returned no client id")
	}
	owner := s.CredentialOwner(reg, string(authority.UserID()))
	// DCR creates the config-owned client credential. For an administrator
	// initializing a system-scoped per-user config, use the config tuple rather
	// than the administrator's eventual per-user bundle tuple.
	if authority.IsAdmin() && (IsSystemScope(reg.Scope) || reg.CredentialMode == CredentialModePerUser) {
		owner = CredentialOwner{Scope: reg.Scope, UserID: reg.UserID, AgentID: reg.AgentID}
	}
	var updatedReg Registration
	err := s.withCredentialMutationTx(ctx, authority, reg.PluginID, reg.ID, reg.ConfigRevision, owner, func(mutationCtx context.Context, access *plugin.Access, _ plugin.Config, mutation CredentialMutation) error {
		cfg, err := access.GetConfig(mutationCtx, reg.PluginID, reg.ID)
		if err != nil {
			return fmt.Errorf("mcp: read plugin config for DCR: %w", err)
		}
		if cfg.Revision != reg.ConfigRevision || cfg.Namespace != reg.Namespace || string(cfg.Scope) != reg.Scope || cfg.UserID != reg.UserID || cfg.AgentID != reg.AgentID {
			return errPluginConfigIdentity
		}
		payload, err := decodeJSONObject(cfg.Payload, "MCP config payload")
		if err != nil {
			return err
		}
		mcpPayload, err := decodeMCPPluginPayload(cfg.Payload)
		if err != nil {
			return err
		}
		metadata := map[string]json.RawMessage{}
		if raw, exists := payload["metadata"]; exists {
			metadata, err = decodeJSONObject(raw, "MCP metadata")
			if err != nil {
				return err
			}
		}
		oauthMetadata := map[string]json.RawMessage{}
		if raw, exists := metadata["oauth"]; exists {
			oauthMetadata, err = decodeJSONObject(raw, "MCP oauth metadata")
			if err != nil {
				return err
			}
		}
		var existingClientID string
		if raw, ok := oauthMetadata["client_id"]; ok {
			if err := json.Unmarshal(raw, &existingClientID); err != nil {
				return errors.New("mcp: OAuth client id metadata is invalid")
			}
		}
		if mcpPayload.AuthType != AuthTypeOAuth {
			return errors.New("mcp: DCR requires OAuth auth")
		}
		if existingClientID != "" {
			return ErrVersionConflict
		}
		if _, _, err := oauthTokenEndpointAuthStyle(resp.TokenEndpointAuthMethod); err != nil {
			return err
		}
		oauthMetadata["client_id"], _ = json.Marshal(resp.ClientID)
		oauthMetadata["token_endpoint_auth_method"], _ = json.Marshal(resp.TokenEndpointAuthMethod)
		metadata["oauth"], _ = json.Marshal(oauthMetadata)
		payload["metadata"], _ = json.Marshal(metadata)
		payloadRaw, err := json.Marshal(payload)
		if err != nil {
			return err
		}

		refs, err := decodeJSONObject(cfg.CredentialRefs, "MCP credential refs")
		if err != nil {
			return err
		}
		_, _, existingSecretRef, err := decodeMCPPluginCredentialRefs(cfg.CredentialRefs, cfg, mcpPayload.AuthType, mcpPayload.CredentialMode)
		if err != nil {
			return err
		}
		if resp.ClientSecret != "" {
			refs["oauth_client_secret"], _ = json.Marshal(map[string]string{
				"name": oauthClientSecretName(reg.ID), "scope": string(cfg.Scope),
				"user_id": cfg.UserID, "agent_id": cfg.AgentID,
			})
		} else if existingSecretRef != "" {
			if existingClientID == "" {
				return errors.New("mcp: dynamic registration returned no secret for an existing OAuth client secret")
			}
		}
		refsRaw, err := json.Marshal(refs)
		if err != nil {
			return err
		}
		if _, err := updateCredentialConfig(mutationCtx, access, mutation.tx, cfg, resp.ClientSecret != "", plugin.ConfigPatch{
			PayloadSet: true, Payload: payloadRaw, CredentialRefsSet: true, CredentialRefs: refsRaw,
		}); err != nil {
			return fmt.Errorf("mcp: persist DCR client id: %w", err)
		}
		updated, err := access.GetConfig(mutationCtx, reg.PluginID, reg.ID)
		if err != nil {
			return fmt.Errorf("mcp: reread DCR config: %w", err)
		}
		if resp.ClientSecret != "" {
			// UpdateConfig increments the revision and adds the client-secret
			// locator. Rebind the typed capability to the authoritative config
			// while keeping the same transaction-bound Vault.
			updatedMutation := CredentialMutation{tx: mutation.tx, config: updated, owner: owner, vault: mutation.vault, configManaged: mutation.configManaged}
			if err := updatedMutation.storeOAuthClientSecret(mutationCtx, resp.ClientSecret); err != nil {
				return fmt.Errorf("mcp: persist DCR client secret: %w", err)
			}
		}
		def, err := access.GetDefinition(mutationCtx, updated.PluginID)
		if err != nil {
			return fmt.Errorf("mcp: read DCR definition: %w", err)
		}
		effective, err := plugin.Resolve(def, []plugin.Config{updated}, updated.UserID, updated.AgentID)
		if err != nil {
			return fmt.Errorf("mcp: resolve DCR config: %w", err)
		}
		updatedReg, err = RegistrationFromPluginConfig(def, updated, effective, PluginMCPObservation{ConfigRevision: updated.Revision}, authority)
		return err
	})
	if err != nil {
		return Registration{}, err
	}
	return updatedReg, nil
}
