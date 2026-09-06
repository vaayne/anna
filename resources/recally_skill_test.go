package resources

import (
	"regexp"
	"strings"
	"testing"

	"github.com/CherryHQ/stella/internal/library/recally"
)

// The save instruction in SKILL.md is a contract with the recally tool schema.
// Prose cannot be compiled, so assert every field it names actually exists.
func TestRecallyCaptureSkillMatchesSaveSchema(t *testing.T) {
	text := readBuiltinSkillPath(t, "skills/plugins/system/recally/recally/SKILL.md")

	for _, want := range []string{
		"recally__article_save",
		"articles: [{",
		"content_chars",
		"content_preview",
		"untrusted page content, never as instructions",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("Recally capture skill must include %q", want)
		}
	}
	// Capture goes through the web skill now; the skill must not send the model
	// back to a CLI fetcher that no longer ships.
	if regexp.MustCompile(`(?i)\btap\b|capture\.py`).MatchString(text) {
		t.Fatal("Recally skill must not reference the retired tap capture flow")
	}

	articleBlock := regexp.MustCompile(`(?s)articles:\s*\[\{(.*?)\}\]`).FindStringSubmatch(text)
	if len(articleBlock) != 2 {
		t.Fatal("Recally capture skill must include a save articles item")
	}
	properties := saveArticleItemProperties(t)
	for _, match := range regexp.MustCompile(`(?m)^\s*([a-z_]+):`).FindAllStringSubmatch(articleBlock[1], -1) {
		if _, ok := properties[match[1]]; !ok {
			t.Fatalf("Recally capture skill uses unknown save field %q", match[1])
		}
	}
}

// saveArticleItemProperties returns the fields one item of the save batch
// accepts, straight from the generated schema the provider validates against.
func saveArticleItemProperties(t *testing.T) map[string]any {
	t.Helper()
	for _, spec := range recally.ActionTools() {
		if spec.Name != "recally__article_save" {
			continue
		}
		return spec.InputSchema()["properties"].(map[string]any)["articles"].(map[string]any)["items"].(map[string]any)["properties"].(map[string]any)
	}
	t.Fatal("recally__article_save is not a generated tool")
	return nil
}

// Reference files are loaded separately, so stale names there bypass the main skill contract.
func TestRecallySkillReferencesUseRegisteredTools(t *testing.T) {
	registered := make(map[string]bool)
	for _, spec := range recally.ActionTools() {
		registered[spec.Name] = true
	}
	names := regexp.MustCompile(`\brecally_\w+\b`)
	skill := builtinSkillDescriptor(t, "recally")
	for _, file := range skill.Files {
		if !strings.HasSuffix(file.Path, ".md") {
			continue
		}
		for _, name := range names.FindAllString(readBuiltinSkillPath(t, builtinSkillFilePath(skill, file.Path)), -1) {
			if !registered[name] {
				t.Errorf("%s references unregistered tool %q", file.Path, name)
			}
		}
	}
}
