package main

// tool_smoke is the closed-coverage gate for the model-facing tool surface: it
// invokes every builtin tool once, in process, through the exact path a model
// uses — Code Mode. setup() builds the production composition root against a
// live PostgreSQL, a scripted provider hands the runner one `code` call per
// case, the VM runs tools.invoke against the real registry, and the assertion
// reads each child tool's own result off the agent event stream rather than
// trusting what the JavaScript chose to return.
//
// Why here and not in test/system: this is the lowest layer that can prove a
// tool is callable, because "callable" means the daemon constructed its
// service, its availability predicate passed for a real user, and its schema
// survived the provider round trip. All of that is in-process; the subprocess,
// HTTP, and SSE hops add nothing to it. test/system keeps a canary journey for
// the transport itself.
//
// The coverage set is closed by strict equality (see
// assertSmokeCoverageIsClosed): the production tool surface must equal the
// tools these cases invoke plus the explicitly listed protocol exceptions.
// There is no pending list and no skip: a tool without a case fails the build.
//
// Two honest limits, both visible in the coverage report this test logs:
//
//  1. protocolExceptions holds three names that no case invokes — `code` (the
//     vehicle every case rides), goal_control (registered only inside a Goal
//     attempt), and the mcp__ prefix (internal/mcp's SSRF guard rejects every
//     address a hermetic test can bind). Each carries the tests that cover it.
//  2. Seven tools assert only their canonical error, because their success
//     precondition cannot be produced here: email__message_list/_read (no IMAP
//     seam), workflow_save/_get/_run (need an accepted composite Goal), and
//     oauth_connect/_flow_status (need a live third-party flow). Each names the
//     test that covers its success path, or says there is none.
//
// Everything else runs its real success path against loopback-only fixtures, so
// a full run makes no request that leaves the host.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	cfgstore "github.com/CherryHQ/stella/cmd/stellad/store"
	"github.com/CherryHQ/stella/internal/agent"
	"github.com/CherryHQ/stella/internal/agent/session"
	"github.com/CherryHQ/stella/internal/auth"
	"github.com/CherryHQ/stella/internal/authz"
	"github.com/CherryHQ/stella/internal/connections"
	"github.com/CherryHQ/stella/internal/connections/oauth"
	appdb "github.com/CherryHQ/stella/internal/db"
	"github.com/CherryHQ/stella/internal/db/dbtest"
	"github.com/CherryHQ/stella/internal/platform/config"
	"github.com/CherryHQ/stella/internal/plugin"
	"github.com/CherryHQ/stella/internal/vault"
	pkgchannel "github.com/CherryHQ/stella/pkg/channel"
	"github.com/CherryHQ/stella/plugins/core"
	"github.com/CherryHQ/stella/plugins/email"
)

// smokeState carries values one case discovers into the cases that need them,
// so a sibling tool verifies the side effect its family member committed
// (vault_secret_set -> vault_secret_list) instead of a case inventing an id.
type smokeState struct {
	values map[string]string
}

func (s *smokeState) set(key, value string) { s.values[key] = value }

// need returns a value an earlier case captured. A missing key means the
// producing case failed, so the dependent case fails with that reason rather
// than invoking its tool with a nonsense argument.
func (s *smokeState) need(t *testing.T, key string) string {
	t.Helper()
	value := s.values[key]
	if value == "" {
		t.Fatalf("tool smoke: %q was never captured; the case that produces it did not succeed", key)
	}
	return value
}

// smokeCase is one `code` call. It invokes its subject tool once, and — only
// where a side effect must be retired inside the same VM call — the tools named
// in covers as well.
type smokeCase struct {
	// tool is the subject: the model-facing name, the subtest name, and the key
	// the coverage closure counts.
	tool string
	// args builds the subject's input, possibly from earlier captured state.
	args func(t *testing.T, s *smokeState) map[string]any
	// script replaces the generated single-invoke program. Use it only when one
	// VM call must invoke more than one tool; covers must then name the rest.
	script func(t *testing.T, s *smokeState) string
	// covers names the further tools this case's script invokes. They count as
	// covered and their results are asserted like the subject's.
	covers []string
	// check validates the case's results, keyed by tool name. A nil check
	// accepts any non-error result: the tool ran, returned, and decoded.
	check func(t *testing.T, s *smokeState, results map[string]string)
	// extraReplies are the model turns a tool triggers before its own turn
	// resumes: a nested session runs its own turn inside session_create, and an
	// image-returning tool triggers a baseline render. They are enqueued after
	// the case's `code` call and before the reply that ends the turn, which is
	// the order the system makes them in.
	extraReplies []string
	// assertsErrorShapeOnly names the canonical error a tool must return when its
	// success precondition cannot be produced in a test deployment. The pattern
	// is matched against the error text. These cases prove the error contract,
	// not the success path, and the coverage report lists them separately.
	assertsErrorShapeOnly string
	// confirm is a second `code` call made after this case's own, invoking a
	// sibling read tool to see whether the side effect actually landed. Without
	// it a write tool is only ever judged by its own return value, which is the
	// one thing a broken write can still get right. The tool it invokes is not
	// counted as coverage: it has its own case, and this call re-reads it.
	confirm *smokeConfirm
}

// smokeConfirm reads a side effect back through a sibling tool.
type smokeConfirm struct {
	tool string
	args func(t *testing.T, s *smokeState) map[string]any
	// wantsError makes the confirming call assert a failure instead of a
	// result, which is how a delete proves the object is gone.
	wantsError string
	check      func(t *testing.T, s *smokeState, result string)
}

// smokeCases is the ordered case list. Order matters inside a family: a create
// runs before the get that reads it and the delete that retires it.
func smokeCases(h *smokeHarness) []smokeCase {
	var cases []smokeCase
	cases = append(cases, coreSmokeCases()...)
	cases = append(cases, agentManagementSmokeCases()...)
	cases = append(cases, schedulerSmokeCases()...)
	cases = append(cases, vaultSmokeCases()...)
	cases = append(cases, goalSmokeCases()...)
	cases = append(cases, workflowSmokeCases()...)
	cases = append(cases, memorySmokeCases()...)
	cases = append(cases, skillSmokeCases()...)
	cases = append(cases, librarySmokeCases()...)
	cases = append(cases, sessionSmokeCases()...)
	cases = append(cases, notifySmokeCases(h.sink)...)
	cases = append(cases, oauthSmokeCases()...)
	cases = append(cases, recallySmokeCases()...)
	cases = append(cases, shareSmokeCases()...)
	cases = append(cases, emailSmokeCases(h.sentMail)...)
	// Deployment mutations run last: their temporary provider/default/plugin
	// state must not change prerequisites for the ordinary tool cases above.
	cases = append(cases, deploymentAndMCPSmokeCases()...)
	return cases
}

func coreSmokeCases() []smokeCase {
	return []smokeCase{{
		// bash and view_image share one call because they must share one sandbox
		// session: every case runs in a fresh session with its own working
		// directory, so an image written by an earlier case is not there to be
		// read by a later one. The markdown file is written to the durable agent
		// work root instead, which is where share_create_artifact resolves paths.
		tool:   "bash",
		covers: []string{"view_image"},
		script: func(t *testing.T, s *smokeState) string {
			command := "echo tool-smoke-bash-" + s.values["runID"] +
				" | tee \"$HOME/tool-smoke.md\" && printf %s " + smokePNGBase64 +
				" | base64 -d > tool-smoke.png"
			return fmt.Sprintf(
				"const wrote = await tools.invoke(\"bash\", %s);\n"+
					"await tools.invoke(\"view_image\", { path: \"tool-smoke.png\" });\n"+
					"return tools.text(wrote);",
				mustJSON(t, map[string]any{"command": command}))
		},
		// The baseline render is its own model call, made while the code call is
		// still open, so its reply is scripted before the turn's closing text.
		extraReplies: []string{"a one pixel test image"},
		check: func(t *testing.T, s *smokeState, results map[string]string) {
			if !strings.Contains(results["bash"], "tool-smoke-bash-"+s.values["runID"]) {
				t.Errorf("bash output = %q, want the echoed run-scoped marker", results["bash"])
			}
		},
	}}
}

func agentManagementSmokeCases() []smokeCase {
	const overrideTool = "scheduler__job_list"
	return []smokeCase{
		{tool: "settings_agent_list", args: noArgs},
		{
			tool: "settings_agent_create",
			args: func(t *testing.T, s *smokeState) map[string]any {
				name := "tool smoke agent " + s.values["runID"]
				s.set("managed_agent_name", name)
				return map[string]any{"name": name}
			},
			check:   captureManagedAgent("settings_agent_create"),
			confirm: &smokeConfirm{tool: "settings_agent_get", args: byID("managed_agent_id"), check: captureManagedAgentConfirm("settings_agent_get")},
		},
		{tool: "settings_agent_get", args: byID("managed_agent_id"), check: func(t *testing.T, s *smokeState, results map[string]string) {
			captureManagedAgentConfirm("settings_agent_get")(t, s, results["settings_agent_get"])
		}},
		{
			tool: "settings_agent_update",
			args: func(t *testing.T, s *smokeState) map[string]any {
				name := s.need(t, "managed_agent_name") + " updated"
				s.set("managed_settings_agent_updated_name", name)
				return map[string]any{"id": s.need(t, "managed_agent_id"), "expected_version": s.need(t, "managed_agent_version"), "name": name}
			},
			check:   captureManagedAgent("settings_agent_update"),
			confirm: &smokeConfirm{tool: "settings_agent_get", args: byID("managed_agent_id"), check: present("settings_agent_get", "managed_settings_agent_updated_name")},
		},
		{
			tool: "settings_agent_tool_list",
			args: func(t *testing.T, s *smokeState) map[string]any {
				return map[string]any{"target_agent_id": s.need(t, "managed_agent_id")}
			},
			check: captureOverrideVersion(overrideTool),
		},
		{
			tool: "settings_agent_tool_update",
			args: func(t *testing.T, s *smokeState) map[string]any {
				return map[string]any{"target_agent_id": s.need(t, "managed_agent_id"), "tool_name": overrideTool, "enabled": false, "expected_version": s.need(t, "managed_override_version")}
			},
			check: captureOverrideMutation("settings_agent_tool_update"),
			confirm: &smokeConfirm{tool: "settings_agent_tool_list", args: func(t *testing.T, s *smokeState) map[string]any {
				return map[string]any{"target_agent_id": s.need(t, "managed_agent_id")}
			}, check: present("settings_agent_tool_list", "managed_override_version")},
		},
		{
			tool: "settings_agent_tool_delete",
			args: func(t *testing.T, s *smokeState) map[string]any {
				return map[string]any{"target_agent_id": s.need(t, "managed_agent_id"), "tool_name": overrideTool, "expected_version": s.need(t, "managed_override_version")}
			},
			confirm: &smokeConfirm{tool: "settings_agent_tool_list", args: func(t *testing.T, s *smokeState) map[string]any {
				return map[string]any{"target_agent_id": s.need(t, "managed_agent_id")}
			}, check: func(t *testing.T, _ *smokeState, result string) {
				if !strings.Contains(result, `"tool_name":"scheduler__job_list"`) || !strings.Contains(result, `"version":"absent"`) {
					t.Fatalf("settings_agent_tool_list after delete = %s, want the absent sentinel", result)
				}
			}},
		},
		{
			tool: "settings_agent_delete",
			args: func(t *testing.T, s *smokeState) map[string]any {
				return map[string]any{"id": s.need(t, "managed_agent_id"), "expected_version": s.need(t, "managed_agent_version")}
			},
			confirm: &smokeConfirm{tool: "settings_agent_get", args: byID("managed_agent_id"), wantsError: `(?i)(not found|no rows)`},
		},
	}
}

