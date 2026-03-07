package skills

import (
	"encoding/json"

	"github.com/vaayne/anna/agent/runner"
)

type installedSkill struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Source      string `json:"source"`
	Path        string `json:"path"`
	Removable   bool   `json:"removable"`
}

func (t *SkillsTool) list() (string, error) {
	all := runner.LoadSkills(t.workspace, t.cwd)
	if len(all) == 0 {
		return "No skills installed.", nil
	}

	results := make([]installedSkill, len(all))
	for i, s := range all {
		results[i] = installedSkill{
			Name:        s.Name,
			Description: s.Description,
			Source:      s.Source,
			Path:        s.FilePath,
			Removable:   s.Source == "project",
		}
	}

	out, _ := json.MarshalIndent(results, "", "  ")
	return string(out), nil
}
