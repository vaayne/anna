package recally

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/CherryHQ/stella/internal/authz"
	pkgsandbox "github.com/CherryHQ/stella/pkg/sandbox"
	"github.com/CherryHQ/stella/pkg/tools"
)

const (
	defaultToolPageSize            = 20
	maxToolPageSize                = 500
	maxRecallyContentFileSize      = 1 << 20
	maxRecallyContentFileTotalSize = 4 << 20
	// minCapturedBodyChars separates an article from a paywall stub or a
	// navigation-only page; below it a save fails rather than storing a body
	// the user would mistake for the article.
	minCapturedBodyChars = 100
	// previewEdgeChars is echoed from each end of a saved body so the caller
	// can judge what was captured without the body entering its context.
	previewEdgeChars = 100
)

// actionDescriptions is the model-facing description per generated tool. A
// split tool's schema is exact, so each description only has to say what the
// call does — it no longer has to disambiguate which fields belong to it.
var actionDescriptions = map[string]string{
	"article_save": "Save articles to the user's Recally library, as a batch even for one URL. Read a new page with the web skill first and pass its markdown file as content_path; a bare URL with no body is rejected. Upserts on canonical URL; omitting the body on a saved article updates metadata only.",
	"article_get":  "Read one saved Recally article by its article id, including the body. Long bodies are truncated for token safety.",
	"article_list": "Browse or free-text search the user's saved Recally articles. Search covers title, summary, tags, and author, not the body. Keep page sizes small.",
	"feed_add":     "Subscribe to an RSS, Twitter/X, or website feed. The server sniffs the kind from the URL unless kind forces it.",
	"feed_list":    "List the user's subscribed feeds, or look one up by exact URL.",
	"feed_poll":    "Poll feeds server-side for new entries. Omit id to poll every due feed.",
	"feed_remove":  "Remove one feed subscription by feed id.",
	"entry_list":   "List pending entries for one feed, the queue the RSS workflow processes.",
	"entry_add":    "Record a discovered feed entry, deduplicated on its per-source guid.",
	"entry_update": "Move one feed entry to its next status. article_id names the article the entry became and is required when status=saved.",
	"digest_get":   "Read the user's reading digest for today.",
	"digest_save":  "Store a narrative reading digest for a date, defaulting to today.",
}

// Tool is one generated Recally action. The tool name carries the action, so
// the provider validates arguments against an exact schema before dispatch.
type Tool struct {
	spec    ActionTool
	svc     *Service
	runtime pkgsandbox.Session
}

// NewTool builds one Recally action tool for spec/registration use.
func NewTool(svc *Service, spec ActionTool) *Tool { return &Tool{spec: spec, svc: svc} }

// NewRuntimeTool builds one Recally action tool bound to a sandbox session.
func NewRuntimeTool(svc *Service, runtime pkgsandbox.Session, spec ActionTool) *Tool {
	return &Tool{spec: spec, svc: svc, runtime: runtime}
}

func (t *Tool) Definition() tools.Definition {
	return t.spec.Definition(actionDescriptions[t.spec.Action] + " The library is shared across this user's agents.")
}

func (t *Tool) Execute(ctx context.Context, args map[string]any) (string, error) {
	if t == nil || t.svc == nil {
		return "", fmt.Errorf("recally service is unavailable — try again later")
	}
	ident, err := authz.ToolIdentity(ctx, t.spec.Name)
	if err != nil {
		return "", err
	}
	// The runtime context identity is the trusted adapter: a delegated agent turn
	// becomes an AgentActor that acts as its delegating user (recally is user-owned
	// and shared across the user's agents). Model-supplied arguments never form
	// identity.
	authority, err := ident.ToAuthority()
	if err != nil {
		return "", authz.MapToolError(t.spec.Name, t.listSibling(), err)
	}
	out, err := Dispatch(ctx, recallyHandler{svc: t.svc, authority: authority, runtime: t.runtime}, t.spec.Action, args)
	if err != nil {
		return "", authz.MapToolError(t.spec.Name, t.listSibling(), err)
	}
	return tools.MarshalResult(out)
}

