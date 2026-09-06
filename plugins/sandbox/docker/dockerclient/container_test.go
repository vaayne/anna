package dockerclient

import (
	"testing"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
)

func TestEnvSlice(t *testing.T) {
	t.Run("nil input", func(t *testing.T) {
		if got := envSlice(nil); got != nil {
			t.Fatalf("expected nil, got %v", got)
		}
	})
	t.Run("empty map", func(t *testing.T) {
		if got := envSlice(map[string]string{}); got != nil {
			t.Fatalf("expected nil, got %v", got)
		}
	})
	t.Run("single entry", func(t *testing.T) {
		got := envSlice(map[string]string{"FOO": "bar"})
		if len(got) != 1 || got[0] != "FOO=bar" {
			t.Fatalf("unexpected: %v", got)
		}
	})
	t.Run("sorted output", func(t *testing.T) {
		got := envSlice(map[string]string{"Z": "last", "A": "first", "M": "mid"})
		want := []string{"A=first", "M=mid", "Z=last"}
		for i, v := range want {
			if got[i] != v {
				t.Fatalf("index %d: got %q want %q", i, got[i], v)
			}
		}
	})
}

func TestMapNetworkMode(t *testing.T) {
	cases := []struct {
		name string
		in   CreateOptions
		want container.NetworkMode
	}{
		{"disabled", CreateOptions{NetworkMode: NetworkDisabled}, container.NetworkMode("none")},
		{"allow all default bridge", CreateOptions{NetworkMode: NetworkAllowAll}, container.NetworkMode("")},
		{"unknown mode default bridge", CreateOptions{NetworkMode: "unknown"}, container.NetworkMode("")},
		{"explicit network", CreateOptions{NetworkMode: NetworkAllowAll, Network: "stella-net"}, container.NetworkMode("stella-net")},
		{"disabled wins over network", CreateOptions{NetworkMode: NetworkDisabled, Network: "stella-net"}, container.NetworkMode("none")},
	}
	for _, c := range cases {
		got := mapNetworkMode(c.in)
		if got != c.want {
			t.Fatalf("%s: mapNetworkMode = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestBuildHostConfigHardening(t *testing.T) {
	hc := buildHostConfig(CreateOptions{Runtime: "runsc", NetworkMode: NetworkDisabled})
	if hc.Runtime != "runsc" {
		t.Fatalf("Runtime = %q, want runsc", hc.Runtime)
	}
	if hc.NetworkMode != container.NetworkMode("none") {
		t.Fatalf("NetworkMode = %q, want none", hc.NetworkMode)
	}
	if hc.Memory != sandboxMemoryLimitBytes {
		t.Fatalf("Memory = %d, want %d", hc.Memory, sandboxMemoryLimitBytes)
	}
	if hc.MemorySwap != sandboxMemoryLimitBytes {
		t.Fatalf("MemorySwap = %d, want %d", hc.MemorySwap, sandboxMemoryLimitBytes)
	}
	if hc.NanoCPUs != sandboxNanoCPUs {
		t.Fatalf("NanoCPUs = %d, want %d", hc.NanoCPUs, sandboxNanoCPUs)
	}
	if hc.PidsLimit == nil || *hc.PidsLimit != sandboxPidsLimit {
		t.Fatalf("PidsLimit = %v, want %d", hc.PidsLimit, sandboxPidsLimit)
	}
	if len(hc.CapDrop) != 1 || hc.CapDrop[0] != "ALL" {
		t.Fatalf("CapDrop = %v, want [ALL]", hc.CapDrop)
	}
	if len(hc.SecurityOpt) != 1 || hc.SecurityOpt[0] != "no-new-privileges" {
		t.Fatalf("SecurityOpt = %v, want [no-new-privileges]", hc.SecurityOpt)
	}
	if !hc.ReadonlyRootfs {
		t.Fatal("ReadonlyRootfs = false, want true")
	}
}

func TestBuildMounts(t *testing.T) {
	t.Run("no workspace no extras", func(t *testing.T) {
		mounts := buildMounts(CreateOptions{})
		if len(mounts) != 0 {
			t.Fatalf("expected 0 mounts, got %d", len(mounts))
		}
	})
	t.Run("workspace only", func(t *testing.T) {
		opts := CreateOptions{WorkspaceHost: "/host/ws", WorkspaceMount: "/container/ws"}
		mounts := buildMounts(opts)
		if len(mounts) != 1 {
			t.Fatalf("expected 1 mount, got %d", len(mounts))
		}
		m := mounts[0]
		if m.Type != mount.TypeBind || m.Source != "/host/ws" || m.Target != "/container/ws" || m.ReadOnly {
			t.Fatalf("unexpected mount: %+v", m)
		}
	})
	t.Run("readonly mounts included", func(t *testing.T) {
		opts := CreateOptions{
			ExtraMounts: []Mount{
				{HostPath: "/host/ro", ContainerPath: "/container/ro", ReadOnly: true},
			},
		}
		mounts := buildMounts(opts)
		if len(mounts) != 1 {
			t.Fatalf("expected 1 mount, got %d", len(mounts))
		}
		if !mounts[0].ReadOnly {
			t.Fatal("expected ReadOnly=true")
		}
	})

	t.Run("volume mounts included", func(t *testing.T) {
		opts := CreateOptions{
			ExtraMounts: []Mount{
				{HostPath: "stella-tools-abc", ContainerPath: "/tools", ReadOnly: true, Type: MountTypeVolume},
			},
		}
		mounts := buildMounts(opts)
		if len(mounts) != 1 {
			t.Fatalf("expected 1 mount, got %d", len(mounts))
		}
		if mounts[0].Type != mount.TypeVolume || mounts[0].Source != "stella-tools-abc" || mounts[0].Target != "/tools" || !mounts[0].ReadOnly {
			t.Fatalf("unexpected volume mount: %+v", mounts[0])
		}
	})
	t.Run("selection volume disables copy-up", func(t *testing.T) {
		opts := CreateOptions{ExtraMounts: []Mount{{
			HostPath: "stella-selection-abc", ContainerPath: "/opt/stella/bin",
			ReadOnly: true, Type: MountTypeVolume, NoCopy: true,
		}}}
		mounts := buildMounts(opts)
		if len(mounts) != 1 || mounts[0].VolumeOptions == nil || !mounts[0].VolumeOptions.NoCopy {
			t.Fatalf("selection mount must set NoCopy: %+v", mounts)
		}
	})
	t.Run("tmpfs mount provides writable helper scratch", func(t *testing.T) {
		mounts := buildMounts(CreateOptions{ExtraMounts: []Mount{{
			ContainerPath: "/tmp", Type: MountTypeTmpfs, TmpfsExec: true,
		}}})
		if len(mounts) != 1 || mounts[0].Type != mount.TypeTmpfs || mounts[0].Target != "/tmp" || mounts[0].ReadOnly || mounts[0].TmpfsOptions == nil || len(mounts[0].TmpfsOptions.Options) != 1 || mounts[0].TmpfsOptions.Options[0][0] != "exec" {
			t.Fatalf("unexpected tmpfs mount: %+v", mounts)
		}
	})
	t.Run("workspace plus readonly", func(t *testing.T) {
		opts := CreateOptions{
			WorkspaceHost:  "/host/ws",
			WorkspaceMount: "/container/ws",
			ExtraMounts: []Mount{
				{HostPath: "/host/ro", ContainerPath: "/container/ro", ReadOnly: true},
			},
		}
		mounts := buildMounts(opts)
		if len(mounts) != 2 {
			t.Fatalf("expected 2 mounts, got %d", len(mounts))
		}
	})
}

func TestBuildContainerConfig(t *testing.T) {
	opts := CreateOptions{
		Image:          "test-image:latest",
		WorkspaceMount: "/workspace",
		Env:            map[string]string{"KEY": "val"},
		Labels:         map[string]string{LabelSessionID: "abc"},
	}
	cfg := buildContainerConfig(opts)
	if cfg.Image != "test-image:latest" {
		t.Fatalf("unexpected image: %s", cfg.Image)
	}
	if cfg.WorkingDir != "/workspace" {
		t.Fatalf("unexpected workdir: %s", cfg.WorkingDir)
	}
	if len(cfg.Env) != 1 || cfg.Env[0] != "KEY=val" {
		t.Fatalf("unexpected env: %v", cfg.Env)
	}
	if cfg.Labels[LabelSessionID] != "abc" {
		t.Fatal("label not set")
	}
	if len(cfg.Entrypoint) == 0 {
		t.Fatal("entrypoint not set")
	}
}

func TestBuildContainerCreateOptions(t *testing.T) {
	opts := CreateOptions{
		Name:           "test-container",
		Image:          "img",
		NetworkMode:    NetworkDisabled,
		WorkspaceHost:  "/h",
		WorkspaceMount: "/c",
	}
	co := buildContainerCreateOptions(opts)
	if co.Name != "test-container" {
		t.Fatalf("unexpected name: %s", co.Name)
	}
	if co.Config == nil || co.HostConfig == nil {
		t.Fatal("Config or HostConfig is nil")
	}
	if co.HostConfig.NetworkMode != container.NetworkMode("none") {
		t.Fatalf("unexpected network mode: %s", co.HostConfig.NetworkMode)
	}
}
