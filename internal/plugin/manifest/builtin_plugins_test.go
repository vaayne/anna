package manifest

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const testBuiltinPluginYAML = `id: tool/demo
kind: tool
name: demo
display_name: Demo
description: A demo plugin
enabled: true
binaries:
  - name: demo
    tool: demo
skills:
  - name: demo-skill
oauth_provider: demo
session_env:
  - env_var: DEMO_TOKEN
    source: oauth.access_token
`

var testReservedRuntimeNames = []string{"mise", "xberg", "fd", "rg"}

func TestGenerateBuiltinPluginsNestedMovePreservesBytesAndIdentity(t *testing.T) {
	root := t.TempDir()
	writeBuiltinPlugin(t, root, "tools/first/plugin.yaml", testBuiltinPluginYAML)
	first, err := GenerateBuiltinPlugins(root, testReservedRuntimeNames, []string{"demo"})
	if err != nil {
		t.Fatal(err)
	}
	firstBytes, err := renderBuiltinPlugins(first)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(root, "tools/first"), filepath.Join(root, "nested/second")); err != nil {
		t.Fatal(err)
	}
	second, err := GenerateBuiltinPlugins(root, testReservedRuntimeNames, []string{"demo"})
	if err != nil {
		t.Fatal(err)
	}
	secondBytes, err := renderBuiltinPlugins(second)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(firstBytes, secondBytes) || second.Plugins[0].ID != "tool/demo" {
		t.Fatalf("moving declaration changed generated catalog or identity: first=%s second=%s", firstBytes, secondBytes)
	}
}

func TestGenerateBuiltinPluginsEmptyDirectoryIsNotPlugin(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "empty", "deeper"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeBuiltinPlugin(t, root, "tools/demo/plugin.yaml", testBuiltinPluginYAML)
	plugins, err := GenerateBuiltinPlugins(root, testReservedRuntimeNames, []string{"demo"})
	if err != nil {
		t.Fatal(err)
	}
	if len(plugins.Plugins) != 1 || plugins.Plugins[0].ID != "tool/demo" {
		t.Fatalf("plugins = %#v, want only the declared plugin", plugins.Plugins)
	}
}

