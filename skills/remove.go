package skills

import (
	"fmt"
	"os"
	"regexp"
)

// safeNameRe matches valid skill names: alphanumeric, hyphens, dots, underscores.
// Aligned with safeSegmentRe in install.go to ensure install/remove consistency.
var safeNameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$|^[a-zA-Z0-9]$`)

// Remove deletes an installed skill directory after validating the name.
func Remove(name, skillDir string) error {
	if name == "" {
		return fmt.Errorf("skill name is required")
	}

	if !safeNameRe.MatchString(name) {
		return fmt.Errorf("invalid skill name %q: must be lowercase alphanumeric with hyphens", name)
	}

	if _, err := os.Stat(skillDir); os.IsNotExist(err) {
		return fmt.Errorf("skill %q not found at %s", name, skillDir)
	}

	if err := os.RemoveAll(skillDir); err != nil {
		return fmt.Errorf("remove skill %q: %w", name, err)
	}

	return nil
}

func (t *SkillsTool) remove(args map[string]any) (string, error) {
	name, _ := args["name"].(string)
	if name == "" {
		return "", fmt.Errorf("name is required for remove action")
	}

	if !safeNameRe.MatchString(name) {
		return "", fmt.Errorf("invalid skill name %q: must be lowercase alphanumeric with hyphens", name)
	}

	skillDir := fmt.Sprintf("%s/skills/%s", t.workspace, name)
	if err := Remove(name, skillDir); err != nil {
		return "", fmt.Errorf("%w (only skills in %s/skills/ can be removed; user-level skills must be removed manually)", err, t.workspace)
	}

	return fmt.Sprintf("Skill %q removed from %s.", name, skillDir), nil
}
