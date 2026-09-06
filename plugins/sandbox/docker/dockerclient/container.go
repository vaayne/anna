package dockerclient

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	mobyclient "github.com/moby/moby/client"
)

// NetworkMode controls the container's network access.
type NetworkMode string

const (
	NetworkDisabled NetworkMode = "disabled"
	NetworkAllowAll NetworkMode = "allow_all"
)

// Deliberate resource ceilings for untrusted agent workloads; raise if a
// legitimate toolchain needs more.
const (
	sandboxMemoryLimitBytes int64 = 2 << 30       // 2 GiB
	sandboxNanoCPUs         int64 = 2_000_000_000 // 2 CPUs
	sandboxPidsLimit        int64 = 512
)

// MountType identifies how Docker should mount Source into the container.
type MountType string

const (
	MountTypeBind   MountType = "bind"
	MountTypeVolume MountType = "volume"
	MountTypeTmpfs  MountType = "tmpfs"
)

// Mount represents a mount from a daemon-visible source to a container path.
// Bind mount sources are interpreted by the daemon, so when stella runs inside a
// container they must already be translated to daemon-visible paths before
// reaching this struct. Volume mount sources are Docker volume names.
type Mount struct {
	HostPath      string
	ContainerPath string
	ReadOnly      bool
	Type          MountType
	// VolumeSubpath selects a sub-path within the named volume to mount.
	// Only valid when Type is MountTypeVolume. Requires Docker Engine 25+.
	VolumeSubpath string
	// NoCopy prevents Docker from copying files from the image into a named
	// volume when the volume is mounted over an image directory. Selection
	// volumes rely on this to mask unselected image binaries.
	NoCopy bool
	// TmpfsExec permits executable files on a tmpfs mount. It is restricted to
	// short-lived trusted helper scratch; ordinary sandbox tmpfs mounts keep the
	// daemon's noexec default.
	TmpfsExec bool
}

// CreateOptions configures a new sandbox container.
type CreateOptions struct {
	Image          string
	Runtime        string      // optional registered OCI runtime; empty uses the daemon default
	WorkspaceHost  string      // absolute host path (daemon-side)
	WorkspaceMount string      // absolute in-container path (e.g. "/workspace")
	ExtraMounts    []Mount     // additional host -> container mounts; ReadOnly is honored per mount
	NetworkMode    NetworkMode // disabled | allow_all
	Network        string      // optional Docker network to join (e.g. to reach stellad in DooD); ignored when NetworkMode is disabled
	Env            map[string]string
	User           string            // optional container user override
	Labels         map[string]string // must include LabelSessionID + LabelStellaHome + LabelCreatedAt
	Name           string            // optional; caller builds "stella-sandbox-<session-id>"
}

// CreateAndStart creates a container with an always-up sentinel entrypoint
// (`sh -c 'tail -f /dev/null'`), starts it, and returns the container ID.
// If the image is not present locally it is pulled automatically.
func (c *Client) CreateAndStart(ctx context.Context, opts CreateOptions) (string, error) {
	if err := c.EnsureImageReady(ctx, opts.Image, opts.Name); err != nil {
		return "", &ImageUnavailableError{Err: err}
	}

	createOpts := buildContainerCreateOptions(opts)

	slog.Info("dockerclient: creating sandbox container", "image", opts.Image, "container_name", opts.Name, "runtime", opts.Runtime, "network_mode", opts.NetworkMode, "mounts", len(createOpts.HostConfig.Mounts))
	created, err := c.api.ContainerCreate(ctx, createOpts)
	if err != nil && errdefs.IsNotFound(err) {
		c.invalidateImageReady(opts.Image)
		if readyErr := c.EnsureImageReady(ctx, opts.Image, opts.Name); readyErr != nil {
			return "", &ImageUnavailableError{Err: readyErr}
		}
		created, err = c.api.ContainerCreate(ctx, createOpts)
	}
	if err != nil {
		slog.Warn("dockerclient: container create failed", "image", opts.Image, "container_name", opts.Name, "error", err)
		return "", fmt.Errorf("dockerclient: container create: %w", err)
	}
	if len(created.Warnings) > 0 {
		for _, warning := range created.Warnings {
			slog.Warn("dockerclient: refusing sandbox container created with daemon warning", "container_id", created.ID, "container_name", opts.Name, "warning", warning)
		}
		c.cleanupCreatedContainer(created.ID, opts.Name)
		return "", fmt.Errorf("dockerclient: container create returned warnings: %s", strings.Join(created.Warnings, "; "))
	}

	slog.Info("dockerclient: starting sandbox container", "container_id", created.ID, "container_name", opts.Name)
	if _, err := c.api.ContainerStart(ctx, created.ID, mobyclient.ContainerStartOptions{}); err != nil {
		slog.Warn("dockerclient: container start failed", "container_id", created.ID, "container_name", opts.Name, "error", err)
		c.cleanupCreatedContainer(created.ID, opts.Name)
		return "", fmt.Errorf("dockerclient: container start %s: %w", created.ID, err)
	}

	slog.Info("dockerclient: sandbox container started", "container_id", created.ID, "container_name", opts.Name)
	return created.ID, nil
}

