package resources

import (
	"path"
	"strings"
	"testing"
)

func readBuiltinSkillPath(t *testing.T, logicalPath string) string {
	t.Helper()
	r, err := Default()
	if err != nil {
		t.Fatal(err)
	}
	logicalPath = strings.TrimPrefix(logicalPath, "skills/")
	for _, skill := range r.BuiltinSkills() {
		prefix := skill.Root + "/"
		if !strings.HasPrefix(logicalPath, prefix) {
			continue
		}
		data, _, err := r.ReadBuiltinSkillFile(skill.Name, strings.TrimPrefix(logicalPath, prefix))
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	t.Fatalf("builtin skill path %q not found", logicalPath)
	return ""
}

func builtinSkillDescriptor(t *testing.T, name string) BuiltinSkillDescriptor {
	t.Helper()
	r, err := Default()
	if err != nil {
		t.Fatal(err)
	}
	skill, ok := r.BuiltinSkill(name)
	if !ok {
		t.Fatalf("builtin skill %q not found", name)
	}
	return skill
}

func builtinSkillFilePath(skill BuiltinSkillDescriptor, file string) string {
	return path.Join(skill.Root, file)
}
