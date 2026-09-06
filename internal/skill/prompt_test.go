package skill

import (
	"context"
	"errors"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"

	pkgplugins "github.com/CherryHQ/stella/pkg/plugins"
	"github.com/CherryHQ/stella/resources"
)

func promptRevision(identity Skill, digest, content string) ManagedRevision {
	current := identity
	current.ContentDigest = digest
	return ManagedRevision{
		Skill: current,
		Files: map[string][]byte{MainFile: []byte(content)},
		Modes: map[string]fs.FileMode{MainFile: 0o644},
	}
}

func TestBuildAuthorizedPromptSectionUsesExactManagedAuthority(t *testing.T) {
	system := Skill{ID: "managed-system", Scope: "system", Name: "managed-system", Description: "Managed system skill", Status: SkillStatusActive}
	user := Skill{ID: "managed-user", Scope: "user", UserID: "user-1", Name: "managed-user", Description: "Managed user skill", Status: SkillStatusActive}
	reader := &projectionReader{
		identities: []Skill{system, user},
		revisions: map[string]ManagedRevision{
			system.ID: promptRevision(system, strings.Repeat("a", 64), "# system"),
			user.ID:   promptRevision(user, strings.Repeat("b", 64), "# user"),
		},
	}
	section, err := BuildAuthorizedPromptSection(context.Background(), pkgplugins.SystemPromptContext{UserID: "user-1"}, nil, reader, allowAllSkillReads{})
	if err != nil {
		t.Fatal(err)
	}
	if section.Title != "Skills" {
		t.Fatalf("prompt section title = %q, want Skills", section.Title)
	}
	for _, want := range []string{"skill_installed_search", "skill_load", "<name>managed-system</name>"} {
		if !strings.Contains(section.Content, want) {
			t.Fatalf("prompt section missing %q: %#v", want, section)
		}
	}
	if strings.Contains(section.Content, "managed-user") {
		t.Fatalf("prompt enumerated non-system managed skill: %s", section.Content)
	}
	if reader.loads != 2 {
		t.Fatalf("managed revision loads = %d, want 2 exact authority checks", reader.loads)
	}
}

func TestBuildAuthorizedPromptSectionFailsClosedWithoutAuthority(t *testing.T) {
	reader := &projectionReader{}
	for name, call := range map[string]func() error{
		"reader": func() error {
			_, err := BuildAuthorizedPromptSection(context.Background(), pkgplugins.SystemPromptContext{}, nil, nil, allowAllSkillReads{})
			return err
		},
		"authorizer": func() error {
			_, err := BuildAuthorizedPromptSection(context.Background(), pkgplugins.SystemPromptContext{}, nil, reader, nil)
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := call(); err == nil {
				t.Fatal("expected missing exact prompt dependency to fail")
			}
		})
	}
}

func TestBuildAuthorizedPromptSectionPropagatesManagedCorruption(t *testing.T) {
	identity := Skill{ID: "broken", Scope: "system", Name: "broken", Status: SkillStatusActive}
	reader := &projectionReader{identities: []Skill{identity}}
	if _, err := BuildAuthorizedPromptSection(context.Background(), pkgplugins.SystemPromptContext{}, nil, reader, allowAllSkillReads{}); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("prompt corruption error = %v, want fs.ErrNotExist", err)
	}
}

type unavailableManagedReader struct{ *projectionReader }

func (unavailableManagedReader) ListIdentityVisible(context.Context, ViewContext) ([]Skill, error) {
	return nil, errors.Join(ErrManagedSkillsUnavailable, ErrManagedSkillsPending)
}

func TestBuildAuthorizedPromptSectionKeepsProjectSkillsWhenManagedUnavailable(t *testing.T) {
	snapshot, err := SnapshotProjectSkills(t.Context(), snapshotRoot{fsys: fstest.MapFS{
		".agents/skills/deploy/SKILL.md": {Data: []byte("---\nname: deploy\ndescription: deploy app\n---\n")},
	}}, ".")
	if err != nil {
		t.Fatal(err)
	}
	reader := unavailableManagedReader{projectionReader: &projectionReader{}}
	section, err := BuildAuthorizedPromptSection(t.Context(), pkgplugins.SystemPromptContext{}, snapshot, reader, allowAllSkillReads{})
	if err != nil || section.Title != "Skills" {
		t.Fatalf("prompt with managed Skills unavailable = %#v, %v", section, err)
	}
}

func TestBuildAuthorizedPromptSectionFiltersRegistryPluginSkill(t *testing.T) {
	reader := &projectionReader{}
	disabled, err := BuildAuthorizedPromptSection(context.Background(), pkgplugins.SystemPromptContext{
		RegisteredPluginIDs: []string{"tool/lark-cli"},
	}, nil, reader, allowAllSkillReads{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(disabled.Content, "<name>lark-cli</name>") {
		t.Fatalf("disabled plugin-owned builtin leaked into prompt: %s", disabled.Content)
	}

	enabled, err := BuildAuthorizedPromptSection(context.Background(), pkgplugins.SystemPromptContext{
		RegisteredPluginIDs: []string{"tool/lark-cli"},
		EnabledPluginIDs:    []string{"tool/lark-cli"},
	}, nil, reader, allowAllSkillReads{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(enabled.Content, "<name>lark-cli</name>") {
		t.Fatalf("enabled plugin-owned builtin missing from prompt: %s", enabled.Content)
	}
}

func TestSkillVisibilityIgnoresMutableOwnerMetadata(t *testing.T) {
	visible := filterVisibleResolvedSkills([]ResolvedSkill{{
		Skill: Skill{
			Scope:    "system",
			Name:     "spoofed",
			Metadata: []byte(`{"owner_plugin":"tool/lark-cli"}`),
		},
	}}, pkgplugins.SystemPromptContext{})
	if len(visible) != 1 || visible[0].Name != "spoofed" {
		t.Fatalf("mutable owner metadata changed visibility: %#v", visible)
	}
}

func TestSkillVisibilityRequiresTrustedBuiltinOwner(t *testing.T) {
	web := ResolvedSkill{
		Skill: Skill{Scope: "system", Name: "web"},
		builtin: &resources.BuiltinSkillDescriptor{
			Name:          "web",
			OwnerPluginID: "tool/bun",
		},
	}
	withoutBun := filterVisibleResolvedSkills([]ResolvedSkill{web}, pkgplugins.SystemPromptContext{
		RegisteredPluginIDs: []string{"tool/bun"},
		EnabledPluginIDs:    []string{},
	})
	if len(withoutBun) != 0 {
		t.Fatalf("web skill visible with missing dependency: %#v", withoutBun)
	}
	withBun := filterVisibleResolvedSkills([]ResolvedSkill{web}, pkgplugins.SystemPromptContext{
		RegisteredPluginIDs: []string{"tool/bun"},
		EnabledPluginIDs:    []string{"tool/bun"},
	})
	if len(withBun) != 1 || withBun[0].Name != "web" {
		t.Fatalf("web skill hidden with dependency enabled: %#v", withBun)
	}
}
