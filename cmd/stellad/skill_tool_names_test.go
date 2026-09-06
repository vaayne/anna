package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	coreagent "github.com/CherryHQ/stella/pkg/agent"
	"github.com/CherryHQ/stella/pkg/toolmeta"
	"github.com/CherryHQ/stella/resources"
)

// Skills and the system prompt name tools as prose, and prose does not compile.
// A union tool absorbed a wrong name silently — "action=pause" on the right
// family still reached a real tool — but a split family has one name per
// action, so a stale mention is a call the model cannot make.
//
// This is the generalization of the recally-only guard in
// internal/scheduler/builtin_schema_test.go: every mention of a generated
// family's prefix, anywhere in the built-in skills or the prompt template, must
// be a tool this build registers.
//
// cmd/stellad is the only package that already imports every family, which is
// why the guard lives beside the registration rather than beside the skills.
// A mention is a whole backtick span or a Code Mode invocation, which is how a
// skill writes a tool name. Matching bare tokens instead would flag every field
// called workflow_id, and a guard with false positives gets deleted.
var (
	backtickMention = regexp.MustCompile("`((?:settings|goal|scheduler|workflow|oauth|email|share|vault|recally|session|skills?|memory)_[a-z_]+)`")
	invokeMention   = regexp.MustCompile(`tools\.invoke\(\s*"([a-z_]+)"`)
	// A union tool was referenced as "the `scheduler` tool", or called as
	// "`oauth connect(provider=feishu)`" — the union's own argument syntax.
	// After the split neither names anything callable, and the bare family word
	// is too common to flag on its own.
	unionMention     = regexp.MustCompile("`(goal|scheduler|workflow|oauth|email|share|vault|recally|session|skills|memory)`\\s+tool")
	unionCallMention = regexp.MustCompile("`((?:goal|scheduler|workflow|oauth|email|share|vault|recally|session|skills|memory) +[a-z_]+)[^`]*`")
)

// thirdPartyFields are identifiers that read like a Stella tool name but belong
// to another product's API — Lark's `workflow_id`, its `share_*` chat fields.
// They are listed one by one rather than skipped by path: a directory-wide skip
// is what let five retired `oauth` instructions survive the split inside
// lark-cli. Adding an entry is a claim that the token is a field of a
// third-party API, and it is the only way a backticked `family_*` token escapes
// being checked against the registry.
var thirdPartyFields = map[string]bool{
	"workflow_id": true, // Lark workflow/approval instance id
	"share_chat":  true, // Lark message type
	"share_info":  true, // Lark share payload
	"share_link":  true, // Lark share payload
	"share_user":  true, // Lark message type
}

// firstPartyFields are Stella's own identifiers that share a family prefix with
// a tool name: an argument, a column, or a frontmatter key. The session and
// skill families are the reason this second list exists — their names read like
// their fields — and the same one-entry-at-a-time rule applies: each entry is a
// claim that the token is a field, not a tool.
var firstPartyFields = map[string]bool{
	"session_id":              true, // the addressed session tools' argument
	"session_mode":            true, // scheduler_job_create's reuse/new argument
	"skill_name":              true, // skill-creator's frontmatter key
	"skill_file":              true, // the legacy managed-Skill table
	"settings_agents":         true, // configuration reference table name
	"settings_plugins":        true, // configuration reference table name
	"settings_users":          true, // configuration reference table name
	"settings_channel_agents": true, // configuration reference table name
}

// toolMentions returns names the prose asks the model to call. They must all be
// tools this build registers.
func toolMentions(text string) []string {
	var out []string
	for _, match := range backtickMention.FindAllStringSubmatch(text, -1) {
		if thirdPartyFields[match[1]] || firstPartyFields[match[1]] {
			continue
		}
		out = append(out, match[1])
	}
	for _, match := range invokeMention.FindAllStringSubmatch(text, -1) {
		out = append(out, match[1])
	}
	return out
}

