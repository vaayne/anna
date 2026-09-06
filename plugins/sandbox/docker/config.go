package docker

import "strings"

// DockerSandboxMode describes how the Docker daemon sees STELLA_HOME.
type DockerSandboxMode string

const (
	// DockerSandboxModeHost means stellad runs on the same host namespace as the
	// Docker daemon, so stella-process paths are already daemon-visible.
	DockerSandboxModeHost DockerSandboxMode = "host"
	// DockerSandboxModeBind means stellad runs in a container and STELLA_HOME is a
	// host bind mount. STELLA_HOME_HOST provides the daemon-visible host path.
	DockerSandboxModeBind DockerSandboxMode = "bind"
	// DockerSandboxModeVolume means stellad runs in a container and STELLA_HOME is
	// backed by a Docker named volume. STELLA_HOME_VOLUME provides the volume name.
	DockerSandboxModeVolume DockerSandboxMode = "volume"
)

// Config configures the docker sandbox factory.
type Config struct {
	// Image is the container image to use. Required.
	Image string

	// Runtime selects the Docker daemon's registered OCI runtime for every
	// sandbox and tool-cache helper container. Empty uses the daemon default.
	// Normally auto-derived from STELLA_DOCKER_RUNTIME by NewFactory.
	Runtime string

	// ExpectedBundleRevision is the revision embedded by the running stellad.
	// Docker readiness rejects an image that labels a different revision.
	ExpectedBundleRevision string

	// StellaHome is the stella-process-view home directory. When stellad runs
	// inside a container this is the in-container path; when it runs on the host
	// this is the host path. Used for orphan cleanup scoping, preflight checks,
	// DooD path translation, and resolving user tool binaries from the plugins
	// manifest.
	StellaHome string

	// RuntimeMode declares how the Docker daemon can access STELLA_HOME. Normally
	// auto-derived from STELLA_DOCKER_SANDBOX_MODE by NewFactory.
	RuntimeMode DockerSandboxMode

	// ContainerPathPrefix / HostPathPrefix enable bind-mount path alignment when
	// stella runs inside a container and talks to the daemon on the host (DooD).
	// Normally auto-derived from STELLA_HOME_HOST by NewFactory; only set
	// explicitly in tests or when overriding the default detection.
	ContainerPathPrefix string
	HostPathPrefix      string

	// StellaHomeVolume is the Docker named volume that backs STELLA_HOME.
	// Set this (via STELLA_HOME_VOLUME env) when stella runs inside a container
	// whose STELLA_HOME is a Docker named volume. Sandbox sessions then use
	// volume subpath mounts (requires Docker Engine 25+) instead of bind mounts,
	// so the host daemon never needs a host-filesystem-visible path.
	// Normally auto-derived from STELLA_HOME_VOLUME by NewFactory.
	StellaHomeVolume string

	// SandboxNetwork, when set, attaches sandbox containers to this Docker
	// network instead of the daemon default bridge. Required in DooD setups
	// where stellad and the sandbox are separate containers: without a shared
	// network the sandbox cannot reach stellad, so server-backed CLI commands
	// (stella task/goal, recally) fail with connection refused. Set via
	// STELLA_SANDBOX_NETWORK, otherwise auto-detected by NewFactory from the
	// network stellad's own container is on.
	SandboxNetwork string

	// ServerURL, when set, is injected into the sandbox as STELLA_SERVER_URL so
	// CLI commands inside the sandbox reach stellad over SandboxNetwork instead
	// of the default 127.0.0.1 loopback (which, inside a separate sandbox
	// container, points at nothing). Set via STELLA_SANDBOX_SERVER_URL, otherwise
	// auto-detected by NewFactory as stellad's address on SandboxNetwork.
	ServerURL string

	// SelectionToolBinaries are all authorized snapshot binaries, across system,
	// system-agent, user, and user-agent scopes. The Linux helper prepares them
	// from the resolved image, never from the host filesystem. Writable per-user
	// trees remain ordered ahead of this immutable selection in PATH.
	SelectionToolBinaries []ToolBinary
}

// TranslateToDaemonPath rewrites a stella-process-view absolute path into the
// path the daemon will use as a bind-mount source. When prefix translation is
// not configured, the input is returned unchanged.
func (c Config) TranslateToDaemonPath(path string) string {
	translated, ok := c.daemonPath(path)
	if !ok {
		return path
	}
	return translated
}

// daemonPath rewrites path and reports whether it is safe to hand to the Docker
// daemon as a bind-mount source. In DooD bind mode, paths outside the configured
// container prefix are not daemon-visible and must be skipped/fail closed.
func (c Config) daemonPath(path string) (string, bool) {
	if c.ContainerPathPrefix == "" || c.HostPathPrefix == "" {
		return path, true
	}
	if path == c.ContainerPathPrefix {
		return c.HostPathPrefix, true
	}
	prefix := c.ContainerPathPrefix
	if !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	if after, ok := strings.CutPrefix(path, prefix); ok {
		return c.HostPathPrefix + "/" + after, true
	}
	return "", false
}

func (c Config) cleanupScope(stellaHome string) string {
	if c.StellaHomeVolume != "" {
		return "volume:" + c.StellaHomeVolume
	}
	return c.TranslateToDaemonPath(stellaHome)
}
