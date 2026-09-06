package manifest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"unicode"

	"github.com/CherryHQ/stella/internal/plugin"
)

// ValidatePayload is the CLI backend's configuration boundary. Definition
// resources are trusted release input; a config is an overlay, so a user may
// pin a release version or select an OAuth source but cannot replace the
// executable, its install location, or the skill that belongs to it.
//
// The plugin service calls this with the resolved definition plus overlay. The
// reset list is checked here as well because reset is an ownership operation,
// even when the resulting value happens to equal the release definition.
var _ plugin.PayloadValidator = ValidatePayload

func ValidatePayload(_ context.Context, definition plugin.Definition, config plugin.Config, resetFields []string) error {
	if err := config.Validate(); err != nil {
		return err
	}
	if definition.Backend != plugin.BackendCLI {
		return invalidPayload("backend %q is not CLI", definition.Backend)
	}
	if err := definition.Validate(); err != nil {
		return invalidPayload("definition: %v", err)
	}
	if err := validateResetFields(config.Scope, resetFields); err != nil {
		return err
	}
	if err := validateCredentialRefs(config); err != nil {
		return err
	}

	shipped, err := decodeCLIPayload(definition.Spec, "definition spec")
	if err != nil {
		return err
	}
	// A nil config payload is still checked against the release resource
	// contract. Only the selected config's completeness is suppressed by false.
	if err := validateResources(shipped, "definition spec", true); err != nil {
		return err
	}
	if len(config.Payload) == 0 {
		if config.Enabled != nil && *config.Enabled {
			return invalidPayload("enabled config has no payload")
		}
		return nil
	}
	resolved, err := decodeCLIPayload(config.Payload, "config payload")
	if err != nil {
		return err
	}
	complete := definition.DefaultEnabled
	if config.Enabled != nil {
		complete = *config.Enabled
	}
	if err := validateResources(resolved, "config payload", complete); err != nil {
		return err
	}
	if err := validateConfigEnvValues(resolved); err != nil {
		return err
	}
	if config.Scope == plugin.ScopeUser || config.Scope == plugin.ScopeUserAgent {
		if err := validateUserOverlay(shipped, resolved, config); err != nil {
			return err
		}
	}
	return nil
}

// cliPayload intentionally mirrors only the fields projected by
// BuiltinDefinitions. Keeping this decoder narrower than ManifestPlugin also
// prevents identity and server-owned metadata from entering plugin_config.
type cliPayload struct {
	Description   string               `json:"description,omitempty"`
	Category      string               `json:"category,omitempty"`
	Prompt        string               `json:"prompt,omitempty"`
	Binaries      []ManifestBinary     `json:"binaries,omitempty"`
	Skills        []ManifestSkill      `json:"skills,omitempty"`
	SessionEnvs   []ManifestSessionEnv `json:"session_env,omitempty"`
	OAuthProvider string               `json:"oauth_provider,omitempty"`
}

// CLIPayload is the validated definition/config projection consumed by the
// runtime adapter. It intentionally aliases the narrow decoder shape rather
// than exposing ManifestPlugin, which also contains server-owned state.
type CLIPayload = cliPayload

// DecodeCLIPayload decodes the fields projected into a CLI plugin definition or
// config. Callers must still apply the common validation boundary before using
// the result as an executable resource description.
func DecodeCLIPayload(raw json.RawMessage, name string) (CLIPayload, error) {
	return decodeCLIPayload(raw, name)
}

func decodeCLIPayload(raw json.RawMessage, name string) (cliPayload, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return cliPayload{}, invalidPayload("%s is empty", name)
	}
	if trimmed[0] != '{' {
		return cliPayload{}, invalidPayload("%s must be an object", name)
	}
	var payload cliPayload
	if err := decodeStrictJSON(trimmed, &payload); err != nil {
		return cliPayload{}, invalidPayload("%s: %v", name, err)
	}
	return payload, nil
}

func decodeStrictJSON(raw json.RawMessage, dst any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("trailing JSON")
		}
		return fmt.Errorf("trailing JSON: %w", err)
	}
	return nil
}

