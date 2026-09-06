package sandbox

import (
	"strings"
	"testing"
)

func TestHostEnvBuildPathUsesOnlySelectionShims(t *testing.T) {
	t.Setenv("PATH", "/usr/bin:/opt/host/bin:/bin")

	allDisabled := HostEnvBuildPath("/opt/stella", "", "")
	if strings.Contains(allDisabled, "/opt/stella") || strings.Contains(allDisabled, ".mise-tools/shims") {
		t.Fatalf("disabled selection leaked Stella paths: %q", allDisabled)
	}

	selection := HostEnvBuildPath("/opt/stella", "", "/opt/stella/.mise-tools/contexts/one/shims")
	if !strings.HasPrefix(selection, "/opt/stella/.mise-tools/contexts/one/shims:") {
		t.Fatalf("selection shims must lead PATH: %q", selection)
	}
	if strings.Contains(selection, "/opt/stella/.mise-tools/shims") || strings.Contains(selection, "/opt/stella/bin") {
		t.Fatalf("selection PATH leaked shared Stella paths: %q", selection)
	}
}

func TestHostEnvBuildPathKeepsScopeSelectionsIndependent(t *testing.T) {
	first := HostEnvBuildPath("/opt/stella", "/users/u1/.mise-tools/shims", "/opt/stella/.mise-tools/contexts/system-a/shims")
	second := HostEnvBuildPath("/opt/stella", "/users/u1/.mise-tools/shims", "/opt/stella/.mise-tools/contexts/system-b/shims")

	if !strings.Contains(first, "contexts/system-a/shims") || strings.Contains(first, "contexts/system-b/shims") {
		t.Fatalf("first scope PATH crossed selection boundary: %q", first)
	}
	if !strings.Contains(second, "contexts/system-b/shims") || strings.Contains(second, "contexts/system-a/shims") {
		t.Fatalf("second scope PATH crossed selection boundary: %q", second)
	}
}
