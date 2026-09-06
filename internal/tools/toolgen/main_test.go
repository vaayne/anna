package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOperationInputSchemaMergesParamsAndBody(t *testing.T) {
	decls := mustDecls(t, `
paths:
  /api/goals/{id}:
    parameters:
      - { name: id, in: path, required: true, schema: { type: string } }
      - { name: agentId, in: path, required: true, schema: { type: string } }
    post:
      summary: Update a goal
      x-agent-tool: { tool: goal, action: update }
      requestBody:
        content:
          application/json:
            schema: { $ref: '#/components/schemas/ThingInput' }
components:
  schemas:
    ThingInput:
      type: object
      required: [name, agent_id]
      properties:
        name: { type: string }
        agent_id: { type: string }
        enabled: { type: boolean }
`)
	if len(decls) != 1 {
		t.Fatalf("decls=%d, want 1", len(decls))
	}
	props := propertyMap(decls[0].Schema)
	if _, ok := props["agentId"]; ok {
		t.Fatal("agentId path param must be omitted")
	}
	if _, ok := props["agent_id"]; ok {
		t.Fatal("agent_id body field must be omitted")
	}
	for _, name := range []string{"id", "name", "enabled"} {
		if _, ok := props[name]; !ok {
			t.Fatalf("property %q missing from %#v", name, props)
		}
	}
	if got := decls[0].Required; len(got) != 2 || got[0] != "id" || got[1] != "name" {
		t.Fatalf("required=%v, want [id name]", got)
	}
}

func TestCollectOperationToolsUsesAuthoredPluginIdentity(t *testing.T) {
	decls := mustDecls(t, `
paths:
  /api/scheduler/jobs:
    get:
      summary: List scheduler jobs
      x-agent-tool: { tool: scheduler, resource: job, action: list }
  /api/recally/feeds:
    get:
      summary: List recally feeds
      x-agent-tool: { tool: recally, action: feed_list }
  /api/email/accounts:
    get:
      summary: List email accounts
      x-agent-tool: { tool: email, action: account_list }
  /api/goals:
    get:
      summary: List goals
      x-agent-tool: { tool: goal, action: list }
components:
  schemas: {}
`)
	got := map[string]toolDecl{}
	for _, decl := range decls {
		got[decl.Family] = decl
	}
	for family, want := range map[string]struct {
		pluginID  string
		namespace string
		localName string
		name      string
	}{
		"email":     {"system/email", "email", "account_list", "email__account_list"},
		"recally":   {"system/recally", "recally", "feed_list", "recally__feed_list"},
		"scheduler": {"system/scheduler", "scheduler", "job_list", "scheduler__job_list"},
		"goal":      {"", "", "", "goal_list"},
	} {
		decl, ok := got[family]
		if !ok {
			t.Fatalf("missing %s declaration", family)
		}
		if decl.PluginID != want.pluginID || decl.Namespace != want.namespace || decl.LocalName != want.localName || decl.Name != want.name {
			t.Fatalf("%s identity = (%q, %q, %q, %q), want (%q, %q, %q, %q)", family, decl.PluginID, decl.Namespace, decl.LocalName, decl.Name, want.pluginID, want.namespace, want.localName, want.name)
		}
	}
	if err := validate(decls); err != nil {
		t.Fatalf("validate generated identities: %v", err)
	}
}

func TestValidateRejectsPluginIdentityCollisions(t *testing.T) {
	plugin := func(family, pluginID, namespace, local, name string) toolDecl {
		return toolDecl{
			Family: family, Action: "list", Name: name, PluginID: pluginID, Namespace: namespace, LocalName: local,
			Description: "List items.", Schema: objectSchema(nil, nil), SourceLocation: family,
			Package: domainPackage{Dir: family, Package: family, PluginID: pluginID, Namespace: namespace, Split: true},
		}
	}
	err := validate([]toolDecl{
		plugin("first", "system/first", "shared", "list", "shared__list"),
		plugin("second", "system/second", "shared", "list", "shared__list_2"),
	})
	if err == nil || !strings.Contains(err.Error(), `plugin namespace "shared" is already authored by plugin "system/first"`) {
		t.Fatalf("collision error=%v, want namespace ownership failure", err)
	}
}

func TestValidateRejectsOverlongPluginToolName(t *testing.T) {
	local := strings.Repeat("x", maxProviderToolNameLen)
	err := validate([]toolDecl{{
		Family: "long", Action: "list", Name: "long__" + local, PluginID: "system/long", Namespace: "long", LocalName: local,
		Description: "List long items.", Schema: objectSchema(nil, nil), SourceLocation: "long",
		Package: domainPackage{Dir: "long", Package: "long", PluginID: "system/long", Namespace: "long", Split: true},
	}})
	if err == nil || !strings.Contains(err.Error(), "at most 64 characters") {
		t.Fatalf("overlong error=%v, want provider length failure", err)
	}
}

