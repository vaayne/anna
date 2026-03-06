package anthropic

import (
	sdk "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/packages/param"
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

	if len(ctx.Tools) > 0 {
		params.Tools = convertTools(ctx.Tools)
	}

	return params
}

func convertTools(tools []types.ToolDefinition) []sdk.ToolUnionParam {
	out := make([]sdk.ToolUnionParam, 0, len(tools))
	for _, t := range tools {
		schema := sdk.ToolInputSchemaParam{
			Properties: t.InputSchema["properties"],
		}
		if req, ok := t.InputSchema["required"].([]string); ok {
			schema.Required = req
		}
		tp := sdk.ToolParam{
			Name:        t.Name,
			InputSchema: schema,
			Description: param.NewOpt(t.Description),
		}
		out = append(out, sdk.ToolUnionParam{OfTool: &tp})
	}
	return out
}
