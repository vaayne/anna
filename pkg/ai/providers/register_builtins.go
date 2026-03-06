package providers

import (
	"github.com/vaayne/anna/pkg/ai/providers/anthropic"
	"github.com/vaayne/anna/pkg/ai/providers/openai"
	"github.com/vaayne/anna/pkg/ai/registry"
)

// RegisterBuiltins registers first-party providers in a registry.
func RegisterBuiltins(r *registry.Registry) {
	r.Register(openai.New(openai.Config{}))
	r.Register(anthropic.New(anthropic.Config{}))
}
