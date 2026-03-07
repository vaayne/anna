package skills

import (
	"encoding/json"

	gorunner "github.com/vaayne/anna/agent/runner/go"
)

type installedSkill struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Source      string `json:"source"`
	Path        string `json:"path"`
}

func (t *SkillsTool) list() (string, error) {
	skills := gorunner.LoadSkills(t.agentsDir, t.cwd)
	if len(skills) == 0 {
		return "No skills installed.", nil
	}

	results := make([]installedSkill, len(skills))
	for i, s := range skills {
		results[i] = installedSkill{
			Name:        s.Name,
			Description: s.Description,
			Source:      s.Source,
			Path:        s.FilePath,
		}
	}

	out, _ := json.MarshalIndent(results, "", "  ")
	return string(out), nil
}
