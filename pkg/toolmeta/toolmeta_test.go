package toolmeta

import (
	"testing"
)

func TestNewRegistryRejectsDuplicateNames(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("NewRegistry accepted duplicate tool names")
		}
	}()
	NewRegistry(ActionTool{Name: "email__send"}, ActionTool{Name: "email__send"})
}

func TestRegistryToolsReturnsStableSnapshot(t *testing.T) {
	reg := NewRegistry(
		ActionTool{Name: "z", Family: "z"},
		ActionTool{Name: "a", Family: "a"},
	)
	tools := reg.Tools()
	if len(tools) != 2 || tools[0].Name != "a" || tools[1].Name != "z" {
		t.Fatalf("Tools() = %#v, want stable name order", tools)
	}
	tools[0].Name = "changed"
	if got, ok := reg.Lookup("a"); !ok || got.Name != "a" {
		t.Fatalf("Tools() exposed registry storage: %#v, %v", got, ok)
	}
}

var recallyTools = []ActionTool{
	{Name: "recally__feed_add", PluginID: "system/recally", Namespace: "recally", LocalName: "feed_add", Family: "recally", Resource: "feed", Action: "feed_add", InputSchemaJSON: `{"type":"object","properties":{"url":{"type":"string"}},"required":["url"],"additionalProperties":false}`},
	{Name: "recally__digest_get", PluginID: "system/recally", Namespace: "recally", LocalName: "digest_get", Family: "recally", Resource: "digest", Action: "digest_get", InputSchemaJSON: `{"type":"object","properties":{},"additionalProperties":false}`},
}

func TestFamilyComesFromTheRegistryNotTheName(t *testing.T) {
	reg := NewRegistry(recallyTools...)
	if got := reg.Family("recally__feed_add"); got != "recally" {
		t.Fatalf("Family=%q, want recally", got)
	}
	// A plugin is free to name itself anything. Splitting on "_" would read
	// "goal_helper" as a member of the goal family and hand it goal's
	// visibility rules; an unregistered name has no family at all.
	if got := reg.Family("goal_helper"); got != "" {
		t.Fatalf("Family(goal_helper)=%q, want no family", got)
	}
	if _, ok := reg.Lookup("goal_helper"); ok {
		t.Fatal("unregistered tool must not resolve")
	}
}

func TestMatchAcceptsExactNamesAndFamilies(t *testing.T) {
	feedAdd := recallyTools[0]
	for _, selector := range []string{"recally__feed_add", "recally"} {
		if !Match(selector, feedAdd) {
			t.Errorf("Match(%q) = false, want true", selector)
		}
	}
	for _, selector := range []string{"", "recally_feed", "feed", "feed_add", "goal"} {
		if Match(selector, feedAdd) {
			t.Errorf("Match(%q) = true, want false", selector)
		}
	}
}

// The family a selector may name is the declared one, so a plugin whose name
// merely starts with a family name is never swept into that family's grant.
func TestMatchDoesNotInferFamilyFromAPrefix(t *testing.T) {
	plugin := ActionTool{Name: "goal_helper"}
	if Match("goal", plugin) {
		t.Fatal("family selector matched a tool that declares no family")
	}
	if !Match("goal_helper", plugin) {
		t.Fatal("exact name must still match")
	}
}

func TestMatchAnyAndNames(t *testing.T) {
	reg := NewRegistry(recallyTools...)
	if got := reg.Names(); len(got) != 2 || got[0] != "recally__digest_get" || got[1] != "recally__feed_add" {
		t.Fatalf("Names=%v, want both tools sorted", got)
	}
	if !MatchAny([]string{"goal", "recally"}, recallyTools[1]) {
		t.Fatal("MatchAny must match on any selector")
	}
	if MatchAny([]string{"goal", "scheduler"}, recallyTools[1]) {
		t.Fatal("MatchAny matched an unrelated selector")
	}
}

