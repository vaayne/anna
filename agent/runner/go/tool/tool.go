package tool

import (
	"context"
	"fmt"

	aitypes "github.com/vaayne/anna/pkg/ai/types"
)

// Tool is a built-in tool that can be executed by the Go runner.
type Tool interface {
	Definition() aitypes.ToolDefinition
	Execute(ctx context.Context, args map[string]any) (string, error)
}

// Registry holds named tools and provides lookup + definitions.
type Registry struct {
	tools map[string]Tool
}

// NewRegistry creates a registry with the default built-in tools.
func NewRegistry(workDir string) *Registry {
	r := &Registry{tools: make(map[string]Tool)}
	r.register(&ReadTool{})
	r.register(&BashTool{workDir: workDir})
	r.register(&EditTool{})
	r.register(&WriteTool{})
	return r
}

func (r *Registry) register(t Tool) {
	r.tools[t.Definition().Name] = t
}

// Definitions returns all tool definitions for passing to the LLM.
func (r *Registry) Definitions() []aitypes.ToolDefinition {
	defs := make([]aitypes.ToolDefinition, 0, len(r.tools))
	for _, t := range r.tools {
		defs = append(defs, t.Definition())
	}
	return defs
}

// Execute runs the named tool with given arguments.
func (r *Registry) Execute(ctx context.Context, name string, args map[string]any) (string, error) {
	t, ok := r.tools[name]
	if !ok {
		return "", fmt.Errorf("unknown tool: %s", name)
	}
	return t.Execute(ctx, args)
}