func captureManagedAgent(tool string) func(*testing.T, *smokeState, map[string]string) {
	return func(t *testing.T, s *smokeState, results map[string]string) {
		var value struct {
			ID      string `json:"id"`
			Version string `json:"version"`
		}
		if err := json.Unmarshal([]byte(results[tool]), &value); err != nil || value.ID == "" || value.Version == "" {
			t.Fatalf("%s result = %q, want agent id and version: %v", tool, results[tool], err)
		}
		s.set("managed_agent_id", value.ID)
		s.set("managed_agent_version", value.Version)
	}
}

func captureManagedAgentConfirm(tool string) func(*testing.T, *smokeState, string) {
	return func(t *testing.T, s *smokeState, result string) {
		var value struct {
			ID      string `json:"id"`
			Version string `json:"version"`
		}
		if err := json.Unmarshal([]byte(result), &value); err != nil || value.ID != s.need(t, "managed_agent_id") || value.Version == "" {
			t.Fatalf("%s result = %q, want managed agent id and version: %v", tool, result, err)
		}
		s.set("managed_agent_version", value.Version)
	}
}

func captureOverrideVersion(toolName string) func(*testing.T, *smokeState, map[string]string) {
	return func(t *testing.T, s *smokeState, results map[string]string) {
		var value struct {
			Tools []struct {
				ToolName string `json:"tool_name"`
				Version  string `json:"version"`
			} `json:"tools"`
		}
		if err := json.Unmarshal([]byte(results["settings_agent_tool_list"]), &value); err != nil {
			t.Fatal(err)
		}
		for _, item := range value.Tools {
			if item.ToolName == toolName && item.Version != "" {
				s.set("managed_override_version", item.Version)
				return
			}
		}
		t.Fatalf("settings_agent_tool_list did not return %q with a version: %s", toolName, results["settings_agent_tool_list"])
	}
}

func captureOverrideMutation(tool string) func(*testing.T, *smokeState, map[string]string) {
	return func(t *testing.T, s *smokeState, results map[string]string) {
		var value struct {
			Version string `json:"version"`
		}
		if err := json.Unmarshal([]byte(results[tool]), &value); err != nil || value.Version == "" {
			t.Fatalf("%s result = %q, want version: %v", tool, results[tool], err)
		}
		s.set("managed_override_version", value.Version)
	}
}

func schedulerSmokeCases() []smokeCase {
	const jobName = "tool-smoke-job"
	return []smokeCase{
		{
			tool: "scheduler__job_create",
			args: func(t *testing.T, s *smokeState) map[string]any {
				return map[string]any{
					"name":            jobName + "-" + s.values["runID"],
					"message":         "tool smoke scheduled message",
					"cron":            "0 4 * * *",
					"enabled":         true,
					"idempotency_key": "tool-smoke-" + s.values["runID"],
				}
			},
			check: captureID("scheduler__job_create", "scheduler_job_id"),
		},
		{
			tool:  "scheduler__job_get",
			args:  byID("scheduler_job_id"),
			check: expectSameID("scheduler__job_get", "scheduler_job_id"),
		},
		{
			tool:  "scheduler__job_list",
			args:  noArgs,
			check: expectMentions("scheduler__job_list", "scheduler_job_id"),
		},
		{
			tool:  "scheduler__job_pause",
			args:  byID("scheduler_job_id"),
			check: expectSameID("scheduler__job_pause", "scheduler_job_id"),
			// A pause that only reports enabled=false in its own reply has proved
			// nothing; the job row is what the scheduler reads.
			confirm: &smokeConfirm{
				tool:  "scheduler__job_get",
				args:  byID("scheduler_job_id"),
				check: expectJSONField("scheduler__job_get", "enabled", false),
			},
		},
		{
			tool:  "scheduler__job_resume",
			args:  byID("scheduler_job_id"),
			check: expectSameID("scheduler__job_resume", "scheduler_job_id"),
			confirm: &smokeConfirm{
				tool:  "scheduler__job_get",
				args:  byID("scheduler_job_id"),
				check: expectJSONField("scheduler__job_get", "enabled", true),
			},
		},
		{
			// The update changes the name rather than the message because the name
			// is what a read-back can see: the job projection omits the message.
			tool: "scheduler__job_update",
			args: func(t *testing.T, s *smokeState) map[string]any {
				s.set("scheduler_job_name", jobName+"-renamed-"+s.values["runID"])
				return map[string]any{"id": s.need(t, "scheduler_job_id"), "name": s.values["scheduler_job_name"]}
			},
			check: expectSameID("scheduler__job_update", "scheduler_job_id"),
			confirm: &smokeConfirm{
				tool:  "scheduler__job_get",
				args:  byID("scheduler_job_id"),
				check: present("scheduler__job_get", "scheduler_job_name"),
			},
		},
		{
			tool: "scheduler__job_delete",
			args: byID("scheduler_job_id"),
			confirm: &smokeConfirm{
				tool:       "scheduler__job_get",
				args:       byID("scheduler_job_id"),
				wantsError: `(?i)(not found|no rows)`,
			},
		},
	}
}

func vaultSmokeCases() []smokeCase {
	const secretName = "TOOL_SMOKE_SECRET"
	return []smokeCase{
		{
			tool: "vault_secret_set",
			args: func(t *testing.T, s *smokeState) map[string]any {
				// A fixture value, never a real credential. The check below is what
				// proves the tool does not echo it back into the transcript.
				return map[string]any{"name": secretName, "scope": "user", "value": "tool-smoke-not-a-secret"}
			},
			check: func(t *testing.T, s *smokeState, results map[string]string) {
				if strings.Contains(results["vault_secret_set"], "tool-smoke-not-a-secret") {
					t.Errorf("vault_secret_set echoed the secret value into the model transcript: %q", results["vault_secret_set"])
				}
				s.set("vault_secret_name", secretName)
			},
		},
		{
			tool:  "vault_secret_list",
			args:  func(t *testing.T, s *smokeState) map[string]any { return map[string]any{"scope": "user"} },
			check: expectMentions("vault_secret_list", "vault_secret_name"),
		},
		{
			tool: "vault_secret_delete",
			args: func(t *testing.T, s *smokeState) map[string]any {
				return map[string]any{"name": s.need(t, "vault_secret_name"), "scope": "user"}
			},
			confirm: &smokeConfirm{
				tool:  "vault_secret_list",
				args:  func(t *testing.T, s *smokeState) map[string]any { return map[string]any{"scope": "user"} },
				check: absent("vault_secret_list", "vault_secret_name"),
			},
		},
	}
}

// goalSmokeCases creates one goal and reads it back. goal_create and
// goal_cancel share a single VM call on purpose: the tool always creates a
// draft composite, and the Goal dispatcher claims a draft composite for
// autonomous decomposition on its next 2s tick. Those planner turns are
// asynchronous model calls on this same agent, and — since Code Mode moved
// goal_control off the provider-facing tool list — they are no longer
// distinguishable from a chat turn, so the fake cannot answer them without
// stealing this journey's scripted responses. Retiring the goal microseconds
// after it is created is what keeps the dispatcher out of this journey. See the
// note on workflowSmokeCases.
func goalSmokeCases() []smokeCase {
	return []smokeCase{
		{
			tool:   "goal_create",
			covers: []string{"goal_cancel"},
			script: func(t *testing.T, s *smokeState) string {
				create := mustJSON(t, map[string]any{
					"title":           "tool smoke goal " + s.values["runID"],
					"intent":          "exist long enough for the goal family to read it",
					"review_policy":   "none",
					"idempotency_key": "tool-smoke-goal-" + s.values["runID"],
				})
				return fmt.Sprintf(
					"const created = await tools.invoke(\"goal_create\", %s);\n"+
						"const id = tools.json(created).id;\n"+
						"await tools.invoke(\"goal_cancel\", { id: id, reason: \"tool smoke retires its own goal\" });\n"+
						"return id;",
					create)
			},
			check: func(t *testing.T, s *smokeState, results map[string]string) {
				s.set("goal_id", requireJSONString(t, "goal_create", results["goal_create"], "id"))
				if got := requireJSONString(t, "goal_cancel", results["goal_cancel"], "id"); got != s.values["goal_id"] {
					t.Errorf("goal_cancel retired %q, want the goal goal_create returned %q", got, s.values["goal_id"])
				}
				if !strings.Contains(results["goal_cancel"], "cancelled") {
					t.Errorf("goal_cancel result does not report a cancelled goal: %s", truncate(results["goal_cancel"], 800))
				}
			},
		},
		{
			tool:  "goal_get",
			args:  byID("goal_id"),
			check: expectSameID("goal_get", "goal_id"),
		},
		{
			tool:  "goal_list",
			args:  noArgs,
			check: expectMentions("goal_list", "goal_id"),
		},
	}
}

// workflowSmokeCases is the journey's one incomplete family, and the reason is
// worth stating rather than hiding behind a nil check. workflow_save requires a
// composite root in done/accepted, a state only the Goal dispatcher's planner
// and executor attempts can produce. Those attempts are model turns that the
// fake can no longer identify: Code Mode keeps goal_control out of the
// provider-facing tool list, so the advertised-action discriminator the fake
// scripts Goal runs with sees nothing (the same regression that leaves the
// goal_lifecycle journey red on main). Until that is fixed, save/get/run assert
// their canonical precondition errors and only workflow_list runs its success
// path.
// workflowSmokeCases covers the workflow family. Three of its four tools assert
// their canonical error: saving needs a parentless composite Goal already in
// lifecycle=done/accepted, and only the Goal dispatcher's async workers can put
// one there — this gate deliberately leaves that driver off (see
// newSmokeHarness). Each case still proves the tool is enabled, that the call
// reached the service with arguments that passed schema admission, and that the
// refusal is the domain's own structured error. The success paths are covered
// in process by internal/workflow's service tests, and end to end by the
// goal_lifecycle journey in test/system.
func workflowSmokeCases() []smokeCase {
	return []smokeCase{
		{
			tool: "workflow_save",
			args: func(t *testing.T, s *smokeState) map[string]any {
				return map[string]any{"goal_id": s.need(t, "goal_id"), "name": "tool-smoke-workflow-" + s.values["runID"]}
			},
			assertsErrorShapeOnly: `(?i)invalid lifecycle transition`,
		},
		{
			tool:                  "workflow_get",
			args:                  func(t *testing.T, s *smokeState) map[string]any { return map[string]any{"id": absentUUID} },
			assertsErrorShapeOnly: `(?i)(not found|no rows)`,
		},
		{
			tool:  "workflow_list",
			args:  noArgs,
			check: expectJSONObject("workflow_list"),
		},
		{
			tool: "workflow_run",
			args: func(t *testing.T, s *smokeState) map[string]any {
				return map[string]any{"id": absentUUID, "idempotency_key": "tool-smoke-run-" + s.values["runID"]}
			},
			assertsErrorShapeOnly: `(?i)(not found|no rows)`,
		},
	}
}

func memorySmokeCases() []smokeCase {
	return []smokeCase{
		{
			tool:  "memory_search",
			args:  func(t *testing.T, s *smokeState) map[string]any { return map[string]any{"q": "the tool smoke run"} },
			check: expectJSONObject("memory_search"),
		},
		{
			// "profile" is one of the fixed refs the schema documents, so the case
			// needs no recalled ref to read: memory_read is exercised on a ref every
			// deployment resolves.
			tool: "memory_read",
			args: func(t *testing.T, s *smokeState) map[string]any { return map[string]any{"ref": "profile"} },
		},
	}
}

