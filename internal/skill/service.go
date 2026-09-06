package skill

import (
	"encoding/json"
	"fmt"
	"slices"

	"github.com/CherryHQ/stella/resources"
)

// Service merges exact mutable snapshots with immutable project and release
// snapshots. It never reads mutable Skill state itself.
type Service struct {
	registry    *resources.Registry
	registryErr error
}

func NewService() *Service {
	return newService(nil)
}

func newService(registry *resources.Registry) *Service {
	var err error
	if registry == nil {
		registry, err = resources.Default()
	}
	return &Service{registry: registry, registryErr: err}
}

// ResolvedSkill is a Skill resolved against immutable project or builtin bytes,
// or against a mutable store identity. Physical provider paths are never retained.
type ResolvedSkill struct {
	Skill
	project  *ProjectSnapshot
	builtin  *resources.BuiltinSkillDescriptor
	registry *resources.Registry
}

// OwnerPluginID returns the trusted owner for a builtin skill. Mutable skill
// metadata and project frontmatter never participate in ownership decisions.
func (s ResolvedSkill) OwnerPluginID() string {
	if s.builtin == nil {
		return ""
	}
	return s.builtin.OwnerPluginID
}

func (s *ResolvedSkill) LoadBuiltinFile(filePath string) (string, error) {
	if s.builtin == nil || s.registry == nil {
		return "", fmt.Errorf("not a builtin skill")
	}
	data, _, err := s.registry.ReadBuiltinSkillFile(s.builtin.Name, filePath)
	return string(data), err
}

func (s *ResolvedSkill) BuiltinFiles() []string {
	if s.builtin == nil {
		return nil
	}
	out := make([]string, 0, len(s.builtin.Files))
	for _, file := range s.builtin.Files {
		out = append(out, file.Path)
	}
	return out
}

// ImmutableFiles lists files from an immutable builtin or project snapshot.
// A nil result means the Skill is backed by the trusted runtime cache instead.
func (s *ResolvedSkill) ImmutableFiles() []string {
	if s.project != nil {
		files, err := s.project.listFiles(s.Name)
		if err != nil {
			return []string{}
		}
		return files
	}
	return s.BuiltinFiles()
}

func (s *ResolvedSkill) LoadImmutableFile(filePath string) (string, error) {
	if s.project != nil {
		data, err := s.project.load(s.Name, filePath)
		return data, err
	}
	return s.LoadBuiltinFile(filePath)
}

// IsImmutable reports whether the Skill comes from a captured project tree or
// the embedded release registry rather than a mutable store identity.
func (s ResolvedSkill) IsImmutable() bool { return s.project != nil || s.builtin != nil }

func (s *ResolvedSkill) immutableProjection() (immutableSkillProjection, error) {
	if s.project != nil {
		return s.project.immutableProjection(s.Name)
	}
	if s.builtin == nil || s.registry == nil {
		return immutableSkillProjection{}, fmt.Errorf("skill %q is not immutable", s.Name)
	}
	files := make([]immutableSkillFile, 0, len(s.builtin.Files))
	for _, descriptor := range s.builtin.Files {
		content, actual, err := s.registry.ReadBuiltinSkillFile(s.builtin.Name, descriptor.Path)
		if err != nil {
			return immutableSkillProjection{}, err
		}
		if actual != descriptor {
			return immutableSkillProjection{}, fmt.Errorf("builtin skill %q descriptor changed while projecting", s.Name)
		}
		mode, err := immutableProjectionMode(descriptor.Mode)
		if err != nil {
			return immutableSkillProjection{}, err
		}
		files = append(files, immutableSkillFile{path: descriptor.Path, content: content, mode: mode})
	}
	return immutableSkillProjection{kind: "builtin", id: s.Name, digest: s.builtin.Digest, files: files}, nil
}

func (s *Service) ListMerged(managedSkills []Skill, snapshot *ProjectSnapshot) []ResolvedSkill {
	projSkills := snapshot.list()
	builtinSkills, err := s.builtinSkills()
	if err != nil {
		return nil
	}

	seen := make(map[string]bool, len(projSkills)+len(managedSkills)+len(builtinSkills))
	out := make([]ResolvedSkill, 0, len(projSkills)+len(managedSkills)+len(builtinSkills))

	for _, sk := range projSkills {
		seen[sk.Name] = true
		out = append(out, ResolvedSkill{Skill: sk, project: snapshot})
	}
	for _, sk := range managedSkills {
		if seen[sk.Name] {
			continue
		}
		seen[sk.Name] = true
		out = append(out, ResolvedSkill{Skill: sk})
	}
	for _, sk := range builtinSkills {
		if seen[sk.Name] {
			continue
		}
		seen[sk.Name] = true
		out = append(out, sk)
	}
	return out
}

// filterDisabled runs only after precedence resolution. A disabled winner is
// absent; callers must never retry a lower-precedence implementation by name.
func filterDisabled(in []ResolvedSkill, disabled []string) []ResolvedSkill {
	if len(disabled) == 0 {
		return in
	}
	out := make([]ResolvedSkill, 0, len(in))
	for _, rs := range in {
		if !isDisabled(rs, disabled) {
			out = append(out, rs)
		}
	}
	return out
}

func isDisabled(rs ResolvedSkill, disabled []string) bool {
	ref, ok := PolicyRef(rs)
	return ok && slices.Contains(disabled, ref)
}

// PolicyRef returns the stable policy identity for policy-addressable Skills.
func PolicyRef(rs ResolvedSkill) (string, bool) {
	switch rs.Scope {
	case "system", "system_agent":
		return rs.Scope + ":" + rs.Name, true
	default:
		return "", false
	}
}

func (s *Service) builtinSkills() ([]ResolvedSkill, error) {
	if s.registryErr != nil {
		return nil, fmt.Errorf("load builtin registry: %w", s.registryErr)
	}
	if s.registry == nil {
		return nil, fmt.Errorf("builtin registry is unavailable")
	}
	descriptors := s.registry.BuiltinSkills()
	out := make([]ResolvedSkill, 0, len(descriptors))
	for i := range descriptors {
		rs, err := s.resolvedBuiltin(&descriptors[i])
		if err != nil {
			return nil, err
		}
		out = append(out, rs)
	}
	return out, nil
}

func (s *Service) builtinNameForReference(reference string) (string, bool) {
	if s.registry == nil {
		return "", false
	}
	for _, descriptor := range s.registry.BuiltinSkills() {
		if reference == descriptor.APIID || reference == descriptor.Ref {
			return descriptor.Name, true
		}
	}
	return "", false
}

func (s *Service) resolvedBuiltin(descriptor *resources.BuiltinSkillDescriptor) (ResolvedSkill, error) {
	metadata, err := json.Marshal(descriptor.Metadata)
	if err != nil {
		return ResolvedSkill{}, fmt.Errorf("encode builtin skill %q metadata: %w", descriptor.Name, err)
	}
	return ResolvedSkill{
		Skill: Skill{
			ID: descriptor.APIID, Scope: "system", Name: descriptor.Name,
			Description: descriptor.Description, Status: SkillStatusActive,
			DisableModelInvocation: descriptor.DisableModelInvocation, Metadata: metadata,
		},
		builtin: descriptor, registry: s.registry,
	}, nil
}