// listTools maps each resource to the tool that lists it, so a not-found error
// points the model at the list for the resource it actually asked about rather
// than at the family's first list action.
var listTools = map[string]string{
	"article": "recally__article_list",
	"feed":    "recally__feed_list",
	"entry":   "recally__entry_list",
}

func (t *Tool) listSibling() string {
	resource, _, _ := strings.Cut(t.spec.Action, "_")
	return listTools[resource]
}

type recallyHandler struct {
	svc       *Service
	authority authz.Authority
	runtime   pkgsandbox.Session
}

func (h recallyHandler) access() (*Access, error) {
	return h.svc.Access(h.authority)
}

func (h recallyHandler) ArticleSave(ctx context.Context, in ArticleSaveInput) (any, error) {
	acc, err := h.access()
	if err != nil {
		return nil, err
	}
	requests := make([]SaveRequest, 0, len(in.Items))
	totalFileBytes := 0
	for index, item := range in.Items {
		if item.Content != "" && item.ContentPath != "" {
			return nil, fmt.Errorf("articles[%d] must use only one of content or content_path", index)
		}
		if item.ContentPath != "" {
			content, err := h.readContentFile(ctx, item.ContentPath)
			if err != nil {
				return nil, fmt.Errorf("articles[%d] content_path: %w", index, err)
			}
			totalFileBytes += len(content)
			if totalFileBytes > maxRecallyContentFileTotalSize {
				return nil, fmt.Errorf("referenced article content exceeds %d bytes total", maxRecallyContentFileTotalSize)
			}
			item.Content = content
		}
		requests = append(requests, recallySaveRequest(item))
	}

	results := make([]recallySaveResult, 0, len(requests))
	for index, request := range requests {
		result := recallySaveResult{URL: in.Items[index].Url}
		if err := h.checkBody(ctx, acc, &request); err != nil {
			result.Status = "error"
			result.Error = err.Error()
			results = append(results, result)
			continue
		}
		saved, err := acc.Save(ctx, request)
		if err != nil {
			result.Status = "error"
			result.Error = err.Error()
			results = append(results, result)
			continue
		}
		result.ID = saved.Article.ID
		result.ContentChars = utf8.RuneCountInString(request.Content)
		result.ContentPreview = bodyPreview(request.Content)
		if saved.Created {
			result.Status = "created"
		} else {
			result.Status = "updated"
		}
		results = append(results, result)
	}
	return map[string]any{"results": results}, nil
}

// checkBody enforces the capture contract before any write. The server never
// fetches a page: a save with no body is a metadata-only update and is only
// valid for a URL already in the library, and any body — inline or from
// content_path — must clear the char floor, because a shorter one reads as a
// stub or paywall page wherever it came from.
func (h recallyHandler) checkBody(ctx context.Context, acc *Access, request *SaveRequest) error {
	if request.URL == "" {
		return errors.New("url is required")
	}
	if request.Content == "" {
		canonical := request.CanonicalURL
		if canonical == "" {
			canonical = NormalizeURL(request.URL)
		}
		if _, err := acc.GetArticleByCanonicalURL(ctx, canonical); err != nil {
			if errors.Is(err, authz.ErrNotFound) {
				return errors.New("no body given and the URL is not saved yet — read the page with the web skill and pass its markdown file as content_path")
			}
			return err
		}
		return nil
	}
	if chars := utf8.RuneCountInString(strings.TrimSpace(request.Content)); chars < minCapturedBodyChars {
		return fmt.Errorf("thin extraction: the body yielded %d characters, which reads as a stub, paywall, or navigation page rather than an article", chars)
	}
	return nil
}

// bodyPreview returns the head and tail of a body. A summary page and a real
// article look nothing alike at the edges, so this is enough to judge a
// capture without the body itself.
func bodyPreview(body string) string {
	text := []rune(strings.Join(strings.Fields(body), " "))
	if len(text) <= previewEdgeChars*2 {
		return string(text)
	}
	return string(text[:previewEdgeChars]) + " […] " + string(text[len(text)-previewEdgeChars:])
}

