package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/CherryHQ/stella/internal/authz"
	"github.com/CherryHQ/stella/internal/plugin"
)

// PluginMCPObservation contains the probe state kept outside plugin_config.
// It is deliberately separate from the backend payload: status and tools are
// remote observations, not authored configuration.
type PluginMCPObservation struct {
	Status           string
	StatusError      string
	ProbedAt         time.Time
	ConfigRevision   int64
	CredentialUserID string
	Tools            []CatalogTool
}

// RegistrationFromPluginConfig adapts one resolved MCP plugin config to the
// legacy service shape while the MCP service is being migrated. The config ID
// remains the registration UUID; credential references are locators only and
// are never decoded as secret values.
func RegistrationFromPluginConfig(def plugin.Definition, cfg plugin.Config, effective plugin.Effective, observation PluginMCPObservation, authority authz.Authority) (Registration, error) {
	if !authority.Valid() {
		return Registration{}, authz.ErrForbidden
	}
	if err := def.Validate(); err != nil {
		return Registration{}, fmt.Errorf("mcp plugin definition: %w", err)
	}
	if def.Backend != plugin.BackendMCP {
		return Registration{}, errors.New("mcp plugin definition has a non-MCP backend")
	}
	if err := cfg.Validate(); err != nil {
		return Registration{}, fmt.Errorf("mcp plugin config: %w", err)
	}
	if cfg.PluginID != def.ID || cfg.Namespace != def.Namespace {
		return Registration{}, errors.New("mcp plugin config does not match its definition")
	}
	if effective.PluginID != def.ID || effective.Namespace != def.Namespace || effective.ConfigID != cfg.ID {
		return Registration{}, errors.New("mcp effective config does not match its source config")
	}
	if effective.SourceScope != cfg.Scope {
		return Registration{}, errors.New("mcp effective config scope does not match its source config")
	}
	if _, err := uuid.Parse(cfg.ID); err != nil {
		return Registration{}, errors.New("mcp plugin config id is not a UUID")
	}

	payload, err := decodeMCPPluginPayload(effective.Payload)
	if err != nil {
		return Registration{}, err
	}
	credentialRef, credentialMode, oauthClientSecretRef, err := decodeMCPPluginCredentialRefs(cfg.CredentialRefs, cfg, payload.AuthType, payload.CredentialMode)
	if err != nil {
		return Registration{}, err
	}
	if oauthClientSecretRef != "" && metadataOAuthClientID(payload.Metadata) == "" {
		return Registration{}, errors.New("MCP OAuth client secret requires a client id")
	}
	status, statusError, observedAt, tools, err := safeMCPObservation(observation, cfg, credentialMode, authority)
	if err != nil {
		return Registration{}, err
	}

	return Registration{
		ID:                   cfg.ID,
		PluginID:             def.ID,
		Namespace:            def.Namespace,
		ConfigRevision:       cfg.Revision,
		Scope:                string(cfg.Scope),
		UserID:               cfg.UserID,
		AgentID:              cfg.AgentID,
		Name:                 def.DisplayName,
		URL:                  payload.URL,
		Transport:            payload.Transport,
		AuthType:             payload.AuthType,
		CredentialRef:        credentialRef,
		Enabled:              effective.IsEffectivelyEnabled,
		Status:               status,
		StatusError:          statusError,
		ProbedAt:             observedAt,
		Tools:                tools,
		CredentialMode:       credentialMode,
		Metadata:             payload.Metadata,
		Description:          payload.Description,
		OAuthClientID:        metadataOAuthClientID(payload.Metadata),
		OAuthClientSecretRef: oauthClientSecretRef,
		CreatedAt:            cfg.CreatedAt.UTC(),
		UpdatedAt:            cfg.UpdatedAt.UTC(),
	}, nil
}

type mcpPluginPayload struct {
	URL            string
	Transport      string
	AuthType       string
	CredentialMode string
	Description    string
	Metadata       map[string]any
}

// NewMCPPayloadValidator returns the plugin service validator for MCP configs.
// Endpoint policy is applied even to disabled payload-bearing configs because
// disabled is an availability decision, not permission to persist unsafe URLs.
func NewMCPPayloadValidator(policy EndpointPolicy) plugin.PayloadValidator {
	return func(ctx context.Context, definition plugin.Definition, config plugin.Config, resetFields []string) error {
		return ValidateMCPPayload(ctx, policy, definition, config, resetFields)
	}
}