// The runner's excluded_tools filter and the delegate preset whitelist only
// have a name, so they resolve the family through the registry they were given.
func TestMatchNameResolvesFamiliesThroughTheRegistry(t *testing.T) {
	// A nil registry is the pre-wiring case: exact names still match, families
	// do not, which is what these call sites did before family selectors.
	var absent *Registry
	if !absent.MatchName("recally__feed_add", "recally__feed_add") {
		t.Fatal("an exact name must match without a registry")
	}
	if absent.MatchName("recally", "recally__feed_add") {
		t.Fatal("a family selector must not match without a registry")
	}

	reg := NewRegistry(recallyTools...)
	if !reg.MatchName("recally", "recally__feed_add") {
		t.Fatal("a family selector must match every member")
	}
	// A retired name is gone, not redirected: the override rows naming it were
	// deleted by the migration, so a selector written against it selects nothing.
	if reg.MatchName("recally_digest", "recally__digest_get") {
		t.Fatal("a retired name must not redirect to its replacement")
	}
	// The skills union split into the singular "skill" family, so its own name
	// is retired too: nothing resolves it, not even to the family it became.
	skillReg := NewRegistry(ActionTool{Name: "skill_load", Family: "skill", Action: "load"})
	if skillReg.MatchName("skills", "skill_load") {
		t.Fatal("the retired skills union name must select nothing")
	}
	if !skillReg.SelectsNothing("skills", []string{"skill_load", "skill_installed_search"}) {
		t.Fatal("a preset listing the retired skills name must report empty")
	}
	// A plugin is free to call itself anything; only registered tools have a
	// family, so a family selector must never sweep one in.
	if reg.MatchName("recally", "recally_helper_plugin") {
		t.Fatal("an unregistered name must match only itself")
	}
	if !reg.MatchAnyName([]string{"goal", "recally"}, "recally__feed_add") {
		t.Fatal("MatchAnyName must match on any selector")
	}

	names := []string{"recally__feed_add", "recally__digest_get", "bash"}
	if reg.SelectsNothing("recally", names) {
		t.Fatal("a family selector that matches members must not report empty")
	}
	if !reg.SelectsNothing("scheduler_pause", names) {
		t.Fatal("a stale selector must report empty so the caller can warn")
	}

	if got := reg.Action("recally__digest_get"); got != "digest_get" {
		t.Fatalf("Action=%q, want digest_get", got)
	}
	if got := reg.Action("bash"); got != "" {
		t.Fatalf("Action(bash)=%q, want empty for a tool with no declaration", got)
	}
}

func TestDefinitionPrefersTheCallerDescription(t *testing.T) {
	declared := ActionTool{Name: "session_list", Description: "declared", InputSchemaJSON: `{"type":"object"}`}
	if got := declared.Definition(""); got.Description != "declared" || got.Name != "session_list" {
		t.Fatalf("Definition=%#v, want the declared description", got)
	}
	if got := declared.Definition("from the adapter"); got.Description != "from the adapter" {
		t.Fatalf("Definition=%#v, want the adapter description to win", got)
	}
	if schema := declared.Definition("").InputSchema; schema["type"] != "object" {
		t.Fatalf("InputSchema=%#v", schema)
	}
}

// The exception list is closed on purpose: a tool that skips toolgen also skips
// the schema, naming and drift checks, so growing this list has to be a
// deliberate edit with a reason in the PR.
func TestHandWrittenExceptionsAreClosed(t *testing.T) {
	for _, name := range []string{"bash", "view_image", "notify", "goal_control", "code"} {
		if !HandWritten(name) {
			t.Errorf("HandWritten(%q) = false, want true", name)
		}
	}
	for _, name := range []string{"mcp__github__search", "recally__digest_get", "goal", "session_list", "memory_search", "library_search", "shell"} {
		if HandWritten(name) {
			t.Errorf("HandWritten(%q) = true, want a generated tool", name)
		}
	}
	// The exact contents are pinned by
	// TestExceptionListsAreExactlyWhatTheRuleDocuments.
}
