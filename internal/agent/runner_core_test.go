package agent

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/CherryHQ/stella/plugins/core"
)

// fixtureRunnerCoreRuntimePlan creates the complete startup-shaped core plan
// without running the installer or downloading tools. Runner tests use this
// same fixture so the none backend exercises the same plan validation as the
// native startup path.
func fixtureRunnerCoreRuntimePlan(t *testing.T, root string) *core.RuntimePlan {
	t.Helper()
	identity, err := core.RuntimeIdentity()
	if err != nil {
		t.Fatalf("core.RuntimeIdentity: %v", err)
	}
	publicDir := filepath.Join(root, "core-runtime")
	if err := os.MkdirAll(publicDir, 0o755); err != nil {
		t.Fatalf("create core fixture directory: %v", err)
	}
	plan := &core.RuntimePlan{
		Identity:     identity,
		PublicDir:    publicDir,
		PublicBinDir: publicDir,
		Runtimes:     make([]core.Runtime, 0, len(core.RuntimeResources())),
	}
	for _, resource := range core.RuntimeResources() {
		name := resource.Name
		if runtime.GOOS == "windows" {
			name += ".exe"
		}
		path := filepath.Join(publicDir, name)
		if err := os.WriteFile(path, []byte("fixture runtime\n"), 0o755); err != nil {
			t.Fatalf("write core fixture %s: %v", resource.Name, err)
		}
		plan.Runtimes = append(plan.Runtimes, core.Runtime{
			Name: resource.Name, Version: resource.Version, Path: path, Available: true,
		})
	}
	if err := core.Verify(*plan); err != nil {
		t.Fatalf("core.Verify fixture: %v", err)
	}
	return plan
}
