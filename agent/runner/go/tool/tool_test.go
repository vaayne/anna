package tool

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestRegistryDefinitions(t *testing.T) {
	reg := NewRegistry("")
	defs := reg.Definitions()
	if len(defs) != 4 {
		t.Fatalf("expected 4 tool definitions, got %d", len(defs))
	}

	names := map[string]bool{}
	for _, d := range defs {
		names[d.Name] = true
	}
	for _, name := range []string{"read", "bash", "edit", "write"} {
		if !names[name] {
			t.Errorf("missing tool definition: %s", name)
		}
	}
}

func TestRegistryExecuteUnknown(t *testing.T) {
	reg := NewRegistry("")
	_, err := reg.Execute(context.Background(), "nonexistent", nil)
	if err == nil {
		t.Fatal("expected error for unknown tool")
	}
}

func TestReadTool(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.txt")
	os.WriteFile(path, []byte("hello world"), 0o644)

	tool := &ReadTool{}
	result, err := tool.Execute(context.Background(), map[string]any{"file_path": path})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "hello world" {
		t.Errorf("result = %q, want %q", result, "hello world")
	}
}

func TestReadToolMissingFile(t *testing.T) {
	tool := &ReadTool{}
	_, err := tool.Execute(context.Background(), map[string]any{"file_path": "/nonexistent/file.txt"})
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestReadToolMissingArg(t *testing.T) {
	tool := &ReadTool{}
	_, err := tool.Execute(context.Background(), map[string]any{})
	if err == nil {
		t.Fatal("expected error for missing file_path")
	}
}

func TestBashTool(t *testing.T) {
	tool := &BashTool{}
	result, err := tool.Execute(context.Background(), map[string]any{"command": "echo hello"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "hello\n" {
		t.Errorf("result = %q, want %q", result, "hello\n")
	}
}

func TestBashToolWorkDir(t *testing.T) {
	dir := t.TempDir()
	tool := &BashTool{workDir: dir}
	result, err := tool.Execute(context.Background(), map[string]any{"command": "pwd -P"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Resolve symlinks (macOS /tmp → /private/tmp).
	resolved, _ := filepath.EvalSymlinks(dir)
	if result != resolved+"\n" {
		t.Errorf("result = %q, want %q", result, resolved+"\n")
	}
}

func TestBashToolFailure(t *testing.T) {
	tool := &BashTool{}
	_, err := tool.Execute(context.Background(), map[string]any{"command": "exit 42"})
	if err == nil {
		t.Fatal("expected error for non-zero exit")
	}
}

func TestBashToolMissingArg(t *testing.T) {
	tool := &BashTool{}
	_, err := tool.Execute(context.Background(), map[string]any{})
	if err == nil {
		t.Fatal("expected error for missing command")
	}
}

func TestEditTool(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.txt")
	os.WriteFile(path, []byte("hello world"), 0o644)

	tool := &EditTool{}
	result, err := tool.Execute(context.Background(), map[string]any{
		"file_path":  path,
		"old_string": "world",
		"new_string": "Go",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == "" {
		t.Error("expected non-empty result")
	}

	data, _ := os.ReadFile(path)
	if string(data) != "hello Go" {
		t.Errorf("file content = %q, want %q", string(data), "hello Go")
	}
}

func TestEditToolNotFound(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.txt")
	os.WriteFile(path, []byte("hello world"), 0o644)

	tool := &EditTool{}
	_, err := tool.Execute(context.Background(), map[string]any{
		"file_path":  path,
		"old_string": "missing",
		"new_string": "replacement",
	})
	if err == nil {
		t.Fatal("expected error when old_string not found")
	}
}

func TestEditToolAmbiguous(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.txt")
	os.WriteFile(path, []byte("aa bb aa"), 0o644)

	tool := &EditTool{}
	_, err := tool.Execute(context.Background(), map[string]any{
		"file_path":  path,
		"old_string": "aa",
		"new_string": "cc",
	})
	if err == nil {
		t.Fatal("expected error when old_string matches multiple times")
	}
}

func TestEditToolMissingArgs(t *testing.T) {
	tool := &EditTool{}
	_, err := tool.Execute(context.Background(), map[string]any{})
	if err == nil {
		t.Fatal("expected error for missing args")
	}
}

func TestWriteTool(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "test.txt")

	tool := &WriteTool{}
	result, err := tool.Execute(context.Background(), map[string]any{
		"file_path": path,
		"content":   "new content",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == "" {
		t.Error("expected non-empty result")
	}

	data, _ := os.ReadFile(path)
	if string(data) != "new content" {
		t.Errorf("file content = %q, want %q", string(data), "new content")
	}
}

func TestWriteToolOverwrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.txt")
	os.WriteFile(path, []byte("old"), 0o644)

	tool := &WriteTool{}
	_, err := tool.Execute(context.Background(), map[string]any{
		"file_path": path,
		"content":   "new",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	data, _ := os.ReadFile(path)
	if string(data) != "new" {
		t.Errorf("file content = %q, want %q", string(data), "new")
	}
}

func TestWriteToolMissingArg(t *testing.T) {
	tool := &WriteTool{}
	_, err := tool.Execute(context.Background(), map[string]any{})
	if err == nil {
		t.Fatal("expected error for missing file_path")
	}
}
