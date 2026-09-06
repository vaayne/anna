package main

import (
	"context"
	"fmt"

	"golang.org/x/oauth2"

	"github.com/jackc/pgx/v5/pgxpool"

	oauth "github.com/CherryHQ/stella/internal/connections/oauth"
	appdb "github.com/CherryHQ/stella/internal/db"
	"github.com/CherryHQ/stella/internal/notify"
	"github.com/CherryHQ/stella/internal/platform/config"
	"github.com/CherryHQ/stella/internal/platform/version"
	"github.com/CherryHQ/stella/internal/plugin"
	pluginhost "github.com/CherryHQ/stella/internal/plugin/host"
	"github.com/CherryHQ/stella/internal/plugin/manifest"
	pkgplugins "github.com/CherryHQ/stella/pkg/plugins"
	"github.com/CherryHQ/stella/resources"
)

type pluginSetup struct {
	catalog                *plugin.Catalog
	host                   *pluginhost.Host
	channelRuntimeServices *pluginhost.ChannelPlatform
	oauthRegistry          *oauth.ProviderRegistry
}

func setupPlugins(ctx context.Context, db *pgxpool.Pool, store config.Store, dispatcher *notify.Dispatcher) (*pluginSetup, error) {
	oidcStore := appdb.NewOIDCStore(db)
	channelRuntimeServices := pluginhost.NewChannelRuntimeServices()
	channelRuntimeServices.SetBuildVersion(version.Version)
	channelRuntimeServices.Set(ctx, nil, nil, nil)
	stateStore := pluginhost.NewStateStore(db)

	phost := pluginhost.New(store,
		pluginhost.WithAuthService(pluginhost.NewAuthService(oidcStore)),
		pluginhost.WithNotificationService(dispatcher),
		pluginhost.WithStateStore(stateStore),
		pluginhost.WithChannelRuntimeServices(channelRuntimeServices),
	)

	code := pkgplugins.NewCatalog()
	for _, id := range pkgplugins.Names() {
		implementation, ok := pkgplugins.Get(id)
		if !ok {
			return nil, fmt.Errorf("missing shipped plugin %q", id)
		}
		code.Register(id, implementation)
	}
	if err := phost.LoadCatalog(code); err != nil {
		return nil, fmt.Errorf("load plugin catalog: %w", err)
	}

	catalog := plugin.NewCatalog()
	codeDefinitions, err := phost.BuiltinDefinitions(code)
	if err != nil {
		return nil, err
	}
	cliDefinitions, err := manifest.BuiltinDefinitions()
	if err != nil {
		return nil, err
	}
	toolDefinitions, err := pluginhost.BuiltinToolDefinitions(newToolMetaRegistry(generatedFamilies()...))
	if err != nil {
		return nil, err
	}
	owners := make(map[string]struct{})
	for _, definitions := range [][]plugin.Definition{codeDefinitions, cliDefinitions, toolDefinitions} {
		for _, definition := range definitions {
			if err := catalog.Register(definition); err != nil {
				return nil, err
			}
			owners[definition.ID] = struct{}{}
		}
	}
	bundled, err := resources.Default()
	if err != nil {
		return nil, err
	}
	if err := bundled.ValidateBuiltinSkillOwners(owners); err != nil {
		return nil, err
	}

	builtinManifest, err := manifest.LoadBuiltin()
	if err != nil {
		return nil, fmt.Errorf("load shipped OAuth definitions: %w", err)
	}
	oauthRegistry := buildOAuthRegistry(builtinManifest)

	return &pluginSetup{
		catalog:                catalog,
		host:                   phost,
		channelRuntimeServices: channelRuntimeServices,
		oauthRegistry:          oauthRegistry,
	}, nil
}

func buildOAuthRegistry(merged *manifest.Manifest) *oauth.ProviderRegistry {
	registry := oauth.NewProviderRegistry()
	for _, op := range merged.OAuthProviders {
		flows := make([]oauth.ProviderFlowConfig, 0, len(op.Flows))
		for _, f := range op.Flows {
			var authStyle oauth2.AuthStyle
			switch f.AuthStyle {
			case "in_params":
				authStyle = oauth2.AuthStyleInParams
			case "in_header":
				authStyle = oauth2.AuthStyleInHeader
			default:
				authStyle = oauth2.AuthStyleAutoDetect
			}
			flows = append(flows, oauth.ProviderFlowConfig{
				Type:          f.Type,
				AuthURL:       f.AuthURL,
				DeviceAuthURL: f.DeviceAuthURL,
				TokenURL:      f.TokenURL,
				AuthStyle:     authStyle,
				PKCE:          f.PKCE,
			})
		}
		registry.Register(oauth.ProviderConfig{
			ID:           op.ID,
			Icon:         op.Icon,
			Scopes:       op.Scopes,
			VaultKey:     op.VaultKey,
			Flows:        flows,
			ClientID:     op.ClientID,
			ClientSecret: op.ClientSecret,
		})
	}
	return registry
}