// ValidateMCPPayload validates authored MCP data without dialing its endpoint.
// A negative config has no backend payload; its empty credential refs are still
// checked by Config.Validate. A disabled payload is fully safety-validated.
func ValidateMCPPayload(_ context.Context, policy EndpointPolicy, definition plugin.Definition, config plugin.Config, resetFields []string) error {
	if definition.Backend != plugin.BackendMCP {
		return errors.New("MCP payload validator requires an MCP definition")
	}
	if err := definition.Validate(); err != nil {
		return fmt.Errorf("MCP definition: %w", err)
	}
	if len(resetFields) != 0 {
		return errors.New("MCP configs do not support reset fields")
	}
	if err := config.Validate(); err != nil {
		return fmt.Errorf("MCP config: %w", err)
	}
	if config.PluginID != definition.ID || config.Namespace != definition.Namespace {
		return errors.New("MCP config does not match its definition")
	}
	if err := validateMCPDefinitionSpec(definition.Spec); err != nil {
		return err
	}
	if len(config.Payload) == 0 {
		// Config.Validate has already enforced disabled + empty refs for a
		// negative record. There is no endpoint or auth payload to inspect.
		return nil
	}
	merged, err := mergeMCPJSONObjects(definition.Spec, config.Payload)
	if err != nil {
		return err
	}
	payload, err := decodeMCPPluginPayload(merged)
	if err != nil {
		return err
	}
	if err := policy.validateEndpointURL(payload.URL); err != nil {
		return errors.New("MCP config endpoint is not allowed by endpoint policy")
	}
	_, _, oauthClientSecretRef, err := decodeMCPPluginCredentialRefs(config.CredentialRefs, config, payload.AuthType, payload.CredentialMode)
	if err != nil {
		return err
	}
	if oauthClientSecretRef != "" && metadataOAuthClientID(payload.Metadata) == "" {
		return errors.New("MCP OAuth client secret requires a client id")
	}
	return nil
}

func validateMCPDefinitionSpec(raw json.RawMessage) error {
	object, err := decodeJSONObject(raw, "MCP definition spec")
	if err != nil {
		return err
	}
	for key, value := range object {
		if key != "description" {
			return fmt.Errorf("MCP definition spec contains unsupported field %q", key)
		}
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return errors.New("MCP definition description must not be null")
		}
		var description string
		if err := json.Unmarshal(value, &description); err != nil {
			return errors.New("MCP definition description must be a string")
		}
	}
	return nil
}

func mergeMCPJSONObjects(definition, config json.RawMessage) (json.RawMessage, error) {
	base, err := decodeJSONObject(definition, "MCP definition spec")
	if err != nil {
		return nil, err
	}
	overlay, err := decodeJSONObject(config, "MCP config payload")
	if err != nil {
		return nil, err
	}
	maps.Copy(base, overlay)
	return json.Marshal(base)
}