func TestOperationToolInputOverridesHTTPBody(t *testing.T) {
	decls := mustDecls(t, `
paths:
  /api/providers:
    post:
      summary: Create a provider
      x-agent-tool:
        tool: settings_provider
        action: create
        input: { $ref: '#/components/schemas/ProviderToolInput' }
      requestBody:
        content:
          application/json:
            schema: { $ref: '#/components/schemas/Provider' }
components:
  schemas:
    Provider:
      type: object
      properties:
        api_key: { type: string }
        models: { type: object }
    ProviderToolInput:
      type: object
      properties:
        models:
          type: object
          additionalProperties:
            type: object
            additionalProperties: false
            properties:
              enabled: { type: boolean }
`)
	if len(decls) != 1 {
		t.Fatalf("decls=%d, want 1", len(decls))
	}
	models := propertyMap(decls[0].Schema)["models"].(map[string]any)
	model := models["additionalProperties"].(map[string]any)
	if model["additionalProperties"] != false {
		t.Fatalf("tool model is not sealed: %#v", model)
	}
	if _, exposed := propertyMap(decls[0].Schema)["api_key"]; exposed {
		t.Fatalf("tool input retained HTTP-only api_key: %#v", decls[0].Schema)
	}
}

func TestToolSchemaIsPlainObjectWithActionEnum(t *testing.T) {
	schema := toolSchema([]toolDecl{
		{Action: "get", Schema: objectSchema(map[string]any{"id": map[string]any{"type": "string"}}, []string{"id"}), Required: []string{"id"}},
		{Action: "list", Schema: objectSchema(nil, nil)},
	})
	props := schema["properties"].(map[string]any)
	action := props["action"].(map[string]any)
	if len(action["enum"].([]any)) != 2 {
		t.Fatalf("action enum=%#v, want two actions", action["enum"])
	}
	// Per-action requiredness cannot live in `required`, so it rides in the
	// action property's description for the model.
	if desc, _ := action["description"].(string); desc != "Required parameters by action: get(id)." {
		t.Errorf("action description=%q, want per-action required list", desc)
	}
	// OpenAI-compatible providers reject function schemas carrying combinators
	// or constraints at the top level; the wire schema must be a plain object.
	if schema["type"] != "object" {
		t.Fatalf("top-level type=%#v, want object", schema["type"])
	}
	for _, banned := range []string{"oneOf", "anyOf", "allOf", "enum", "const", "not"} {
		if _, ok := schema[banned]; ok {
			t.Errorf("top-level schema must not carry %q: %#v", banned, schema[banned])
		}
	}
}

func TestToolSchemaHoistsBranchPropertiesToTopLevel(t *testing.T) {
	schema := toolSchema([]toolDecl{
		{Action: "create", Schema: objectSchema(map[string]any{"title": map[string]any{"type": "string"}}, []string{"title"})},
		{Action: "list", Schema: objectSchema(map[string]any{"q": map[string]any{"type": "string"}}, nil)},
	})
	props := schema["properties"].(map[string]any)
	// Every branch field is visible at the top level, but only `action` is required.
	for _, want := range []string{"action", "title", "q"} {
		if _, ok := props[want]; !ok {
			t.Errorf("top-level properties missing %q: %#v", want, props)
		}
	}
	// Per-action requiredness (create requires title) is deliberately absent
	// from the schema; Dispatch/DecodeInput enforce it at runtime.
	if req := schema["required"].([]any); len(req) != 1 || req[0] != "action" {
		t.Fatalf("top-level required=%#v, want [action]", req)
	}
}

func TestToolSchemaLoosensConflictingBranchTypes(t *testing.T) {
	// `inputs` is an object for one action and an array for another; the hoisted
	// top-level copy must not commit to either type, or it would contradict a branch.
	schema := toolSchema([]toolDecl{
		{Action: "run", Schema: objectSchema(map[string]any{"inputs": map[string]any{"type": "object"}}, nil)},
		{Action: "save", Schema: objectSchema(map[string]any{"inputs": map[string]any{"type": "array"}}, nil)},
	})
	inputs := schema["properties"].(map[string]any)["inputs"].(map[string]any)
	if _, ok := inputs["type"]; ok {
		t.Fatalf("top-level inputs kept a conflicting type: %#v", inputs)
	}
	// The dropped type is replaced by a description naming each action's shape.
	if desc, _ := inputs["description"].(string); desc != "Type depends on action — run: object; save: array." {
		t.Errorf("inputs description=%q, want per-action type note", desc)
	}
	// A field all branches agree on keeps its type.
	schema2 := toolSchema([]toolDecl{
		{Action: "a", Schema: objectSchema(map[string]any{"name": map[string]any{"type": "string"}}, nil)},
		{Action: "b", Schema: objectSchema(map[string]any{"name": map[string]any{"type": "string", "minLength": 1}}, nil)},
	})
	name := schema2["properties"].(map[string]any)["name"].(map[string]any)
	if name["type"] != "string" {
		t.Errorf("agreed type dropped: %#v", name)
	}
	if _, ok := name["minLength"]; ok {
		t.Errorf("per-branch constraint leaked to top level: %#v", name)
	}
}

