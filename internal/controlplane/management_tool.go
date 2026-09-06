package controlplane

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"sort"
	"strings"

	"github.com/CherryHQ/stella/internal/authz"
	"github.com/CherryHQ/stella/internal/model/embedding"
	"github.com/CherryHQ/stella/internal/platform/config"
	pluginapi "github.com/CherryHQ/stella/internal/plugin"
	"github.com/CherryHQ/stella/pkg/tools"
)

const deploymentToolSibling = "settings_provider_list"

var deploymentToolDescriptions = map[string]map[string]string{
	"provider": {
		"list":   "List up to 50 configured providers without exposing API keys.",
		"get":    "Read one provider's safe configuration and version. Use its version for settings_provider_update or settings_provider_delete.",
		"create": "Create a provider without an API key. Add credentials only in the Web UI.",
		"update": "Update safe provider metadata using the version from settings_provider_get. Endpoint origin changes require the Web UI.",
		"delete": "Delete a provider using the version from settings_provider_get. This refuses a stale version.",
	},
	"default_model": {
		"get":    "Read deployment default model roles and their version.",
		"update": "Set deployment default model roles using the version from settings_default_model_get.",
	},
	"embedding_setting": {
		"get":    "Read deployment embedding settings and their version.",
		"update": "Update deployment embedding settings using the version from settings_embedding_setting_get.",
	},
	"plugin": {
		"list":    "List up to 50 registered plugins and whether each is enabled.",
		"enable":  "Enable one registered plugin by kind and name.",
		"disable": "Disable one registered plugin by kind and name.",
	},
}

// ManagementTool is one Stella-only deployment action. It obtains direct
// human authority at execution time; deployment actions then require the
// control-plane admin capability, while settings_plugin uses common plugin PEPs.
type ManagementTool struct {
	providerSpec         *SettingsProviderActionTool
	defaultModelSpec     *SettingsDefaultModelActionTool
	embeddingSettingSpec *SettingsEmbeddingSettingActionTool
	pluginSpec           *SettingsPluginActionTool
	service              func() *Service
	pluginService        func() *pluginapi.Service
}

func NewProviderManagementTool(spec SettingsProviderActionTool, service func() *Service) *ManagementTool {
	return &ManagementTool{providerSpec: &spec, service: service}
}

func NewDefaultModelManagementTool(spec SettingsDefaultModelActionTool, service func() *Service) *ManagementTool {
	return &ManagementTool{defaultModelSpec: &spec, service: service}
}

func NewEmbeddingSettingManagementTool(spec SettingsEmbeddingSettingActionTool, service func() *Service) *ManagementTool {
	return &ManagementTool{embeddingSettingSpec: &spec, service: service}
}

func NewPluginManagementTool(spec SettingsPluginActionTool, service func() *pluginapi.Service) *ManagementTool {
	return &ManagementTool{pluginSpec: &spec, pluginService: service}
}

func (t *ManagementTool) Definition() tools.Definition {
	family, action, spec := t.spec()
	return spec.Definition(deploymentToolDescriptions[family][action])
}