func skillSmokeCases() []smokeCase {
	return []smokeCase{
		{
			tool: "settings_skill_list",
			args: func(t *testing.T, _ *smokeState) map[string]any {
				return map[string]any{"scope": "user"}
			},
			check: expectJSONArrayField("settings_skill_list", "skills"),
		},
		{
			tool: "settings_skill_create",
			script: func(t *testing.T, s *smokeState) string {
				name := "tool-smoke-skill-" + s.values["runID"]
				s.set("managed_skill_name", name)
				content := "---\nname: " + name + "\ndescription: smoke skill\n---\n# Smoke\n"
				command := "mkdir -p " + shellQuote(name) + " && printf %s " + shellQuote(content) + " > " + shellQuote(name+"/SKILL.md")
				return fmt.Sprintf("await tools.invoke(\"bash\", {command: %s}); return tools.invoke(\"settings_skill_create\", %s);", mustJSON(t, command), mustJSON(t, map[string]any{"scope": "user", "content_path": name}))
			},
			check:   captureVersionedID("settings_skill_create", "managed_skill_id", "managed_skill_version"),
			confirm: &smokeConfirm{tool: "settings_skill_get", args: byID("managed_skill_id"), check: captureVersionedResult("settings_skill_get", "managed_skill_version")},
		},
		{
			tool: "settings_skill_get",
			args: byID("managed_skill_id"),
			check: func(t *testing.T, s *smokeState, results map[string]string) {
				captureVersionedResult("settings_skill_get", "managed_skill_version")(t, s, results["settings_skill_get"])
			},
		},
		{
			tool: "settings_skill_update",
			script: func(t *testing.T, s *smokeState) string {
				name := s.need(t, "managed_skill_name")
				content := "---\nname: " + name + "\ndescription: updated smoke skill\n---\n# Updated\n"
				return fmt.Sprintf("await tools.invoke(\"bash\", {command: %s}); return tools.invoke(\"settings_skill_update\", %s);", mustJSON(t, "printf %s "+shellQuote(content)+" > "+shellQuote(name+"/SKILL.md")), mustJSON(t, map[string]any{"id": s.need(t, "managed_skill_id"), "expected_version": s.need(t, "managed_skill_version"), "content_path": name}))
			},
			check: func(t *testing.T, s *smokeState, results map[string]string) {
				captureVersionedResult("settings_skill_update", "managed_skill_version")(t, s, results["settings_skill_update"])
			},
			confirm: &smokeConfirm{tool: "settings_skill_get", args: byID("managed_skill_id"), check: captureVersionedResult("settings_skill_get", "managed_skill_version")},
		},
		{
			tool: "settings_skill_delete",
			args: func(t *testing.T, s *smokeState) map[string]any {
				return map[string]any{"id": s.need(t, "managed_skill_id"), "expected_version": s.need(t, "managed_skill_version")}
			},
			confirm: &smokeConfirm{tool: "settings_skill_get", args: byID("managed_skill_id"), wantsError: `(?i)(not found|no rows)`},
		},
		{
			tool: "skill_installed_search",
			args: func(t *testing.T, s *smokeState) map[string]any {
				return map[string]any{"q": "stella", "limit": 5}
			},
			check: func(t *testing.T, s *smokeState, results map[string]string) {
				name := firstSkillName(t, results["skill_installed_search"])
				s.set("skill_name", name)
			},
		},
		{
			// The name comes from the search above, so skill_load reads a skill this
			// deployment actually installed rather than one the test assumed.
			tool: "skill_load",
			args: func(t *testing.T, s *smokeState) map[string]any {
				return map[string]any{"name": s.need(t, "skill_name")}
			},
			check: func(t *testing.T, s *smokeState, results map[string]string) {
				if strings.TrimSpace(results["skill_load"]) == "" {
					t.Error("skill_load returned empty content for an installed skill")
				}
			},
		},
	}
}

// firstSkillName pulls one installed skill's name out of the search result. An
// empty result set is a failure: this deployment syncs its built-in skills at
// startup, so a search that matches nothing means the sync did not happen.
func firstSkillName(t *testing.T, output string) string {
	t.Helper()
	var decoded []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal([]byte(output), &decoded); err != nil {
		t.Fatalf("skill_installed_search result is not a JSON array: %v\n%s", err, truncate(output, 800))
	}
	if len(decoded) == 0 || decoded[0].Name == "" {
		t.Fatalf("skill_installed_search returned no installed skill: %s", truncate(output, 800))
	}
	return decoded[0].Name
}

func librarySmokeCases() []smokeCase {
	return []smokeCase{
		{
			// The library is empty in a fresh deployment, so this proves the search
			// path answers with a well-formed empty result rather than an error.
			tool:  "library_search",
			args:  func(t *testing.T, s *smokeState) map[string]any { return map[string]any{"query": "tool smoke"} },
			check: expectJSONObject("library_search"),
		},
		{tool: "settings_library_file_list", args: func(t *testing.T, _ *smokeState) map[string]any { return map[string]any{"scope": "user"} }, check: expectJSONObject("settings_library_file_list")},
		{
			tool: "settings_library_file_upload",
			script: func(t *testing.T, s *smokeState) string {
				name := "tool-smoke-library-" + s.values["runID"] + ".txt"
				return fmt.Sprintf("await tools.invoke(\"bash\", {command: %s}); return tools.invoke(\"settings_library_file_upload\", %s);", mustJSON(t, "printf %s 'library smoke source' > tool-smoke-library.txt"), mustJSON(t, map[string]any{"scope": "user", "name": name, "content_path": "tool-smoke-library.txt"}))
			},
			check:   captureVersionedID("settings_library_file_upload", "managed_library_id", "managed_library_version"),
			confirm: &smokeConfirm{tool: "settings_library_file_get", args: byID("managed_library_id"), check: captureVersionedResult("settings_library_file_get", "managed_library_version")},
		},
		{tool: "settings_library_file_get", args: byID("managed_library_id"), check: func(t *testing.T, s *smokeState, results map[string]string) {
			captureVersionedResult("settings_library_file_get", "managed_library_version")(t, s, results["settings_library_file_get"])
		}},
		{
			tool: "settings_library_file_delete",
			args: func(t *testing.T, s *smokeState) map[string]any {
				return map[string]any{"id": s.need(t, "managed_library_id"), "expected_version": s.need(t, "managed_library_version")}
			},
			confirm: &smokeConfirm{tool: "settings_library_file_get", args: byID("managed_library_id"), wantsError: `(?i)(not found|no rows)`},
		},
	}
}

func captureVersionedID(tool, idKey, versionKey string) func(*testing.T, *smokeState, map[string]string) {
	return func(t *testing.T, s *smokeState, results map[string]string) {
		var value struct {
			ID      string `json:"id"`
			Version string `json:"version"`
		}
		if err := json.Unmarshal([]byte(results[tool]), &value); err != nil || value.ID == "" || value.Version == "" {
			t.Fatalf("%s result = %q, want id and version: %v", tool, results[tool], err)
		}
		s.set(idKey, value.ID)
		s.set(versionKey, value.Version)
	}
}

func captureVersionedResult(tool, versionKey string) func(*testing.T, *smokeState, string) {
	return func(t *testing.T, s *smokeState, result string) {
		var value struct {
			Version string `json:"version"`
		}
		if err := json.Unmarshal([]byte(result), &value); err != nil || value.Version == "" {
			t.Fatalf("%s result = %q, want version: %v", tool, result, err)
		}
		s.set(versionKey, value.Version)
	}
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\\"'\\\"'") + "'"
}

// sessionSmokeCases reaches another session's transcript, which is why each
// create/send runs a nested model turn of its own: extraReplies scripts them.
func sessionSmokeCases() []smokeCase {
	return []smokeCase{
		{
			tool: "session_create",
			args: func(t *testing.T, s *smokeState) map[string]any {
				return map[string]any{"message": "tool smoke child session " + s.values["runID"], "wait": true}
			},
			extraReplies: []string{"child session answered"},
			check:        captureSessionID("session_create", "child_session_id"),
		},
		{
			tool: "session_send",
			args: func(t *testing.T, s *smokeState) map[string]any {
				return map[string]any{"session_id": s.need(t, "child_session_id"), "message": "second turn", "wait": true}
			},
			extraReplies: []string{"child session answered again"},
		},
		{
			tool: "session_get",
			args: func(t *testing.T, s *smokeState) map[string]any {
				return map[string]any{"session_id": s.need(t, "child_session_id")}
			},
			check: expectMentions("session_get", "child_session_id"),
		},
		{
			tool:  "session_list",
			args:  noArgs,
			check: expectMentions("session_list", "child_session_id"),
		},
	}
}

func captureSessionID(tool, key string) func(*testing.T, *smokeState, map[string]string) {
	return func(t *testing.T, s *smokeState, results map[string]string) {
		s.set(key, requireJSONString(t, tool, results[tool], "session_id"))
	}
}

// notifySmokeCases proves a delivery, not just a return value: seedFixtures
// registers a fake channel with the notifier the way a channel plugin does, and
// the case asserts the message arrived there. A registered channel with no user
// identities falls back to a broadcast, which is why registering the sink is
// enough to route to it.
func notifySmokeCases(sink *smokeChannel) []smokeCase {
	return []smokeCase{{
		tool: "notify",
		args: func(t *testing.T, s *smokeState) map[string]any {
			return map[string]any{"message": "tool smoke notification " + s.values["runID"]}
		},
		// The assertion is the sink, not the tool's own return: a notifier that
		// reports success without delivering anywhere is exactly the failure a
		// smoke case for notify has to catch.
		check: func(t *testing.T, s *smokeState, results map[string]string) {
			want := "tool smoke notification " + s.values["runID"]
			for _, got := range sink.messages() {
				if strings.Contains(got, want) {
					return
				}
			}
			t.Fatalf("the notify sink never received %q; it saw %q", want, sink.messages())
		},
	}}
}

// oauthSmokeCases runs against a deployment whose github provider is seeded
// with fixture credentials and a fixture token bundle (see seedFixtures). No
// flow ever runs, so nothing leaves the host; the seed exists because a
// disconnect can only be proved by disconnecting something.
func oauthSmokeCases() []smokeCase {
	return []smokeCase{
		{
			tool: "oauth_list",
			args: noArgs,
			check: func(t *testing.T, s *smokeState, results map[string]string) {
				if !smokeOAuthConnected(t, results["oauth_list"], smokeOAuthProvider) {
					t.Errorf("oauth_list reports %s disconnected, but the fixture bundle is seeded: %s",
						smokeOAuthProvider, truncate(results["oauth_list"], 800))
				}
			},
		},
		{
			// oauth_connect is asserted on its unknown-provider error on purpose: a
			// real provider name starts a device-authorization flow against that
			// third party's live endpoint, and this gate must make no external
			// network call. Measured, not assumed — provider "github" returned a
			// genuine github.com device code when this case first ran. The success
			// path is covered in process by internal/connections' service tests.
			tool: "oauth_connect",
			args: func(t *testing.T, s *smokeState) map[string]any {
				return map[string]any{"provider": "tool-smoke-absent-provider"}
			},
			assertsErrorShapeOnly: `(?i)(not configured|unknown|unsupported|no provider)`,
		},
		{
			// Disconnect is local, so it runs its success path against the seeded
			// connection and is judged by what oauth_list reports afterwards.
			tool: "oauth_disconnect",
			args: func(t *testing.T, s *smokeState) map[string]any {
				return map[string]any{"provider": smokeOAuthProvider}
			},
			check: expectJSONObject("oauth_disconnect"),
			confirm: &smokeConfirm{
				tool: "oauth_list",
				args: noArgs,
				check: func(t *testing.T, s *smokeState, result string) {
					if smokeOAuthConnected(t, result, smokeOAuthProvider) {
						t.Errorf("oauth_list still reports %s connected after oauth_disconnect: %s",
							smokeOAuthProvider, truncate(result, 800))
					}
				},
			},
		},
		{
			// A live flow id only exists after a real device-authorization call, so
			// this asserts the lookup's structured miss. internal/connections covers
			// the populated flow.
			tool: "oauth_flow_status",
			args: func(t *testing.T, s *smokeState) map[string]any {
				return map[string]any{"provider": smokeOAuthProvider, "flow_id": absentUUID}
			},
			assertsErrorShapeOnly: `(?i)(not found|expired|unknown|not configured)`,
		},
	}
}

// smokeOAuthProvider is the provider the gate seeds a fixture connection for.
const smokeOAuthProvider = "github"