// proseProblems is what the guard reports for one document. It is separate from
// the walk so the guard's own tests can drive it against the real registry.
func proseProblems(text string, registered map[string]bool) []string {
	var out []string
	for _, mention := range toolMentions(text) {
		if registered[mention] || toolmeta.HandWritten(mention) {
			continue
		}
		out = append(out, fmt.Sprintf("names %q, which no family registers", mention))
	}
	for _, mention := range unionCallMentions(text) {
		out = append(out, fmt.Sprintf("says %q, which addresses a family that is no longer one tool", mention))
	}
	return out
}

// registeredToolNames is every generated tool name this build registers.
func registeredToolNames() map[string]bool {
	registered := map[string]bool{}
	for _, family := range generatedFamilies() {
		for _, spec := range family {
			registered[spec.Name] = true
		}
	}
	return registered
}

// unionCallMentions returns prose that still addresses a family the way the
// union was addressed. Every one of these is wrong regardless of what this
// build registers, because no union tool exists any more.
func unionCallMentions(text string) []string {
	var out []string
	for _, match := range unionMention.FindAllStringSubmatch(text, -1) {
		out = append(out, match[1]+" tool")
	}
	for _, match := range unionCallMention.FindAllStringSubmatch(text, -1) {
		out = append(out, match[1])
	}
	return out
}

func TestBuiltinProseOnlyNamesRegisteredTools(t *testing.T) {
	registered := registeredToolNames()
	for path, text := range builtinProse(t) {
		for _, problem := range proseProblems(text, registered) {
			t.Errorf("%s %s", path, problem)
		}
	}
}

var wildcardToolFamily = regexp.MustCompile(`(?m)^([a-z][a-z_]*)\*\s+#`)

func TestStellaSkillWildcardFamiliesMatchRegisteredTools(t *testing.T) {
	registry, err := resources.Default()
	if err != nil {
		t.Fatal(err)
	}
	body, _, err := registry.ReadBuiltinSkillFile("stella", "SKILL.md")
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	sectionStart := strings.Index(text, "## Stella tools")
	if sectionStart < 0 {
		t.Fatal("Stella tool inventory section is missing")
	}
	codeStart := strings.Index(text[sectionStart:], "```")
	if codeStart < 0 {
		t.Fatal("Stella tool inventory code block is missing")
	}
	codeStart += sectionStart + len("```")
	codeEnd := strings.Index(text[codeStart:], "```")
	if codeEnd < 0 {
		t.Fatal("Stella tool inventory code block is unterminated")
	}
	inventory := text[codeStart : codeStart+codeEnd]
	registered := registeredToolNames()
	for _, match := range wildcardToolFamily.FindAllStringSubmatch(inventory, -1) {
		prefix := match[1]
		found := false
		for name := range registered {
			if strings.HasPrefix(name, prefix) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("Stella tool inventory names wildcard %q, which matches no registered tool", prefix+"*")
		}
	}
}

// builtinProse is every embedded skill document plus the system prompt
// template: the two surfaces that tell a model what to call.
func builtinProse(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	registry, err := resources.Default()
	if err != nil {
		t.Fatalf("load builtin skills: %v", err)
	}
	for _, skill := range registry.BuiltinSkills() {
		for _, file := range skill.Files {
			if ext := filepath.Ext(file.Path); ext != ".md" && ext != ".mdx" {
				continue
			}
			body, _, readErr := registry.ReadBuiltinSkillFile(skill.Name, file.Path)
			if readErr != nil {
				t.Fatalf("read builtin skill %q/%q: %v", skill.Name, file.Path, readErr)
			}
			out[skill.Root+"/"+file.Path] = string(body)
		}
	}
	if len(out) == 0 {
		t.Fatal("no builtin skills found: the walk root is wrong, not the skills")
	}
	// The prompt templates are embedded into internal/agent/prompt behind
	// unexported variables, so they are read from the tree instead. A test
	// binary always runs in its own package directory, which makes the relative
	// path stable.
	templates, err := filepath.Glob(filepath.Join("..", "..", "internal", "agent", "prompt", "template", "*"))
	if err != nil {
		t.Fatalf("glob prompt templates: %v", err)
	}
	if len(templates) == 0 {
		t.Fatal("no prompt templates found: the relative path is wrong, not the templates")
	}
	for _, path := range templates {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		out[path] = string(body)
	}
	return out
}