func TestRestrictAnnotationNarrowsPropertyEnum(t *testing.T) {
	decls := mustDecls(t, `
paths:
  /api/vault:
    get:
      summary: List vault entries
      x-agent-tool: { tool: vault, action: list, restrict: { scope: [user, user_agent] } }
      parameters:
        - name: scope
          in: query
          schema: { type: string, enum: [user, user_agent, system, system_agent], default: user }
components:
  schemas: {}
`)
	scope := propertyMap(decls[0].Schema)["scope"].(map[string]any)
	enum := scope["enum"].([]any)
	if len(enum) != 2 || enum[0] != "user" || enum[1] != "user_agent" {
		t.Fatalf("scope enum=%#v, want [user user_agent]", enum)
	}
}

func TestRequireAnnotationMarksOptionalFieldRequired(t *testing.T) {
	decls := mustDecls(t, `
paths:
  /api/email/send:
    post:
      summary: Send an email
      x-agent-tool: { tool: email, action: send, require: [idempotency_key] }
      requestBody:
        content:
          application/json:
            schema:
              type: object
              required: [to, subject, body]
              properties:
                to: { type: array, items: { type: string } }
                subject: { type: string }
                body: { type: string }
                idempotency_key: { type: string }
components:
  schemas: {}
`)
	assertStrings(t, "required", decls[0].Required, []string{"body", "idempotency_key", "subject", "to"})
}

func TestOptionalAnnotationMarksRequiredFieldOptional(t *testing.T) {
	decls := mustDecls(t, `
paths:
  /api/feeds/{id}/poll:
    parameters:
      - { name: id, in: path, required: true, schema: { type: string } }
    post:
      summary: Poll feeds
      x-agent-tool: { tool: recally, action: feed_poll, optional: [id] }
components:
  schemas: {}
`)
	assertStrings(t, "required", decls[0].Required, nil)
}

func TestMultiActionAnnotationOmitFixedFields(t *testing.T) {
	decls := mustDecls(t, `
paths:
  /api/jobs/{jobId}/{flowId}:
    parameters:
      - { name: jobId, in: path, required: true, schema: { type: string } }
      - { name: flowId, in: path, required: true, schema: { type: string } }
      - { name: agentId, in: path, required: true, schema: { type: string } }
    patch:
      summary: Update a job
      x-agent-tool:
        - { tool: scheduler, resource: job, action: update }
        - { tool: scheduler, resource: job, action: pause, fixed: { enabled: false }, body: false }
      requestBody:
        content:
          application/json:
            schema:
              type: object
              properties:
                enabled: { type: boolean }
                name: { type: string }
components:
  schemas: {}
`)
	byAction := byAction(decls)
	if _, ok := byAction["update"]; !ok {
		t.Fatal("update action missing")
	}
	pauseProps := propertyMap(byAction["pause"].Schema)
	if _, ok := pauseProps["enabled"]; ok {
		t.Fatal("fixed enabled field must be omitted from pause input")
	}
	if _, ok := pauseProps["id"]; !ok {
		t.Fatal("jobId path param should become model-friendly id on a job-resource tool")
	}
	if _, ok := pauseProps["flow_id"]; !ok {
		t.Fatal("flowId path param should become model-friendly flow_id")
	}
}

// The implicit rule this replaces ("a fixed field and no restrict means
// params-only") made an unrelated annotation change the shape of the input, and
// forced a placeholder restrict on the share actions to opt out of it.
func TestBodyFalseTakesParamsOnly(t *testing.T) {
	decls := mustDecls(t, `
paths:
  /api/agents/{agentId}/scheduler/jobs/{jobId}:
    parameters:
      - { name: agentId, in: path, required: true, schema: { type: string } }
      - { name: jobId, in: path, required: true, schema: { type: string } }
    patch:
      summary: Update a scheduler job
      x-agent-tool:
        - { tool: scheduler, resource: job, action: pause, fixed: { enabled: false }, body: false }
        - { tool: scheduler, resource: job, action: update, fixed: { enabled: false } }
      requestBody:
        content:
          application/json:
            schema:
              type: object
              properties:
                enabled: { type: boolean }
                name: { type: string }
                schedule: { type: string }
components:
  schemas: {}
`)
	byAction := byAction(decls)
	pause := propertyMap(byAction["pause"].Schema)
	if _, ok := pause["name"]; ok {
		t.Fatalf("body:false must drop body fields: %#v", pause)
	}
	if _, ok := pause["id"]; !ok {
		t.Fatalf("body:false must keep path params: %#v", pause)
	}
	// Without body:false the body still merges, even though a field is fixed.
	update := propertyMap(byAction["update"].Schema)
	for _, want := range []string{"name", "schedule"} {
		if _, ok := update[want]; !ok {
			t.Fatalf("body field %q dropped without body:false: %#v", want, update)
		}
	}
	if _, ok := update["enabled"]; ok {
		t.Fatalf("fixed field must still be removed: %#v", update)
	}
}

func TestOmitAnnotationDropsPropertyWithoutTouchingTheBody(t *testing.T) {
	decls := mustDecls(t, `
paths:
  /api/goals:
    post:
      summary: Create a goal
      x-agent-tool: { tool: goal, action: create, omit: [kind, activate] }
      requestBody:
        content:
          application/json:
            schema:
              type: object
              required: [title, kind]
              properties:
                title: { type: string }
                kind: { type: string }
                activate: { type: boolean }
                description: { type: string }
components:
  schemas: {}
`)
	props := propertyMap(decls[0].Schema)
	for _, gone := range []string{"kind", "activate"} {
		if _, ok := props[gone]; ok {
			t.Fatalf("omitted property %q still present: %#v", gone, props)
		}
	}
	if _, ok := props["description"]; !ok {
		t.Fatalf("omit must not degrade the input to params-only: %#v", props)
	}
	// An omitted property cannot stay required.
	assertStrings(t, "required", decls[0].Required, []string{"title"})
}

