// Package toolmeta describes generated model-facing tools: the name, the
// family it belongs to, and the exact input schema bound to one action.
//
// It deliberately depends on nothing but pkg/tools. The runner, the delegate
// preset resolver and every generated tool_gen.go import it, so any dependency
// on a domain package would close an import cycle. It lives in pkg/toolmeta so
// generated plugin contracts can use the same metadata types as internal code.
package toolmeta

import (
	"sort"

	"github.com/CherryHQ/stella/pkg/tools"
)

// ActionTool is one generated tool: an exact schema bound to one action.
// Family and Resource are carried, not inferred — a plugin free to call itself
// "goal_helper" must never be mistaken for a member of the goal family.
type ActionTool struct {
	// Name is the model-facing exported tool name, e.g. "recally__feed_add".
	Name string
	// PluginID is the trusted logical plugin identity. It is empty for core
	// tools; callers must not infer ownership from Name.
	PluginID string
	// Namespace is the stable plugin namespace used in Name. It is empty for
	// core tools and is validated by toolgen for generated plugin tools.
	Namespace string
	// LocalName is the plugin-local tool name. It is empty for core tools.
	LocalName string
	// Family groups tools that share a domain, e.g. "recally".
	Family string
	// Resource is the family's sub-resource, e.g. "feed". Empty when the
	// family owns the action directly.
	Resource string
	// Action is the dispatch key inside the family, e.g. "feed_add".
	Action string
	// Description is the declared model-facing description. Operation-backed
	// tools leave it empty and pass prose in from their hand-written adapter.
	Description string
	// InputSchemaJSON is the generated JSON Schema for this tool's arguments.
	InputSchemaJSON string
}

// InputSchema decodes the generated schema. It panics on malformed JSON
// because the input is a compile-time constant produced by toolgen.
func (a ActionTool) InputSchema() map[string]any {
	return tools.MustInputSchema(a.InputSchemaJSON)
}

// Definition builds the model-facing definition. description wins when
// non-empty: operation-backed tools keep their prose in the hand-written
// adapter next to the handler, declaration-backed tools carry it in the spec.
func (a ActionTool) Definition(description string) tools.Definition {
	if description == "" {
		description = a.Description
	}
	return tools.Definition{Name: a.Name, Description: description, InputSchema: a.InputSchema()}
}

// Registry is a lookup over the generated tools a build actually registered.
// Family answers from this metadata rather than by splitting a name on "_",
// which would misread every plugin and MCP tool that happens to contain one.
type Registry struct {
	byName map[string]ActionTool
}

// NewRegistry indexes tools by name. A duplicate name is a build-time bug in
// toolgen's uniqueness check, so reject it rather than silently changing the
// trusted metadata selected by callers.
func NewRegistry(all ...ActionTool) *Registry {
	byName := make(map[string]ActionTool, len(all))
	for _, tool := range all {
		if _, exists := byName[tool.Name]; exists {
			panic("toolmeta: duplicate tool name " + tool.Name)
		}
		byName[tool.Name] = tool
	}
	return &Registry{byName: byName}
}

// Lookup returns the declaration for a tool name.
func (r *Registry) Lookup(name string) (ActionTool, bool) {
	if r == nil {
		return ActionTool{}, false
	}
	tool, ok := r.byName[name]
	return tool, ok
}

// Family returns the family a registered tool belongs to, or "" when the name
// is not a generated tool (plugins, MCP, hand-written core tools).
func (r *Registry) Family(name string) string {
	tool, ok := r.Lookup(name)
	if !ok {
		return ""
	}
	return tool.Family
}

// Names lists every registered tool name in a stable order.
func (r *Registry) Names() []string {
	if r == nil {
		return nil
	}
	out := make([]string, 0, len(r.byName))
	for name := range r.byName {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Tools returns every generated declaration in stable name order. The result
// is a value snapshot, so callers cannot mutate the registry's trusted index.
func (r *Registry) Tools() []ActionTool {
	if r == nil {
		return nil
	}
	names := r.Names()
	out := make([]ActionTool, 0, len(names))
	for _, name := range names {
		out = append(out, r.byName[name])
	}
	return out
}

// Match reports whether a selector from a delegate preset's tools: list or from
// excluded_tools refers to this tool. A selector is either an exact tool name
// or a family name; family membership comes from the declaration, so a plugin
// named "goal_helper" is never swept in by the family "goal".
func Match(selector string, tool ActionTool) bool {
	if selector == "" {
		return false
	}
	return selector == tool.Name || selector == tool.Family
}

// MatchAny reports whether any selector matches the tool.
func MatchAny(selectors []string, tool ActionTool) bool {
	for _, selector := range selectors {
		if Match(selector, tool) {
			return true
		}
	}
	return false
}

// MatchName is Match for a call site that only has a tool name: the runner's
// excluded_tools filter and the delegate preset whitelist both work off
// tools.Definition, which carries no family.
//
// A name this registry does not know matches only itself, so a plugin called
// "goal_helper" is not swept up by the family selector "goal".
func (r *Registry) MatchName(selector, name string) bool {
	if selector == "" {
		return false
	}
	if selector == name {
		return true
	}
	tool, ok := r.Lookup(name)
	if !ok {
		return false
	}
	return Match(selector, tool)
}

// MatchAnyName reports whether any selector matches the named tool.
func (r *Registry) MatchAnyName(selectors []string, name string) bool {
	for _, selector := range selectors {
		if r.MatchName(selector, name) {
			return true
		}
	}
	return false
}

// SelectsNothing reports whether a selector matches none of the given tools, so
// a caller can warn about a stale entry in a user-written preset instead of
// silently hiding every tool.
func (r *Registry) SelectsNothing(selector string, names []string) bool {
	for _, name := range names {
		if r.MatchName(selector, name) {
			return false
		}
	}
	return true
}

// Action returns the action a registered tool performs, or "" for a name this
// registry does not know. A split tool carries its action in its name, so an
// observability attribute can read it here instead of from an argument that no
// longer exists.
func (r *Registry) Action(name string) string {
	tool, ok := r.Lookup(name)
	if !ok {
		return ""
	}
	return tool.Action
}

// handWritten is the closed list of model-facing tools that legitimately have
// no declaration: core sandbox tools, the plugin and MCP surfaces, and the two
// protocols whose schema is not a REST contract. Everything else goes through
// toolgen, so its schema, its name and its Go input type stay in one place.
//
// Adding an entry is a design decision, not a convenience: it means the tool
// has no HTTP operation and no declaration file that could describe it. Say why
// in the PR (see rules/agent-tools.md §2).
var handWritten = map[string]bool{
	"bash":         true, // core sandbox
	"view_image":   true, // core sandbox
	"notify":       true, // channel dispatcher, not a REST resource
	"goal_control": true, // attempt protocol, one name with three schemas
	"code":         true, // meta-tool over the other tools
}

// HandWritten reports whether a tool name is an accepted exception to "every
// model-facing tool is generated". The list is closed: memory was the last
// union awaiting a split, so the pendingSplit map that held it is gone rather
// than kept empty — an empty second mechanism only invites a third entry.
func HandWritten(name string) bool {
	return handWritten[name]
}