func (h recallyHandler) readContentFile(ctx context.Context, filePath string) (string, error) {
	if h.runtime == nil {
		return "", fmt.Errorf("sandbox file access is unavailable")
	}
	view, err := pkgsandbox.SelectFileView(ctx, h.runtime)
	if err != nil {
		return "", err
	}
	resolved := filePath
	if strings.HasPrefix(resolved, "$") {
		resolved, err = pkgsandbox.ExpandPathVariables(resolved, view.Policy.Env)
		if err != nil {
			return "", err
		}
	}
	resolved, err = tools.ResolvePath(view.WorkingDir, resolved)
	if err != nil {
		return "", err
	}
	info, err := view.Files.Stat(resolved)
	if err != nil {
		return "", err
	}
	if info.IsDir {
		return "", fmt.Errorf("path is a directory")
	}
	if info.Size > maxRecallyContentFileSize {
		return "", fmt.Errorf("file is %d bytes, over the %d-byte limit", info.Size, maxRecallyContentFileSize)
	}
	content, err := view.Files.ReadFile(resolved)
	if err != nil {
		return "", err
	}
	if len(content) > maxRecallyContentFileSize {
		return "", fmt.Errorf("file is %d bytes, over the %d-byte limit", len(content), maxRecallyContentFileSize)
	}
	if !utf8.Valid(content) {
		return "", fmt.Errorf("file is not valid UTF-8")
	}
	return string(content), nil
}

func (h recallyHandler) ArticleList(ctx context.Context, in ArticleListInput) (any, error) {
	if in.Q != "" && in.PageToken != "" {
		return nil, fmt.Errorf("page_token is not supported with q")
	}
	limit, offset, err := tools.ParsePage(in.PageSize, in.PageToken, defaultToolPageSize, maxToolPageSize)
	if err != nil {
		return nil, fmt.Errorf("invalid pagination — use page_size between 1 and %d and pass next_page_token unchanged", maxToolPageSize)
	}
	acc, err := h.access()
	if err != nil {
		return nil, err
	}
	if in.CanonicalUrl != "" {
		article, err := acc.GetArticleByCanonicalURL(ctx, in.CanonicalUrl)
		if err != nil {
			if errors.Is(err, authz.ErrNotFound) {
				return listResponse[recallyArticleListItem]{Items: []recallyArticleListItem{}, HasMore: false}, nil
			}
			return nil, err
		}
		return listResponse[recallyArticleListItem]{Items: []recallyArticleListItem{recallyArticleListSummary(*article)}, HasMore: false}, nil
	}
	var articles []Article
	if in.Q != "" {
		articles, err = acc.SearchArticles(ctx, in.Q, limit)
	} else {
		articles, err = acc.ListArticles(ctx, ArticleFilter{Status: ArticleStatus(in.Status), SourceType: SourceType(in.SourceType), Starred: in.Starred, Limit: limit + 1, Offset: offset})
	}
	if err != nil {
		return nil, err
	}
	page, next := tools.PageRows(articles, limit, offset)
	items := make([]recallyArticleListItem, 0, len(page))
	for _, article := range page {
		items = append(items, recallyArticleListSummary(article))
	}
	return listResponse[recallyArticleListItem]{Items: items, HasMore: next != "", NextPageToken: next}, nil
}

func (h recallyHandler) ArticleGet(ctx context.Context, in ArticleGetInput) (any, error) {
	acc, err := h.access()
	if err != nil {
		return nil, err
	}
	article, err := acc.GetArticle(ctx, in.Id)
	if err != nil {
		return nil, err
	}
	content, err := acc.ReadArticleBody(ctx, article)
	if err != nil {
		return nil, err
	}
	content, truncated := tools.TruncateText(content, 50*1024)
	return recallyArticleDetail{Article: recallyArticleListSummary(*article), Content: content, Truncated: truncated, Note: recallyTruncationNote(truncated)}, nil
}

