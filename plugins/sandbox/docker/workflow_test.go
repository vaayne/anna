package docker_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSandboxImageWorkflowPassesBuiltinBundleRevision(t *testing.T) {
	workflowPath := filepath.Join("..", "..", "..", ".github", "workflows", "sandbox-docker.yml")
	data, err := os.ReadFile(workflowPath)
	if err != nil {
		t.Fatalf("read sandbox image workflow: %v", err)
	}

	workflow := string(data)
	for _, want := range []string{
		`- "resources/**"`,
		`id: builtin-bundle`,
		`revision=$(go run ./cmd/stellad system-bundle revision)`,
		`BUILTIN_BUNDLE_REVISION=${{ steps.builtin-bundle.outputs.revision }}`,
	} {
		if !strings.Contains(workflow, want) {
			t.Errorf("sandbox image workflow does not contain %q", want)
		}
	}
}

func TestSandboxImageBuildPreparesBuiltinArtifactsExplicitly(t *testing.T) {
	dockerfile, err := os.ReadFile(filepath.Join("..", "..", "..", "plugins", "sandbox", "docker", "Dockerfile"))
	if err != nil {
		t.Fatalf("read sandbox Dockerfile: %v", err)
	}
	if !strings.Contains(string(dockerfile), "system-bundle install --core-path /opt/stella/core-runtime --prepare-builtin-artifacts") {
		t.Fatal("sandbox image build does not explicitly prepare builtin artifacts")
	}
	for _, privatePath := range []string{"/opt/stella/.mise-tools/config", "/opt/stella/.mise-tools/state", "/opt/stella/.mise-private"} {
		if !strings.Contains(string(dockerfile), privatePath) {
			t.Fatalf("sandbox image build does not clean private path %s", privatePath)
		}
	}
}
