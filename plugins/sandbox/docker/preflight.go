package docker

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/CherryHQ/stella/plugins/sandbox/docker/dockerclient"
)

const builtinBundleRevisionLabel = "org.cherryhq.stella.builtin-bundle-revision"

// ImageUnavailableError is exposed at the plugin boundary so callers do not
// need to depend on the Docker client implementation package.
type ImageUnavailableError = dockerclient.ImageUnavailableError

// PreflightConfig configures a Preflight check.
type PreflightConfig struct {
	StellaHome string
	Docker     Config
}

// sharedClient is a package-level cached client.
// Tests bypass this by using dockerclient.NewWithPath and passing a client directly.
var (
	sharedClientOnce sync.Once
	sharedClient     *dockerclient.Client
	sharedClientErr  error
)

// getSharedClient returns the cached dockerclient.Client, initializing it once.
func getSharedClient() (*dockerclient.Client, error) {
	sharedClientOnce.Do(func() {
		sharedClient, sharedClientErr = dockerclient.New()
	})
	return sharedClient, sharedClientErr
}

// Preflight checks daemon reachability and image availability. A missing image
// is pulled automatically; the caller is expected to have built or published
// the image ahead of time for dev builds (the pull will fail with a registry
// error in that case).
func Preflight(ctx context.Context, cfg PreflightConfig) error {
	return preflightWithClient(ctx, cfg, nil)
}

// preflightWithClient is the testable variant that accepts an optional client override.
func preflightWithClient(ctx context.Context, cfg PreflightConfig, client *dockerclient.Client) error {
	if cfg.Docker.Image == "" {
		return fmt.Errorf("docker preflight: Image is required")
	}

	var err error
	if client == nil {
		client, err = getSharedClient()
		if err != nil {
			return fmt.Errorf("docker preflight: %w", err)
		}
	}

	if _, err := client.Version(ctx); err != nil {
		return fmt.Errorf("docker preflight: daemon not reachable: %w", err)
	}
	security, err := client.Security(ctx)
	if err != nil {
		return fmt.Errorf("docker preflight: inspect daemon security: %w", err)
	}
	if security.UserNamespace {
		return fmt.Errorf("docker preflight: Docker userns-remap is unsupported because Stella cannot map writable sandbox mounts safely")
	}
	unsupported := unsupportedResourceLimits(security)
	if len(unsupported) > 0 {
		return fmt.Errorf("docker preflight: Docker daemon cannot enforce required sandbox resource limits: %s", strings.Join(unsupported, ", "))
	}
	runtimeInfo, available, err := client.Runtime(ctx, cfg.Docker.Runtime)
	if err != nil {
		return fmt.Errorf("docker preflight: inspect runtime %q: %w", cfg.Docker.Runtime, err)
	}
	if !available {
		if runtime := cfg.Docker.Runtime; runtime != "" {
			return fmt.Errorf("docker preflight: runtime %q is not registered with the Docker daemon", runtime)
		}
		return fmt.Errorf("docker preflight: Docker daemon did not report its default OCI runtime")
	}
	if arg, unsafe := unsafeRuntimeResourceArg(runtimeInfo.Args); unsafe {
		return fmt.Errorf("docker preflight: runtime %q uses %q, which disables Stella sandbox resource limits", runtimeInfo.Name, arg)
	}

	if err := client.EnsureImageReady(ctx, cfg.Docker.Image, "preflight"); err != nil {
		return fmt.Errorf("docker preflight: %w", &ImageUnavailableError{Err: err})
	}
	if expected := cfg.Docker.ExpectedBundleRevision; expected != "" {
		imageInfo, err := client.ImageInfo(ctx, cfg.Docker.Image)
		if err != nil {
			return fmt.Errorf("docker preflight: inspect builtin bundle revision: %w", err)
		}
		actual := imageInfo.Labels[builtinBundleRevisionLabel]
		if actual != expected {
			return fmt.Errorf("docker preflight: builtin bundle revision mismatch (expected %s, image has %s); run `mise run sandbox:docker:build` for the local image or rebuild your custom sandbox image from this Stella revision", expected, actual)
		}
	}

	return nil
}

func unsupportedResourceLimits(security dockerclient.DaemonSecurity) []string {
	var unsupported []string
	if !security.MemoryLimit {
		unsupported = append(unsupported, "memory")
	}
	if !security.SwapLimit {
		unsupported = append(unsupported, "swap")
	}
	if !security.CPUCfsPeriod || !security.CPUCfsQuota {
		unsupported = append(unsupported, "CPU quota")
	}
	if !security.PidsLimit {
		unsupported = append(unsupported, "PID")
	}
	return unsupported
}

func unsafeRuntimeResourceArg(args []string) (string, bool) {
	for _, arg := range args {
		trimmed := strings.TrimSpace(arg)
		if !strings.HasPrefix(trimmed, "-") {
			continue
		}
		flag := strings.TrimLeft(trimmed, "-")
		if flag == "ignore-cgroups" {
			return arg, true
		}
		value, found := strings.CutPrefix(flag, "ignore-cgroups=")
		if !found {
			continue
		}
		enabled, err := strconv.ParseBool(value)
		if err != nil || enabled {
			return arg, true
		}
	}
	return "", false
}
