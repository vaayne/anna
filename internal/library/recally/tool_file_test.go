package recally

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/CherryHQ/stella/internal/authz"
	pkgagent "github.com/CherryHQ/stella/pkg/agent"
	"github.com/CherryHQ/stella/pkg/ai"
	"github.com/CherryHQ/stella/pkg/providers"
	pkgsandbox "github.com/CherryHQ/stella/pkg/sandbox"
)

type recallyFileAccess struct {
	files map[string][]byte
	sizes map[string]int64
	dirs  map[string]bool
}

func (a recallyFileAccess) ReadFile(path string) ([]byte, error) {
	content, ok := a.files[path]
	if !ok {
		return nil, fs.ErrNotExist
	}
	return append([]byte(nil), content...), nil
}

func (a recallyFileAccess) ReadDir(string) ([]pkgsandbox.DirEntry, error) {
	return nil, fs.ErrPermission
}

func (a recallyFileAccess) Stat(path string) (pkgsandbox.FileInfo, error) {
	if a.dirs[path] {
		return pkgsandbox.FileInfo{IsDir: true}, nil
	}
	if size, ok := a.sizes[path]; ok {
		return pkgsandbox.FileInfo{Size: size}, nil
	}
	content, ok := a.files[path]
	if !ok {
		return pkgsandbox.FileInfo{}, fs.ErrNotExist
	}
	return pkgsandbox.FileInfo{Size: int64(len(content))}, nil
}
func (a recallyFileAccess) WriteFile(string, []byte, fs.FileMode) error { return fs.ErrPermission }
func (a recallyFileAccess) ProjectFiles(string, []pkgsandbox.ProjectedFile) error {
	return fs.ErrPermission
}

func (a recallyFileAccess) ProjectTempFiles(string, []pkgsandbox.ProjectedFile) (string, error) {
	return "", fs.ErrPermission
}

type recallyFileSession struct {
	pkgsandbox.Session
	files pkgsandbox.FileAccess
}

func (s recallyFileSession) Policy() pkgsandbox.Policy {
	return pkgsandbox.Policy{Env: map[string]string{
		pkgsandbox.EnvHome:            "/workspace",
		pkgsandbox.EnvStellaAssetsDir: "/user/assets",
		pkgsandbox.EnvTempDir:         "/tmp/session",
	}}
}
func (s recallyFileSession) Files() pkgsandbox.FileAccess { return s.files }
func (recallyFileSession) WorkingDir() string             { return "/workspace" }

func recallyFileToolContext() context.Context {
	return authz.WithAgentID(authz.WithUserID(context.Background(), testUserID), "agent-1")
}

func TestRecallyToolSaveReadsContentPathBeforeWriting(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()
	svc := NewService(NewStore(db), t.TempDir())
	body := []byte("# Article\n\npassword: preserved-as-article-data\n\n" + strings.Repeat("article body ", 20))
	session := recallyFileSession{Session: pkgsandbox.NopSession(), files: recallyFileAccess{files: map[string][]byte{
		"/tmp/session/article.md": body,
	}}}
	tool := NewRuntimeTool(svc, session, actionSpec("article_save"))

	out, err := tool.Execute(recallyFileToolContext(), map[string]any{"articles": []any{
		map[string]any{"url": "https://example.com/file", "title": "File", "content_path": "$TMPDIR/article.md"},
	}})
	if err != nil || out == "" {
		t.Fatalf("save output=%q err=%v", out, err)
	}
	articles, err := mustAccess(t, svc, testUserID).ListArticles(t.Context(), ArticleFilter{Limit: 10})
	if err != nil || len(articles) != 1 {
		t.Fatalf("articles=%d err=%v", len(articles), err)
	}
	got, err := mustAccess(t, svc, testUserID).ReadArticleBody(t.Context(), &articles[0])
	if err != nil || got != string(body) {
		t.Fatalf("stored body=%q err=%v, want exact file bytes", got, err)
	}
	// content_chars is how a caller tells a captured article from a captured
	// summary without spending a get_article round trip.
	if want := fmt.Sprintf(`"content_chars":%d`, utf8.RuneCountInString(string(body))); !strings.Contains(out, want) {
		t.Fatalf("save result %q must report %s", out, want)
	}
}