func TestRenameAnnotationRenamesPropertyAndRequiredEntry(t *testing.T) {
	decls := mustDecls(t, `
paths:
  /api/goals/{id}/save-as-workflow:
    parameters:
      - { name: id, in: path, required: true, schema: { type: string } }
    post:
      summary: Save a goal as a workflow
      x-agent-tool: { tool: workflow, action: save, rename: { id: goal_id } }
      requestBody:
        content:
          application/json:
            schema:
              type: object
              required: [name]
              properties:
                name: { type: string }
components:
  schemas: {}
`)
	props := propertyMap(decls[0].Schema)
	if _, ok := props["id"]; ok {
		t.Fatalf("renamed property still present under its old name: %#v", props)
	}
	if _, ok := props["goal_id"]; !ok {
		t.Fatalf("renamed property missing: %#v", props)
	}
	assertStrings(t, "required", decls[0].Required, []string{"goal_id", "name"})
}

func TestToolFieldNameMapsIdentifiersByResource(t *testing.T) {
	cases := []struct{ param, resource, want string }{
		{"id", "job", "id"},
		{"jobId", "job", "id"},
		{"job_id", "job", "id"},
		{"jobId", "scheduler", "job_id"},
		{"feedId", "recally", "feed_id"},
		{"flowId", "oauth", "flow_id"},
		{"upstreamId", "goal", "upstream_id"},
		{"uid", "email", "uid"},
		{"provider", "oauth", "provider"},
	}
	for _, tc := range cases {
		if got := toolFieldName(tc.param, tc.resource); got != tc.want {
			t.Errorf("toolFieldName(%q, %q)=%q, want %q", tc.param, tc.resource, got, tc.want)
		}
	}
}

func TestBatchAnnotationWrapsRequestBody(t *testing.T) {
	decls := mustDecls(t, `
paths:
  /api/articles:
    post:
      summary: Save articles
      x-agent-tool:
        tool: recally
        action: save
        batch: articles
        add:
          content_path: { type: string, description: sandbox path }
      requestBody:
        content:
          application/json:
            schema:
              type: object
              required: [url]
              properties:
                url: { type: string }
                title: { type: string }
                agent_id: { type: string }
components:
  schemas: {}
`)
	decl := decls[0]
	if decl.Batch != "articles" {
		t.Fatalf("Batch=%q, want articles", decl.Batch)
	}
	articles := propertyMap(decl.Schema)["articles"].(map[string]any)
	if articles["minItems"] != 1 || articles["maxItems"] != 20 {
		t.Fatalf("articles bounds=%#v", articles)
	}
	itemProps := articles["items"].(map[string]any)["properties"].(map[string]any)
	if _, ok := itemProps["agent_id"]; ok {
		t.Fatal("identity field must be omitted from batch item")
	}
	if _, ok := itemProps["url"]; !ok {
		t.Fatal("url missing from batch item")
	}
	if contentPath, ok := itemProps["content_path"].(map[string]any); !ok || contentPath["type"] != "string" {
		t.Fatalf("tool-only batch addition missing: %#v", itemProps["content_path"])
	}
	assertStrings(t, "required", decl.Required, []string{"articles"})
	// Both the wrapper and the item object are sealed: an unknown key in an
	// item is exactly the mistake the split was supposed to make visible.
	schema := actionSchema(decl)
	if schema["additionalProperties"] != false {
		t.Fatalf("batch wrapper not sealed: %#v", schema)
	}
	item := schema["properties"].(map[string]any)["articles"].(map[string]any)["items"].(map[string]any)
	if item["additionalProperties"] != false {
		t.Fatalf("batch item not sealed: %#v", item)
	}
}

func TestActionSchemaKeepsDeclaredAdditionalProperties(t *testing.T) {
	// A free-form map property declares its own additionalProperties; sealing
	// the tool input must not turn it into a closed object.
	decl := toolDecl{
		Action: "save",
		Batch:  "articles",
		Schema: batchInputSchema("articles", map[string]any{
			"type":                 "object",
			"additionalProperties": map[string]any{"type": "string"},
			"properties":           map[string]any{"url": map[string]any{"type": "string"}},
		}),
	}
	item := actionSchema(decl)["properties"].(map[string]any)["articles"].(map[string]any)["items"].(map[string]any)
	declared, ok := item["additionalProperties"].(map[string]any)
	if !ok || declared["type"] != "string" {
		t.Fatalf("declared additionalProperties overwritten: %#v", item["additionalProperties"])
	}
}

