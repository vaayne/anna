package core

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestRuntimeResourcesAreFixedAndOrdered(t *testing.T) {
	resources := RuntimeResources()
	if len(resources) != 4 {
		t.Fatalf("runtime resource count = %d, want 4", len(resources))
	}
	for index, want := range []struct {
		name, tool, version, skillRef string
	}{
		{name: "mise"},
		{name: "xberg", skillRef: "builtin:xberg"},
		{name: "fd", tool: "github:sharkdp/fd", version: "10.4.2"},
		{name: "rg", tool: "github:BurntSushi/ripgrep", version: "15.2.0"},
	} {
		got := resources[index]
		if got.Name != want.name || got.MiseTool != want.tool || got.Version != want.version || got.SkillRef != want.skillRef {
			t.Errorf("resource[%d] = %+v, want name=%q tool=%q version=%q skill=%q", index, got, want.name, want.tool, want.version, want.skillRef)
		}
	}
	resources[0].Name = "mutated"
	if RuntimeResources()[0].Name != "mise" {
		t.Fatal("runtime declaration must not be mutable through returned slices")
	}
}

func TestVerifyRejectsIncompletePlan(t *testing.T) {
	identity, err := RuntimeIdentity()
	if err != nil {
		t.Fatal(err)
	}
	plan := RuntimePlan{
		Identity:     identity,
		PublicDir:    "/tmp/" + identity,
		PublicBinDir: "/tmp/" + identity,
	}
	if err := Verify(plan); err == nil {
		t.Fatal("Verify accepted a plan without every declared runtime")
	}
}

func TestPrepareCanonicalizesCachedHome(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink fixture requires Unix")
	}
	home := t.TempDir()
	physical, err := filepath.EvalSymlinks(home)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := RuntimeIdentity()
	if err != nil {
		t.Fatal(err)
	}
	public := filepath.Join(physical, ".mise-tools", "public", identity)
	if err := os.MkdirAll(public, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, resource := range RuntimeResources() {
		if err := os.WriteFile(filepath.Join(public, resource.Name), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(public, ".stella-shell-env"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(t.TempDir(), "home")
	if err := os.Symlink(physical, alias); err != nil {
		t.Fatal(err)
	}
	plan, err := Prepare(t.Context(), alias)
	if err != nil {
		t.Fatal(err)
	}
	if plan.PublicDir != public {
		t.Fatalf("public path = %q, want physical path %q", plan.PublicDir, public)
	}
}