func validateResources(payload cliPayload, name string, complete bool) error {
	seenBinaries := make(map[string]struct{}, len(payload.Binaries))
	for i, binary := range payload.Binaries {
		if _, ok := seenBinaries[binary.Name]; ok {
			return invalidPayload("%s binaries[%d] duplicates name %q", name, i, binary.Name)
		}
		seenBinaries[binary.Name] = struct{}{}
		if err := validateString(binary.Name, "binary name"); err != nil {
			return err
		}
		if err := validateString(binary.Tool, "binary tool"); err != nil {
			return err
		}
		if err := validateString(binary.Version, "binary version"); err != nil {
			return err
		}
		if binary.Options == nil {
			continue
		}
		for key, value := range binary.Options {
			if err := validateString(key, "binary option name"); err != nil {
				return err
			}
			if err := validateJSONValue(value, "binary option "+key); err != nil {
				return err
			}
		}
	}

	seenSkills := make(map[string]struct{}, len(payload.Skills))
	for i, skill := range payload.Skills {
		if err := validateString(skill.Name, "skill name"); err != nil {
			return err
		}
		if _, ok := seenSkills[skill.Name]; ok {
			return invalidPayload("%s skills[%d] duplicates %q", name, i, skill.Name)
		}
		seenSkills[skill.Name] = struct{}{}
	}

	seenEnv := make(map[string]struct{}, len(payload.SessionEnvs))
	for i, env := range payload.SessionEnvs {
		if err := validateString(env.EnvVar, "session env var"); err != nil {
			return err
		}
		if err := validateString(env.Source, "session env source"); err != nil {
			return err
		}
		if _, ok := seenEnv[env.EnvVar]; ok {
			return invalidPayload("%s session_env[%d] duplicates %q", name, i, env.EnvVar)
		}
		seenEnv[env.EnvVar] = struct{}{}
		if env.Source != "" {
			if after, ok := strings.CutPrefix(env.Source, "oauth."); ok {
				if !knownOAuthField(after) {
					return invalidPayload("%s session_env[%d] has unknown OAuth field %q", name, i, env.Source)
				}
			} else if env.Source != "static" {
				return invalidPayload("%s session_env[%d] has unknown source %q", name, i, env.Source)
			}
		}
	}
	if complete {
		if err := validateCompleteManifest(payload, name); err != nil {
			return err
		}
	}
	return nil
}

// ValidateBundledSkillNames checks the immutable skill membership declared by
// a plugin against the release descriptor owned by that plugin. Configs may
// pin mutable CLI fields, but they cannot add, remove, or redirect skills.
func ValidateBundledSkillNames(declared []ManifestSkill, expected []string) error {
	declaredSet := make(map[string]struct{}, len(declared))
	for i, skill := range declared {
		if err := validateString(skill.Name, "skill name"); err != nil {
			return err
		}
		if _, exists := declaredSet[skill.Name]; exists {
			return invalidPayload("skills[%d] duplicates %q", i, skill.Name)
		}
		declaredSet[skill.Name] = struct{}{}
	}
	expectedSet := make(map[string]struct{}, len(expected))
	for _, name := range expected {
		if err := validateString(name, "bundled skill name"); err != nil {
			return err
		}
		if _, exists := expectedSet[name]; exists {
			return invalidPayload("release descriptor duplicates skill %q", name)
		}
		expectedSet[name] = struct{}{}
	}
	if len(declaredSet) != len(expectedSet) {
		return invalidPayload("skills must exactly match the release descriptor")
	}
	for name := range expectedSet {
		if _, exists := declaredSet[name]; !exists {
			return invalidPayload("skills must exactly match the release descriptor: missing %q", name)
		}
	}
	return nil
}

// validateCompleteManifest delegates required resource checks to the existing
// manifest validator used by the installer. This adapter only supplies the
// provider ID because Definition.Spec intentionally carries no provider flow
// definitions.
func validateCompleteManifest(payload cliPayload, name string) error {
	definition := ManifestPluginDefinition{
		Description: payload.Description, Category: payload.Category, Prompt: payload.Prompt,
		Binaries: payload.Binaries, Skills: payload.Skills, SessionEnvs: payload.SessionEnvs,
		OAuthProvider: payload.OAuthProvider,
	}
	manifest := &Manifest{Plugins: []ManifestPlugin{{ID: "validated", ManifestPluginDefinition: definition}}}
	if payload.OAuthProvider != "" {
		manifest.OAuthProviders = []ManifestOAuthProvider{{
			ID: payload.OAuthProvider, VaultKey: "validated",
			Flows: []ManifestOAuthFlow{{Type: "device_code", DeviceAuthURL: "https://validated.invalid/device", TokenURL: "https://validated.invalid/token"}},
		}}
	}
	if err := Validate(manifest); err != nil {
		return invalidPayload("%s: %v", name, err)
	}
	return nil
}

