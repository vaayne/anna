package docker

import (
	"fmt"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strings"

	sandboxpkg "github.com/CherryHQ/stella/pkg/sandbox"
	"github.com/CherryHQ/stella/plugins/sandbox/docker/dockerclient"
	"github.com/CherryHQ/stella/plugins/sandbox/internal/sessionfs"
)

// configureSessionMounts wires the generic mount plan for the active mode and
// returns the mounted process-view mounts, the mounted temp-dir host, and the
// mounted user-data host ("" when /user could not be mounted, so callers don't
// wire a /user that isn't really there).
func (f *dockerFactory) configureSessionMounts(opts *dockerclient.CreateOptions, mounts []sessionfs.Mount, workspaceHost, userDataHost, tempDirHost string) ([]sessionfs.Mount, string, string, error) {
	if len(mounts) == 0 {
		mounts = append(mounts, sessionfs.Mount{HostPath: workspaceHost, SandboxPath: workspaceMount})
	}
	if userDataHost != "" && hostPathForSandboxMount(mounts, userDataMount) == "" {
		mounts = append(mounts, sessionfs.Mount{HostPath: userDataHost, SandboxPath: userDataMount})
	}
	if f.cfg.RuntimeMode == DockerSandboxModeVolume {
		mounted, mountedTempDirHost, mountedUserDataHost, err := f.configureVolumeMounts(opts, mounts, workspaceHost, userDataHost, tempDirHost)
		return mounted, mountedTempDirHost, mountedUserDataHost, err
	}
	return f.configureBindMounts(opts, mounts, workspaceHost, userDataHost, tempDirHost)
}

func (f *dockerFactory) configureVolumeMounts(opts *dockerclient.CreateOptions, mounts []sessionfs.Mount, workspaceHost, userDataHost, tempDirHost string) ([]sessionfs.Mount, string, string, error) {
	workspaceSubpath, ok := relativePathWithin(f.cfg.StellaHome, workspaceHost)
	if !ok {
		return nil, "", "", fmt.Errorf("docker session: workspace %q is not inside STELLA_HOME %q; cannot use volume mode", workspaceHost, f.cfg.StellaHome)
	}
	if workspaceSubpath == "." {
		return nil, "", "", fmt.Errorf("docker session: workspace must be a subdirectory of STELLA_HOME, not STELLA_HOME itself")
	}
	opts.ExtraMounts = append(opts.ExtraMounts, dockerclient.Mount{
		HostPath:      f.cfg.StellaHomeVolume,
		ContainerPath: workspaceMount,
		ReadOnly:      false,
		Type:          dockerclient.MountTypeVolume,
		VolumeSubpath: filepath.ToSlash(workspaceSubpath),
	})
	mounted := []sessionfs.Mount{{HostPath: workspaceHost, SandboxPath: workspaceMount}}
	mountedUserDataHost := ""
	for _, m := range nonWorkspacePolicyMounts(mounts) {
		if m.ReadOnly && !dirExists(m.HostPath) {
			continue
		}
		subpath, ok := relativePathWithin(f.cfg.StellaHome, m.HostPath)
		if !ok || subpath == "." {
			logSkippedSandboxMount(DockerSandboxModeVolume, m.HostPath, "path is outside STELLA_HOME and cannot be mounted from the named volume")
			continue
		}
		opts.ExtraMounts = append(opts.ExtraMounts, dockerclient.Mount{
			HostPath:      f.cfg.StellaHomeVolume,
			ContainerPath: m.SandboxPath,
			ReadOnly:      m.ReadOnly,
			Type:          dockerclient.MountTypeVolume,
			VolumeSubpath: filepath.ToSlash(subpath),
		})
		mounted = append(mounted, m)
		if m.SandboxPath == userDataMount {
			mountedUserDataHost = m.HostPath
		}
	}
	mountedTempDirHost := ""
	if tempDirHost != "" {
		tmpSubpath, ok := relativePathWithin(f.cfg.StellaHome, tempDirHost)
		if !ok || tmpSubpath == "." {
			return nil, "", "", fmt.Errorf("docker session: temp directory %q is not a STELLA_HOME subdirectory in volume mode", tempDirHost)
		}
		opts.ExtraMounts = append(opts.ExtraMounts, dockerclient.Mount{
			HostPath:      f.cfg.StellaHomeVolume,
			ContainerPath: "/tmp",
			Type:          dockerclient.MountTypeVolume,
			VolumeSubpath: filepath.ToSlash(tmpSubpath),
		})
		mountedTempDirHost = tempDirHost
	}
	opts.WorkspaceHost = ""
	return mounted, mountedTempDirHost, mountedUserDataHost, nil
}

