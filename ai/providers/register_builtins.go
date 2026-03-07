package providers

import (
	"github.com/vaayne/anna/ai/providers/anthropic"
	"github.com/vaayne/anna/ai/providers/openai"
	openairesponse "github.com/vaayne/anna/ai/providers/openai-response"
	"github.com/vaayne/anna/ai/registry"
)

// RegisterBuiltins registers first-party providers in a registry.
func RegisterBuiltins(r *registry.Registry) {
	r.Register(openai.New(openai.Config{}))
	r.Register(openairesponse.New(openairesponse.Config{}))
	r.Register(anthropic.New(anthropic.Config{}))
}
