package main

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strings"

	agentsandbox "github.com/CherryHQ/stella/internal/agent/sandbox"
	"github.com/CherryHQ/stella/internal/platform/config"
	"github.com/CherryHQ/stella/internal/platform/version"
	pkgsandbox "github.com/CherryHQ/stella/pkg/sandbox"
	bridgebackend "github.com/CherryHQ/stella/plugins/sandbox/bridge"
	dockerbackend "github.com/CherryHQ/stella/plugins/sandbox/docker"
	localbackend "github.com/CherryHQ/stella/plugins/sandbox/local"
	nonebackend "github.com/CherryHQ/stella/plugins/sandbox/none"
	"github.com/CherryHQ/stella/resources"
)

const (
	dockerImageRepo = "ghcr.io/cherryhq/stella-sandbox"
	dockerDevImage  = "stella-sandbox:dev"
)

func setupSandboxBackends() (*agentsandbox.BackendRegistry, error) {
	return agentsandbox.NewBackendRegistry(
		agentsandbox.BackendDefinition{Name: config.SandboxBackendDocker, Create: func(ctx context.Context, request agentsandbox.BackendRequest) (session pkgsandbox.Session, err error) {
			request.Policy.InheritEnv = true
			resourceRegistry, err := resources.Default()
			if err != nil {
				return nil, fmt.Errorf("load builtin skill bundle: %w", err)
			}
			backendConfig := dockerbackend.Config{
				Image:                  sandboxDockerImage(),
				StellaHome:             request.Paths.StellaHome,
				ExpectedBundleRevision: resourceRegistry.BundleRevision(),
			}
			// Every resolved selection is prepared in the isolated Linux helper.
			// User-scoped installers never execute on the host.
			for _, spec := range request.BinarySpecs {
				backendConfig.SelectionToolBinaries = append(backendConfig.SelectionToolBinaries, dockerbackend.ToolBinary{
					PluginID: spec.PluginID, ConfigID: spec.ConfigID, Scope: spec.Scope, Revision: spec.Revision,
					Name: spec.Name, Tool: spec.Tool, Version: spec.Version, Options: maps.Clone(spec.Options),
				})
			}

			factory, err := dockerbackend.NewFactoryWithMountSources(backendConfig, request.MountSources)
			if err != nil {
				return nil, err
			}
			session, err = factory.CreateSession(ctx, request.Policy)
			if err == nil {
				return session, nil
			}
			return nil, sandboxDockerSessionError(err)
		}},
		agentsandbox.BackendDefinition{Name: config.SandboxBackendLocal, Create: func(ctx context.Context, request agentsandbox.BackendRequest) (pkgsandbox.Session, error) {
			session, err := localbackend.NewFactoryWithMountSources(request.MountSources, localbackend.Config{StellaHome: request.Paths.StellaHome}).CreateSession(ctx, request.Policy)
			if err != nil {
				return nil, fmt.Errorf("create local session: %w", err)
			}
			return session, nil
		}},
		agentsandbox.BackendDefinition{Name: config.SandboxBackendNone, Create: func(ctx context.Context, request agentsandbox.BackendRequest) (pkgsandbox.Session, error) {
			session, err := nonebackend.NewFactoryWithMountSources(request.MountSources, nonebackend.Config{StellaHome: request.Paths.StellaHome}).CreateSession(ctx, request.Policy)
			if err != nil {
				return nil, fmt.Errorf("create host session: %w", err)
			}
			return session, nil
		}},
		agentsandbox.BackendDefinition{Name: config.SandboxBackendBridge, Create: func(ctx context.Context, request agentsandbox.BackendRequest) (pkgsandbox.Session, error) {
			session, err := bridgebackend.NewFactory(bridgebackend.Config{
				BindingDir: config.EvalBridgeBindingDir(),
				UserID:     request.UserID,
				GroupID:    request.GroupID,
			}).CreateSession(ctx, request.Policy)
			if err != nil {
				return nil, fmt.Errorf("create bridge session: %w", err)
			}
			return session, nil
		}},
	)
}

func sandboxDockerImage() string {
	if version.IsDev() {
		return dockerDevImage
	}
	return dockerImageRepo + ":" + strings.TrimPrefix(version.Version, "v")
}

func sandboxDockerSessionError(err error) error {
	var imageErr *dockerbackend.ImageUnavailableError
	if version.IsDev() && errors.As(err, &imageErr) {
		return fmt.Errorf("%w (run `mise run sandbox:docker:build` to build the local %q image)", err, sandboxDockerImage())
	}
	return fmt.Errorf("create docker session: %w", err)
}