func TestRenderToolUsesPackageTrimmedNamesAndCamelActions(t *testing.T) {
	// The package is a literal, not a domainPackages lookup: this test pins the
	// union render path, which must keep working after the last operation-backed
	// family flips to Split.
	out, err := renderTool("goal", domainPackage{Dir: "goal", Package: "goal"}, []toolDecl{{Family: "goal", Action: "create", Schema: objectSchema(nil, nil)}})
	if err != nil {
		t.Fatalf("render goal: %v", err)
	}
	text := string(out)
	if !strings.Contains(text, "package goal") || !strings.Contains(text, `const ToolName = "goal"`) || !strings.Contains(text, "type ToolCreateInput struct") {
		t.Fatalf("goal render did not use package/fallback name:\n%s", text)
	}
	// A union schema legitimately carries every action's fields, so it keeps
	// the lenient decoder.
	if !strings.Contains(text, "tools.DecodeInput(args") {
		t.Fatalf("union render must keep the lenient decoder:\n%s", text)
	}
}

func TestRenderPreserveEmptyStringAsPointer(t *testing.T) {
	out, err := renderTool("settings_agent", domainPackages["settings_agent"], []toolDecl{{
		Family: "settings_agent", Action: "update", Name: "settings_agent_update", Package: domainPackages["settings_agent"],
		Schema: objectSchema(map[string]any{"model": map[string]any{"type": "string", "x-stella-preserve-empty": true}}, nil),
	}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "Model *string") {
		t.Fatalf("preserve_empty string did not preserve presence:\n%s", out)
	}
}

func TestRenderSplitToolEmitsPrefixNamesAndStrictDecode(t *testing.T) {
	out, err := renderTool("recally", domainPackages["recally"], []toolDecl{
		{Family: "recally", Action: "article_list", Name: "recally__article_list", PluginID: "system/recally", Namespace: "recally", LocalName: "article_list", Schema: objectSchema(nil, nil)},
		{Family: "recally", Resource: "feed", Action: "feed_add", Name: "recally__feed_add", PluginID: "system/recally", Namespace: "recally", LocalName: "feed_add", Schema: objectSchema(nil, nil)},
	})
	if err != nil {
		t.Fatalf("render recally: %v", err)
	}
	text := string(out)
	if !strings.Contains(text, "ArticleList(context.Context, ArticleListInput)") || strings.Contains(text, "Article_list") {
		t.Fatalf("recally render did not camel-case action:\n%s", text)
	}
	// A split domain emits one exact-schema tool per action instead of a union
	// with an `action` enum, so the provider can validate each call.
	if !strings.Contains(text, `{Name: "recally__article_list", PluginID: "system/recally", Namespace: "recally", LocalName: "article_list", Family: "recally", Action: "article_list"`) || strings.Contains(text, `"action"`) {
		t.Fatalf("recally render did not split into per-action tools:\n%s", text)
	}
	for _, want := range []string{
		`const ToolPrefix = "recally"`,
		"func ToolNames() []string",
		"type ActionTool = toolmeta.ActionTool",
		`Resource: "feed"`,
		"tools.DecodeInputStrict(args",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("split render missing %q:\n%s", want, text)
		}
	}
	// ToolName named the union that no longer exists; callers gate on
	// ToolPrefix or ToolNames() now.
	if strings.Contains(text, "const ToolName =") {
		t.Fatalf("split render must not emit the union ToolName:\n%s", text)
	}
}

func TestCollectStandaloneToolsDeclaresToolsWithoutAnHTTPOperation(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "session.yaml"), `
family: session
package: agent/session/access
tools:
  - action: list
    description: List the user's sessions.
    input:
      type: object
      properties:
        q: { type: string }
  - resource: message
    action: send
    description: Send a message to another session.
    input: { $ref: '#/components/schemas/SendInput' }
    required: [session_id]
`)
	doc := mustDoc(t, []byte(`
paths:
  /api/goals:
    get:
      summary: List goals
      x-agent-tool: { tool: goal, action: list }
components:
  schemas:
    SendInput:
      type: object
      properties:
        session_id: { type: string }
        text: { type: string }
`))
	decls, err := collectStandaloneTools(dir, doc)
	if err != nil {
		t.Fatalf("collectStandaloneTools: %v", err)
	}
	if len(decls) != 2 {
		t.Fatalf("decls=%d, want 2", len(decls))
	}
	list := decls[0]
	if list.Name != "session_list" || list.Family != "session" || !list.Declared {
		t.Fatalf("list decl=%#v", list)
	}
	if list.Package.Dir != "agent/session/access" || list.Package.Package != "access" || !list.Package.Split {
		t.Fatalf("package target=%#v, want the declared dir, its base package, split", list.Package)
	}
	send := decls[1]
	if send.Name != "session_message_send" {
		t.Fatalf("Name=%q, want family_resource_action", send.Name)
	}
	// The $ref resolves against the assembled OpenAPI components, so a
	// declared tool can still reuse a schema the API already defines.
	if _, ok := propertyMap(send.Schema)["text"]; !ok {
		t.Fatalf("$ref input not resolved: %#v", send.Schema)
	}
	assertStrings(t, "required", send.Required, []string{"session_id"})
	// The description is model-facing prose, so it ships in the generated file.
	out, err := renderTool("session", decls[0].Package, decls)
	if err != nil {
		t.Fatalf("render session: %v", err)
	}
	if !strings.Contains(string(out), `Description: "List the user's sessions."`) {
		t.Fatalf("declared description not emitted:\n%s", out)
	}
}