func (t *ManagementTool) Execute(ctx context.Context, args map[string]any) (string, error) {
	if t == nil {
		return "", fmt.Errorf("deployment management is unavailable — try again later")
	}
	authority, err := authz.DirectAuthority(ctx, authz.UserIDFromContext(ctx))
	if err != nil {
		return "", authz.MapToolError(t.name(), deploymentToolSibling, err)
	}
	var out any
	switch {
	case t.pluginSpec != nil:
		if t.pluginService == nil {
			return "", fmt.Errorf("deployment management is unavailable — try again later")
		}
		service := t.pluginService()
		if service == nil {
			return "", fmt.Errorf("deployment management is unavailable — try again later")
		}
		access, beginErr := service.Begin(authority)
		if beginErr != nil {
			return "", authz.MapToolError(t.name(), deploymentToolSibling, beginErr)
		}
		out, err = SettingsPluginDispatch(ctx, NewUnifiedPluginManagementHandler(access), t.pluginSpec.Action, args)
	case t.providerSpec != nil, t.defaultModelSpec != nil, t.embeddingSettingSpec != nil:
		if t.service == nil || t.service() == nil {
			return "", fmt.Errorf("deployment management is unavailable — try again later")
		}
		access, beginErr := t.service().Begin(ctx, authority)
		if beginErr != nil {
			return "", authz.MapToolError(t.name(), deploymentToolSibling, beginErr)
		}
		switch {
		case t.providerSpec != nil:
			out, err = SettingsProviderDispatch(ctx, providerManagementHandler{access: access}, t.providerSpec.Action, args)
		case t.defaultModelSpec != nil:
			out, err = SettingsDefaultModelDispatch(ctx, defaultModelManagementHandler{access: access}, t.defaultModelSpec.Action, args)
		case t.embeddingSettingSpec != nil:
			out, err = SettingsEmbeddingSettingDispatch(ctx, embeddingManagementHandler{access: access}, t.embeddingSettingSpec.Action, args)
		}
	default:
		err = fmt.Errorf("deployment management tool has no action")
	}
	if err != nil {
		return "", authz.MapToolError(t.name(), deploymentToolSibling, err)
	}
	return tools.MarshalResult(out)
}

func (t *ManagementTool) spec() (string, string, interface{ Definition(string) tools.Definition }) {
	if t.providerSpec != nil {
		return "provider", t.providerSpec.Action, t.providerSpec
	}
	if t.defaultModelSpec != nil {
		return "default_model", t.defaultModelSpec.Action, t.defaultModelSpec
	}
	if t.embeddingSettingSpec != nil {
		return "embedding_setting", t.embeddingSettingSpec.Action, t.embeddingSettingSpec
	}
	return "plugin", t.pluginSpec.Action, t.pluginSpec
}

func (t *ManagementTool) name() string {
	if t.providerSpec != nil {
		return t.providerSpec.Name
	}
	if t.defaultModelSpec != nil {
		return t.defaultModelSpec.Name
	}
	if t.embeddingSettingSpec != nil {
		return t.embeddingSettingSpec.Name
	}
	return t.pluginSpec.Name
}

type providerToolView struct {
	ID                   string                                  `json:"id"`
	Type                 string                                  `json:"type"`
	Name                 string                                  `json:"name"`
	Enabled              bool                                    `json:"enabled"`
	BaseURL              string                                  `json:"base_url"`
	EndpointRedacted     bool                                    `json:"endpoint_redacted,omitempty"`
	Models               map[string]config.ProviderModelOverride `json:"models,omitempty"`
	CredentialConfigured bool                                    `json:"credential_configured"`
	Version              string                                  `json:"version"`
}

func projectProvider(p config.Provider, version string) providerToolView {
	if len(p.Models) == 0 {
		p.Models = nil
	}
	baseURL, redacted := safeToolEndpoint(p.BaseURL)
	return providerToolView{ID: p.ID, Type: p.Type, Name: p.Name, Enabled: p.Enabled, BaseURL: baseURL, EndpointRedacted: redacted, Models: p.Models, CredentialConfigured: p.APIKey != "", Version: version}
}

// safeToolEndpoint prevents a legacy DB row containing query text, a fragment,
// or userinfo from leaking through a model-facing projection.
func safeToolEndpoint(raw string) (string, bool) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", true
	}
	return u.String(), false
}