func TestRecallyContentPathSupportsPublishedRoots(t *testing.T) {
	files := recallyFileAccess{files: map[string][]byte{
		"/workspace/relative.md":  []byte("relative"),
		"/workspace/home.md":      []byte("home"),
		"/user/assets/asset.md":   []byte("asset"),
		"/tmp/session/scratch.md": []byte("scratch"),
	}}
	h := recallyHandler{runtime: recallyFileSession{Session: pkgsandbox.NopSession(), files: files}}
	for _, tt := range []struct {
		path string
		want string
	}{
		{path: "relative.md", want: "relative"},
		{path: "$HOME/home.md", want: "home"},
		{path: "$STELLA_ASSETS_DIR/asset.md", want: "asset"},
		{path: "$TMPDIR/scratch.md", want: "scratch"},
	} {
		t.Run(tt.path, func(t *testing.T) {
			got, err := h.readContentFile(t.Context(), tt.path)
			if err != nil || got != tt.want {
				t.Fatalf("read=%q err=%v, want %q", got, err, tt.want)
			}
		})
	}
}

func TestRecallyToolContentPathValidationPrecedesWrites(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()
	svc := NewService(NewStore(db), t.TempDir())
	session := recallyFileSession{Session: pkgsandbox.NopSession(), files: recallyFileAccess{files: map[string][]byte{
		"/workspace/valid.md": []byte("valid"),
	}}}
	tool := NewRuntimeTool(svc, session, actionSpec("article_save"))

	_, err := tool.Execute(recallyFileToolContext(), map[string]any{"articles": []any{
		map[string]any{"url": "https://example.com/valid", "content_path": "valid.md"},
		map[string]any{"url": "https://example.com/ambiguous", "content": "inline", "content_path": "valid.md"},
	}})
	if err == nil {
		t.Fatal("ambiguous content unexpectedly succeeded")
	}
	articles, listErr := mustAccess(t, svc, testUserID).ListArticles(t.Context(), ArticleFilter{Limit: 10})
	if listErr != nil || len(articles) != 0 {
		t.Fatalf("validation wrote partial state: articles=%d err=%v", len(articles), listErr)
	}
}