func validateConfigEnvValues(payload cliPayload) error {
	for i, env := range payload.SessionEnvs {
		if env.Value != "" {
			return invalidPayload("config payload session_env[%d].value must be empty; use credential_refs", i)
		}
	}
	return nil
}

func validateUserOverlay(shipped, resolved cliPayload, config plugin.Config) error {
	if resolved.Description != shipped.Description || resolved.Category != shipped.Category ||
		resolved.Prompt != shipped.Prompt || resolved.OAuthProvider != shipped.OAuthProvider ||
		!reflect.DeepEqual(resolved.Skills, shipped.Skills) {
		return invalidPayload("user scope may only change binary version/options and OAuth session env source")
	}
	if len(resolved.Binaries) != len(shipped.Binaries) {
		return invalidPayload("user scope cannot add or remove binaries")
	}
	for i := range shipped.Binaries {
		want, got := shipped.Binaries[i], resolved.Binaries[i]
		if got.Name != want.Name || got.Tool != want.Tool {
			return invalidPayload("user scope cannot change binary[%d] name or tool", i)
		}
		if err := validateUserOptions(want.Options, got.Options, got.Version); err != nil {
			return fmt.Errorf("%w: binary[%d] options: %w", plugin.ErrInvalidConfig, i, err)
		}
		if err := validateVersion(got.Version); err != nil {
			return err
		}
	}
	if len(resolved.SessionEnvs) != len(shipped.SessionEnvs) {
		return invalidPayload("user scope cannot add or remove session env declarations")
	}
	for i := range shipped.SessionEnvs {
		want, got := shipped.SessionEnvs[i], resolved.SessionEnvs[i]
		if got.EnvVar != want.EnvVar || got.Required != want.Required || got.Value != want.Value {
			return invalidPayload("user scope cannot change session_env[%d] declaration", i)
		}
		if got.Source != want.Source && !strings.HasPrefix(got.Source, "oauth.") {
			return invalidPayload("user scope session_env[%d] source must be an OAuth source", i)
		}
		if strings.HasPrefix(got.Source, "oauth.") && config.UserID == "" {
			return invalidPayload("OAuth session env requires a user-owned config")
		}
	}
	return nil
}

func validateUserOptions(shipped, resolved map[string]any, version string) error {
	for key, value := range resolved {
		if key == "version" {
			optionVersion, _ := value.(string)
			if _, ok := value.(string); !ok {
				return errors.New("options.version must be a string")
			}
			if optionVersion != version {
				return errors.New("options.version must match binary version")
			}
		}
		if key == "extras" {
			if _, ok := value.(string); !ok {
				return errors.New("options.extras must be a string")
			}
			continue
		}
		published, ok := shipped[key]
		if !ok {
			return fmt.Errorf("option %q is unknown", key)
		}
		if !reflect.DeepEqual(published, value) {
			return fmt.Errorf("option %q is release-owned", key)
		}
	}
	for key, value := range shipped {
		if key == "extras" {
			continue
		}
		if resolvedValue, ok := resolved[key]; !ok || !reflect.DeepEqual(resolvedValue, value) {
			return fmt.Errorf("option %q is release-owned", key)
		}
	}
	return nil
}

func validateVersion(version string) error {
	if err := validateString(version, "binary version"); err != nil {
		return err
	}
	if len(version) > 256 {
		return invalidPayload("binary version is too long")
	}
	// mise tool keys and URLs belong to the release definition. A config
	// version is a registry version or tag, never another implementation source.
	if strings.ContainsAny(version, "/\\:@?%") {
		return invalidPayload("binary version must be a published version or tag")
	}
	return nil
}

func validateResetFields(scope plugin.Scope, fields []string) error {
	seen := make(map[string]struct{}, len(fields))
	for _, field := range fields {
		if _, ok := seen[field]; ok {
			return invalidPayload("reset_fields contains duplicate %q", field)
		}
		seen[field] = struct{}{}
		if !IsOwnableField(field) {
			return invalidPayload("reset_fields contains unknown field %q", field)
		}
		if scope == plugin.ScopeUser || scope == plugin.ScopeUserAgent {
			if field != "binaries" && field != "session_env" {
				return invalidPayload("user scope cannot reset %q", field)
			}
		}
	}
	return nil
}