// smokeOAuthConnected reads one provider's connected flag out of an oauth_list
// result. A provider missing from the list is a failure, not a false: it would
// silently satisfy the post-disconnect assertion.
func smokeOAuthConnected(t *testing.T, result, provider string) bool {
	t.Helper()
	var decoded struct {
		Providers []struct {
			Provider  string `json:"provider"`
			Connected bool   `json:"connected"`
		} `json:"providers"`
	}
	if err := json.Unmarshal([]byte(result), &decoded); err != nil {
		t.Fatalf("oauth_list result is not a JSON object: %v\n%s", err, truncate(result, 800))
	}
	for _, p := range decoded.Providers {
		if p.Provider == provider {
			return p.Connected
		}
	}
	t.Fatalf("oauth_list does not mention provider %q at all: %s", provider, truncate(result, 800))
	return false
}

// recallySmokeCases walks the reading-list family: articles and the daily
// digest first, because share_create_article needs an article to share, then
// feeds against a loopback RSS server so the poll path runs for real without
// leaving the host.
func recallySmokeCases() []smokeCase {
	return []smokeCase{
		{
			tool: "recally__article_save",
			args: func(t *testing.T, s *smokeState) map[string]any {
				return map[string]any{"articles": []map[string]any{{
					"url":         "https://tool-smoke.invalid/article/" + s.values["runID"],
					"title":       "tool smoke article " + s.values["runID"],
					"content":     "# tool smoke\n\n" + strings.Repeat("A saved article body. ", 12),
					"source_type": "web",
				}}}
			},
			check: func(t *testing.T, s *smokeState, results map[string]string) {
				s.set("recally_article_id", firstSavedArticleID(t, results["recally__article_save"]))
			},
		},
		{
			tool:  "recally__article_get",
			args:  byID("recally_article_id"),
			check: expectMentions("recally__article_get", "recally_article_id"),
		},
		{
			tool:  "recally__article_list",
			args:  noArgs,
			check: expectMentions("recally__article_list", "recally_article_id"),
		},
		{
			tool: "recally__digest_save",
			args: func(t *testing.T, s *smokeState) map[string]any {
				return map[string]any{"narrative": "tool smoke digest " + s.values["runID"]}
			},
		},
		{
			tool:  "recally__digest_get",
			args:  noArgs,
			check: expectJSONObject("recally__digest_get"),
		},
		{
			tool: "recally__feed_add",
			args: func(t *testing.T, s *smokeState) map[string]any {
				return map[string]any{"url": s.need(t, "rss_url"), "kind": "rss"}
			},
			check: captureID("recally__feed_add", "recally_feed_id"),
		},
		{
			tool:  "recally__feed_list",
			args:  noArgs,
			check: expectMentions("recally__feed_list", "recally_feed_id"),
		},
		{
			// The poll fetches and parses the loopback feed for real. Its own result
			// is only shape-checked here; what it actually ingested is asserted by
			// the recally__entry_list case below.
			tool:  "recally__feed_poll",
			args:  byID("recally_feed_id"),
			check: expectJSONObject("recally__feed_poll"),
		},
		{
			// The entries under test are the ones recally__feed_poll fetched and
			// parsed out of the loopback feed, so this is where the poll's real
			// side effect is judged: an empty list would mean the poll reported
			// success without ingesting anything.
			tool: "recally__entry_list",
			args: func(t *testing.T, s *smokeState) map[string]any {
				return map[string]any{"feed_id": s.need(t, "recally_feed_id")}
			},
			check: func(t *testing.T, s *smokeState, results map[string]string) {
				for _, want := range []string{smokeFeedItemTitle, smokeFeedItemURL} {
					if !strings.Contains(results["recally__entry_list"], want) {
						t.Errorf("recally__entry_list does not contain the fixture feed's %q: %s",
							want, truncate(results["recally__entry_list"], 800))
					}
				}
			},
		},
		{
			tool: "recally__entry_add",
			args: func(t *testing.T, s *smokeState) map[string]any {
				return map[string]any{
					"feed_id": s.need(t, "recally_feed_id"),
					"guid":    "tool-smoke-entry-" + s.values["runID"],
					"title":   "tool smoke entry",
					"url":     "https://tool-smoke.invalid/entry/" + s.values["runID"],
				}
			},
			check: func(t *testing.T, s *smokeState, results map[string]string) {
				var added struct {
					Entry struct {
						ID string `json:"id"`
					} `json:"entry"`
				}
				if err := json.Unmarshal([]byte(results["recally__entry_add"]), &added); err != nil || added.Entry.ID == "" {
					t.Fatalf("recally__entry_add returned no entry id: %s", truncate(results["recally__entry_add"], 800))
				}
				s.set("recally_entry_id", added.Entry.ID)
			},
		},
		{
			tool: "recally__entry_update",
			args: func(t *testing.T, s *smokeState) map[string]any {
				return map[string]any{
					"feed_id": s.need(t, "recally_feed_id"),
					"id":      s.need(t, "recally_entry_id"),
					"status":  "skipped",
				}
			},
		},
		{
			tool: "recally__feed_remove",
			args: byID("recally_feed_id"),
			confirm: &smokeConfirm{
				tool:  "recally__feed_list",
				args:  noArgs,
				check: absent("recally__feed_list", "recally_feed_id"),
			},
		},
	}
}

// firstSavedArticleID pulls the id out of a batch save result, whose shape is a
// list even for one article.
func firstSavedArticleID(t *testing.T, output string) string {
	t.Helper()
	var batch struct {
		Results []struct {
			ID string `json:"id"`
		} `json:"results"`
	}
	if err := json.Unmarshal([]byte(output), &batch); err != nil {
		t.Fatalf("recally__article_save result is not JSON: %v\n%s", err, truncate(output, 800))
	}
	saved := batch.Results
	if len(saved) == 0 || saved[0].ID == "" {
		t.Fatalf("recally__article_save returned no article id: %s", truncate(output, 800))
	}
	return saved[0].ID
}

// shareSmokeCases publishes the two things a deployment can share: a workspace
// artifact (the file the bash case wrote) and a saved article.
func shareSmokeCases() []smokeCase {
	return []smokeCase{
		{
			tool: "share_create_artifact",
			args: func(t *testing.T, s *smokeState) map[string]any {
				return map[string]any{"path": "tool-smoke.md", "scope": "agent", "expires_in": "1h"}
			},
			check: captureID("share_create_artifact", "share_artifact_id"),
		},
		{
			tool: "share_create_article",
			args: func(t *testing.T, s *smokeState) map[string]any {
				return map[string]any{"article_id": s.need(t, "recally_article_id"), "expires_in": "1h"}
			},
			check: captureID("share_create_article", "share_article_id"),
		},
		{
			// Both shares this family created must be listed: an index that only
			// ever returns the artifact would hide a broken article share.
			tool: "share_list",
			args: noArgs,
			check: func(t *testing.T, s *smokeState, results map[string]string) {
				for _, key := range []string{"share_artifact_id", "share_article_id"} {
					if !strings.Contains(results["share_list"], s.need(t, key)) {
						t.Errorf("share_list does not list %s %q: %s", key, s.values[key], truncate(results["share_list"], 800))
					}
				}
			},
		},
		{
			tool: "share_revoke",
			args: byID("share_artifact_id"),
			confirm: &smokeConfirm{
				tool: "share_list",
				args: noArgs,
				check: func(t *testing.T, s *smokeState, result string) {
					absent("share_list", "share_artifact_id")(t, s, result)
					// The article share is untouched, so a revoke that emptied the
					// whole index would not pass as a successful revoke.
					present("share_list", "share_article_id")(t, s, result)
				},
			},
		},
	}
}

// The mail fixture's constants: the SMTP host is what distinguishes the
// reachable "smoke" account from the loopback "unreachable" one in a recorded
// delivery.
const (
	smokeMailRecipient = "tool-smoke@example.test"
	smokeMailBody      = "sent by the tool smoke gate"
	smokeMailSMTPHost  = "198.51.100.11"
)

// smokeMailbox records what the email service handed to delivery. It replaces
// the SMTP dialer through Service.SetSendFunc, the production substitution
// seam, so nothing is ever put on a socket.
type smokeMailbox struct {
	mu   sync.Mutex
	sent []smokeDelivery
}

type smokeDelivery struct {
	account email.EmailAccount
	opts    email.SendOptions
}

func (m *smokeMailbox) record(account email.EmailAccount, opts email.SendOptions) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sent = append(m.sent, smokeDelivery{account: account, opts: opts})
	return nil
}

func (m *smokeMailbox) delivered() []smokeDelivery {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]smokeDelivery(nil), m.sent...)
}

// absentUUID is a well-formed identifier that no fixture creates, so a lookup
// tool is exercised on its real not-found path rather than on a parse error.
const absentUUID = "00000000-0000-4000-8000-000000000000"

func noArgs(t *testing.T, s *smokeState) map[string]any { return map[string]any{} }

func byID(key string) func(*testing.T, *smokeState) map[string]any {
	return func(t *testing.T, s *smokeState) map[string]any {
		return map[string]any{"id": s.need(t, key)}
	}
}

func captureID(tool, key string) func(*testing.T, *smokeState, map[string]string) {
	return func(t *testing.T, s *smokeState, results map[string]string) {
		s.set(key, requireJSONString(t, tool, results[tool], "id"))
	}
}

// expectSameID proves a sibling tool answered about the object the producing
// case created, not merely that it returned something.
func expectSameID(tool, key string) func(*testing.T, *smokeState, map[string]string) {
	return func(t *testing.T, s *smokeState, results map[string]string) {
		if got := requireJSONString(t, tool, results[tool], "id"); got != s.need(t, key) {
			t.Errorf("%s returned id %q, want the captured %s %q", tool, got, key, s.values[key])
		}
	}
}

func expectMentions(tool, key string) func(*testing.T, *smokeState, map[string]string) {
	return func(t *testing.T, s *smokeState, results map[string]string) {
		want := s.need(t, key)
		if !strings.Contains(results[tool], want) {
			t.Errorf("%s does not mention %s %q: %s", tool, key, want, truncate(results[tool], 800))
		}
	}
}

// expectJSONObject is the weakest useful contract: the tool answered with a
// decodable JSON object rather than prose or an empty body. JSON null decodes
// into a nil map without error, so it is rejected explicitly — a tool that
// answers `null` has told the model nothing.
func expectJSONObject(tool string) func(*testing.T, *smokeState, map[string]string) {
	return func(t *testing.T, s *smokeState, results map[string]string) {
		var decoded map[string]any
		if err := json.Unmarshal([]byte(results[tool]), &decoded); err != nil {
			t.Errorf("%s result is not a JSON object: %v\n%s", tool, err, truncate(results[tool], 800))
			return
		}
		if decoded == nil {
			t.Errorf("%s answered JSON null, not an object: %s", tool, truncate(results[tool], 800))
		}
	}
}

func expectJSONArrayField(tool, field string) func(*testing.T, *smokeState, map[string]string) {
	return func(t *testing.T, _ *smokeState, results map[string]string) {
		var decoded map[string]any
		if err := json.Unmarshal([]byte(results[tool]), &decoded); err != nil {
			t.Fatalf("%s result is not a JSON object: %v\n%s", tool, err, truncate(results[tool], 800))
		}
		if _, ok := decoded[field].([]any); !ok {
			t.Fatalf("%s result has no JSON array field %q: %s", tool, field, truncate(results[tool], 800))
		}
	}
}

// expectJSONField asserts one decoded field of a confirming read, so a state
// change is judged by the value the next reader sees, not by prose.
func expectJSONField(tool, field string, want any) func(*testing.T, *smokeState, string) {
	return func(t *testing.T, s *smokeState, result string) {
		var decoded map[string]any
		if err := json.Unmarshal([]byte(result), &decoded); err != nil {
			t.Fatalf("%s result is not a JSON object: %v\n%s", tool, err, truncate(result, 800))
		}
		if got, ok := decoded[field]; !ok || got != want {
			t.Errorf("%s reports %s = %v (present=%v), want %v: %s", tool, field, got, ok, want, truncate(result, 800))
		}
	}
}