func TestCodeFetchFileSaveShareJourneyKeepsBodyOutOfCode(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()
	svc := NewService(NewStore(db), t.TempDir())
	files := recallyFileAccess{files: map[string][]byte{}}
	session := recallyFileSession{Session: pkgsandbox.NopSession(), files: files}
	recallyTool := NewRuntimeTool(svc, session, actionSpec("article_save"))
	body := "# Orchestrated article\n\npassword: preserved-as-data\n\n" + strings.Repeat("article body ", 20)
	source := `
await tools.invoke("bash", {command:"fetch-to-file"});
const saved = tools.json(await tools.invoke("recally__article_save", {articles:[{url:"https://example.com/orchestrated", title:"Orchestrated", content_path:"$TMPDIR/article.md"}]}));
return await tools.invoke("share", {action:"article", article_id:saved.results[0].id});
`
	if strings.Contains(source, body) {
		t.Fatal("article body entered Code source")
	}
	calls := 0
	providerLeak := false
	stream := func(_ context.Context, _ ai.Model, request ai.Context, _ ai.StreamOptions) (providers.AssistantEventStream, error) {
		calls++
		if encoded, _ := json.Marshal(request.Messages); strings.Contains(string(encoded), body) {
			providerLeak = true
		}
		out := providers.NewChannelEventStream(3)
		go func() {
			if calls == 1 {
				raw, _ := json.Marshal(map[string]string{"code": source})
				out.Emit(ai.EventToolCallDelta{ID: "outer", Name: "code", Arguments: string(raw)})
				out.Emit(ai.EventStop{Reason: ai.StopReasonToolUse})
			} else {
				out.Emit(ai.EventStop{Reason: ai.StopReasonStop})
			}
			out.Finish(nil)
		}()
		return out, nil
	}
	runner, err := pkgagent.NewRunner(pkgagent.RunnerConfig{
		Stream: stream,
		Tools: pkgagent.ToolSet{
			"bash": func(context.Context, ai.ToolCall) ([]ai.ContentBlock, error) {
				files.files["/tmp/session/article.md"] = []byte(body)
				return []ai.ContentBlock{ai.TextContent{Text: "fetched"}}, nil
			},
			"recally__article_save": func(ctx context.Context, call ai.ToolCall) ([]ai.ContentBlock, error) {
				out, err := recallyTool.Execute(ctx, call.Arguments)
				return []ai.ContentBlock{ai.TextContent{Text: out}}, err
			},
			"share": func(ctx context.Context, call ai.ToolCall) ([]ai.ContentBlock, error) {
				id, _ := call.Arguments["article_id"].(string)
				article, err := mustAccess(t, svc, testUserID).GetArticle(ctx, id)
				if err != nil {
					return nil, err
				}
				stored, err := mustAccess(t, svc, testUserID).ReadArticleBody(ctx, article)
				if err != nil || stored != body {
					t.Fatalf("shared body=%q err=%v", stored, err)
				}
				return []ai.ContentBlock{ai.TextContent{Text: `{"url":"https://stella.test/s/article"}`}}, nil
			},
		},
		ToolDefinitions: []ai.ToolDefinition{{Name: "bash"}, recallyTool.Definition(), {Name: "share"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := recallyFileToolContext()
	var events []pkgagent.LoopEvent
	history, err := runner.RunWithActiveStart(ctx, []ai.Message{ai.UserMessage{Content: "save and share"}}, 0, func(event pkgagent.LoopEvent) {
		events = append(events, event)
	})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, message := range history {
		if result, ok := message.(ai.ToolResultMessage); ok && result.ToolName == "code" {
			found = strings.Contains(ai.FlattenText(result.Content), "https://stella.test/s/article")
			if strings.Contains(ai.FlattenText(result.Content), body) {
				t.Fatal("article body entered outer Code result")
			}
		}
	}
	if !found {
		t.Fatalf("share result missing from history: %#v", history)
	}
	if providerLeak {
		t.Fatal("article body entered provider context")
	}
	for _, event := range events {
		switch event := event.(type) {
		case pkgagent.ChildToolStarted:
			raw, _ := json.Marshal(event.ToolCall.Arguments)
			if strings.Contains(string(raw), body) {
				t.Fatal("article body entered child arguments")
			}
		case pkgagent.ChildToolFinished:
			if strings.Contains(ai.FlattenText(event.Result.Content), body) {
				t.Fatal("article body entered child result")
			}
		}
	}
}

func TestRecallyToolContentPathTotalLimitPrecedesWrites(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()
	svc := NewService(NewStore(db), t.TempDir())
	files := recallyFileAccess{files: map[string][]byte{}}
	articles := make([]any, 0, 5)
	for i := range 5 {
		path := "/workspace/article-" + string(rune('a'+i)) + ".md"
		files.files[path] = []byte(strings.Repeat("x", maxRecallyContentFileSize))
		articles = append(articles, map[string]any{"url": "https://example.com/" + string(rune('a'+i)), "content_path": path})
	}
	tool := NewRuntimeTool(svc, recallyFileSession{Session: pkgsandbox.NopSession(), files: files}, actionSpec("article_save"))
	_, err := tool.Execute(recallyFileToolContext(), map[string]any{"articles": articles})
	if err == nil || !strings.Contains(err.Error(), "exceeds 4194304 bytes total") {
		t.Fatalf("aggregate error=%v", err)
	}
	rows, listErr := mustAccess(t, svc, testUserID).ListArticles(t.Context(), ArticleFilter{Limit: 10})
	if listErr != nil || len(rows) != 0 {
		t.Fatalf("aggregate validation wrote state: rows=%d err=%v", len(rows), listErr)
	}
}

func TestRecallyToolContentPathRejectsInvalidFiles(t *testing.T) {
	files := recallyFileAccess{
		files: map[string][]byte{"/workspace/non-utf8.md": {0xff, 0xfe}},
		sizes: map[string]int64{"/workspace/large.md": maxRecallyContentFileSize + 1},
		dirs:  map[string]bool{"/workspace/directory": true},
	}
	h := recallyHandler{runtime: recallyFileSession{Session: pkgsandbox.NopSession(), files: files}}
	for _, tt := range []struct {
		path string
		want string
	}{
		{path: "non-utf8.md", want: "valid UTF-8"},
		{path: "large.md", want: "over the"},
		{path: "directory", want: "directory"},
		{path: "missing.md", want: "file does not exist"},
	} {
		t.Run(tt.path, func(t *testing.T) {
			_, err := h.readContentFile(t.Context(), tt.path)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("read error=%v, want %q", err, tt.want)
			}
			if tt.path == "missing.md" && !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("missing error=%v, want fs.ErrNotExist", err)
			}
		})
	}
}

// actionSpec finds one generated tool spec by action name.
func actionSpec(action string) ActionTool {
	for _, spec := range ActionTools() {
		if spec.Action == action {
			return spec
		}
	}
	panic("unknown recally action " + action)
}
