package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/vaayne/anna/pkg/ai/stream"
	"github.com/vaayne/anna/pkg/ai/types"
)

// HTTPDoer abstracts outbound requests.
type HTTPDoer interface {
	Do(*http.Request) (*http.Response, error)
}

// Config configures the OpenAI provider.
type Config struct {
	HTTPClient HTTPDoer
	BaseURL    string
}

// Provider implements stream.Provider for OpenAI completions.
type Provider struct {
	httpClient HTTPDoer
	baseURL    string
}

// New returns an OpenAI provider.
func New(cfg Config) *Provider {
	client := cfg.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	baseURL := cfg.BaseURL
	if baseURL == "" {
		baseURL = "https://api.openai.com/v1/completions"
	}
	return &Provider{httpClient: client, baseURL: strings.TrimRight(baseURL, "/")}
}

// API returns provider key.
func (p *Provider) API() string { return "openai" }

// Stream starts OpenAI completion stream.
func (p *Provider) Stream(model types.Model, ctx types.Context, opts types.StreamOptions) (stream.AssistantEventStream, error) {
	prompt := ConvertMessagesToPrompt(ctx)
	body := mapOptions(model, prompt, opts, true)
	return p.streamRequest(opts, body)
}

// StreamSimple delegates to Stream with mapped options.
func (p *Provider) StreamSimple(model types.Model, ctx types.Context, opts types.SimpleStreamOptions) (stream.AssistantEventStream, error) {
	return p.Stream(model, ctx, opts.StreamOptions)
}

func (p *Provider) streamRequest(opts types.StreamOptions, body RequestOptions) (stream.AssistantEventStream, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, p.baseURL, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if opts.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+opts.APIKey)
	}

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		defer resp.Body.Close()
		return nil, fmt.Errorf("openai request failed: %s", resp.Status)
	}

	out := stream.NewChannelEventStream(32)
	go func() {
		defer resp.Body.Close()
		defer out.Finish(nil)
		out.Emit(types.EventStart{})
		if parseErr := parseSSEReader(resp.Body, out); parseErr != nil {
			out.Emit(types.EventError{Err: parseErr})
			out.Finish(parseErr)
		}
	}()

	return out, nil
}