func (h recallyHandler) FeedAdd(ctx context.Context, in FeedAddInput) (any, error) {
	acc, err := h.access()
	if err != nil {
		return nil, err
	}
	feed, err := acc.CreateFeed(ctx, in.Url, FeedKind(in.Kind), in.Title, nil)
	if err != nil {
		return nil, err
	}
	return recallyFeedSummary(*feed), nil
}

func (h recallyHandler) FeedList(ctx context.Context, in FeedListInput) (any, error) {
	limit, offset, err := tools.ParsePage(in.PageSize, in.PageToken, defaultToolPageSize, maxToolPageSize)
	if err != nil {
		return nil, fmt.Errorf("invalid pagination — use page_size between 1 and %d and pass next_page_token unchanged", maxToolPageSize)
	}
	acc, err := h.access()
	if err != nil {
		return nil, err
	}
	if in.Url != "" {
		feed, err := acc.GetFeedByURL(ctx, in.Url)
		if err != nil {
			if errors.Is(err, authz.ErrNotFound) {
				return listResponse[recallyFeedItem]{Items: []recallyFeedItem{}, HasMore: false}, nil
			}
			return nil, err
		}
		return listResponse[recallyFeedItem]{Items: []recallyFeedItem{recallyFeedSummary(*feed)}, HasMore: false}, nil
	}
	feeds, err := acc.ListFeeds(ctx, limit+1, offset)
	if err != nil {
		return nil, err
	}
	page, next := tools.PageRows(feeds, limit, offset)
	items := make([]recallyFeedItem, 0, len(page))
	for _, feed := range page {
		items = append(items, recallyFeedSummary(feed))
	}
	return listResponse[recallyFeedItem]{Items: items, HasMore: next != "", NextPageToken: next}, nil
}

func (h recallyHandler) FeedPoll(ctx context.Context, in FeedPollInput) (any, error) {
	limit := in.Limit
	if limit == 0 {
		limit = defaultToolPageSize
	}
	if limit < 1 || limit > 500 {
		return nil, fmt.Errorf("invalid limit — use limit between 1 and 500")
	}
	acc, err := h.access()
	if err != nil {
		return nil, err
	}
	var results []FeedPollResult
	if in.Id == "" {
		results, err = acc.PollFeeds(ctx, limit)
	} else {
		var result FeedPollResult
		result, err = acc.PollFeed(ctx, in.Id, limit)
		results = []FeedPollResult{result}
	}
	if err != nil {
		return nil, err
	}
	items := make([]recallyFeedPollResult, 0, len(results))
	for _, result := range results {
		entries := make([]recallyFeedEntryItem, 0, len(result.NewEntries))
		for _, entry := range result.NewEntries {
			entries = append(entries, recallyFeedEntrySummary(entry))
		}
		out := recallyFeedPollResult{Feed: recallyFeedSummary(result.Feed), NewEntries: entries}
		if len(result.Errors) > 0 {
			out.Error = result.Errors[0]
		}
		items = append(items, out)
	}
	return map[string]any{"results": items}, nil
}

func (h recallyHandler) FeedRemove(ctx context.Context, in FeedRemoveInput) (any, error) {
	acc, err := h.access()
	if err != nil {
		return nil, err
	}
	if err := acc.DeleteFeed(ctx, in.Id); err != nil {
		return nil, err
	}
	return map[string]any{"id": in.Id, "status": "removed"}, nil
}

func (h recallyHandler) EntryList(ctx context.Context, in EntryListInput) (any, error) {
	limit, offset, err := tools.ParsePage(in.PageSize, in.PageToken, defaultToolPageSize, maxToolPageSize)
	if err != nil {
		return nil, fmt.Errorf("invalid pagination — use page_size between 1 and %d and pass next_page_token unchanged", maxToolPageSize)
	}
	acc, err := h.access()
	if err != nil {
		return nil, err
	}
	entries, err := acc.ListFeedEntries(ctx, in.FeedId, FeedEntryFilter{Status: RSSEntryStatus(in.Status), Limit: limit + 1, Offset: offset})
	if err != nil {
		return nil, err
	}
	page, next := tools.PageRows(entries, limit, offset)
	items := make([]recallyFeedEntryItem, 0, len(page))
	for _, entry := range page {
		items = append(items, recallyFeedEntrySummary(entry))
	}
	return listResponse[recallyFeedEntryItem]{Items: items, HasMore: next != ""}, nil
}