func decodeMCPPluginPayload(raw json.RawMessage) (mcpPluginPayload, error) {
	object, err := decodeJSONObject(raw, "MCP config payload")
	if err != nil {
		return mcpPluginPayload{}, err
	}
	for key := range object {
		switch key {
		case "url", "transport", "auth_type", "credential_mode", "metadata", "description":
		default:
			return mcpPluginPayload{}, fmt.Errorf("mcp config payload contains unsupported field %q", key)
		}
	}
	payload := mcpPluginPayload{CredentialMode: CredentialModeShared, Metadata: map[string]any{}}
	if payload.URL, err = requiredJSONString(object, "url"); err != nil {
		return mcpPluginPayload{}, err
	}
	if payload.Transport, err = requiredJSONString(object, "transport"); err != nil {
		return mcpPluginPayload{}, err
	}
	if !ValidTransport(payload.Transport) {
		return mcpPluginPayload{}, errors.New("mcp config payload has unsupported transport")
	}
	if payload.AuthType, err = requiredJSONString(object, "auth_type"); err != nil {
		return mcpPluginPayload{}, err
	}
	if !ValidAuthType(payload.AuthType) {
		return mcpPluginPayload{}, errors.New("mcp config payload has unsupported auth type")
	}
	if value, ok := object["credential_mode"]; ok {
		if isJSONNull(value) {
			return mcpPluginPayload{}, errors.New("mcp config payload credential mode must not be null")
		}
		if err := json.Unmarshal(value, &payload.CredentialMode); err != nil || !ValidCredentialMode(payload.CredentialMode) {
			return mcpPluginPayload{}, errors.New("mcp config payload has unsupported credential mode")
		}
	}
	if payload.CredentialMode == CredentialModePerUser && payload.AuthType != AuthTypeOAuth {
		return mcpPluginPayload{}, errors.New("mcp config payload has per-user credentials without OAuth")
	}
	if value, ok := object["metadata"]; ok {
		payload.Metadata, err = decodeMCPPluginMetadata(value)
		if err != nil {
			return mcpPluginPayload{}, err
		}
	}
	if value, ok := object["description"]; ok {
		if isJSONNull(value) {
			return mcpPluginPayload{}, errors.New("MCP config payload description must not be null")
		}
		if err := json.Unmarshal(value, &payload.Description); err != nil {
			return mcpPluginPayload{}, errors.New("MCP config payload description must be a string")
		}
	}
	return payload, nil
}

func decodeMCPPluginCredentialRefs(raw json.RawMessage, cfg plugin.Config, authType, mode string) (string, string, string, error) {
	object, err := decodeJSONObject(raw, "MCP credential refs")
	if err != nil {
		return "", "", "", err
	}
	for key := range object {
		switch key {
		case "bearer", "oauth_bundle", "oauth_client_secret":
		default:
			return "", "", "", fmt.Errorf("MCP credential refs contain unsupported field %q", key)
		}
	}
	if authType == AuthTypeNone && len(object) != 0 {
		return "", "", "", errors.New("MCP no-auth config contains credential refs")
	}
	if authType == AuthTypeBearer {
		ref, ok := object["bearer"]
		if !ok {
			return "", "", "", errors.New("MCP bearer config is missing its credential locator")
		}
		locator, err := decodeLocator(ref, "bearer")
		if err != nil {
			return "", "", "", err
		}
		if locator.Name != credentialName(cfg.ID) {
			return "", "", "", errors.New("MCP bearer locator does not match config identity")
		}
		if err := validateLocatorOwner(locator, cfg); err != nil {
			return "", "", "", fmt.Errorf("MCP bearer locator: %w", err)
		}
		if len(object) != 1 {
			return "", "", "", errors.New("MCP bearer config contains unrelated credential refs")
		}
		return locator.Name, CredentialModeShared, "", nil
	}
	if authType != AuthTypeOAuth {
		if len(object) != 0 {
			return "", "", "", errors.New("MCP config contains credential refs for an unsupported auth type")
		}
		return "", CredentialModeShared, "", nil
	}
	bundleRaw, ok := object["oauth_bundle"]
	if !ok {
		return "", "", "", errors.New("MCP OAuth config is missing its bundle locator")
	}
	bundle, err := decodeLocator(bundleRaw, "oauth_bundle")
	if err != nil {
		return "", "", "", err
	}
	if bundle.Name != oauthBundleName(cfg.ID) {
		return "", "", "", errors.New("MCP OAuth bundle locator does not match config identity")
	}
	if bundle.Mode == "" {
		bundle.Mode = mode
	}
	if bundle.Mode != mode {
		return "", "", "", errors.New("MCP OAuth bundle mode does not match config payload")
	}
	if mode == CredentialModePerUser {
		if bundle.Owner != "per_user" || bundle.ScopeSet || bundle.UserIDSet || bundle.AgentIDSet {
			return "", "", "", errors.New("MCP per-user OAuth bundle has a registration owner")
		}
	} else if err := validateLocatorOwner(bundle, cfg); err != nil {
		return "", "", "", fmt.Errorf("MCP shared OAuth bundle: %w", err)
	}
	if len(object) > 2 {
		return "", "", "", errors.New("MCP OAuth config contains unsupported credential refs")
	}
	secretRef := ""
	if secret, exists := object["oauth_client_secret"]; exists {
		decoded, err := decodeLocator(secret, "oauth_client_secret")
		if err != nil {
			return "", "", "", err
		}
		if err := validateLocatorOwner(decoded, cfg); err != nil {
			return "", "", "", fmt.Errorf("MCP OAuth client secret: %w", err)
		}
		if decoded.Name != oauthClientSecretName(cfg.ID) {
			return "", "", "", errors.New("MCP OAuth client secret locator does not match config identity")
		}
		secretRef = decoded.Name
	}
	return "", mode, secretRef, nil
}

