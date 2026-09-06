package resources

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

const siteScriptsSkillDir = "skills/core/web"

var siteScriptMeta = regexp.MustCompile(`(?s)^\s*/\*\s*@meta\s*(\{.*?\})\s*\*/\s*async\s+function\b`)

type siteScriptMetaDoc struct {
	Description  string                     `json:"description"`
	Domain       string                     `json:"domain"`
	Args         map[string]json.RawMessage `json:"args"`
	ReadOnly     bool                       `json:"readOnly"`
	AuthRequired bool                       `json:"authRequired"`
	Headers      map[string]string          `json:"headers"`
}

func siteScriptNames(t *testing.T) map[string]siteScriptMetaDoc {
	t.Helper()
	scripts := map[string]siteScriptMetaDoc{}
	skill := builtinSkillDescriptor(t, "web")
	for _, file := range skill.Files {
		if !strings.HasPrefix(file.Path, "sites/") || !strings.HasSuffix(file.Path, ".js") {
			continue
		}
		rel := strings.TrimSuffix(strings.TrimPrefix(file.Path, "sites/"), ".js")
		if strings.Count(rel, "/") != 1 {
			t.Fatalf("site script %s must live at sites/<site>/<name>.js", file.Path)
		}
		contentBytes, _, err := builtinSkillFile(t, skill, file.Path)
		if err != nil {
			t.Fatal(err)
		}
		match := siteScriptMeta.FindSubmatch(contentBytes)
		if match == nil {
			t.Fatalf("site script %s must start with /* @meta {...} */ followed by async function", file.Path)
		}
		var meta siteScriptMetaDoc
		if err := json.Unmarshal(match[1], &meta); err != nil {
			t.Fatalf("site script %s has invalid @meta JSON: %v", file.Path, err)
		}
		scripts[rel] = meta
	}
	if len(scripts) == 0 {
		t.Fatal("no bundled site scripts found")
	}
	return scripts
}

// Every bundled script must be runnable anonymously through Lightpanda: no
// browser login, read-only, and a declared domain the runner navigates to
// and scopes headers by.
func TestSiteScriptsAreAnonymousReadOnlyAndDescribed(t *testing.T) {
	for name, meta := range siteScriptNames(t) {
		if meta.Description == "" || meta.Domain == "" {
			t.Errorf("%s: @meta needs description and domain", name)
		}
		if meta.AuthRequired {
			t.Errorf("%s: authRequired scripts cannot run without a browser session", name)
		}
		if !meta.ReadOnly {
			t.Errorf("%s: only readOnly scripts ship with the skill", name)
		}
		for key, value := range meta.Headers {
			if strings.Contains(value, "${") && !regexp.MustCompile(`^[^$]*\$\{[A-Z][A-Z0-9_]*\}[^$]*$`).MatchString(value) {
				t.Errorf("%s: header %s must reference exactly one ${UPPER_CASE} variable", name, key)
			}
		}
	}
}

// SKILL.md examples are a contract with the bundled catalog: a name the prose
// invokes must exist, or the model learns a command that fails.
func TestSiteScriptsSkillNamesRealScripts(t *testing.T) {
	text := readBuiltinSkillPath(t, siteScriptsSkillDir+"/SKILL.md")
	scripts := siteScriptNames(t)
	found := 0
	for _, match := range regexp.MustCompile(`web\.ts site (?:run|info) ([a-z0-9_-]+/[a-z0-9_-]+)`).FindAllStringSubmatch(text, -1) {
		found++
		if _, ok := scripts[match[1]]; !ok {
			t.Errorf("SKILL.md invokes %q, which is not a bundled site script", match[1])
		}
	}
	if found == 0 {
		t.Fatal("SKILL.md must show at least one `web.ts site run` example")
	}
	if regexp.MustCompile(`\btap (site|fetch|run|browser|doctor)\b|agent-browser|AGENT_BROWSER`).MatchString(text) {
		t.Fatal("web SKILL.md must not send the model to the retired Tap CLI")
	}
}

