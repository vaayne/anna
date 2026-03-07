package tool

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	aitypes "github.com/vaayne/anna/ai/types"

	readability "codeberg.org/readeck/go-readability/v2"
	md "github.com/JohannesKaufmann/html-to-markdown"
)

// WebFetchTool fetches a URL, extracts readable content, and converts it to markdown.
type WebFetchTool struct {
	client *http.Client
}

func NewWebFetchTool() *WebFetchTool {
	return &WebFetchTool{
		client: &http.Client{Timeout: 30 * time.Second},
	}
}

func (t *WebFetchTool) Definition() aitypes.ToolDefinition {
	return aitypes.ToolDefinition{
		Name:        "webfetch",
		Description: "Fetch a web page and return its main content as markdown. Useful for reading articles, documentation, and other web pages.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"url": map[string]any{
					"type":        "string",
					"description": "The URL to fetch (http or https).",
				},
			},
			"required": []string{"url"},
		},
	}
}

func (t *WebFetchTool) Execute(ctx context.Context, args map[string]any) (string, error) {
	rawURL, ok := args["url"].(string)
	if !ok || rawURL == "" {
		return "", fmt.Errorf("webfetch: url is required")
	}

	parsed, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("webfetch: invalid url: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Errorf("webfetch: unsupported scheme %q (only http/https)", parsed.Scheme)
	}

	// Fetch HTML
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", fmt.Errorf("webfetch: %w", err)
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; Anna/1.0)")

	resp, err := t.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("webfetch: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("webfetch: HTTP %d %s", resp.StatusCode, resp.Status)
	}

	// Extract readable content
	article, err := readability.FromReader(resp.Body, parsed)
	if err != nil {
		return "", fmt.Errorf("webfetch: readability parse failed: %w", err)
	}

	// Render clean HTML from readability
	var htmlBuf strings.Builder
	if err := article.RenderHTML(&htmlBuf); err != nil {
		return "", fmt.Errorf("webfetch: render html failed: %w", err)
	}

	// Convert HTML to markdown
	converter := md.NewConverter(parsed.Host, true, nil)
	markdown, err := converter.ConvertString(htmlBuf.String())
	if err != nil {
		return "", fmt.Errorf("webfetch: html-to-markdown failed: %w", err)
	}

	// Build result with metadata
	var result strings.Builder
	if title := article.Title(); title != "" {
		fmt.Fprintf(&result, "# %s\n\n", title)
	}
	if author := article.Byline(); author != "" {
		fmt.Fprintf(&result, "**Author:** %s\n\n", author)
	}
	result.WriteString(markdown)

	tr := TruncateTail(result.String())
	return tr.Content, nil
}
