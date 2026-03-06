package openairesponse

import (
	"github.com/openai/openai-go/packages/param"
	"github.com/openai/openai-go/responses"
	"github.com/openai/openai-go/shared"
	"github.com/vaayne/anna/pkg/ai/types"
)

func buildParams(model types.Model, ctx types.Context, opts types.StreamOptions) responses.ResponseNewParams {
	input := convertMessages(ctx)

	params := responses.ResponseNewParams{
		Model: shared.ResponsesModel(model.Name),
		Input: responses.ResponseNewParamsInputUnion{
			OfInputItemList: input,
		},
	}

	if ctx.System != "" {
		params.Instructions = param.NewOpt(ctx.System)
	}

	if opts.Temperature != nil {
		params.Temperature = param.NewOpt(*opts.Temperature)
	}
	if opts.MaxTokens != nil {
		params.MaxOutputTokens = param.NewOpt(int64(*opts.MaxTokens))
	}

	return params
}
