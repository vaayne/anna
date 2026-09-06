package scheduler

import (
	"regexp"
	"testing"

	"github.com/CherryHQ/stella/internal/library/recally"
)

// The builtin job templates are prompts stored as Go strings. They tell a
// worker to call specific native recally tools with specific arguments. Those
// strings drifted from the tool surface once already (they instructed a deleted
// CLI and named a tool action that did not exist), silently breaking every
// scheduled RSS/digest run.
//
// This test makes that whole bug class impossible: it extracts every
// `recally_*` tool name and every `<name>=` argument from each template and
// asserts them against the generated per-action input schemas. Add
// `recally__does_not_exist` or a typo like `limitt=20` and this test fails,
// naming the offending template key and token.

// toolMention matches a native tool name from any split family a builtin
// template could drive (recally__feed_poll, scheduler__job_create).
var toolMention = regexp.MustCompile(`(?:recally|scheduler)_\w+`)

// tokenAssign matches `<word>=` assignments (limit=20).
var tokenAssign = regexp.MustCompile(`(\w+)=(\w+)?`)

// recallyTemplates maps each builtin template to the tools whose argument
// surface it drives. Both current builtins speak only recally tools, so the
// mapping is hard-coded (there is no builtin-template registry to iterate); add
// new entries here when a recally-driven builtin is introduced.
func recallyTemplates() map[string]JobTemplate {
	return map[string]JobTemplate{
		RecallyRSSTemplate.Key:    RecallyRSSTemplate,
		RecallyDigestTemplate.Key: RecallyDigestTemplate,
	}
}

// templateToolProps maps each generated tool name to the property names its
// schema declares — the exact arguments a provider will accept for that tool.
// Every family a builtin template may name goes in: a template that reaches for
// a scheduler tool must be checked against the same surface as a recally one.
func templateToolProps(t *testing.T) map[string]map[string]bool {
	t.Helper()
	out := map[string]map[string]bool{}
	specs := append(append([]ActionTool(nil), recally.ActionTools()...), ActionTools()...)
	for _, spec := range specs {
		properties, ok := spec.InputSchema()["properties"].(map[string]any)
		if !ok {
			t.Fatalf("tool %q has no properties in its schema", spec.Name)
		}
		set := map[string]bool{}
		for name := range properties {
			set[name] = true
		}
		out[spec.Name] = set
	}
	return out
}

func TestBuiltinTemplatesMatchToolSchemas(t *testing.T) {
	toolProps := templateToolProps(t)

	for key, tmpl := range recallyTemplates() {
		for _, name := range toolMention.FindAllString(tmpl.Message, -1) {
			if _, ok := toolProps[name]; !ok {
				t.Errorf("template %q references unknown tool %q", key, name)
			}
		}

		// Scan the message line by line: a line naming a tool binds every
		// `<name>=` argument on that line to it. Arguments never cross lines in
		// these templates, and a line-scoped rule keeps step 2's prose from
		// being attributed to step 1's tool.
		for _, line := range regexp.MustCompile(`\n`).Split(tmpl.Message, -1) {
			names := toolMention.FindAllString(line, -1)
			if len(names) == 0 {
				continue
			}
			for _, m := range tokenAssign.FindAllStringSubmatch(line, -1) {
				argument := m[1]
				accepted := false
				for _, name := range names {
					if toolProps[name][argument] {
						accepted = true
						break
					}
				}
				if !accepted {
					t.Errorf("template %q passes argument %q to %v, which declares no such property", key, argument, names)
				}
			}
		}
	}
}