func validateCredentialRefs(config plugin.Config) error {
	if len(config.CredentialRefs) == 0 {
		return nil
	}
	trimmed := bytes.TrimSpace(config.CredentialRefs)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return invalidPayload("credential_refs must be an object")
	}
	var refs cliCredentialRefs
	if err := decodeStrictJSON(trimmed, &refs); err != nil {
		return invalidPayload("credential_refs: %v", err)
	}
	var rawRefs map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &rawRefs); err != nil {
		return invalidPayload("credential_refs must be an object")
	}
	if raw, ok := rawRefs["session_env"]; ok && bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return invalidPayload("credential_refs.session_env must be an object")
	}
	if refs.SessionEnv != nil {
		if err := validateCredentialRef(*refs.SessionEnv, config); err != nil {
			return err
		}
	}
	return nil
}

type cliCredentialRefs struct {
	SessionEnv *cliCredentialRef `json:"session_env,omitempty"`
}

type cliCredentialRef struct {
	Name    string       `json:"name"`
	Scope   plugin.Scope `json:"scope"`
	UserID  string       `json:"user_id,omitempty"`
	AgentID string       `json:"agent_id,omitempty"`
	Mode    string       `json:"mode,omitempty"`
	Owner   string       `json:"owner,omitempty"`
}

func validateCredentialRef(ref cliCredentialRef, config plugin.Config) error {
	if ref.Name == "" {
		return invalidPayload("credential_refs.session_env.name is required")
	}
	for name, value := range map[string]string{
		"name": ref.Name, "user_id": ref.UserID, "agent_id": ref.AgentID,
		"mode": ref.Mode, "owner": ref.Owner,
	} {
		if value != "" {
			if err := validateString(value, "credential ref "+name); err != nil {
				return err
			}
		}
	}
	if !credentialOwnerMatches(ref.Scope, ref.UserID, ref.AgentID, config) {
		return invalidPayload("credential ref owner does not match config scope")
	}
	return nil
}

func credentialOwnerMatches(scope plugin.Scope, refUserID, refAgentID string, config plugin.Config) bool {
	switch scope {
	case plugin.ScopeSystem:
		return config.Scope == plugin.ScopeSystem && refUserID == "" && refAgentID == "" && config.UserID == "" && config.AgentID == ""
	case plugin.ScopeSystemAgent:
		return config.Scope == plugin.ScopeSystemAgent && refUserID == "" && refAgentID == config.AgentID && config.UserID == "" && config.AgentID != ""
	case plugin.ScopeUser:
		return (config.Scope == plugin.ScopeUser || config.Scope == plugin.ScopeUserAgent) && refUserID == config.UserID && refAgentID == "" && config.UserID != ""
	case plugin.ScopeUserAgent:
		return config.Scope == plugin.ScopeUserAgent && refUserID == config.UserID && refAgentID == config.AgentID && config.UserID != "" && config.AgentID != ""
	default:
		return false
	}
}

func knownOAuthField(field string) bool {
	switch field {
	case "access_token", "client_id", "brand", "refresh_token":
		return true
	default:
		return false
	}
}

func validateString(value, field string) error {
	if strings.IndexByte(value, 0) >= 0 {
		return invalidPayload("%s contains NUL", field)
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return invalidPayload("%s contains control character", field)
		}
	}
	return nil
}

func validateJSONValue(value any, field string) error {
	switch value := value.(type) {
	case nil, bool, string, float64:
		if text, ok := value.(string); ok {
			return validateString(text, field)
		}
		return nil
	case []any:
		for _, item := range value {
			if err := validateJSONValue(item, field); err != nil {
				return err
			}
		}
		return nil
	case map[string]any:
		for key, item := range value {
			if err := validateString(key, field+" key"); err != nil {
				return err
			}
			if err := validateJSONValue(item, field+"."+key); err != nil {
				return err
			}
		}
		return nil
	default:
		return invalidPayload("%s has unsupported JSON type %T", field, value)
	}
}

func invalidPayload(format string, args ...any) error {
	return fmt.Errorf("%w: %s", plugin.ErrInvalidConfig, fmt.Sprintf(format, args...))
}