func TestCollectStandaloneToolsRejectsUnknownKeys(t *testing.T) {
	dir := t.TempDir()
	// `name` is not a field: the spelling is `name_override`. Silently ignoring
	// it would ship a tool under a name nobody chose.
	write(t, filepath.Join(dir, "session.yaml"), `
family: session
package: agent/session/access
tools:
  - action: list
    name: session_all
    description: List sessions.
    input: { type: object }
`)
	if _, err := collectStandaloneTools(dir, mustDoc(t, []byte(minimalDoc))); err == nil {
		t.Fatal("unknown declaration key must fail the build")
	}
}

func TestCollectStandaloneToolsTreatsMissingDirectoryAsEmpty(t *testing.T) {
	decls, err := collectStandaloneTools(filepath.Join(t.TempDir(), "absent"), mustDoc(t, []byte(minimalDoc)))
	if err != nil || len(decls) != 0 {
		t.Fatalf("decls=%v err=%v, want no tools and no error", decls, err)
	}
}

func TestParseActionSpecsRejectsUnknownModifier(t *testing.T) {
	doc := mustDoc(t, []byte(`
paths:
  /api/goals:
    get:
      summary: List goals
      x-agent-tool: { tool: goal, action: list, resources: [thing] }
components:
  schemas: {}
`))
	if _, err := collectOperationTools(doc); err == nil {
		t.Fatal("an unknown modifier must fail the build, not be ignored")
	}
}

func TestValidateRejectsBadDeclarations(t *testing.T) {
	split := domainPackage{Dir: "recally", Package: "recally", Split: true}
	cases := []struct {
		name  string
		decls []toolDecl
		want  string
	}{
		{
			name: "duplicate name",
			decls: []toolDecl{
				{Family: "recally", Action: "get", Name: "recally_get", Description: "a", Package: split, SourceLocation: "one", Schema: objectSchema(nil, nil)},
				{Family: "recally", Action: "read", Name: "recally_get", Description: "b", Package: split, SourceLocation: "two", Schema: objectSchema(nil, nil)},
			},
			want: `tool name "recally_get" is already declared`,
		},
		{
			name: "provider-illegal name",
			decls: []toolDecl{
				{Family: "recally", Action: "get", Name: "recally get!", Description: "a", Package: split, SourceLocation: "one", Schema: objectSchema(nil, nil)},
			},
			want: "must match",
		},
		{
			name: "camelCase property",
			decls: []toolDecl{
				{Family: "recally", Action: "get", Name: "recally_get", Description: "a", Package: split, SourceLocation: "one", Schema: objectSchema(map[string]any{"articleId": map[string]any{"type": "string"}}, nil)},
			},
			want: `property "articleId" must match`,
		},
		{
			name: "missing description",
			decls: []toolDecl{
				{Family: "recally", Action: "get", Name: "recally_get", Package: split, SourceLocation: "one", Schema: objectSchema(nil, nil)},
			},
			want: "tool has no description",
		},
		{
			name: "action discriminator on a split tool",
			decls: []toolDecl{
				{Family: "recally", Action: "get", Name: "recally_get", Description: "a", Package: split, SourceLocation: "one", Schema: objectSchema(map[string]any{"action": map[string]any{"type": "string"}}, nil)},
			},
			want: "must not carry an `action` property",
		},
		{
			name: "two families writing one package",
			decls: []toolDecl{
				{Family: "recally", Action: "get", Name: "recally_get", Description: "a", Package: split, SourceLocation: "one", Schema: objectSchema(nil, nil)},
				{Family: "reader", Action: "get", Name: "reader_get", Description: "b", Package: split, SourceLocation: "two", Schema: objectSchema(nil, nil)},
			},
			want: "already generated for family",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validate(tc.decls)
			if err == nil {
				t.Fatalf("validate accepted %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error=%q, want it to mention %q", err, tc.want)
			}
			if !strings.Contains(err.Error(), tc.decls[len(tc.decls)-1].SourceLocation) {
				t.Fatalf("error=%q, want the source location", err)
			}
		})
	}
}

func TestValidateAcceptsAUnionFamilySharingOneName(t *testing.T) {
	union := domainPackage{Dir: "goal", Package: "goal"}
	err := validate([]toolDecl{
		{Family: "goal", Action: "list", Name: "goal_list", Description: "a", Package: union, SourceLocation: "one", Schema: objectSchema(nil, nil)},
		{Family: "goal", Action: "get", Name: "goal_get", Description: "b", Package: union, SourceLocation: "two", Schema: objectSchema(map[string]any{"action": map[string]any{"type": "string"}}, nil)},
	})
	if err != nil {
		t.Fatalf("validate rejected a union family: %v", err)
	}
}

const minimalDoc = `
paths:
  /api/goals:
    get:
      summary: List goals
      x-agent-tool: { tool: goal, action: list }
components:
  schemas: {}
`

func mustDoc(t *testing.T, data []byte) *openAPIDoc {
	t.Helper()
	doc, err := parseDoc(data)
	if err != nil {
		t.Fatalf("parseDoc: %v", err)
	}
	return doc
}