type mcpCredentialLocator struct {
	Name                            string `json:"name"`
	Scope                           string `json:"scope,omitempty"`
	UserID                          string `json:"user_id,omitempty"`
	AgentID                         string `json:"agent_id,omitempty"`
	Mode                            string `json:"mode,omitempty"`
	Owner                           string `json:"owner,omitempty"`
	ScopeSet, UserIDSet, AgentIDSet bool
}

func decodeLocator(raw json.RawMessage, kind string) (mcpCredentialLocator, error) {
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return mcpCredentialLocator{}, fmt.Errorf("MCP %s locator must be an object", kind)
	}
	object, err := decodeJSONObject(raw, "MCP "+kind+" locator")
	if err != nil {
		return mcpCredentialLocator{}, err
	}
	for key, value := range object {
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return mcpCredentialLocator{}, fmt.Errorf("MCP %s locator field %q must not be null", kind, key)
		}
	}
	var locator mcpCredentialLocator
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&locator); err != nil {
		return mcpCredentialLocator{}, fmt.Errorf("MCP %s locator has invalid fields", kind)
	}
	_, locator.ScopeSet = object["scope"]
	_, locator.UserIDSet = object["user_id"]
	_, locator.AgentIDSet = object["agent_id"]
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return mcpCredentialLocator{}, fmt.Errorf("MCP %s locator contains trailing data", kind)
	}
	if locator.Name == "" {
		return mcpCredentialLocator{}, fmt.Errorf("MCP %s locator has no name", kind)
	}
	return locator, nil
}

func validateLocatorOwner(locator mcpCredentialLocator, cfg plugin.Config) error {
	if !locator.ScopeSet || !locator.UserIDSet || !locator.AgentIDSet {
		return errors.New("locator owner tuple is incomplete")
	}
	if locator.Scope != string(cfg.Scope) || locator.UserID != cfg.UserID || locator.AgentID != cfg.AgentID {
		return errors.New("locator owner does not match config owner")
	}
	if locator.Owner != "" {
		return errors.New("registration-scoped locator has an unexpected per-user owner")
	}
	return nil
}

func decodeMCPPluginMetadata(raw json.RawMessage) (map[string]any, error) {
	object, err := decodeJSONObject(raw, "MCP metadata")
	if err != nil {
		return nil, err
	}
	for key := range object {
		switch key {
		case "call_timeout_seconds", "oauth", "registry":
		default:
			return nil, fmt.Errorf("MCP metadata contains unsupported field %q", key)
		}
	}
	metadata := make(map[string]any, len(object))
	if value, ok := object["call_timeout_seconds"]; ok {
		if isJSONNull(value) {
			return nil, errors.New("MCP call timeout metadata must not be null")
		}
		var seconds float64
		if err := json.Unmarshal(value, &seconds); err != nil || math.IsNaN(seconds) || math.IsInf(seconds, 0) {
			return nil, errors.New("MCP call timeout metadata is not a finite number")
		}
		metadata["call_timeout_seconds"] = seconds
	}
	if value, ok := object["oauth"]; ok {
		oauth, err := decodeMCPMetadataObject(value, "oauth", map[string]bool{
			"client_id":                  true,
			"token_endpoint_auth_method": true,
		})
		if err != nil {
			return nil, err
		}
		if method, ok := oauth["token_endpoint_auth_method"].(string); ok {
			_, normalized, err := oauthTokenEndpointAuthStyle(method)
			if err != nil {
				return nil, fmt.Errorf("MCP OAuth metadata: %w", err)
			}
			oauth["token_endpoint_auth_method"] = normalized
		}
		metadata["oauth"] = oauth
	}
	if value, ok := object["registry"]; ok {
		registry, err := decodeMCPMetadataObject(value, "registry", map[string]bool{"source": true, "id": true, "version": true, "installed_at": true})
		if err != nil {
			return nil, err
		}
		metadata["registry"] = registry
	}
	return metadata, nil
}

