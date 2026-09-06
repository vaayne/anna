package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDiscoverAndWriteAssetsIsDeterministic(t *testing.T) {
	pluginsRoot := t.TempDir()
	owner := filepath.Join(pluginsRoot, "tools", "demo")
	if err := os.MkdirAll(filepath.Join(owner, "skills", "demo"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(owner, "assets.yaml"), []byte("assets:\n  - name: demo\n    source: skills/demo\n    logical_root: plugins/tool/demo/demo\n    owner_plugin_id: tool/demo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(owner, "skills", "demo", "SKILL.md"), []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	assets, err := discover(pluginsRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(assets) != 1 || assets[0].Name != "demo" || len(assets[0].Files) != 1 {
		t.Fatalf("discovered assets = %#v", assets)
	}
	output := filepath.Join(pluginsRoot, "builtin_assets_gen.go")
	if err := writeGenerated(output, assets); err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(want), `//go:embed "tools/demo/skills/demo/SKILL.md"`) {
		t.Fatalf("generated output omits explicit file embed: %s", want)
	}
	if err := writeGenerated(output, assets); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatal("repeated generation changed output")
	}
}

func TestDiscoverRejectsUnknownOwnerShape(t *testing.T) {
	pluginsRoot := t.TempDir()
	dir := filepath.Join(pluginsRoot, "guidance")
	if err := os.MkdirAll(filepath.Join(dir, "skills", "demo"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "assets.yaml"), []byte("assets:\n  - name: demo\n    source: skills/demo\n    logical_root: core/demo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "skills", "demo", "SKILL.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := discover(pluginsRoot); err == nil || !strings.Contains(err.Error(), "no owner") {
		t.Fatalf("discover error = %v, want missing owner", err)
	}
}