func (c *Client) cleanupCreatedContainer(containerID, containerName string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := c.api.ContainerRemove(ctx, containerID, mobyclient.ContainerRemoveOptions{Force: true}); err != nil && !errdefs.IsNotFound(err) {
		slog.Warn("dockerclient: cleanup failed after container setup", "container_id", containerID, "container_name", containerName, "error", err)
	}
}

// Stop sends SIGTERM with a 2-second grace period, then removes the container.
// Missing-container errors are swallowed so Close is idempotent.
func (c *Client) Stop(ctx context.Context, containerID string) error {
	timeout := 2
	_, err := c.api.ContainerStop(ctx, containerID, mobyclient.ContainerStopOptions{Timeout: &timeout})
	if err != nil && !errdefs.IsNotFound(err) {
		return fmt.Errorf("dockerclient: container stop %s: %w", containerID, err)
	}

	if _, err := c.api.ContainerRemove(ctx, containerID, mobyclient.ContainerRemoveOptions{}); err != nil {
		if errdefs.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("dockerclient: container remove %s: %w", containerID, err)
	}
	return nil
}

// ContainerAlive reports whether the container is running. Returns (false, nil)
// when the container no longer exists.
func (c *Client) ContainerAlive(ctx context.Context, containerID string) (bool, error) {
	res, err := c.api.ContainerInspect(ctx, containerID, mobyclient.ContainerInspectOptions{})
	if err != nil {
		if errdefs.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("dockerclient: container inspect %s: %w", containerID, err)
	}
	if res.Container.State == nil {
		return false, nil
	}
	return res.Container.State.Running, nil
}

// ContainerState holds the container running state and last exit code.
type ContainerState struct {
	Running  bool
	ExitCode int
}

// InspectContainerState returns the running state and exit code of a container
// referenced by ID or name. Returns (nil, nil) when the container does not exist.
func (c *Client) InspectContainerState(ctx context.Context, containerRef string) (*ContainerState, error) {
	res, err := c.api.ContainerInspect(ctx, containerRef, mobyclient.ContainerInspectOptions{})
	if err != nil {
		if errdefs.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("dockerclient: container inspect %s: %w", containerRef, err)
	}
	if res.Container.State == nil {
		return &ContainerState{}, nil
	}
	return &ContainerState{
		Running:  res.Container.State.Running,
		ExitCode: res.Container.State.ExitCode,
	}, nil
}

// SelfNetwork is one network the current container is attached to.
type SelfNetwork struct {
	Name string
	IP   string // this container's IP on the network, empty if unset
}

// SelfContainer describes the container the current process runs in, so a
// sibling sandbox can be wired to reach it. Networks holds the user-defined
// networks (DNS-capable; a sandbox can reach this container by Name on them),
// sorted by name. BridgeIP is the address on the default bridge, used as a
// last resort when there is no user-defined network (the bridge has no embedded
// DNS, so only its IP is reachable).
type SelfContainer struct {
	ID       string
	Name     string // without the leading slash docker prepends
	Networks []SelfNetwork
	BridgeIP string
}

// InspectSelf inspects the container referenced by ref — typically os.Hostname(),
// which Docker defaults to the short container ID — and reports its networks so
// a sibling sandbox can be attached and pointed back at this process. Returns
// (nil, nil) when ref is not a known container (e.g. stellad runs on the host),
// so callers fall back to explicit configuration or loopback.
func (c *Client) InspectSelf(ctx context.Context, ref string) (*SelfContainer, error) {
	res, err := c.api.ContainerInspect(ctx, ref, mobyclient.ContainerInspectOptions{})
	if err != nil {
		if errdefs.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("dockerclient: inspect self %s: %w", ref, err)
	}
	self := &SelfContainer{ID: res.Container.ID, Name: strings.TrimPrefix(res.Container.Name, "/")}
	if res.Container.NetworkSettings == nil {
		return self, nil
	}
	for name, ep := range res.Container.NetworkSettings.Networks {
		ip := ""
		if ep != nil && ep.IPAddress.IsValid() {
			ip = ep.IPAddress.String()
		}
		switch name {
		case "host", "none":
			continue
		case "bridge":
			self.BridgeIP = ip
		default:
			self.Networks = append(self.Networks, SelfNetwork{Name: name, IP: ip})
		}
	}
	sort.Slice(self.Networks, func(i, j int) bool { return self.Networks[i].Name < self.Networks[j].Name })
	return self, nil
}