func TestGenerateBuiltinPluginsRejectsDuplicateIDsNamespacesAndResources(t *testing.T) {
	tests := []struct {
		name  string
		left  string
		right string
		want  string
	}{
		{name: "id", left: testBuiltinPluginYAML, right: testBuiltinPluginYAML, want: "duplicate builtin plugin ID"},
		{name: "namespace", left: testBuiltinPluginYAML, right: strings.Replace(strings.Replace(testBuiltinPluginYAML, "id: tool/demo", "id: channel/demo", 1), "kind: tool", "kind: channel", 1), want: "duplicate builtin plugin namespace"},
		{name: "resource", left: testBuiltinPluginYAML, right: strings.Replace(strings.Replace(testBuiltinPluginYAML, "id: tool/demo", "id: tool/other", 1), "name: demo\n", "name: other\n", 1), want: "duplicate builtin plugin resource"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			writeBuiltinPlugin(t, root, "a/plugin.yaml", test.left)
			writeBuiltinPlugin(t, root, "b/plugin.yaml", test.right)
			if _, err := GenerateBuiltinPlugins(root, testReservedRuntimeNames, []string{"demo"}); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("GenerateBuiltinPlugins() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestGenerateBuiltinPluginsRejectsSymlinkAndNonRegularManifest(t *testing.T) {
	if runtime.GOOS != "windows" {
		root := t.TempDir()
		writeBuiltinPlugin(t, root, "real/plugin.yaml", testBuiltinPluginYAML)
		if err := os.Symlink(filepath.Join(root, "real"), filepath.Join(root, "alias")); err != nil {
			t.Fatal(err)
		}
		if _, err := GenerateBuiltinPlugins(root, testReservedRuntimeNames, []string{"demo"}); err == nil || !strings.Contains(err.Error(), "symlink") {
			t.Fatalf("GenerateBuiltinPlugins() error = %v, want symlink rejection", err)
		}
	}
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "bad", "plugin.yaml"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := GenerateBuiltinPlugins(root, testReservedRuntimeNames, []string{"demo"}); err == nil || !strings.Contains(err.Error(), "unsupported type directory") {
		t.Fatalf("GenerateBuiltinPlugins() error = %v, want non-regular rejection", err)
	}
}

func TestWriteBuiltinPluginsRefreshesGeneratedBytes(t *testing.T) {
	root := t.TempDir()
	pluginsRoot := filepath.Join(root, "plugins")
	output := filepath.Join(root, "builtin_plugins_gen.go")
	manifest := filepath.Join(pluginsRoot, "tools", "demo", "plugin.yaml")
	writeBuiltinPlugin(t, pluginsRoot, "tools/demo/plugin.yaml", testBuiltinPluginYAML)
	if err := WriteBuiltinPlugins(pluginsRoot, output, testReservedRuntimeNames, []string{"demo"}); err != nil {
		t.Fatal(err)
	}
	first, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	updated := strings.Replace(testBuiltinPluginYAML, "description: A demo plugin", "description: Updated demo plugin", 1)
	if err := os.WriteFile(manifest, []byte(updated), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteBuiltinPlugins(pluginsRoot, output, testReservedRuntimeNames, []string{"demo"}); err != nil {
		t.Fatal(err)
	}
	second, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(first, second) || !bytes.Contains(second, []byte("Updated demo plugin")) {
		t.Fatalf("generated output did not refresh: %q", second)
	}
}

func TestGenerateBuiltinPluginsRejectsUnknownFieldsAndMultipleDocuments(t *testing.T) {
	for _, test := range []struct {
		name    string
		content string
		want    string
	}{
		{name: "unknown field", content: testBuiltinPluginYAML + "unexpected: true\n", want: "field unexpected not found"},
		{name: "remote skill source", content: strings.Replace(testBuiltinPluginYAML, "  - name: demo-skill\n", "  - name: demo-skill\n    repo: github:example/demo-skill\n", 1), want: "field repo not found"},
		{name: "multiple documents", content: testBuiltinPluginYAML + "---\nid: tool/other\n", want: "more than one YAML document"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			writeBuiltinPlugin(t, root, "plugin.yaml", test.content)
			if _, err := GenerateBuiltinPlugins(root, testReservedRuntimeNames, []string{"demo"}); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("GenerateBuiltinPlugins() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestGenerateBuiltinPluginsRejectsCoreIDsAndReleaseFields(t *testing.T) {
	for _, test := range []struct {
		name    string
		content string
		want    string
	}{
		{name: "reserved core ID", content: strings.Replace(testBuiltinPluginYAML, "id: tool/demo", "id: tool/rg", 1), want: "reserved core ID"},
		{name: "essential", content: testBuiltinPluginYAML + "essential: false\n", want: "essential or bundled_binaries"},
		{name: "empty bundled binaries", content: testBuiltinPluginYAML + "bundled_binaries: []\n", want: "essential or bundled_binaries"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			writeBuiltinPlugin(t, root, "plugin.yaml", test.content)
			if _, err := GenerateBuiltinPlugins(root, testReservedRuntimeNames, []string{"demo"}); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("GenerateBuiltinPlugins() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestGenerateBuiltinPluginsRejectsReservedCoreBinaryNames(t *testing.T) {
	for _, name := range testReservedRuntimeNames {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			content := strings.Replace(testBuiltinPluginYAML, "- name: demo\n    tool: demo", "- name: "+name+"\n    tool: demo", 1)
			writeBuiltinPlugin(t, root, "plugin.yaml", content)
			if _, err := GenerateBuiltinPlugins(root, testReservedRuntimeNames, []string{"demo"}); err == nil || !strings.Contains(err.Error(), "reserved core binary name") {
				t.Fatalf("GenerateBuiltinPlugins() error = %v, want reserved binary rejection", err)
			}
		})
	}
}

func writeBuiltinPlugin(t *testing.T, root, relative, content string) {
	t.Helper()
	filename := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filename, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