func decodeMCPMetadataObject(raw json.RawMessage, name string, allowed map[string]bool) (map[string]any, error) {
	object, err := decodeJSONObject(raw, "MCP "+name+" metadata")
	if err != nil {
		return nil, err
	}
	out := make(map[string]any, len(object))
	for key, value := range object {
		if !allowed[key] {
			return nil, fmt.Errorf("MCP %s metadata contains unsupported field %q", name, key)
		}
		if isJSONNull(value) {
			return nil, fmt.Errorf("MCP %s metadata field %q must not be null", name, key)
		}
		var text string
		if err := json.Unmarshal(value, &text); err != nil {
			return nil, fmt.Errorf("MCP %s metadata field %q is not a string", name, key)
		}
		out[key] = text
	}
	return out, nil
}

func isJSONNull(raw json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

func decodeJSONObject(raw json.RawMessage, label string) (map[string]json.RawMessage, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, fmt.Errorf("%s is missing", label)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	var object map[string]json.RawMessage
	if err := decoder.Decode(&object); err != nil || object == nil {
		return nil, fmt.Errorf("%s must be an object", label)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%s contains trailing data", label)
	}
	return object, nil
}

func requiredJSONString(object map[string]json.RawMessage, key string) (string, error) {
	raw, ok := object[key]
	if !ok {
		return "", fmt.Errorf("MCP config payload is missing %q", key)
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil || strings.TrimSpace(value) == "" {
		return "", fmt.Errorf("MCP config payload field %q is not a non-empty string", key)
	}
	return value, nil
}

func safeMCPObservation(observation PluginMCPObservation, cfg plugin.Config, credentialMode string, authority authz.Authority) (string, string, time.Time, []CatalogTool, error) {
	if observation.ConfigRevision != 0 && observation.ConfigRevision != cfg.Revision {
		return StatusUnknown, "", time.Time{}, nil, nil
	}
	if !observationOwnerMatches(observation.CredentialUserID, credentialMode, authority) {
		return StatusUnknown, "", time.Time{}, nil, nil
	}
	status := observation.Status
	if status == "" {
		status = StatusUnknown
	}
	if !ValidStatus(status) {
		return "", "", time.Time{}, nil, errors.New("MCP observation has an unsupported status")
	}
	statusError := ""
	switch status {
	case StatusNeedsAuth:
		if observation.StatusError != "" {
			statusError = credentialRejectedHint
		}
	case StatusError:
		if observation.StatusError != "" {
			statusError = "MCP probe failed"
		}
	}
	tools, err := cloneMCPObservationTools(observation.Tools)
	if err != nil {
		return "", "", time.Time{}, nil, err
	}
	return status, statusError, observation.ProbedAt.UTC(), tools, nil
}

func observationOwnerMatches(observedUserID, credentialMode string, authority authz.Authority) bool {
	if credentialMode == CredentialModeShared {
		return observedUserID == ""
	}
	if credentialMode != CredentialModePerUser {
		return false
	}
	if authority.Kind() != authz.ActorUser && authority.Kind() != authz.ActorAgent {
		return false
	}
	return observedUserID != "" && observedUserID == string(authority.UserID())
}

func cloneMCPObservationTools(in []CatalogTool) ([]CatalogTool, error) {
	if len(in) == 0 {
		return nil, nil
	}
	out := make([]CatalogTool, len(in))
	for i, tool := range in {
		if strings.TrimSpace(tool.Name) == "" {
			return nil, errors.New("MCP observation contains a tool without a remote name")
		}
		out[i] = CatalogTool{Name: tool.Name, Description: tool.Description}
		out[i].InputSchema = cloneSchema(tool.InputSchema)
		out[i].Annotations = cloneSchema(tool.Annotations)
	}
	return out, nil
}

func metadataOAuthClientID(metadata map[string]any) string {
	oauth, _ := metadata["oauth"].(map[string]any)
	clientID, _ := oauth["client_id"].(string)
	return clientID
}
