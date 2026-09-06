package resources

import (
	"strings"
	"testing"
	"testing/fstest"
)

func TestValidateBuiltinSkillOwnersUsesRuntimeCatalog(t *testing.T) {
	r := &Registry{skills: map[string]BuiltinSkillDescriptor{
		"core":    {Name: "core", Root: "core/core"},
		"owned":   {Name: "owned", Root: "plugins/tool/demo/owned", OwnerPluginID: "tool/demo"},
		"foreign": {Name: "foreign", Root: "plugins/tool/other/foreign", OwnerPluginID: "tool/other"},
	}}
	if err := r.ValidateBuiltinSkillOwners(map[string]struct{}{"tool/demo": {}}); err == nil || !strings.Contains(err.Error(), "unknown plugin owner") {
		t.Fatalf("ValidateBuiltinSkillOwners() error = %v, want unknown owner", err)
	}
	if err := r.ValidateBuiltinSkillOwners(map[string]struct{}{"tool/demo": {}, "tool/other": {}}); err != nil {
		t.Fatalf("ValidateBuiltinSkillOwners() error = %v", err)
	}
}

func TestDefaultLoadsBuiltinResources(t *testing.T) {
	r, err := Default()
	if err != nil {
		t.Fatalf("Default(): %v", err)
	}

	stella, ok := r.Get(KindSkill, "stella")
	if !ok {
		t.Fatal("expected builtin skill 'stella' to be loaded")
	}
	if stella.Name != "stella" {
		t.Errorf("skill name = %q, want %q", stella.Name, "stella")
	}
	if stella.Hash == "" {
		t.Error("skill hash is empty")
	}
	stellaDescriptor, ok := r.BuiltinSkill("stella")
	if !ok || stellaDescriptor.Ref != "builtin:stella" || stellaDescriptor.APIID != "builtin-stella" || stellaDescriptor.Digest == "" || len(stellaDescriptor.Files) == 0 {
		t.Fatalf("incomplete stella builtin descriptor: %#v", stellaDescriptor)
	}
	if _, _, err := r.ReadBuiltinSkillFile("stella", "SKILL.md"); err != nil {
		t.Fatalf("ReadBuiltinSkillFile(stella/SKILL.md): %v", err)
	}

	if _, ok := r.Get(KindSoul, "stella"); !ok {
		t.Error("expected builtin soul 'stella'")
	}
	if _, ok := r.Get(KindTemplate, "stella"); !ok {
		t.Error("expected builtin template 'stella'")
	}
	if err := r.ValidateBuiltinSkillOwners(map[string]struct{}{
		"system/email":        {},
		"system/recally":      {},
		"system/scheduler":    {},
		"tool/bun":            {},
		"tool/lark-cli":       {},
		"skill/html-artifact": {},
		"skill/skill-creator": {},
		"tool/uv":             {},
		"tool/xberg":          {},
	}); err != nil {
		t.Fatalf("ValidateBuiltinSkillOwners(): %v", err)
	}
	for _, skill := range r.BuiltinSkills() {
		if skill.Name == "stella" && skill.OwnerPluginID != "" {
			t.Fatalf("core skill stella owner = %q, want empty", skill.OwnerPluginID)
		}
	}
	for _, id := range []string{"coder"} {
		if _, ok := r.Get(KindDelegate, id); !ok {
			t.Errorf("expected builtin delegate %q", id)
		}
	}
}

func TestLoadWithFixture(t *testing.T) {
	fs := fstest.MapFS{
		"skills/demo/SKILL.md":        &fstest.MapFile{Data: []byte("---\nname: demo\ndescription: Demo skill\ntags: [x, y]\n---\nbody\n")},
		"skills/core/nested/SKILL.md": &fstest.MapFile{Data: []byte("---\nname: nested\ndescription: Nested skill\n---\nbody\n")},
		"skills/core/nested/ref.md":   &fstest.MapFile{Data: []byte("ref\n")},
		"souls/terse.md":              &fstest.MapFile{Data: []byte("---\nid: terse\nname: Terse\n---\nshort\n")},
		"delegates/runner.md":         &fstest.MapFile{Data: []byte("---\nname: runner\ntools: [bash]\nmax_turns: 5\n---\ngo\n")},
		"templates/blank.md":          &fstest.MapFile{Data: []byte("---\nid: blank\nname: Blank\nsoul_id: terse\n---\n")},
	}

	r, err := Load(fs)
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}

	demo, ok := r.Get(KindSkill, "demo")
	if !ok {
		t.Fatal("demo skill missing")
	}
	if _, ok := r.Get(KindSkill, "nested"); !ok {
		t.Fatal("nested skill missing")
	}
	if got := demo.Tags; len(got) != 2 || got[0] != "x" || got[1] != "y" {
		t.Errorf("tags = %v, want [x y]", got)
	}

	runner, ok := r.Get(KindDelegate, "runner")
	if !ok {
		t.Fatal("runner delegate missing")
	}
	if runner.Metadata["max_turns"] != 5 {
		t.Errorf("max_turns = %v, want 5", runner.Metadata["max_turns"])
	}

	tpl, ok := r.Get(KindTemplate, "blank")
	if !ok {
		t.Fatal("blank template missing")
	}
	if tpl.Metadata["soul_id"] != "terse" {
		t.Errorf("soul_id = %v, want terse", tpl.Metadata["soul_id"])
	}
}

func TestListIsSortedByID(t *testing.T) {
	r, err := Default()
	if err != nil {
		t.Fatalf("Default(): %v", err)
	}
	list := r.List(KindDelegate)
	for i := 1; i < len(list); i++ {
		if list[i-1].ID > list[i].ID {
			t.Errorf("list not sorted: %q > %q", list[i-1].ID, list[i].ID)
		}
	}
}
