package skills

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
)

var safeNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*[a-z0-9]$|^[a-z0-9]$`)

func (t *SkillsTool) remove(args map[string]any) (string, error) {
	name, _ := args["name"].(string)
	if name == "" {
		return "", fmt.Errorf("name is required for remove action")
	}

	if !safeNameRe.MatchString(name) {
		return "", fmt.Errorf("invalid skill name %q: must be lowercase alphanumeric with hyphens", name)
	}

	skillDir := filepath.Join(t.cwd, ".agents", "skills", name)
	if _, err := os.Stat(skillDir); os.IsNotExist(err) {
		return "", fmt.Errorf("skill %q not found at %s", name, skillDir)
	}

	if err := os.RemoveAll(skillDir); err != nil {
		return "", fmt.Errorf("remove skill %q: %w", name, err)
	}

	return fmt.Sprintf("Skill %q removed from %s.", name, skillDir), nil
}