// requireJSONString decodes one string field out of a tool result. A tool whose
// result is not JSON, or lacks the field, has broken its output contract.
func requireJSONString(t *testing.T, tool, output, field string) string {
	t.Helper()
	var decoded map[string]any
	if err := json.Unmarshal([]byte(output), &decoded); err != nil {
		t.Fatalf("%s result is not a JSON object: %v\n%s", tool, err, truncate(output, 800))
	}
	value, _ := decoded[field].(string)
	if value == "" {
		t.Fatalf("%s result has no non-empty %q field: %s", tool, field, truncate(output, 800))
	}
	return value
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// testToolSmoke is the journey entry point.

// emailSmokeCases covers the mail family. Its success paths run against a fake
// send function (Service.SetSendFunc, the production seam the email package
// already exposes for exactly this) and a seeded EMAIL_CONFIG whose hosts are
// documentation IP literals: ValidateAccountEgress must resolve no name and
// reach no host for these cases to be deterministic.
//
// The read paths have no equivalent seam — email.List/Read (plugins/email
// imap.go) dial IMAP directly — so they assert the egress boundary's structured
// refusal instead, against a second account that deliberately points at
// loopback. That is the same boundary a real misconfiguration hits, and it is
// reached only after the tool was enabled and the arguments passed schema
// admission. The refusal itself is covered by plugins/email
// TestValidateAccountEgressRejectsPrivateHosts. There is no success-path
// coverage for the fetch: no test in this repository stands up an IMAP server,
// so nothing exercises email.List or email.Read against a live mailbox.
func emailSmokeCases(mail *smokeMailbox) []smokeCase {
	return []smokeCase{
		{
			tool:  "email__account_list",
			args:  noArgs,
			check: expectMentions("email__account_list", "email_account"),
		},
		{
			tool: "email__message_send",
			args: func(t *testing.T, s *smokeState) map[string]any {
				return map[string]any{
					"account":         s.need(t, "email_account"),
					"to":              []string{smokeMailRecipient},
					"subject":         "tool smoke " + s.values["runID"],
					"body":            smokeMailBody,
					"idempotency_key": "tool-smoke-" + s.values["runID"],
				}
			},
			// The tool's own {"status":"sent"} is exactly what a send that never
			// reached delivery would also return, so the assertion is the recorded
			// delivery: one call, on the account the arguments named, carrying the
			// recipient, subject, and body the model asked for.
			check: func(t *testing.T, s *smokeState, results map[string]string) {
				expectJSONObject("email__message_send")(t, s, results)
				sent := mail.delivered()
				if len(sent) != 1 {
					t.Fatalf("the delivery seam was called %d times, want exactly 1: %+v", len(sent), sent)
				}
				got := sent[0]
				if got.account.SMTPHost != smokeMailSMTPHost {
					t.Errorf("delivery used SMTP host %q, want the %q account's %q",
						got.account.SMTPHost, s.values["email_account"], smokeMailSMTPHost)
				}
				if len(got.opts.To) != 1 || got.opts.To[0] != smokeMailRecipient {
					t.Errorf("delivery addressed %v, want [%s]", got.opts.To, smokeMailRecipient)
				}
				if want := "tool smoke " + s.values["runID"]; got.opts.Subject != want {
					t.Errorf("delivery subject = %q, want %q", got.opts.Subject, want)
				}
				if !strings.Contains(got.opts.Body, smokeMailBody) {
					t.Errorf("delivery body = %q, want it to carry %q", truncate(got.opts.Body, 400), smokeMailBody)
				}
			},
		},
		{
			tool: "email__message_list",
			args: func(t *testing.T, s *smokeState) map[string]any {
				return map[string]any{"account": s.need(t, "email_unreachable_account"), "limit": 1}
			},
			assertsErrorShapeOnly: `imap_host .* resolves to disallowed address`,
		},
		{
			tool: "email__message_read",
			args: func(t *testing.T, s *smokeState) map[string]any {
				return map[string]any{"account": s.need(t, "email_unreachable_account"), "folder": "INBOX", "uid": 1}
			},
			assertsErrorShapeOnly: `imap_host .* resolves to disallowed address`,
		},
	}
}

func deploymentAndMCPSmokeCases() []smokeCase {
	return []smokeCase{
		{tool: "settings_provider_list", args: noArgs},
		{tool: "settings_provider_create", args: func(t *testing.T, s *smokeState) map[string]any {
			id := "tool-smoke-provider-" + s.values["runID"]
			s.set("deployment_provider_id", id)
			return map[string]any{"id": id, "type": "anthropic", "name": id, "enabled": true, "base_url": "https://provider.example.test"}
		}, check: captureIDAndVersion("settings_provider_create", "deployment_provider_id", "deployment_provider_version")},
		{tool: "settings_provider_get", args: byID("deployment_provider_id"), check: captureVersion("settings_provider_get", "deployment_provider_version")},
		{tool: "settings_provider_update", args: func(t *testing.T, s *smokeState) map[string]any {
			return map[string]any{"id": s.need(t, "deployment_provider_id"), "expected_version": s.need(t, "deployment_provider_version"), "type": "anthropic", "name": "updated " + s.need(t, "deployment_provider_id"), "enabled": true, "base_url": "https://provider.example.test"}
		}, check: captureVersion("settings_provider_update", "deployment_provider_version")},
		{tool: "settings_provider_delete", args: func(t *testing.T, s *smokeState) map[string]any {
			return map[string]any{"id": s.need(t, "deployment_provider_id"), "expected_version": s.need(t, "deployment_provider_version")}
		}, confirm: &smokeConfirm{tool: "settings_provider_get", args: byID("deployment_provider_id"), wantsError: `(?i)(not found|no rows)`}},
		{tool: "settings_default_model_get", args: noArgs, check: captureVersion("settings_default_model_get", "default_model_version")},
		{tool: "settings_default_model_update", args: func(t *testing.T, s *smokeState) map[string]any {
			return map[string]any{"expected_version": s.need(t, "default_model_version"), "model": "", "model_thinking": "", "model_strong": "", "model_strong_thinking": "", "model_fast": "", "model_fast_thinking": "", "model_vision": "", "model_embedding": ""}
		}, check: captureVersion("settings_default_model_update", "default_model_version")},
		{tool: "settings_embedding_setting_get", args: noArgs, check: captureVersion("settings_embedding_setting_get", "embedding_setting_version")},
		{tool: "settings_embedding_setting_update", args: func(t *testing.T, s *smokeState) map[string]any {
			return map[string]any{"expected_version": s.need(t, "embedding_setting_version"), "enabled": false, "dim": 1536, "normalize": false}
		}, check: captureVersion("settings_embedding_setting_update", "embedding_setting_version")},
		{tool: "settings_plugin_list", args: noArgs},
		{tool: "settings_plugin_disable", args: func(t *testing.T, _ *smokeState) map[string]any {
			return map[string]any{"kind": "channel", "name": "telegram"}
		}, confirm: &smokeConfirm{tool: "settings_plugin_list", args: noArgs, check: pluginListedEnabled("telegram", false)}},
		{tool: "settings_plugin_enable", args: func(t *testing.T, _ *smokeState) map[string]any {
			return map[string]any{"kind": "channel", "name": "telegram"}
		}, confirm: &smokeConfirm{tool: "settings_plugin_list", args: noArgs, check: pluginListedEnabled("telegram", true)}},
		{tool: "settings_mcp_server_list", args: noArgs},
		{tool: "settings_mcp_server_create", args: func(t *testing.T, s *smokeState) map[string]any {
			return map[string]any{"scope": "user", "name": "tool-smoke-mcp-" + s.values["runID"], "url": "https://mcp.example.test"}
		}, check: captureIDAndVersion("settings_mcp_server_create", "mcp_server_id", "mcp_server_version")},
		{tool: "settings_mcp_server_get", args: byID("mcp_server_id"), check: captureVersion("settings_mcp_server_get", "mcp_server_version")},
		{tool: "settings_mcp_server_update", args: func(t *testing.T, s *smokeState) map[string]any {
			return map[string]any{"id": s.need(t, "mcp_server_id"), "expected_version": s.need(t, "mcp_server_version"), "name": "tool-smoke-mcp-updated-" + s.values["runID"]}
		}, check: captureVersion("settings_mcp_server_update", "mcp_server_version")},
		{tool: "settings_mcp_server_probe", args: func(t *testing.T, s *smokeState) map[string]any {
			return map[string]any{"id": s.need(t, "mcp_server_id")}
		}, check: mcpProbeStatusIsError},
		{tool: "settings_mcp_server_delete", args: func(t *testing.T, s *smokeState) map[string]any {
			return map[string]any{"id": s.need(t, "mcp_server_id"), "expected_version": s.need(t, "mcp_server_version")}
		}, confirm: &smokeConfirm{tool: "settings_mcp_server_get", args: byID("mcp_server_id"), wantsError: `(?i)(not found|no rows)`}},
	}
}

// mcpProbeStatusIsError judges the probe tool by its persisted effect: the
// smoke registration points at an unreachable host, so a healthy catalog is
// impossible and status must read error. The success path is covered by
// TestMCPServerCreateProbesAndReturnsCatalog (internal/server) and
// TestProbeSuccessPersistsToolsAndStatus (internal/mcp).
func mcpProbeStatusIsError(t *testing.T, s *smokeState, results map[string]string) {
	t.Helper()
	var out struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal([]byte(results["settings_mcp_server_probe"]), &out); err != nil {
		t.Fatalf("probe result = %q, want JSON: %v", results["settings_mcp_server_probe"], err)
	}
	if out.Status != "error" {
		t.Fatalf("probe status = %q, want error for the unreachable smoke registration", out.Status)
	}
}

func captureIDAndVersion(tool, idKey, versionKey string) func(*testing.T, *smokeState, map[string]string) {
	return func(t *testing.T, s *smokeState, results map[string]string) {
		var value struct {
			ID      string `json:"id"`
			Version string `json:"version"`
		}
		if err := json.Unmarshal([]byte(results[tool]), &value); err != nil || value.ID == "" || value.Version == "" {
			t.Fatalf("%s result = %q, want id and version: %v", tool, results[tool], err)
		}
		s.set(idKey, value.ID)
		s.set(versionKey, value.Version)
	}
}

func captureVersion(tool, key string) func(*testing.T, *smokeState, map[string]string) {
	return func(t *testing.T, s *smokeState, results map[string]string) {
		var value struct {
			Version string `json:"version"`
		}
		if err := json.Unmarshal([]byte(results[tool]), &value); err != nil || value.Version == "" {
			t.Fatalf("%s result = %q, want version: %v", tool, results[tool], err)
		}
		s.set(key, value.Version)
	}
}

func pluginListedEnabled(name string, enabled bool) func(*testing.T, *smokeState, string) {
	return func(t *testing.T, _ *smokeState, result string) {
		var value struct {
			Plugins []struct {
				Name    string `json:"name"`
				Enabled bool   `json:"enabled"`
			} `json:"plugins"`
		}
		if err := json.Unmarshal([]byte(result), &value); err != nil {
			t.Fatal(err)
		}
		for _, plugin := range value.Plugins {
			if plugin.Name == name {
				if plugin.Enabled != enabled {
					t.Fatalf("plugin %q enabled = %t, want %t", name, plugin.Enabled, enabled)
				}
				return
			}
		}
		t.Fatalf("settings_plugin_list did not return %q: %s", name, result)
	}
}

// protocolExceptions are the model-facing tools this gate deliberately does not
// invoke, each with the coverage that stands in its place. The list is closed:
// the coverage assertion requires every entry to be a real tool name in this
// build, and every tool not listed here to have a case.
var protocolExceptions = map[string]string{
	// `code` is the vehicle: every case in this file is a `code` call, so its
	// outer dispatch — schema admission, VM boot, catalog, child fan-out, result
	// marshalling — is proven once per case rather than once in a case of its own.
	"code": "invoked by every case as the Code Mode entry point",
	// goal_control is registered only inside a Goal attempt's executor, with a
	// different schema per attempt stage. It is unreachable from a chat session
	// by construction, and driving it needs the Goal dispatcher's async workers.
	// Its coverage is internal/goal's TestExecutorRoutesDecomposeFlag, which
	// executes the injected control tool for both the decompose and submit
	// stages, and TestGoalControlSchemasMarshalAndPinDriftFields, which pins the
	// per-stage schemas. The goal_lifecycle system journey is deliberately not
	// cited: it is red on main because Code Mode moved goal_control off the
	// provider-facing tool list and the fake can no longer identify a Goal turn
	// (STELLA-45). Cite it again once that is fixed.
	"goal_control": "Goal attempt protocol; internal/goal TestExecutorRoutesDecomposeFlag and TestGoalControlSchemasMarshalAndPinDriftFields",
	// The mcp__ prefix cannot be closed from a hermetic test: internal/mcp
	// refuses any endpoint that resolves to a loopback, private, link-local or
	// unspecified address (client.go validatePublicIP), which is the SSRF guard
	// that makes user-registered MCP endpoints safe. A stub server on 127.0.0.1
	// is rejected at registration, and every address a test can bind is in one of
	// those ranges, so proving the prefix end to end would mean weakening the
	// guard. internal/mcp's own tests cover registration, transport selection,
	// name prefixing, and invocation against a fake client instead.
	smokeMCPPrefixTool: "user-registered MCP endpoints; internal/mcp tests cover them, and its SSRF guard rejects any endpoint a hermetic test can bind",
}

// smokeMCPPrefixTool stands for the whole mcp__ family, whose names exist only
// once a user registers a server. It is a representative name, not a tool this
// build registers on its own.
const smokeMCPPrefixTool = "mcp__tool-smoke__echo"

// smokePNGBase64 is a 1x1 PNG. view_image needs a real image on disk, and a
// literal keeps the fixture in the file that uses it.
const smokePNGBase64 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="

const smokeModel = "claude-sonnet-4-6"

// smokeHarness is one fully wired deployment: the production composition root
// (setup) against a live database, a scripted provider, and the fixtures the
// external-dependency tools need. Every fixture is either a loopback server or
// an unrouted documentation address nothing dials. The cutover setup performs
// no startup manifest installation, so this harness reaches nothing outside
// the host.
type smokeHarness struct {
	setup     *setupResult
	fake      *smokeProvider
	sink      *smokeChannel
	sentMail  *smokeMailbox
	userID    string
	agentID   string
	authority authz.Authority
	runID     string
	ctx       context.Context
}

func TestToolSmoke(t *testing.T) {
	h := newSmokeHarness(t)
	state := h.seedFixtures(t)

	cases := smokeCases(h)
	catalog := h.readCodeCatalog(t)
	assertSmokeCoverageIsClosed(t, smokeToolUniverse(t), catalog, cases)

	report := make([]string, 0, len(cases))
	for _, smoke := range cases {
		outcome := "ok"
		if smoke.assertsErrorShapeOnly != "" {
			outcome = "error-shape-only"
		}
		if !t.Run(smoke.tool, func(t *testing.T) { h.runSmokeCase(t, smoke, state) }) {
			outcome = "FAILED"
		}
		report = append(report, fmt.Sprintf("%-26s %s", smoke.tool, outcome))
		// A tool folded into a sibling case still gets its own report line: the
		// report is the coverage answer, so it must name every tool, not every case.
		for _, covered := range smoke.covers {
			report = append(report, fmt.Sprintf("%-26s %-17s %s", covered, outcome, "invoked inside the "+smoke.tool+" case"))
		}
	}
	for tool, reason := range protocolExceptions {
		report = append(report, fmt.Sprintf("%-26s %-17s %s", tool, "exception", reason))
	}
	sort.Strings(report)
	t.Logf("tool smoke coverage (%d cases, %d exceptions):\n%s",
		len(cases), len(protocolExceptions), strings.Join(report, "\n"))
}

// newSmokeHarness boots the real server composition in process: a migrated
// database from dbtest, a temporary STELLA_HOME, and setup() itself, so the
// tools under test are the ones newBuiltinTools registers in production rather
// than a test-assembled lookalike. River and the scheduler run because the
// scheduler tools need them; the Goal dispatch tick stays off, or it would run
// planning turns concurrently and consume a case's scripted response.
func newSmokeHarness(t *testing.T) *smokeHarness {
	t.Helper()
	db := dbtest.NewAtMigration(t, toolSmokeImportMigration41)
	prepareToolSmokeCutover(t, db)
	runID := strings.ToLower(uuid.Must(uuid.NewV7()).String()[24:])

	vaultKey, err := vault.GenerateMasterIdentity()
	if err != nil {
		t.Fatalf("tool smoke: generate vault key: %v", err)
	}
	stellaHome := t.TempDir()
	prepareToolSmokeCoreCache(t, stellaHome)
	t.Setenv("STELLA_HOME", stellaHome)
	t.Setenv("STELLA_DATABASE_URL", db.Config().ConnString())
	t.Setenv("STELLA_VAULT_KEY", vaultKey)
	config.ResetStellaHome()
	t.Cleanup(config.ResetStellaHome)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	fake := newSmokeProvider(t)
	store := cfgstore.NewDBStore(db)
	// Provider and Agent are created before setup so StartAll builds the agent
	// exactly as a restart would, from durable rows.
	if err := store.CreateProvider(ctx, config.Provider{
		ID: "tool-smoke", Type: "anthropic", Name: "tool smoke", Enabled: true,
		APIKey: "tool-smoke-not-a-key", BaseURL: fake.server.URL,
		Models: map[string]config.ProviderModelOverride{smokeModel: {
			Name: config.ValuePtr(smokeModel), Enabled: config.ValuePtr(true), ContextWindow: config.ValuePtr(200000), MaxTokens: config.ValuePtr(8192),
		}},
	}); err != nil {
		t.Fatalf("tool smoke: create provider: %v", err)
	}
	// Settings tools are opt-in per Agent. The smoke harness enables its scripted
	// direct-chat Agent explicitly so the complete production catalog remains
	// covered while every ordinary new Agent still defaults off.
	agentID := cfgstore.DefaultStellaAgentID
	if err := store.CreateAgent(ctx, config.Agent{
		ID: agentID, Name: "tool-smoke-" + runID, Model: "tool-smoke/" + smokeModel,
		Scope: config.AgentScopeSystem, Enabled: true, SystemSettingsToolsEnabled: true,
	}); err != nil {
		t.Fatalf("tool smoke: create agent: %v", err)
	}

	// view_image renders a baseline description through the vision model; without
	// one configured it falls back to the local extractor, which has nothing to
	// say about a 1x1 image. Pointing it at the same scripted provider keeps the
	// render on loopback and makes the extra turn schedulable.
	if err := config.SaveDefaultModels(ctx, store, config.DefaultModels{ModelVision: "tool-smoke/" + smokeModel}); err != nil {
		t.Fatalf("tool smoke: set the vision model: %v", err)
	}

	cfg, err := config.LoadServerConfig(os.LookupEnv)
	if err != nil {
		t.Fatalf("tool smoke: load server config: %v", err)
	}
	if err := ensureEmbeddedAssets(); err != nil {
		t.Fatalf("tool smoke: install embedded assets: %v", err)
	}
	// The cutover setup has no manifest installation hook. This smoke harness
	// supplies embedded assets and loopback services, so it never downloads or
	// installs a CLI while exercising the production composition root.
	result, err := setup(ctx, cfg, "http://127.0.0.1:0")
	if err != nil {
		t.Fatalf("tool smoke: setup: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		result.waitBackgroundTasks()
		_ = result.poolManager.Close()
		_ = result.workspaceManager.Close()
	})

	// River and the scheduler run because scheduler__job_create registers a River
	// periodic job and panics without a started client. The Goal dispatch tick is
	// deliberately NOT started: it would begin planning turns of its own against
	// a provider whose every turn is scripted, and steal a case's response. The
	// goal tools are still invoked; only the async driver stays off, and
	// goal_lifecycle in test/system is the journey that drives it.
	if err := result.riverClient.Start(ctx); err != nil {
		t.Fatalf("tool smoke: start river: %v", err)
	}
	t.Cleanup(func() { _ = result.riverClient.Stop(context.Background()) })
	if err := result.schedulerSvc.Start(ctx); err != nil {
		t.Fatalf("tool smoke: start scheduler: %v", err)
	}
	t.Cleanup(func() { _ = result.schedulerSvc.Stop() })

	publicKey, privateKey, err := vault.GenerateUserKeys(result.vaultSvc.MasterRecipient())
	if err != nil {
		t.Fatalf("tool smoke: generate user keys: %v", err)
	}
	user, err := appdb.NewAuthStore(db).CreateUser(ctx, auth.User{
		ID: uuid.Must(uuid.NewV7()).String(), Email: "tool-smoke-" + runID + "@example.test",
		Name: "tool smoke", Role: auth.RoleAdmin, AgePublicKey: publicKey, AgePrivateKey: privateKey,
	})
	if err != nil {
		t.Fatalf("tool smoke: create user: %v", err)
	}
	authority, err := authz.NewUserAuthority(authz.UserID(user.ID), true)
	if err != nil {
		t.Fatalf("tool smoke: build authority: %v", err)
	}
	if err := disableSmokeCLIPlugins(ctx, result.pluginService, authority); err != nil {
		t.Fatalf("tool smoke: disable optional CLI plugins: %v", err)
	}

	return &smokeHarness{
		setup: result, fake: fake, userID: user.ID, agentID: agentID,
		authority: authority, runID: runID, ctx: ctx,
	}
}

// disableSmokeCLIPlugins closes optional CLI definitions through the common
// plugin mutation boundary before the first runner is built. The smoke suite
// exercises Go and core tools, while CLI installation is a separate production
// path that would require network access and a host-managed toolchain.
func disableSmokeCLIPlugins(ctx context.Context, service *plugin.Service, authority authz.Authority) error {
	access, err := service.Begin(authority)
	if err != nil {
		return err
	}
	definitions, err := access.ListDefinitions(ctx)
	if err != nil {
		return err
	}
	disabled := false
	for _, definition := range definitions {
		if definition.Backend != plugin.BackendCLI {
			continue
		}
		configs, err := access.ListConfigs(ctx, definition.ID, plugin.ScopeSystem, "")
		if err != nil {
			return fmt.Errorf("list %s configs: %w", definition.ID, err)
		}
		for _, config := range configs {
			if _, err := access.UpdateConfig(ctx, definition.ID, config.ID, config.Revision, plugin.ConfigPatch{EnabledSet: true, Enabled: &disabled}); err != nil {
				return fmt.Errorf("disable %s config %s: %w", definition.ID, config.ID, err)
			}
		}
	}
	return nil
}

const toolSmokeImportMigration41 = int64(90000000000041)

// prepareToolSmokeCutover exercises the supported startup ordering in a
// hermetic fixture: import at the migration-41 boundary, then advance the
// database to the current schema before setup opens it. This keeps startup's
// import call from incorrectly treating migration 42 as the import boundary.
func prepareToolSmokeCutover(t *testing.T, db *pgxpool.Pool) {
	t.Helper()
	ctx := t.Context()
	if err := plugin.ImportLegacyState(ctx, db, plugin.NewCatalog(), nil); err != nil {
		t.Fatalf("tool smoke: import migration-41 fixture: %v", err)
	}
	upgraded, err := appdb.OpenDB(db.Config().ConnString())
	if err != nil {
		t.Fatalf("tool smoke: advance fixture after import: %v", err)
	}
	upgraded.Close()
}

// prepareToolSmokeCoreCache seeds the exact content-addressed selection that
// core.Prepare accepts as complete. The smoke suite exercises plugin/tool
// wiring, so fixed network-backed fd/rg installation is outside its boundary.
func prepareToolSmokeCoreCache(t *testing.T, stellaHome string) {
	t.Helper()
	identity, err := core.RuntimeIdentity()
	if err != nil {
		t.Fatalf("tool smoke: resolve core runtime identity: %v", err)
	}
	publicDir := filepath.Join(stellaHome, ".mise-tools", "public", identity)
	if err := os.MkdirAll(publicDir, 0o755); err != nil {
		t.Fatalf("tool smoke: create core runtime cache: %v", err)
	}
	for _, resource := range core.RuntimeResources() {
		if err := os.WriteFile(filepath.Join(publicDir, resource.Name), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatalf("tool smoke: seed core runtime %s: %v", resource.Name, err)
		}
	}
	if err := os.WriteFile(filepath.Join(publicDir, ".stella-shell-env"), nil, 0o644); err != nil {
		t.Fatalf("tool smoke: seed core shell environment: %v", err)
	}
}

// seedFixtures installs everything a tool needs before it can succeed, and
// returns the state the cases read it from. No fixture can reach off-host: the
// servers bind loopback and the mail hosts are RFC 5737 documentation literals.
func (h *smokeHarness) seedFixtures(t *testing.T) *smokeState {
	t.Helper()
	// notify has no "web channel" to fall back on: a Notifier with no registered
	// channel can only fail. Registering a sink is what a channel plugin does,
	// and it lets the case assert the message actually arrived.
	h.sink = &smokeChannel{}
	h.setup.notifier.Register(h.sink)
	h.sentMail = &smokeMailbox{}

	// Both accounts point at address literals, so ValidateAccountEgress resolves
	// no name and the gate makes no DNS query. 198.51.100.0/24 is TEST-NET-2
	// (RFC 5737), reserved for documentation and never routed.
	emailConfig := `{"default":"smoke","accounts":{` +
		`"smoke":{"email":"tool-smoke@example.test","username":"tool-smoke","password":"tool-smoke-not-a-secret",` +
		`"imap_host":"198.51.100.10","imap_port":993,"imap_tls":"ssl",` +
		`"smtp_host":"198.51.100.11","smtp_port":465,"smtp_tls":"ssl"},` +
		`"unreachable":{"email":"tool-smoke@example.test","username":"tool-smoke","password":"tool-smoke-not-a-secret",` +
		`"imap_host":"127.0.0.1","imap_port":993,"imap_tls":"ssl",` +
		`"smtp_host":"127.0.0.1","smtp_port":465,"smtp_tls":"ssl"}}}`
	if err := h.setup.vaultSvc.Set(h.ctx, h.userID, "EMAIL_CONFIG", emailConfig); err != nil {
		t.Fatalf("tool smoke: seed EMAIL_CONFIG: %v", err)
	}
	// The send seam is production API (SetSendFunc), not a test-only hook: it
	// exists so a deployment can substitute delivery. Nothing is ever put on a
	// socket, and the recorded call proves the tool reached delivery.
	h.setup.emailSvc.SetSendFunc(h.sentMail.record)

	// oauth_disconnect can only prove it removed something if something is
	// connected. Both the client credentials and the token are fixtures, and the
	// bundle is written straight into the vault, so no authorization flow runs
	// and nothing is sent to github.
	if err := h.setup.credSvc.SetOAuthProviderConfig(h.ctx, connections.OAuthProviderConfig{
		ProviderID:   smokeOAuthProvider,
		ClientID:     "tool-smoke-not-a-client-id",
		ClientSecret: "tool-smoke-not-a-secret",
		RedirectURL:  "http://127.0.0.1/oauth/callback",
	}); err != nil {
		t.Fatalf("tool smoke: seed the oauth provider config: %v", err)
	}
	if err := h.setup.oauthRegistry.SaveBundle(h.ctx, h.setup.vaultSvc, smokeOAuthProvider, h.userID, oauth.OAuthBundle{
		Version:         1,
		ClientID:        "tool-smoke-not-a-client-id",
		AccessToken:     "tool-smoke-not-a-token",
		AccessExpiresAt: time.Now().Add(time.Hour).UTC(),
	}); err != nil {
		t.Fatalf("tool smoke: seed the oauth token bundle: %v", err)
	}

	return &smokeState{values: map[string]string{
		"runID": h.runID,
		// The feed lives on loopback so recally__feed_poll runs its real fetch and
		// parse path without leaving the host.
		"rss_url":                   newFakeRSSServer(t, h.runID),
		"email_account":             "smoke",
		"email_unreachable_account": "unreachable",
	}}
}

// runSmokeCase drives one turn: the model calls `code`, the VM invokes the
// case's tools, and each child's own result comes back on the agent event
// stream, keyed by the tool that produced it.
func (h *smokeHarness) runSmokeCase(t *testing.T, smoke smokeCase, state *smokeState) {
	t.Helper()
	h.fake.enqueueTool("toolu_smoke_"+smoke.tool, "code", h.smokeCodeArgs(t, smoke, state))
	for _, reply := range smoke.extraReplies {
		h.fake.enqueueText(reply)
	}
	h.fake.enqueueText("smoke " + smoke.tool + " done")

	settled := h.runTurn(t, "smoke "+smoke.tool)
	results := make(map[string]string, len(smoke.covers)+1)
	for _, tool := range append([]string{smoke.tool}, smoke.covers...) {
		child, ok := settled[tool]
		if !ok {
			t.Fatalf("no child tool result for %q; the code call never reached it (saw: %s)\ncode returned: %s",
				tool, strings.Join(settledToolNames(settled), " "), truncate(settled["code"].text, 1200))
		}
		t.Logf("%s result: %s", tool, truncate(child.text, 400))
		if tool == smoke.tool && smoke.assertsErrorShapeOnly != "" {
			if !child.failed {
				t.Fatalf("%s succeeded, but the case only asserts its canonical error; drop assertsErrorShapeOnly: %s",
					tool, truncate(child.text, 800))
			}
			if !regexp.MustCompile(smoke.assertsErrorShapeOnly).MatchString(child.text) {
				t.Fatalf("%s error = %q, want a match for %q", tool, truncate(child.text, 800), smoke.assertsErrorShapeOnly)
			}
			// An error-shape-only case must reach the tool's own logic. A schema
			// rejection means the arguments never got that far, which would let a
			// case claim coverage for a tool it never really called.
			if schemaRejection.MatchString(child.text) {
				t.Fatalf("%s was rejected before it ran, so the case proves nothing about the tool: %s",
					tool, truncate(child.text, 800))
			}
			return
		}
		if child.failed {
			t.Fatalf("%s returned an error result: %s", tool, truncate(child.text, 2000))
		}
		results[tool] = child.text
	}
	if smoke.check != nil {
		smoke.check(t, state, results)
	}
	if smoke.confirm != nil {
		h.runSmokeConfirm(t, smoke, state)
	}
}

// runSmokeConfirm re-reads the case's side effect through a sibling tool, in a
// turn of its own so the read cannot see anything the writing VM held in memory.
func (h *smokeHarness) runSmokeConfirm(t *testing.T, smoke smokeCase, state *smokeState) {
	t.Helper()
	confirm := smoke.confirm
	args := map[string]any{}
	if confirm.args != nil {
		args = confirm.args(t, state)
	}
	script := fmt.Sprintf(
		"return tools.invoke(%s, %s).then("+
			"result => tools.text(result),"+
			"failure => \"confirm failed\""+
			");",
		mustJSON(t, confirm.tool), mustJSON(t, args))
	h.fake.enqueueTool("toolu_confirm_"+smoke.tool, "code", mustJSON(t, map[string]string{"code": script}))
	h.fake.enqueueText("confirm " + smoke.tool + " done")

	settled := h.runTurn(t, "confirm "+smoke.tool)
	child, ok := settled[confirm.tool]
	if !ok {
		t.Fatalf("confirming %s with %s: the code call never reached it (saw: %s)",
			smoke.tool, confirm.tool, strings.Join(settledToolNames(settled), " "))
	}
	t.Logf("%s confirm via %s: %s", smoke.tool, confirm.tool, truncate(child.text, 400))
	if confirm.wantsError != "" {
		if !child.failed {
			t.Fatalf("%s ran, but %s should have made it fail: %s", confirm.tool, smoke.tool, truncate(child.text, 800))
		}
		if !regexp.MustCompile(confirm.wantsError).MatchString(child.text) {
			t.Fatalf("after %s, %s failed with %q, want a match for %q",
				smoke.tool, confirm.tool, truncate(child.text, 800), confirm.wantsError)
		}
		return
	}
	if child.failed {
		t.Fatalf("confirming %s: %s returned an error: %s", smoke.tool, confirm.tool, truncate(child.text, 800))
	}
	if confirm.check != nil {
		confirm.check(t, state, child.text)
	}
}

// absent asserts a sibling read no longer mentions a value an earlier case
// captured, which is what a delete tool has to prove.
func absent(tool, key string) func(*testing.T, *smokeState, string) {
	return func(t *testing.T, s *smokeState, result string) {
		gone := s.need(t, key)
		if strings.Contains(result, gone) {
			t.Errorf("%s still lists %s %q after it was removed: %s", tool, key, gone, truncate(result, 800))
		}
	}
}

// present asserts a sibling read does mention a captured value.
func present(tool, key string) func(*testing.T, *smokeState, string) {
	return func(t *testing.T, s *smokeState, result string) {
		want := s.need(t, key)
		if !strings.Contains(result, want) {
			t.Errorf("%s does not mention %s %q: %s", tool, key, want, truncate(result, 800))
		}
	}
}

// smokeCodeArgs renders the `code` tool input for one case. The generated
// single-invoke program settles the rejection with an explicit onRejected
// handler rather than try/catch around await: the Code Mode VM marks a child
// failure observed only through the promise's own then/catch, so an awaited
// rejection would still be rethrown when the executor drains its children.
func (h *smokeHarness) smokeCodeArgs(t *testing.T, smoke smokeCase, state *smokeState) string {
	t.Helper()
	var script string
	switch {
	case smoke.script != nil:
		script = smoke.script(t, state)
	default:
		args := map[string]any{}
		if smoke.args != nil {
			args = smoke.args(t, state)
		}
		script = fmt.Sprintf(
			"return tools.invoke(%s, %s).then("+
				"result => ({ ok: true, text: tools.text(result) }),"+
				"failure => ({ ok: false, error: failure && failure.value ? tools.text(failure.value) : String(failure && failure.message) })"+
				");",
			mustJSON(t, smoke.tool), mustJSON(t, args))
	}
	return mustJSON(t, map[string]string{"code": script})
}

// childToolResult is one settled tool invocation observed on the event stream.
type childToolResult struct {
	text   string
	failed bool
}

// runTurn runs one chat turn to completion and returns every settled tool
// result by tool name. Each case gets a fresh session: nothing a case leaves in
// a transcript can change the next case's request.
func (h *smokeHarness) runTurn(t *testing.T, prompt string) map[string]childToolResult {
	t.Helper()
	svc := h.setup.poolManager.GetService(h.agentID)
	if svc == nil {
		t.Fatalf("tool smoke: no agent service for %s", h.agentID)
	}
	ctx, cancel := context.WithTimeout(h.ctx, 3*time.Minute)
	defer cancel()

	settled := map[string]childToolResult{}
	for event := range svc.Chat(ctx, agent.ChatRequest{
		UserID: h.userID, AgentID: h.agentID, Authority: h.authority,
		Channel: session.ChannelWeb, Kind: session.KindChat, Message: prompt,
	}) {
		if event.Err != nil {
			t.Fatalf("tool smoke: turn failed: %v", event.Err)
		}
		if use := event.ToolUse; use != nil && use.Status != "running" {
			settled[use.Tool] = childToolResult{text: use.Content, failed: use.Status == "error"}
		}
	}
	return settled
}

func settledToolNames(settled map[string]childToolResult) []string {
	names := make([]string, 0, len(settled))
	for name := range settled {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// readCodeCatalog asks the running system what tools the model can reach. The
// catalog has no Go-side accessor by design, so this pages it out through the
// same tools.search the model uses.
func (h *smokeHarness) readCodeCatalog(t *testing.T) []string {
	t.Helper()
	const listCatalog = `{"code":"const names = []; for (let offset = 0; ; ) { const page = tools.search(\"\", offset); for (const tool of page) { names.push(tool.name); } if (!page.hasMore) { break; } offset = page.nextOffset; } return names;"}`
	h.fake.enqueueTool("toolu_smoke_catalog", "code", listCatalog)
	h.fake.enqueueText("catalog listed")

	settled := h.runTurn(t, "list the tool catalog")
	raw, ok := settled["code"]
	if !ok || raw.failed {
		t.Fatalf("tool smoke: the catalog call did not settle successfully: %+v", raw)
	}
	var names []string
	if err := json.Unmarshal([]byte(raw.text), &names); err != nil {
		t.Fatalf("tool smoke: catalog is not a JSON array: %v\n%s", err, truncate(raw.text, 800))
	}
	sort.Strings(names)
	t.Logf("code catalog (%d tools): %s", len(names), strings.Join(names, " "))
	return names
}

// smokeToolUniverse is every tool this build can put in front of a model:
// the production builtin surface (defaultToolNames, itself pinned to
// newBuiltinTools by TestDefaultToolNamesMatchGolden), plus the three protocol
// names that are covered by protocolExceptions rather than a generated case.
func smokeToolUniverse(t *testing.T) []string {
	t.Helper()
	runtimeRegistered := []string{"code", "goal_control", smokeMCPPrefixTool}
	for _, name := range runtimeRegistered {
		if _, ok := protocolExceptions[name]; !ok {
			t.Errorf("tool smoke: %q is listed as runtime-registered, but has no protocol exception", name)
		}
	}
	return append(defaultToolNames(t), runtimeRegistered...)
}

// assertSmokeCoverageIsClosed is the discipline this gate exists for, as one
// set equation: the tools this build can show a model must equal the tools the
// cases invoke plus the explicitly documented protocol exceptions. A new tool
// with no case fails; a case for a tool that no longer exists fails; an
// exception that is not a real tool fails. There is no pending list, and a tool
// missing from the runtime catalog is a failure rather than a free pass.
func assertSmokeCoverageIsClosed(t *testing.T, universe, catalog []string, cases []smokeCase) {
	t.Helper()
	covered := map[string]bool{}
	for _, smoke := range cases {
		for _, tool := range append([]string{smoke.tool}, smoke.covers...) {
			if covered[tool] {
				t.Errorf("tool smoke: %q is invoked by more than one case; a tool is invoked exactly once", tool)
			}
			covered[tool] = true
		}
	}
	known := map[string]bool{}
	for _, name := range universe {
		known[name] = true
		switch {
		case covered[name]:
		case protocolExceptions[name] != "":
		default:
			t.Errorf("tool smoke: %q is model-facing but has no smoke case; add one to smokeCases() or document it in protocolExceptions", name)
		}
		if covered[name] && protocolExceptions[name] != "" {
			t.Errorf("tool smoke: %q is both invoked and listed as a protocol exception; delete the exception", name)
		}
	}
	for tool := range covered {
		if !known[tool] {
			t.Errorf("tool smoke: %q has a case but this build registers no such tool; the case proves nothing", tool)
		}
	}
	for tool := range protocolExceptions {
		if !known[tool] {
			t.Errorf("tool smoke: %q is listed as a protocol exception but is not a tool this build registers", tool)
		}
	}

	// The runtime catalog is the model's actual reach. Every invoked tool must be
	// in it, and it must contain nothing the universe does not know about.
	inCatalog := map[string]bool{}
	for _, name := range catalog {
		inCatalog[name] = true
		if !known[name] {
			t.Errorf("tool smoke: the model's catalog offers %q, which is not in the covered universe", name)
		}
	}
	for tool := range covered {
		if !inCatalog[tool] {
			t.Errorf("tool smoke: %q has a case but the catalog does not offer it to the model (catalog: %s)",
				tool, strings.Join(catalog, " "))
		}
	}
	// The code tool is the entry point, never a catalog entry: every case reaches
	// its tool through it.
	if inCatalog["code"] {
		t.Error("tool smoke: `code` is inside its own catalog; the entry point must not be reachable as a child call")
	}
}

// smokeChannel is a notification sink implementing the channel contract, which
// is the only way a Notifier can have anywhere to deliver: there is no built-in
// "web" channel to fall back on.
type smokeChannel struct {
	mu       sync.Mutex
	received []string
}

func (c *smokeChannel) Name() string                    { return "tool-smoke-sink" }
func (c *smokeChannel) Start(ctx context.Context) error { <-ctx.Done(); return nil }
func (c *smokeChannel) Stop()                           {}
func (c *smokeChannel) Notify(_ context.Context, n pkgchannel.Notification) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.received = append(c.received, n.Text)
	return nil
}

func (c *smokeChannel) messages() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.received)
}