// buildContainerCreateOptions translates CreateOptions into the SDK request.
// Pure function so tests can assert the wiring without a daemon.
func buildContainerCreateOptions(opts CreateOptions) mobyclient.ContainerCreateOptions {
	return mobyclient.ContainerCreateOptions{
		Name:       opts.Name,
		Config:     buildContainerConfig(opts),
		HostConfig: buildHostConfig(opts),
	}
}

func buildContainerConfig(opts CreateOptions) *container.Config {
	cfg := &container.Config{
		Image:      opts.Image,
		User:       opts.User,
		Labels:     opts.Labels,
		Entrypoint: []string{"/bin/sh"},
		Cmd:        []string{"-c", "tail -f /dev/null"},
	}
	if opts.WorkspaceMount != "" {
		cfg.WorkingDir = opts.WorkspaceMount
	}
	cfg.Env = envSlice(opts.Env)
	return cfg
}

func buildHostConfig(opts CreateOptions) *container.HostConfig {
	pidsLimit := sandboxPidsLimit
	hc := &container.HostConfig{
		Runtime:     opts.Runtime,
		NetworkMode: mapNetworkMode(opts),
		Resources: container.Resources{
			Memory:     sandboxMemoryLimitBytes,
			MemorySwap: sandboxMemoryLimitBytes,
			NanoCPUs:   sandboxNanoCPUs,
			PidsLimit:  &pidsLimit,
		},
		// Drop all capabilities by default; relax narrowly if a toolchain genuinely needs one.
		CapDrop:        []string{"ALL"},
		SecurityOpt:    []string{"no-new-privileges"},
		ReadonlyRootfs: true,
		Mounts:         buildMounts(opts),
	}
	return hc
}

// mapNetworkMode picks the container network. A disabled policy always wins
// (fully isolated). Otherwise an explicit Network joins that user-defined
// network — required so DooD sandboxes can reach stellad by service name —
// falling back to the daemon default bridge.
func mapNetworkMode(opts CreateOptions) container.NetworkMode {
	if opts.NetworkMode == NetworkDisabled {
		return container.NetworkMode("none")
	}
	if opts.Network != "" {
		return container.NetworkMode(opts.Network)
	}
	return container.NetworkMode("")
}

func buildMounts(opts CreateOptions) []mount.Mount {
	n := len(opts.ExtraMounts)
	if opts.WorkspaceHost != "" && opts.WorkspaceMount != "" {
		n++
	}
	mounts := make([]mount.Mount, 0, n)

	if opts.WorkspaceHost != "" && opts.WorkspaceMount != "" {
		mounts = append(mounts, mount.Mount{
			Type:   mount.TypeBind,
			Source: opts.WorkspaceHost,
			Target: opts.WorkspaceMount,
		})
	}
	for _, m := range opts.ExtraMounts {
		mm := mount.Mount{
			Type:     dockerMountType(m.Type),
			Source:   m.HostPath,
			Target:   m.ContainerPath,
			ReadOnly: m.ReadOnly,
		}
		switch {
		case m.Type == MountTypeVolume && m.VolumeSubpath != "":
			mm.VolumeOptions = &mount.VolumeOptions{Subpath: m.VolumeSubpath, NoCopy: m.NoCopy}
		case m.Type == MountTypeVolume && m.NoCopy:
			mm.VolumeOptions = &mount.VolumeOptions{NoCopy: true}
		case m.Type == MountTypeTmpfs && m.TmpfsExec:
			mm.TmpfsOptions = &mount.TmpfsOptions{Options: [][]string{{"exec"}}}
		}
		mounts = append(mounts, mm)
	}
	return mounts
}

func dockerMountType(t MountType) mount.Type {
	switch t {
	case MountTypeVolume:
		return mount.TypeVolume
	case MountTypeTmpfs:
		return mount.TypeTmpfs
	default:
		return mount.TypeBind
	}
}

// envSlice returns env in deterministic KEY=VALUE form sorted by key.
// The daemon accepts any order, but deterministic output simplifies testing
// and telemetry.
func envSlice(env map[string]string) []string {
	if len(env) == 0 {
		return nil
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, fmt.Sprintf("%s=%s", k, env[k]))
	}
	return out
}