func (f *dockerFactory) configureBindMounts(opts *dockerclient.CreateOptions, mounts []sessionfs.Mount, workspaceHost, userDataHost, tempDirHost string) ([]sessionfs.Mount, string, string, error) {
	daemonWorkspaceHost, ok := f.cfg.daemonPath(workspaceHost)
	if !ok {
		return nil, "", "", fmt.Errorf("docker session: workspace %q is not under STELLA_HOME %q; cannot use bind-mount mode", workspaceHost, f.cfg.StellaHome)
	}
	opts.WorkspaceHost = daemonWorkspaceHost
	mounted := []sessionfs.Mount{{HostPath: workspaceHost, SandboxPath: workspaceMount}}
	mountedUserDataHost := ""
	for _, m := range nonWorkspacePolicyMounts(mounts) {
		if m.ReadOnly && !dirExists(m.HostPath) {
			continue
		}
		daemonPath, ok := f.cfg.daemonPath(m.HostPath)
		if !ok {
			logSkippedSandboxMount(f.cfg.RuntimeMode, m.HostPath, "path is not visible to the Docker daemon")
			continue
		}
		opts.ExtraMounts = append(opts.ExtraMounts, dockerclient.Mount{
			HostPath:      daemonPath,
			ContainerPath: m.SandboxPath,
			ReadOnly:      m.ReadOnly,
		})
		mounted = append(mounted, m)
		if m.SandboxPath == userDataMount {
			mountedUserDataHost = m.HostPath
		}
	}
	mountedTempDirHost := ""
	if tempDirHost != "" {
		if daemonPath, ok := f.cfg.daemonPath(tempDirHost); ok {
			opts.ExtraMounts = append(opts.ExtraMounts, dockerclient.Mount{
				HostPath:      daemonPath,
				ContainerPath: "/tmp",
			})
			mountedTempDirHost = tempDirHost
		} else {
			logSkippedSandboxMount(f.cfg.RuntimeMode, tempDirHost, "path is not visible to the Docker daemon")
		}
	}
	return mounted, mountedTempDirHost, mountedUserDataHost, nil
}

func logSkippedSandboxMount(mode DockerSandboxMode, path, reason string) {
	slog.Warn("docker backend: skipping sandbox mount",
		"component", "runner_sandbox",
		"mode", mode,
		"path", path,
		"reason", reason,
	)
}

// dirExists reports whether path exists on the host (file or directory). Used to
// skip optional mounts whose source is absent for this session.
func dirExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func relativePathWithin(root, path string) (string, bool) {
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return rel, true
}

// cleanContainerPath canonicalizes a value at a Linux-container path boundary.
func cleanContainerPath(containerPath string) string {
	return path.Clean(strings.ReplaceAll(containerPath, "\\", "/"))
}

// dockerProviderMounts joins the canonical policy with provider-private sources.
func dockerProviderMounts(mounts []sandboxpkg.Mount, sources map[string]string) ([]sessionfs.Mount, error) {
	out := make([]sessionfs.Mount, 0, len(mounts))
	for _, mount := range mounts {
		target := cleanContainerPath(mount.SandboxPath)
		source := ""
		for candidate, host := range sources {
			if cleanContainerPath(candidate) == target {
				source = host
				break
			}
		}
		if source == "" {
			return nil, fmt.Errorf("docker session: provider-private source for mount %q is required", target)
		}
		out = append(out, sessionfs.Mount{HostPath: source, SandboxPath: target, ReadOnly: mount.Access == sandboxpkg.MountReadOnly})
	}
	return out, nil
}