// The guard is regex prose matching, so it needs its own guard: a pattern that
// silently stops matching turns the whole test green for the wrong reason.
func TestProseGuardDetectsRetiredCallSyntax(t *testing.T) {
	for _, tc := range []struct {
		name      string
		text      string
		wantNames []string
		wantUnion []string
	}{
		{
			name:      "backticked tool name is a mention",
			text:      "call `recally_article_save` with the fetched body",
			wantNames: []string{"recally_article_save"},
		},
		{
			name:      "code mode invocation is a mention",
			text:      `return await tools.invoke("share_create_article", {})`,
			wantNames: []string{"share_create_article"},
		},
		{
			name:      "the union addressed as a tool",
			text:      "use the `oauth` tool for authorization",
			wantUnion: []string{"oauth tool"},
		},
		{
			name:      "the union called with its own action argument",
			text:      "run `oauth status`, then `oauth connect(provider=feishu)`",
			wantUnion: []string{"oauth status", "oauth connect"},
		},
		{
			// The allowlist covers the exact field, not the document it appears
			// in: a retired union call in the same sentence is still reported.
			name:      "an allowlisted third-party field does not shield its neighbours",
			text:      "pass `workflow_id` and then use `oauth list`",
			wantUnion: []string{"oauth list"},
		},
		{
			name: "allowlisted third-party fields are not mentions",
			text: "pass `workflow_id`, `share_chat`, `share_info`, `share_link` and `share_user`",
		},
		{
			// The session and skill families read like their own arguments, so
			// the field allowlist has to hold for them too.
			name: "allowlisted first-party fields are not mentions",
			text: "it returns a `session_id`; set `session_mode`, `skill_name`, `skill_file`",
		},
		{
			name:      "the skills union addressed as a tool",
			text:      "use the `skills` tool to find a runbook",
			wantUnion: []string{"skills tool"},
		},
		{
			name:      "the session union called with its own action argument",
			text:      "run `session list`, then `session send(session_id=abc)`",
			wantUnion: []string{"session list", "session send"},
		},
		{
			name:      "split session and skill names are mentions",
			text:      "call `skill_installed_search`, load it with `skill_load`, then `session_create`",
			wantNames: []string{"skill_installed_search", "skill_load", "session_create"},
		},
		{
			name: "a bare family word in prose is not a mention",
			text: "the scheduler runs jobs; share the article by email",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := toolMentions(tc.text); !slices.Equal(got, tc.wantNames) {
				t.Errorf("toolMentions = %v, want %v", got, tc.wantNames)
			}
			if got := unionCallMentions(tc.text); !slices.Equal(got, tc.wantUnion) {
				t.Errorf("unionCallMentions = %v, want %v", got, tc.wantUnion)
			}
		})
	}
}

// A guard that only ever runs against correct prose proves nothing. These are
// the documents the guard exists to reject, checked against the real registry:
// if any of them comes back clean, the guard is decorative.
func TestProseGuardRejectsStaleToolNames(t *testing.T) {
	registered := registeredToolNames()
	for _, tc := range []struct {
		name string
		text string
		want string
	}{
		{
			name: "a tool name with a stale suffix",
			text: "call `oauth_connect_old` to authorize",
			want: `names "oauth_connect_old", which no family registers`,
		},
		{
			name: "a pre-split recally name",
			text: "call `recally_save_article` with the fetched body",
			want: `names "recally_save_article", which no family registers`,
		},
		{
			name: "a Code Mode invocation of a retired union",
			text: `await tools.invoke("scheduler", {action: "pause"})`,
			want: `names "scheduler", which no family registers`,
		},
		{
			name: "a third-party skill's own field name is not a licence for a stale tool",
			text: "pass `workflow_id`, then call `workflow_execute`",
			want: `names "workflow_execute", which no family registers`,
		},
		{
			name: "a Code Mode invocation of the retired skills union",
			text: `await tools.invoke("skills", {action: "load"})`,
			want: `names "skills", which no family registers`,
		},
		{
			name: "the skills union's search action is not a tool name",
			text: "call `skills_search_installed` for a runbook",
			want: `names "skills_search_installed", which no family registers`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			problems := proseProblems(tc.text, registered)
			if !slices.Contains(problems, tc.want) {
				t.Fatalf("problems = %v, want one of them to be %q", problems, tc.want)
			}
		})
	}

	// The counterpart: prose that names only real tools must come back clean,
	// or the guard is noise everyone learns to ignore.
	clean := "call `oauth_connect`, then `oauth_flow_status`; `skill_load` a runbook and `session_send` to it; pass `workflow_id`, `share_chat` and `session_id` along"
	if problems := proseProblems(clean, registered); len(problems) != 0 {
		t.Fatalf("correct prose reported %v", problems)
	}
}