func (h recallyHandler) EntryAdd(ctx context.Context, in EntryAddInput) (any, error) {
	acc, err := h.access()
	if err != nil {
		return nil, err
	}
	entry, created, err := acc.CreateFeedEntry(ctx, in.FeedId, in.Guid, in.Url, in.Title)
	if err != nil {
		return nil, err
	}
	result := recallyCreateFeedEntryResult{Created: created}
	if entry != nil {
		item := recallyFeedEntrySummary(*entry)
		result.Entry = &item
	}
	return result, nil
}

func (h recallyHandler) EntryUpdate(ctx context.Context, in EntryUpdateInput) (any, error) {
	var articleID *string
	if in.ArticleId != "" {
		articleID = &in.ArticleId
	}
	acc, err := h.access()
	if err != nil {
		return nil, err
	}
	entry, err := acc.UpdateFeedEntry(ctx, in.FeedId, in.Id, RSSEntryStatus(in.Status), articleID, in.ErrorMsg)
	if err != nil {
		return nil, err
	}
	return recallyFeedEntrySummary(*entry), nil
}

func (h recallyHandler) DigestGet(ctx context.Context, _ DigestGetInput) (any, error) {
	acc, err := h.access()
	if err != nil {
		return nil, err
	}
	digest, err := acc.GetDigest(ctx)
	if err != nil {
		return nil, err
	}
	return map[string]any{"date": digest.Date.UTC().Format(time.RFC3339), "text": recallyDigestText(digest)}, nil
}

func (h recallyHandler) DigestSave(ctx context.Context, in DigestSaveInput) (any, error) {
	acc, err := h.access()
	if err != nil {
		return nil, err
	}
	stored, err := acc.SaveDigest(ctx, in.Narrative, in.Date)
	if err != nil {
		return nil, err
	}
	return map[string]any{"date": stored.Date, "saved": true}, nil
}

type recallySaveResult struct {
	URL    string `json:"url"`
	ID     string `json:"id,omitempty"`
	Status string `json:"status"`
	// ContentChars reports what was actually stored. A save that succeeds with
	// a few hundred characters captured a summary or a paywall stub, not the
	// article; without this the caller has to spend a get_article round trip to
	// find that out.
	ContentChars int `json:"content_chars"`
	// ContentPreview is the head and tail of the stored body, enough to spot
	// an excerpt or aggregator page without the body entering the caller's
	// context.
	ContentPreview string `json:"content_preview,omitempty"`
	Error          string `json:"error,omitempty"`
}
type recallyArticleListItem struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	URL     string `json:"url"`
	SavedAt string `json:"saved_at"`
}
type recallyArticleDetail struct {
	Article   recallyArticleListItem `json:"article"`
	Content   string                 `json:"content"`
	Truncated bool                   `json:"truncated"`
	Note      string                 `json:"note,omitempty"`
}
type recallyFeedItem struct {
	ID        string            `json:"id"`
	URL       string            `json:"url"`
	Kind      string            `json:"kind"`
	Title     string            `json:"title"`
	Enabled   bool              `json:"enabled"`
	Metadata  map[string]string `json:"metadata,omitempty"`
	UpdatedAt string            `json:"updated_at"`
}
type recallyFeedEntryItem struct {
	ID           string  `json:"id"`
	FeedID       string  `json:"feed_id"`
	GUID         string  `json:"guid"`
	URL          string  `json:"url"`
	Title        string  `json:"title"`
	Status       string  `json:"status"`
	ArticleID    *string `json:"article_id,omitempty"`
	Attempts     int     `json:"attempts"`
	ErrorMsg     string  `json:"error_msg,omitempty"`
	DiscoveredAt string  `json:"discovered_at"`
	ProcessedAt  string  `json:"processed_at,omitempty"`
}
type recallyFeedPollResult struct {
	Feed       recallyFeedItem        `json:"feed"`
	NewEntries []recallyFeedEntryItem `json:"new_entries"`
	Error      string                 `json:"error,omitempty"`
}
type recallyCreateFeedEntryResult struct {
	Created bool                  `json:"created"`
	Entry   *recallyFeedEntryItem `json:"entry,omitempty"`
}
type listResponse[T any] struct {
	Items         []T    `json:"items"`
	HasMore       bool   `json:"has_more"`
	NextPageToken string `json:"next_page_token,omitempty"`
}

