package openairesponse

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"

	sdk "github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
	"github.com/vaayne/anna/ai/stream"
	"github.com/vaayne/anna/ai/types"
)

// Config configures the OpenAI Responses provider.
type Config struct {
	BaseURL string
	APIKey  string
}

// Provider implements stream.Provider for OpenAI Responses API.
type Provider struct {
	client sdk.Client
}

// New returns an OpenAI Responses provider.
func New(cfg Config) *Provider {
	opts := []option.RequestOption{
		option.WithMiddleware(stripLeadingNewlines),
	}
	if cfg.APIKey != "" {
		opts = append(opts, option.WithAPIKey(cfg.APIKey))
	}
	if cfg.BaseURL != "" {
		opts = append(opts, option.WithBaseURL(cfg.BaseURL))
	}
	return &Provider{client: sdk.NewClient(opts...)}
}

// stripLeadingNewlines removes leading newlines from SSE response bodies
// that some proxies prepend, which would cause the SDK's SSE parser to fail.
func stripLeadingNewlines(req *http.Request, next option.MiddlewareNext) (*http.Response, error) {
	resp, err := next(req)
	if err != nil || resp == nil || resp.Body == nil {
		return resp, err
	}
	if resp.Header.Get("Content-Type") != "text/event-stream" {
		return resp, nil
	}
	resp.Body = &leadingNewlineStripper{rc: resp.Body}
	return resp, nil
}

type leadingNewlineStripper struct {
	rc      io.ReadCloser
	trimmed bool
}

func (s *leadingNewlineStripper) Read(p []byte) (int, error) {
	if s.trimmed {
		return s.rc.Read(p)
	}
	n, err := s.rc.Read(p)
	if n > 0 {
		data := p[:n]
		trimmed := bytes.TrimLeft(data, "\n\r")
		removed := n - len(trimmed)
		if removed > 0 {
			copy(p, trimmed)
			n = len(trimmed)
		}
		if n > 0 {
			s.trimmed = true
		}
	}
	return n, err
}

func (s *leadingNewlineStripper) Close() error {
	return s.rc.Close()
}

// API returns provider key.
func (p *Provider) API() string { return "openai-response" }

// Stream starts an OpenAI Responses API stream.
func (p *Provider) Stream(model types.Model, ctx types.Context, opts types.StreamOptions) (stream.AssistantEventStream, error) {
	params := buildParams(model, ctx, opts)
	reqOpts := buildRequestOptions(opts)
	sdkStream := p.client.Responses.NewStreaming(context.Background(), params, reqOpts...)

	out := stream.NewChannelEventStream(32)
	go func() {
		defer out.Finish(nil)
		out.Emit(types.EventStart{})
		consumeStream(sdkStream, out)
		if err := sdkStream.Err(); err != nil {
			out.Emit(types.EventError{Err: err})
		}
	}()
	return out, nil
}

// StreamSimple delegates to Stream with mapped options.
func (p *Provider) StreamSimple(model types.Model, ctx types.Context, opts types.SimpleStreamOptions) (stream.AssistantEventStream, error) {
	return p.Stream(model, ctx, opts.StreamOptions)
}

// ListModels fetches available models from the OpenAI API.
func (p *Provider) ListModels(ctx context.Context) ([]types.Model, error) {
	page, err := p.client.Models.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("openai-response list models: %w", err)
	}

	var models []types.Model
	for _, m := range page.Data {
		models = append(models, types.Model{
			ID:       m.ID,
			Name:     m.ID,
			API:      "openai-response",
			Provider: "openai-response",
		})
	}
	return models, nil
}

var _ stream.ModelLister = (*Provider)(nil)

func buildRequestOptions(opts types.StreamOptions) []option.RequestOption {
	var reqOpts []option.RequestOption
	if opts.APIKey != "" {
		reqOpts = append(reqOpts, option.WithAPIKey(opts.APIKey))
	}
	if opts.BaseURL != "" {
		reqOpts = append(reqOpts, option.WithBaseURL(opts.BaseURL))
	}
	for k, v := range opts.Headers {
		reqOpts = append(reqOpts, option.WithHeader(k, v))
	}
	return reqOpts
}