// smokeProvider is a scripted Anthropic-compatible endpoint: a FIFO of turns,
// each either a text reply or one tool call. It deliberately never branches on
// prompt prose — a case's turn is chosen by arrival order alone — so editing a
// system prompt can never turn into a failure here.
type smokeProvider struct {
	t       *testing.T
	server  *httptest.Server
	mu      sync.Mutex
	scripts []smokeTurn
	served  int
}

type smokeTurn struct {
	text     string
	toolID   string
	toolName string
	toolArgs string
}

func newSmokeProvider(t *testing.T) *smokeProvider {
	t.Helper()
	p := &smokeProvider{t: t}
	p.server = httptest.NewServer(http.HandlerFunc(p.handle))
	t.Cleanup(p.server.Close)
	// An unconsumed script means the system made fewer model calls than the case
	// assumed, which would silently skip a tool.
	t.Cleanup(func() {
		p.mu.Lock()
		defer p.mu.Unlock()
		if len(p.scripts) != 0 {
			t.Errorf("tool smoke: %d scripted model turns went unconsumed", len(p.scripts))
		}
	})
	return p
}

func (p *smokeProvider) enqueueText(text string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.scripts = append(p.scripts, smokeTurn{text: text})
}

func (p *smokeProvider) enqueueTool(id, name, args string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.scripts = append(p.scripts, smokeTurn{toolID: id, toolName: name, toolArgs: args})
}