// The runner is the skill's own bun program. Drive it with a fake lightpanda
// that captures the generated PandaScript, so the test pins the wrapper
// (navigation target, args, header scoping) without network.
func TestSiteScriptRunnerBuildsPandaScript(t *testing.T) {
	skillDir := webSkillDir(t)
	runner := filepath.Join(skillDir, "scripts", "web.ts")

	bin := t.TempDir()
	captured := filepath.Join(bin, "program.js")
	fake := filepath.Join(bin, "lightpanda")
	// The fake copies the script it was handed and answers like `run` does:
	// the program's JSON.stringify result printed verbatim.
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nfor a; do last=$a; done\ncp \"$last\" \""+captured+"\"\nprintf '{\"ok\":true,\"args\":%s}' \"$LP_FAKE_ARGS\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	cache := t.TempDir()
	run := func(env []string, args ...string) (string, string, int) {
		cmd := exec.Command("bun", append([]string{runner, "site"}, args...)...)
		cmd.Env = append([]string{"PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH"), "HOME=" + t.TempDir(), "XDG_CACHE_HOME=" + cache}, env...)
		var stdout, stderr strings.Builder
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		code := 0
		exitErr := &exec.ExitError{}
		if errors.As(err, &exitErr) {
			code = exitErr.ExitCode()
		} else if err != nil {
			t.Fatal(err)
		}
		return stdout.String(), stderr.String(), code
	}

	stdout, stderr, code := run([]string{"LP_FAKE_ARGS=1", "EXA_API_KEY=sekret"}, "run", "twitter/fxembed-status", "id=20", "lang=en", "--raw")
	if code != 0 {
		t.Fatalf("run exit %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, `"ok": true`) {
		t.Fatalf("run must print the JSON lightpanda returned, got %q", stdout)
	}
	program, err := os.ReadFile(captured)
	if err != nil {
		t.Fatal(err)
	}
	text := string(program)
	for _, want := range []string{
		`await page.goto("https://api.fxtwitter.com/"`,
		`\"id\": \"20\", \"lang\": \"en\"`,
		`url.origin === __origin`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("generated PandaScript missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "sekret") {
		t.Fatal("an environment secret leaked into a script that declares no header for it")
	}

	if _, stderr, code := run(nil, "run", "twitter/fxembed-status"); code != 2 || !strings.Contains(stderr, "missing required args: id") {
		t.Fatalf("missing arg: exit %d, stderr %q", code, stderr)
	}
	if _, stderr, code := run(nil, "run", "nope/nothing"); code != 2 || !strings.Contains(stderr, "unknown script") {
		t.Fatalf("unknown script: exit %d, stderr %q", code, stderr)
	}
	stdout, _, code = run(nil, "list")
	if code != 0 || !strings.Contains(stdout, "twitter/fxembed-status") || !strings.Contains(stdout, "api.fxtwitter.com") {
		t.Fatalf("list must show every bundled script with its domain, got %q", stdout)
	}

	// `add` installs into $XDG_CACHE_HOME/site-scripts, where a user script
	// shadows a bundled one of the same name and a new name joins the catalog.
	// A local file infers <parent dir>/<stem>; "My Site" is not a valid site
	// name, so the install below needs --name and the one after it fails.
	custom := filepath.Join(t.TempDir(), "My Site", "custom.js")
	if err := os.MkdirAll(filepath.Dir(custom), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(custom, []byte(`/* @meta {"description": "mine", "domain": "example.com", "readOnly": true} */
async function(args) { return {mine: true}; }
`), 0o644); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, code = run(nil, "add", custom, "--name", "twitter/fxembed-status")
	if code != 0 || !strings.Contains(stdout, filepath.Join(cache, "site-scripts", "twitter", "fxembed-status.js")) {
		t.Fatalf("add: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	stdout, _, code = run(nil, "info", "twitter/fxembed-status")
	if code != 0 || !strings.Contains(stdout, `"domain": "example.com"`) || !strings.Contains(stdout, `"name": "twitter/fxembed-status"`) {
		t.Fatalf("user script must shadow the bundled one, got %q", stdout)
	}
	if _, stderr, code := run(nil, "add", custom); code != 2 || !strings.Contains(stderr, "--name") {
		t.Fatalf("add without an inferable name: exit %d, stderr %q", code, stderr)
	}
	if _, stderr, code := run(nil, "add", "nope/nothing.js"); code != 2 || !strings.Contains(stderr, "not a catalog name, URL, or existing file") {
		t.Fatalf("add of a missing file: exit %d, stderr %q", code, stderr)
	}
}

// Without lightpanda the runner must explain where the binary comes from
// instead of failing on a runtime traceback.
func TestSiteScriptRunnerExplainsMissingLightpanda(t *testing.T) {
	cmd := exec.Command("bun", filepath.Join(webSkillDir(t), "scripts", "web.ts"), "site", "run", "exa/search", "query=x")
	cmd.Env = []string{"PATH=" + t.TempDir(), "HOME=" + t.TempDir()}
	out, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "lightpanda is not on PATH") || strings.Contains(string(out), "Traceback") {
		t.Fatalf("missing lightpanda must be explained, got %v: %s", err, out)
	}
}

func webSkillDir(t *testing.T) string {
	t.Helper()
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate web skill")
	}
	return filepath.Join(filepath.Dir(filename), "..", "plugins", "tools", "bun", "skills", "web")
}

func builtinSkillFile(t *testing.T, skill BuiltinSkillDescriptor, name string) ([]byte, BuiltinSkillFile, error) {
	t.Helper()
	r, err := Default()
	if err != nil {
		return nil, BuiltinSkillFile{}, err
	}
	return r.ReadBuiltinSkillFile(skill.Name, name)
}
