package openai

import (
	sdk "github.com/openai/openai-go"
	"github.com/vaayne/anna/pkg/ai/types"
)

func buildParams(model types.Model, ctx types.Context, opts types.StreamOptions) sdk.ChatCompletionNewParams {
	messages := convertMessages(ctx)

	params := sdk.ChatCompletionNewParams{
		Model:    model.Name,
		Messages: messages,
	}

	if opts.Temperature != nil {
		params.Temperature = sdk.Float(*opts.Temperature)
	}
	if opts.MaxTokens != nil {
		params.MaxCompletionTokens = sdk.Int(int64(*opts.MaxTokens))
	}

	return params
}