func (p *smokeProvider) handle(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/v1/messages" || r.Method != http.MethodPost {
		p.t.Errorf("tool smoke: unexpected provider request %s %s", r.Method, r.URL.Path)
		http.Error(w, "unexpected path", http.StatusNotFound)
		return
	}
	advertised := p.advertisedTools(r)
	p.mu.Lock()
	p.served++
	if len(p.scripts) == 0 {
		served := p.served
		p.mu.Unlock()
		p.t.Errorf("tool smoke: unscripted model request #%d", served)
		http.Error(w, "unscripted", http.StatusInternalServerError)
		return
	}
	turn := p.scripts[0]
	p.scripts = p.scripts[1:]
	p.mu.Unlock()

	// A scripted tool call is only honest if the request actually offered that
	// tool. Without this the gate would keep passing after a regression that
	// stopped advertising `code`, because the fake would inject the call anyway.
	if turn.toolName != "" && !slices.Contains(advertised, turn.toolName) {
		p.t.Errorf("tool smoke: request #%d does not advertise %q; the system offered %v",
			p.served, turn.toolName, advertised)
	}
	// `code` is the whole model-facing tool path (#1182), so any request that
	// carries tools at all must carry it.
	if len(advertised) > 0 && !slices.Contains(advertised, "code") {
		p.t.Errorf("tool smoke: request #%d advertises %v without `code`; Code Mode is the only tool path",
			p.served, advertised)
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	flusher, ok := w.(http.Flusher)
	if !ok {
		p.t.Error("tool smoke: response writer cannot flush; the provider stream needs it")
		return
	}
	for _, frame := range turn.frames() {
		if _, err := io.WriteString(w, frame); err != nil {
			return
		}
		flusher.Flush()
	}
}