func mustDecls(t *testing.T, spec string) []toolDecl {
	t.Helper()
	decls, err := collectOperationTools(mustDoc(t, []byte(spec)))
	if err != nil {
		t.Fatalf("collectOperationTools: %v", err)
	}
	return decls
}

func byAction(decls []toolDecl) map[string]toolDecl {
	out := map[string]toolDecl{}
	for _, decl := range decls {
		out[decl.Action] = decl
	}
	return out
}

func assertStrings(t *testing.T, what string, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s=%v, want %v", what, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s=%v, want %v", what, got, want)
		}
	}
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func objectSchema(props map[string]any, required []string) map[string]any {
	if props == nil {
		props = map[string]any{}
	}
	schema := map[string]any{"type": "object", "properties": props}
	for _, req := range required {
		addRequired(schema, req)
	}
	return schema
}

const fixtureDir = "../../../test/toolgenfixture"

// TestGeneratedFixtureIsCurrent closes the loop the unit tests leave open: a
// real declaration file, rendered by the real pipeline, compared against Go
// that `go build ./...` compiles against the real toolmeta and pkg/tools. It is
// what catches a generated type name colliding with hand-written code, or a
// render that produces something that is not valid Go.
//
// Regenerate with TOOLGEN_UPDATE_FIXTURE=1 go test ./internal/tools/toolgen.
func TestGeneratedFixtureIsCurrent(t *testing.T) {
	decls, err := collectStandaloneTools(filepath.Join(fixtureDir, "agent-tools"), mustDoc(t, []byte(minimalDoc)))
	if err != nil {
		t.Fatalf("collectStandaloneTools: %v", err)
	}
	if err := validate(decls); err != nil {
		t.Fatalf("validate: %v", err)
	}
	group := groupByOutput(decls)["session\x00internal\x00test/toolgenfixture"]
	got, err := renderTool("session", group[0].Package, group)
	if err != nil {
		t.Fatalf("renderTool: %v", err)
	}
	path := filepath.Join(fixtureDir, "tool_gen.go")
	if os.Getenv("TOOLGEN_UPDATE_FIXTURE") == "1" {
		write(t, path, string(got))
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("fixture is stale; rerun with TOOLGEN_UPDATE_FIXTURE=1\n--- got ---\n%s", got)
	}
	// The names the fixture package depends on, spelled out so a rename here
	// reads as the deliberate change it is.
	for _, want := range []string{
		"type SessionSendInput struct",
		"type SessionListInput struct",
		`{Name: "session_send", Family: "session", Action: "send", Description: `,
		"tools.DecodeInputStrict(args, &in, []string{\"message\", \"session_id\"})",
	} {
		if !strings.Contains(string(got), want) {
			t.Errorf("generated fixture missing %q", want)
		}
	}
	// A declared family must not reuse the bare action name: the fixture
	// package (like internal/agent/session/access) already has a SendInput.
	if strings.Contains(string(got), "type SendInput struct") {
		t.Error("generated type collides with the hand-written SendInput")
	}
}

func TestCollectStandaloneToolsRejectsRelativeRefs(t *testing.T) {
	doc := mustDoc(t, []byte(`
paths:
  /api/goals:
    get:
      summary: List goals
      x-agent-tool: { tool: goal, action: list }
components:
  schemas:
    Outer:
      type: object
      properties:
        inner: { $ref: '../../components.yaml#/components/schemas/Inner' }
    Inner:
      type: object
      properties: { a: { type: string } }
`))
	for _, ref := range []string{
		"../../components.yaml#/components/schemas/Inner",
		"#/components/schemas/Outer", // resolves, then hits the nested relative ref
	} {
		dir := t.TempDir()
		write(t, filepath.Join(dir, "session.yaml"), `
family: session
package: agent/session/access
tools:
  - action: list
    description: List sessions.
    input: { $ref: '`+ref+`' }
`)
		if _, err := collectStandaloneTools(dir, doc); err == nil {
			t.Fatalf("declaration resolved a relative ref %q", ref)
		} else if !strings.Contains(err.Error(), "unsupported ref") {
			t.Fatalf("error=%q, want an unsupported-ref error", err)
		}
	}
}

func TestValidateRejectsUnsatisfiableRequired(t *testing.T) {
	split := domainPackage{Dir: "recally", Package: "recally", Split: true}
	err := validate([]toolDecl{{
		Family: "recally", Action: "get", Name: "recally_get", Description: "a",
		Package: split, SourceLocation: "one",
		Schema: objectSchema(map[string]any{"id": map[string]any{"type": "string"}}, []string{"id", "session_id"}),
		// The required list names a field the input does not have, so no
		// argument object can satisfy the schema.
		Required: []string{"id", "session_id"},
	}})
	if err == nil || !strings.Contains(err.Error(), `required field "session_id" is not a property`) {
		t.Fatalf("err=%v, want an unsatisfiable-required error", err)
	}
}