// settingsDefaultDocs are the surfaces that must agree on the exceptional
// default for the reserved built-in Stella Agent.
var settingsDefaultDocs = map[string]string{
	filepath.Join("..", "..", "plugins", "core", "skills", "stella", "SKILL.md"):                       "Built-in `stella` starts with them enabled; every other Agent starts disabled",
	filepath.Join("..", "..", "plugins", "core", "skills", "stella", "references", "configuration.md"): "Stella starts enabled, including after an upgrade",
	filepath.Join("..", "..", "web", "content", "docs", "start-here", "configuration.md"):              "Built-in **Stella** starts with System settings tools enabled. Every other Agent",
	filepath.Join("..", "..", "web", "content", "docs", "start-here", "configuration.zh.md"):           "内置 **Stella** 初始开启系统设置工具；其他 Agent 初始关闭",
}

func TestSettingsDefaultPolicyProseMatchesBuiltInStella(t *testing.T) {
	for path, marker := range settingsDefaultDocs {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if !strings.Contains(string(body), marker) {
			t.Errorf("%s does not state the built-in Stella default with marker %q", path, marker)
		}
	}
}

// hotSetDocs are the documents that quote the Code Mode hot set as prose. The
// set is a product decision the model reads about, so a name added to or
// removed from coreagent.HotToolNames has to reach every one of them; prose
// that disagrees with the code teaches a call the runtime will not honour.
var hotSetDocs = []string{
	filepath.Join("..", "..", "internal", "agent", "prompt", "template", "system_prompt.tmpl"),
	filepath.Join("..", "..", "plugins", "core", "skills", "stella", "references", "configuration.md"),
	filepath.Join("..", "..", "web", "content", "docs", "start-here", "configuration.md"),
	filepath.Join("..", "..", "web", "content", "docs", "start-here", "configuration.zh.md"),
}

// hotSetMarkers are how each document introduces the set, in both languages.
var hotSetMarkers = []string{"hot set", "Hot keeps", "is Hot", "热集"}

func TestHotSetProseMatchesTheDeclaredHotTools(t *testing.T) {
	// `code` rides along in every hot-set sentence: it is what the cold tools
	// are reached through, not a member of the set.
	want := append(slices.Clone(coreagent.HotToolNames), "code")
	slices.Sort(want)
	registered := registeredToolNames()
	for _, path := range hotSetDocs {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		got := hotSetToolNames(string(body), registered)
		if !slices.Equal(got, want) {
			t.Errorf("%s names hot tools %v, want %v", path, got, want)
		}
	}
}

// hotSetToolNames collects the tool names backticked on the lines that describe
// the hot set. Names are filtered against what this build actually offers, so
// `native`, `stellad server` and the environment variable itself are ignored
// without an allowlist to maintain.
func hotSetToolNames(text string, registered map[string]bool) []string {
	seen := map[string]bool{}
	for line := range strings.SplitSeq(text, "\n") {
		marked := false
		for _, marker := range hotSetMarkers {
			if strings.Contains(line, marker) {
				marked = true
				break
			}
		}
		if !marked {
			continue
		}
		for _, match := range anyBacktickToken.FindAllStringSubmatch(line, -1) {
			if registered[match[1]] || toolmeta.HandWritten(match[1]) {
				seen[match[1]] = true
			}
		}
	}
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	slices.Sort(out)
	return out
}

// anyBacktickToken is deliberately looser than backtickMention: the hot set
// contains hand-written names like bash that carry no family prefix.
var anyBacktickToken = regexp.MustCompile("`([a-z_]+)`")
