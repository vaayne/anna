package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	readability "codeberg.org/readeck/go-readability/v2"
	md "github.com/JohannesKaufmann/html-to-markdown"
	aitypes "github.com/vaayne/anna/ai/types"
)

const (
	formatMarkdown = "markdown"
	formatHTML     = "html"
	formatText     = "text"
	formatJSON     = "json"
)

// WebFetchTool fetches a URL, extracts readable content, and returns it in the requested format.
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
		Description: "Fetch a web page and return its main content. Supports multiple output formats: markdown (default), html, text, and json.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"url": map[string]any{
					"type":        "string",
					"description": "The URL to fetch (http or https).",
				},
				"format": map[string]any{
					"type":        "string",
					"description": "Output format: markdown (default), html, text, or json.",
					"enum":        []string{"markdown", "html", "text", "json"},
					"default":     "markdown",
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

	format, _ := args["format"].(string)
	if format == "" {
		format = formatMarkdown
	}

	parsed, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("webfetch: invalid url: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Errorf("webfetch: unsupported scheme %q (only http/https)", parsed.Scheme)
	}

	article, err := t.fetch(ctx, rawURL, parsed)
	if err != nil {
		return "", err
	}

	content, err := t.render(article, parsed, format)
	if err != nil {
		return "", err
	}

	tr := TruncateTail(content)
	return tr.Content, nil
}

func (t *WebFetchTool) fetch(ctx context.Context, rawURL string, parsed *url.URL) (readability.Article, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return readability.Article{}, fmt.Errorf("webfetch: %w", err)
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; Anna/1.0)")

	resp, err := t.client.Do(req)
	if err != nil {
		return readability.Article{}, fmt.Errorf("webfetch: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		return readability.Article{}, fmt.Errorf("webfetch: HTTP %d %s", resp.StatusCode, resp.Status)
	}

	article, err := readability.FromReader(resp.Body, parsed)
	if err != nil {
		return readability.Article{}, fmt.Errorf("webfetch: readability parse failed: %w", err)
	}
	return article, nil
}

func (t *WebFetchTool) render(article readability.Article, parsed *url.URL, format string) (string, error) {
	switch format {
	case formatHTML:
		return t.renderHTML(article)
	case formatText:
		return t.renderText(article)
	case formatJSON:
		return t.renderJSON(article, parsed)
	default:
		return t.renderMarkdown(article, parsed)
	}
}

func (t *WebFetchTool) renderMarkdown(article readability.Article, parsed *url.URL) (string, error) {
	var htmlBuf strings.Builder
	if err := article.RenderHTML(&htmlBuf); err != nil {
		return "", fmt.Errorf("webfetch: render html failed: %w", err)
	}

	converter := md.NewConverter(parsed.Host, true, nil)
	markdown, err := converter.ConvertString(htmlBuf.String())
	if err != nil {
		return "", fmt.Errorf("webfetch: html-to-markdown failed: %w", err)
	}

	var result strings.Builder
	t.writeMetadata(&result, article)
	result.WriteString(markdown)
	return result.String(), nil
}

func (t *WebFetchTool) renderHTML(article readability.Article) (string, error) {
	var htmlBuf strings.Builder
	if err := article.RenderHTML(&htmlBuf); err != nil {
		return "", fmt.Errorf("webfetch: render html failed: %w", err)
	}
	return htmlBuf.String(), nil
}

func (t *WebFetchTool) renderText(article readability.Article) (string, error) {
	var textBuf strings.Builder
	if err := article.RenderText(&textBuf); err != nil {
		return "", fmt.Errorf("webfetch: render text failed: %w", err)
	}

	var result strings.Builder
	if title := article.Title(); title != "" {
		fmt.Fprintf(&result, "%s\n\n", title)
	}
	result.WriteString(textBuf.String())
	return result.String(), nil
}

type webFetchJSON struct {
	Title       string `json:"title,omitempty"`
	Author      string `json:"author,omitempty"`
	Description string `json:"description,omitempty"`
	SiteName    string `json:"site_name,omitempty"`
	URL         string `json:"url"`
	Content     string `json:"content"`
}

func (t *WebFetchTool) renderJSON(article readability.Article, parsed *url.URL) (string, error) {
	var textBuf strings.Builder
	if err := article.RenderText(&textBuf); err != nil {
		return "", fmt.Errorf("webfetch: render text failed: %w", err)
	}

	data := webFetchJSON{
		Title:       article.Title(),
		Author:      article.Byline(),
		Description: article.Excerpt(),
		SiteName:    article.SiteName(),
		URL:         parsed.String(),
		Content:     textBuf.String(),
	}

	b, err := json.Marshal(data)
	if err != nil {
		return "", fmt.Errorf("webfetch: json marshal failed: %w", err)
	}
	return string(b), nil
}

func (t *WebFetchTool) writeMetadata(w *strings.Builder, article readability.Article) {
	if title := article.Title(); title != "" {
		fmt.Fprintf(w, "# %s\n\n", title)
	}
	if author := article.Byline(); author != "" {
		fmt.Fprintf(w, "**Author:** %s\n\n", author)
	}
}