// advertisedTools reads the tool names the system offered the model on this
// request. A body it cannot parse is a failure, not an empty list: silently
// returning nothing would disable both checks above.
func (p *smokeProvider) advertisedTools(r *http.Request) []string {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		p.t.Errorf("tool smoke: read provider request body: %v", err)
		return nil
	}
	var payload struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		p.t.Errorf("tool smoke: provider request is not JSON: %v", err)
		return nil
	}
	names := make([]string, 0, len(payload.Tools))
	for _, tool := range payload.Tools {
		names = append(names, tool.Name)
	}
	return names
}

// frames renders one turn as the Anthropic streaming events the runner parses.
func (t smokeTurn) frames() []string {
	var frames []string
	emit := func(event string, data map[string]any) {
		payload, err := json.Marshal(data)
		if err != nil {
			// The maps are literals here, so a marshal failure is a bug in the fake.
			panic(fmt.Sprintf("tool smoke: marshal %s: %v", event, err))
		}
		frames = append(frames, fmt.Sprintf("event: %s\ndata: %s\n\n", event, payload))
	}
	emit("message_start", map[string]any{"type": "message_start", "message": map[string]any{
		"id": "msg_" + strings.TrimPrefix(t.toolID, "toolu_"), "type": "message", "role": "assistant",
		"model": smokeModel, "content": []any{}, "stop_reason": nil, "stop_sequence": nil,
		"usage": map[string]any{"input_tokens": 1, "output_tokens": 1},
	}})
	stopReason := "end_turn"
	if t.toolName != "" {
		stopReason = "tool_use"
		emit("content_block_start", map[string]any{
			"type": "content_block_start", "index": 0,
			"content_block": map[string]any{"type": "tool_use", "id": t.toolID, "name": t.toolName, "input": map[string]any{}},
		})
		emit("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": 0,
			"delta": map[string]any{"type": "input_json_delta", "partial_json": t.toolArgs},
		})
	} else {
		emit("content_block_start", map[string]any{
			"type": "content_block_start", "index": 0,
			"content_block": map[string]any{"type": "text", "text": ""},
		})
		emit("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": 0,
			"delta": map[string]any{"type": "text_delta", "text": t.text},
		})
	}
	emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
	emit("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": stopReason, "stop_sequence": nil},
		"usage": map[string]any{"output_tokens": 5},
	})
	emit("message_stop", map[string]any{"type": "message_stop"})
	return frames
}

// schemaRejection matches the errors a tool call gets before the tool runs:
// unknown name, or arguments the generated schema refused.
var schemaRejection = regexp.MustCompile(`(?i)(tool not found|unknown tool|unknown .* action|invalid input|unknown field|required|must be|failed to decode|cannot unmarshal)`)

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("tool smoke: marshal json: %v", err)
	}
	return string(b)
}

// newFakeRSSServer serves one static feed over loopback and returns its URL. It
// exists so the recally feed family exercises fetch, parse, and dedup for real
// while the suite's no-external-network rule holds.
// The first item's title and link are what recally__entry_list asserts, so the
// poll cannot pass by ingesting nothing.
const (
	smokeFeedItemTitle = "tool smoke item one"
	smokeFeedItemURL   = "http://127.0.0.1/tool-smoke/one"
)

func newFakeRSSServer(t *testing.T, runID string) string {
	t.Helper()
	feed := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0"><channel>
  <title>tool smoke feed %[1]s</title>
  <link>http://127.0.0.1/tool-smoke</link>
  <description>a loopback feed for the tool smoke journey</description>
  <item>
    <title>`+smokeFeedItemTitle+`</title>
    <link>`+smokeFeedItemURL+`</link>
    <guid>tool-smoke-%[1]s-one</guid>
  </item>
  <item>
    <title>tool smoke item two</title>
    <link>http://127.0.0.1/tool-smoke/two</link>
    <guid>tool-smoke-%[1]s-two</guid>
  </item>
</channel></rss>`, runID)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml; charset=utf-8")
		_, _ = io.WriteString(w, feed)
	}))
	t.Cleanup(server.Close)
	return server.URL + "/feed.xml"
}