func deploymentVersion(v ...any) string {
	b, _ := json.Marshal(v)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func providerFromInput(id string, in SettingsProviderCreateInput) (config.Provider, error) {
	var models map[string]config.ProviderModelOverride
	if in.Models != nil {
		encoded, err := json.Marshal(in.Models)
		if err != nil {
			return config.Provider{}, invalid("invalid provider models")
		}
		if err := json.Unmarshal(encoded, &models); err != nil {
			return config.Provider{}, invalid("invalid provider models")
		}
	}
	p := config.Provider{ID: id, Type: strings.TrimSpace(in.Type), Name: strings.TrimSpace(in.Name), Enabled: in.Enabled, BaseURL: strings.TrimSpace(in.BaseUrl), Models: models}
	if p.Type == "" {
		p.Type = p.ID
	}
	if p.Name == "" {
		p.Name = p.ID
	}
	if p.ID == "" {
		return config.Provider{}, invalid("id is required")
	}
	if err := validateProviderEndpoint(p.BaseURL); err != nil {
		return config.Provider{}, invalid(err.Error())
	}
	return p, nil
}

func validateProviderEndpoint(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return fmt.Errorf("base_url must be an absolute HTTP URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("base_url must be an HTTP URL without userinfo, query, or fragment")
	}
	return nil
}

func endpointOrigin(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	host := u.Hostname()
	if host == "" {
		return "", fmt.Errorf("missing host")
	}
	return strings.ToLower(u.Scheme) + "://" + net.JoinHostPort(strings.ToLower(host), u.Port()), nil
}

type providerManagementHandler struct{ access *Access }

func (h providerManagementHandler) List(ctx context.Context, _ SettingsProviderListInput) (any, error) {
	snapshots, err := h.access.ListProviderSnapshots(ctx)
	if err != nil {
		return nil, err
	}
	sort.Slice(snapshots, func(i, j int) bool { return snapshots[i].Provider.ID < snapshots[j].Provider.ID })
	truncated := len(snapshots) > 50
	if truncated {
		snapshots = snapshots[:50]
	}
	out := make([]providerToolView, 0, len(snapshots))
	for _, snapshot := range snapshots {
		out = append(out, projectProvider(snapshot.Provider, snapshot.Version))
	}
	return map[string]any{"providers": out, "truncated": truncated}, nil
}

func (h providerManagementHandler) Get(ctx context.Context, in SettingsProviderGetInput) (any, error) {
	snapshot, err := h.access.GetProviderSnapshot(ctx, in.Id)
	if err != nil {
		return nil, err
	}
	return projectProvider(snapshot.Provider, snapshot.Version), nil
}

func (h providerManagementHandler) Create(ctx context.Context, in SettingsProviderCreateInput) (any, error) {
	p, err := providerFromInput(in.Id, in)
	if err != nil {
		return nil, err
	}
	p.APIKey = ""
	if err := h.access.CreateProvider(ctx, p); err != nil {
		return nil, err
	}
	snapshot, err := h.access.GetProviderSnapshot(ctx, p.ID)
	if err != nil {
		return nil, err
	}
	return projectProvider(snapshot.Provider, snapshot.Version), nil
}

func (h providerManagementHandler) Update(ctx context.Context, in SettingsProviderUpdateInput) (any, error) {
	currentSnapshot, err := h.access.GetProviderSnapshot(ctx, in.Id)
	if err != nil {
		return nil, err
	}
	current := currentSnapshot.Provider
	create := SettingsProviderCreateInput{Id: in.Id, Type: in.Type, Name: in.Name, Enabled: in.Enabled, BaseUrl: in.BaseUrl, Models: in.Models}
	candidate, err := providerFromInput(in.Id, create)
	if err != nil {
		return nil, err
	}
	oldOrigin, oldErr := endpointOrigin(current.BaseURL)
	newOrigin, newErr := endpointOrigin(candidate.BaseURL)
	if oldErr != nil || newErr != nil {
		return nil, invalid("base_url must be an absolute HTTP URL")
	}
	if oldOrigin != newOrigin {
		// An Agent may hold an encrypted per-Provider key even when this global
		// Provider has no API key. Model-originated updates cannot retarget either.
		return nil, &ConflictError{Msg: "provider endpoint origin must be changed in the Web UI"}
	}
	candidate.APIKey = current.APIKey
	// Catalog selection and model policy are deployment-level fields, not part of
	// the agent-facing tool input. Preserve them across tool updates.
	candidate.CatalogID = current.CatalogID
	candidate.ModelPolicy = current.ModelPolicy
	if in.Models == nil {
		candidate.Models = current.Models
	}
	if _, err := h.access.UpdateProviderIfVersion(ctx, candidate, in.ExpectedVersion); err != nil {
		return nil, err
	}
	snapshot, err := h.access.GetProviderSnapshot(ctx, candidate.ID)
	if err != nil {
		return nil, err
	}
	return projectProvider(snapshot.Provider, snapshot.Version), nil
}

func (h providerManagementHandler) Delete(ctx context.Context, in SettingsProviderDeleteInput) (any, error) {
	if err := h.access.DeleteProviderIfVersion(ctx, in.Id, in.ExpectedVersion); err != nil {
		return nil, err
	}
	return map[string]string{"id": in.Id, "status": "deleted"}, nil
}

type defaultModelToolView struct {
	config.DefaultModels
	Version string `json:"version"`
}

func projectDefaultModels(v config.DefaultModels) defaultModelToolView {
	return defaultModelToolView{DefaultModels: v, Version: deploymentVersion(v)}
}

type defaultModelManagementHandler struct{ access *Access }

func (h defaultModelManagementHandler) Get(ctx context.Context, _ SettingsDefaultModelGetInput) (any, error) {
	v, e := h.access.GetDefaultModels(ctx)
	return projectDefaultModels(v), e
}

func (h defaultModelManagementHandler) Update(ctx context.Context, in SettingsDefaultModelUpdateInput) (any, error) {
	next, e := h.access.SetDefaultModelsIfVersion(ctx, config.DefaultModels{Model: in.Model, ModelThinking: in.ModelThinking, ModelStrong: in.ModelStrong, ModelStrongThinking: in.ModelStrongThinking, ModelFast: in.ModelFast, ModelFastThinking: in.ModelFastThinking, ModelVision: in.ModelVision, ModelEmbedding: in.ModelEmbedding}, in.ExpectedVersion)
	if e != nil {
		return nil, e
	}
	return projectDefaultModels(next), nil
}

type embeddingToolView struct {
	Enabled   bool   `json:"enabled"`
	Dim       int    `json:"dim"`
	Normalize bool   `json:"normalize"`
	Active    bool   `json:"active"`
	Version   string `json:"version"`
}

func projectEmbedding(v EmbeddingState) embeddingToolView {
	return embeddingToolView{Enabled: v.Settings.Enabled, Dim: v.Settings.Dim, Normalize: v.Settings.Normalize, Active: v.Active, Version: deploymentVersion(v.Settings)}
}

type embeddingManagementHandler struct{ access *Access }

func (h embeddingManagementHandler) Get(ctx context.Context, _ SettingsEmbeddingSettingGetInput) (any, error) {
	v, e := h.access.GetEmbeddingSettings(ctx)
	return projectEmbedding(v), e
}

func (h embeddingManagementHandler) Update(ctx context.Context, in SettingsEmbeddingSettingUpdateInput) (any, error) {
	next, e := h.access.SetEmbeddingSettingsIfVersion(ctx, EmbeddingUpdate{Enabled: in.Enabled, Dim: in.Dim, Normalize: in.Normalize}, in.ExpectedVersion)
	if e != nil {
		return nil, e
	}
	return projectEmbedding(next), nil
}

type pluginToolView struct {
	Kind    string `json:"kind"`
	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`
	Version string `json:"version"`
}

func projectPlugin(kind, name string, enabled bool) pluginToolView {
	return pluginToolView{Kind: kind, Name: name, Enabled: enabled, Version: deploymentVersion(kind, name, enabled)}
}

// validateEmbeddingDim is shared by HTTP and tool callers. Keeping it here
// stops the two transports from accepting deployment states the worker cannot use.
func validateEmbeddingDim(dim int) error {
	if dim < 0 || dim > embedding.StorageDim {
		return invalid(fmt.Sprintf("dim must be between 0 and %d", embedding.StorageDim))
	}
	return nil
}
