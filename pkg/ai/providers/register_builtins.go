package providers

import (
	"github.com/vaayne/anna/pkg/ai/providers/anthropic"
	"github.com/vaayne/anna/pkg/ai/providers/openai"
	openairesponse "github.com/vaayne/anna/pkg/ai/providers/openai-response"
	"github.com/vaayne/anna/pkg/ai/registry"
)

// RegisterBuiltins registers first-party providers in a registry.
func RegisterBuiltins(r *registry.Registry) {
	r.Register(openai.New(openai.Config{}))
	r.Register(openairesponse.New(openairesponse.Config{}))
	r.Register(anthropic.New(anthropic.Config{}))
}