func TestValidateTreatsRootAsPartOfOutputDirectory(t *testing.T) {
	decls := []toolDecl{
		{Family: "internal_email", Action: "list", Name: "internal_email_list", Description: "internal", Package: domainPackage{Root: "internal", Dir: "email", Package: "email", Split: true}, Schema: objectSchema(nil, nil), SourceLocation: "internal"},
		{Family: "email", Action: "list", Name: "email_list", Description: "plugin", Package: domainPackage{Root: "plugins", Dir: "email", Package: "email", Split: true}, Schema: objectSchema(nil, nil), SourceLocation: "plugins"},
	}
	if err := validate(decls); err != nil {
		t.Fatalf("validate same directory under separate roots: %v", err)
	}
	grouped := groupByOutput([]toolDecl{
		{Family: "email", Action: "list", Package: domainPackage{Root: "internal", Dir: "email"}},
		{Family: "email", Action: "send", Package: domainPackage{Root: "plugins", Dir: "email"}},
	})
	if got := len(grouped); got != 2 {
		t.Fatalf("groupByOutput count=%d, want 2 for one family in separate roots", got)
	}
}

// The generated file for a family is the whole story: creating the first
// declaration must write it, and deleting the last one must remove it. A stale
// file keeps a removed tool registered and drifts past `git diff`, because
// nothing changed.
func TestRunEmitsOneFamilyIntoMultiplePackages(t *testing.T) {
	declDir := t.TempDir()
	outRoot := t.TempDir()
	spec := filepath.Join(t.TempDir(), "docs_spec.yaml")
	write(t, spec, minimalDoc)
	write(t, filepath.Join(declDir, "demo.yaml"), `
family: demo
package: alpha
tools:
  - action: search
    description: Search the web.
    input: { type: object, properties: { q: { type: string } } }
  - action: fetch
    package: beta
    description: Fetch one page.
    input: { type: object, properties: { url: { type: string } } }
`)

	if err := run(spec, declDir, outRoot); err != nil {
		t.Fatalf("run: %v", err)
	}
	for _, dir := range []string{"alpha", "beta"} {
		data, err := os.ReadFile(filepath.Join(outRoot, "internal", dir, generatedFileName("demo")))
		if err != nil {
			t.Fatalf("read %s output: %v", dir, err)
		}
		if !strings.Contains(string(data), "package "+dir) {
			t.Fatalf("%s output has wrong package:\n%s", dir, data)
		}
	}
}

func TestRunCreatesAndPrunesGeneratedFiles(t *testing.T) {
	declDir := t.TempDir()
	outRoot := t.TempDir()
	spec := filepath.Join(t.TempDir(), "docs_spec.yaml")
	write(t, spec, minimalDoc)
	generated := filepath.Join(outRoot, "internal", "agent", "session", "access", generatedFileName("session"))

	write(t, filepath.Join(declDir, "session.yaml"), `
family: session
package: agent/session/access
tools:
  - action: list
    description: List sessions.
    input: { type: object, properties: { q: { type: string } } }
`)
	if err := run(spec, declDir, outRoot); err != nil {
		t.Fatalf("run: %v", err)
	}
	if _, err := os.Stat(generated); err != nil {
		t.Fatalf("first run did not create the output: %v", err)
	}
	// A file the generator did not write is never touched, whatever it is named.
	handWritten := filepath.Join(outRoot, "keepme", generatedFileName("handwritten"))
	if err := os.MkdirAll(filepath.Dir(handWritten), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, handWritten, "package keepme\n")

	if err := os.Remove(filepath.Join(declDir, "session.yaml")); err != nil {
		t.Fatal(err)
	}
	if err := run(spec, declDir, outRoot); err != nil {
		t.Fatalf("run after removing the declaration: %v", err)
	}
	if _, err := os.Stat(generated); !os.IsNotExist(err) {
		t.Fatalf("stale generated file survived: %v", err)
	}
	if _, err := os.Stat(handWritten); err != nil {
		t.Fatalf("hand-written file was pruned: %v", err)
	}
}

func TestRunPrunesLastPluginFamily(t *testing.T) {
	declDir := t.TempDir()
	outRoot := t.TempDir()
	spec := filepath.Join(t.TempDir(), "docs_spec.yaml")
	write(t, spec, minimalDoc)
	declaration := filepath.Join(declDir, "email.yaml")
	write(t, declaration, `
family: email
package: email
tools:
  - action: test
    description: Test email routing.
    input: { type: object, properties: { value: { type: string } } }
`)
	generated := filepath.Join(outRoot, "plugins", "email", generatedFileName("email"))
	if err := run(spec, declDir, outRoot); err != nil {
		t.Fatalf("run with plugin declaration: %v", err)
	}
	if _, err := os.Stat(generated); err != nil {
		t.Fatalf("plugin output was not created: %v", err)
	}
	handWritten := filepath.Join(outRoot, "plugins", "email", generatedFileName("handwritten"))
	write(t, handWritten, "package email\n")
	if err := os.Remove(declaration); err != nil {
		t.Fatal(err)
	}
	if err := run(spec, declDir, outRoot); err != nil {
		t.Fatalf("run after removing the last plugin declaration: %v", err)
	}
	if _, err := os.Stat(generated); !os.IsNotExist(err) {
		t.Fatalf("stale plugin output survived: %v", err)
	}
	if _, err := os.Stat(handWritten); err != nil {
		t.Fatalf("hand-written plugin file was pruned: %v", err)
	}
}