// normalizeDockerPolicyMounts canonicalizes the container-space policy once at
// CreateSession's boundary.
func normalizeDockerPolicyMounts(mounts []sandboxpkg.Mount) []sandboxpkg.Mount {
	out := append([]sandboxpkg.Mount(nil), mounts...)
	for i := range out {
		out[i].SandboxPath = cleanContainerPath(out[i].SandboxPath)
	}
	return out
}

func hostPathForSandboxMount(mounts []sessionfs.Mount, sandboxPath string) string {
	for _, m := range mounts {
		if m.SandboxPath == sandboxPath {
			return m.HostPath
		}
	}
	return ""
}

// applyDockerFilesystemEnv renders the exact container coordinates exposed by
// both commands and Session.Files().
func applyDockerFilesystemEnv(env map[string]string, hasUserData, hasTemp bool) error {
	userData := ""
	if hasUserData {
		userData = userDataMount
	}
	tempDir := ""
	if hasTemp {
		tempDir = "/tmp"
	}
	return sandboxpkg.ApplyFilesystemEnv(env, sandboxpkg.FilesystemView{
		Home:          workspaceMount,
		SharedDataDir: userData,
		TempDir:       tempDir,
	})
}

func dockerMountProvidedByImage(m sessionfs.Mount) bool {
	containerPath := m.SandboxPath
	if !strings.HasPrefix(containerPath, stellaHomeMount+"/") {
		return false
	}
	rel, ok := sandboxpkg.POSIXPathRelative(stellaHomeMount, containerPath)
	if !ok {
		return false
	}
	_, ok = dockerImageProvidedStellaDirs[rel]
	return ok
}

func nonWorkspacePolicyMounts(mounts []sessionfs.Mount) []sessionfs.Mount {
	out := make([]sessionfs.Mount, 0, len(mounts))
	for _, m := range mounts {
		if m.SandboxPath == workspaceMount || dockerMountProvidedByImage(m) {
			continue
		}
		out = append(out, m)
	}
	return out
}

// dockerImageProvidedStellaDirs are STELLA_HOME subdirs the sandbox image bakes
// itself, built for the container's linux platform: the mise binary (bin) and the
// core runtime bin. It must NOT be mounted from the host, whose binaries may be
// a different platform. Selection-owned config and artifact directories are
// mounted separately from the snapshot, never by mounting .mise-tools wholesale.
var dockerImageProvidedStellaDirs = map[string]struct{}{
	"bin":            {},
	"skills/builtin": {},
}

// writableMount is a per-user writable tree mounted into the container, recording
// both ends so the caller can register it in the mount table and derive PATH.
type writableMount struct {
	Host      string
	Container string
}

func writableToolTrees(mounts []sessionfs.Mount) []writableMount {
	out := []writableMount{}
	for _, m := range mounts {
		if !m.ReadOnly && m.SandboxPath != workspaceMount && m.SandboxPath != userDataMount {
			out = append(out, writableMount{Host: m.HostPath, Container: m.SandboxPath})
		}
	}
	return out
}

type mountTableOptions struct {
	WorkspaceHost  string
	WorkspaceMount string
	Mounts         []sessionfs.Mount
	TempHost       string
}

// buildMountTable returns the process-view bind mount set that path resolution
// should consult. Host paths here are intentionally not daemon-translated: file
// tools run in the Stella process namespace, not the Docker daemon namespace.
func buildMountTable(opts mountTableOptions) []dockerclient.Mount {
	mounts := make([]dockerclient.Mount, 0, len(opts.Mounts)+1)
	if len(opts.Mounts) == 0 {
		mounts = append(mounts, dockerclient.Mount{HostPath: opts.WorkspaceHost, ContainerPath: cleanContainerPath(opts.WorkspaceMount)})
	} else {
		for _, m := range opts.Mounts {
			mounts = append(mounts, dockerclient.Mount{
				HostPath:      m.HostPath,
				ContainerPath: m.SandboxPath,
				ReadOnly:      m.ReadOnly,
			})
		}
	}
	if opts.TempHost != "" {
		mounts = append(mounts, dockerclient.Mount{HostPath: opts.TempHost, ContainerPath: "/tmp"})
	}
	return mounts
}