func recallySaveRequest(item ArticleSaveItem) SaveRequest {
	return SaveRequest{URL: item.Url, CanonicalURL: item.CanonicalUrl, SourceType: SourceType(item.SourceType), Title: item.Title, Author: item.Author, Summary: item.Summary, Tags: stringItems(item.Tags), Content: item.Content, Metadata: stringMap(item.Metadata), PublishedAt: parseOptionalTime(item.PublishedAt)}
}

func recallyArticleListSummary(article Article) recallyArticleListItem {
	return recallyArticleListItem{ID: article.ID, Title: article.Title, URL: article.URL, SavedAt: article.SavedAt.UTC().Format(time.RFC3339)}
}

func recallyFeedSummary(feed Feed) recallyFeedItem {
	return recallyFeedItem{ID: feed.ID, URL: feed.URL, Kind: string(feed.Kind), Title: feed.Title, Enabled: feed.Enabled, Metadata: feed.Metadata, UpdatedAt: feed.UpdatedAt.UTC().Format(time.RFC3339)}
}

func recallyFeedEntrySummary(entry FeedEntry) recallyFeedEntryItem {
	item := recallyFeedEntryItem{ID: entry.ID, FeedID: entry.FeedID, GUID: entry.GUID, URL: entry.URL, Title: entry.Title, Status: string(entry.Status), ArticleID: entry.ArticleID, Attempts: entry.Attempts, ErrorMsg: entry.ErrorMsg, DiscoveredAt: entry.DiscoveredAt.UTC().Format(time.RFC3339)}
	if entry.ProcessedAt != nil {
		item.ProcessedAt = entry.ProcessedAt.UTC().Format(time.RFC3339)
	}
	return item
}

func recallyTruncationNote(truncated bool) string {
	if truncated {
		return "truncated — use the web UI for the full article"
	}
	return ""
}

func recallyDigestText(d *Digest) string {
	if d == nil {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Digest for %s\n", d.Date.UTC().Format("2006-01-02"))
	fmt.Fprintf(&b, "Total articles: %d; unread: %d; read: %d; archived: %d; starred: %d\n", d.TotalArticles, d.UnreadCount, d.ReadCount, d.ArchivedCount, d.StarredCount)
	if len(d.SavedYesterday) > 0 {
		b.WriteString("\nSaved yesterday:\n")
		for _, article := range d.SavedYesterday {
			fmt.Fprintf(&b, "- %s — %s\n", article.Title, article.URL)
		}
	}
	if len(d.WorthRevisiting) > 0 {
		b.WriteString("\nWorth revisiting:\n")
		for _, article := range d.WorthRevisiting {
			fmt.Fprintf(&b, "- %s — %s\n", article.Title, article.URL)
		}
	}
	if len(d.TopTags) > 0 {
		b.WriteString("\nTop tags:\n")
		for _, tag := range d.TopTags {
			fmt.Fprintf(&b, "- %s (%d)\n", tag.Tag, tag.Count)
		}
	}
	return b.String()
}

func stringItems(items []any) []string {
	out := make([]string, 0, len(items))
	for _, item := range items {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func stringMap(items map[string]any) map[string]string {
	if len(items) == 0 {
		return nil
	}
	out := make(map[string]string, len(items))
	for k, v := range items {
		if s, ok := v.(string); ok {
			out[k] = s
		}
	}
	return out
}

func parseOptionalTime(value string) *time.Time {
	if value == "" {
		return nil
	}
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return nil
	}
	return &parsed
}
