package anthropic

import (
	sdk "github.com/anthropics/anthropic-sdk-go"
	"github.com/vaayne/anna/pkg/ai/types"
)

func buildParams(model types.Model, ctx types.Context, opts types.StreamOptions) sdk.MessageNewParams {
	maxTokens := int64(1024)
	if opts.MaxTokens != nil {
		maxTokens = int64(*opts.MaxTokens)
	}

	params := sdk.MessageNewParams{
		Model:     sdk.Model(model.Name),
		MaxTokens: maxTokens,
		Messages:  convertMessages(ctx),
	}

	if ctx.System != "" {
		params.System = []sdk.TextBlockParam{{Text: ctx.System}}
	}

	if opts.Temperature != nil {
		params.Temperature = sdk.Float(*opts.Temperature)
	}

	return params
}
