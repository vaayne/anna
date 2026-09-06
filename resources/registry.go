package resources

import (
	"errors"
	"fmt"
	"io/fs"
	"path"
	"reflect"
	"sort"
	"strings"
	"sync"

	builtinplugins "github.com/CherryHQ/stella/plugins"
)

// Registry is the read-only catalog of builtin resources, keyed by Kind and ID.
type Registry struct {
	byKind      map[Kind]map[string]Resource
	manifest    BuiltinManifest
	skills      map[string]BuiltinSkillDescriptor
	source      fs.FS
	skillReader BuiltinSkillReader
}

var (
	defaultOnce sync.Once
	defaultReg  *Registry
	defaultErr  error
)

// Default returns the process-wide registry, loaded lazily from the embedded FS.
func Default() (*Registry, error) {
	defaultOnce.Do(func() {
		manifest, err := GeneratedBuiltinManifest()
		if err != nil {
			defaultErr = err
			return
		}
		defaultReg, defaultErr = LoadBuiltinWithSkillReader(fsys, manifest, func(skill BuiltinSkillDescriptor, file string) ([]byte, error) {
			return builtinplugins.ReadBuiltinSkillFile(skill.SourceRoot, file)
		})
	})
	return defaultReg, defaultErr
}

// Load walks sourceFS and parses every supported resource kind it finds.
// sourceFS must have subdirectories matching Kind.subdir() for each kind to load.
// Missing subdirectories are silently skipped (useful for tests with partial fixtures).
func Load(sourceFS fs.FS) (*Registry, error) {
	r := &Registry{byKind: make(map[Kind]map[string]Resource, len(AllKinds())), source: sourceFS}
	for _, kind := range AllKinds() {
		r.byKind[kind] = map[string]Resource{}
		sub := kind.subdir()
		if sub == "" {
			continue
		}
		subFS, err := fs.Sub(sourceFS, sub)
		if err != nil {
			continue
		}
		if err := loadKind(r, kind, subFS); err != nil {
			return nil, fmt.Errorf("load %s: %w", kind, err)
		}
	}
	return r, nil
}

// LoadBuiltin loads a registry from an embedded-style filesystem and validates
// every manifest-described byte before making it available to callers.
func LoadBuiltin(sourceFS fs.FS, manifest BuiltinManifest) (*Registry, error) {
	return LoadBuiltinWithSkillReader(sourceFS, manifest, nil)
}

// BuiltinSkillReader reads a manifest file from the release-owned asset
// package. It separates physical source layout from the stable bundle Root.
type BuiltinSkillReader func(skill BuiltinSkillDescriptor, file string) ([]byte, error)

// LoadBuiltinWithSkillReader loads a manifest and obtains skills through the
// explicit asset reader when the resource package no longer embeds skill files.
func LoadBuiltinWithSkillReader(sourceFS fs.FS, manifest BuiltinManifest, reader BuiltinSkillReader) (*Registry, error) {
	if err := validateBuiltinManifest(manifest); err != nil {
		return nil, err
	}
	r, err := Load(sourceFS)
	if err != nil {
		return nil, err
	}
	r.manifest = BuiltinManifest{Revision: manifest.Revision, Skills: make([]BuiltinSkillDescriptor, 0, len(manifest.Skills))}
	r.skills = make(map[string]BuiltinSkillDescriptor, len(manifest.Skills))
	r.skillReader = reader
	for _, skill := range manifest.Skills {
		if _, exists := r.skills[skill.Name]; exists {
			return nil, fmt.Errorf("duplicate builtin descriptor %q", skill.Name)
		}
		for _, file := range skill.Files {
			data, err := r.readBuiltinSkillFile(skill, file.Path)
			if err != nil {
				return nil, fmt.Errorf("read builtin skill %q file %q: %w", skill.Name, file.Path, err)
			}
			if int64(len(data)) != file.Size || sha256Hex(data) != file.Digest {
				return nil, fmt.Errorf("builtin skill %q file %q does not match manifest", skill.Name, file.Path)
			}
		}
		resource, ok := r.Get(KindSkill, skill.Name)
		if reader != nil {
			raw, readErr := r.readBuiltinSkillFile(skill, "SKILL.md")
			if readErr != nil {
				return nil, fmt.Errorf("read builtin skill %q metadata: %w", skill.Name, readErr)
			}
			parsed, parseErr := parseResource(KindSkill, skill.Name, string(raw))
			if parseErr != nil {
				return nil, fmt.Errorf("parse builtin skill %q: %w", skill.Name, parseErr)
			}
			r.byKind[KindSkill][parsed.ID] = parsed
			resource, ok = parsed, true
		}
		if !ok || resource.Name != skill.Name || resource.Description != skill.Description || !reflect.DeepEqual(resource.Tags, skill.Tags) || boolMetadata(resource.Metadata, "disable_model_invocation") != skill.DisableModelInvocation || !reflect.DeepEqual(resource.Metadata, skill.Metadata) {
			return nil, fmt.Errorf("builtin skill %q metadata does not match manifest", skill.Name)
		}
		cloned := cloneBuiltinSkillDescriptor(skill)
		r.skills[skill.Name] = cloned
		r.manifest.Skills = append(r.manifest.Skills, cloned)
	}
	if len(r.byKind[KindSkill]) != len(r.skills) {
		return nil, fmt.Errorf("builtin manifest lists %d skills but embedded resources contain %d", len(r.skills), len(r.byKind[KindSkill]))
	}
	return r, nil
}

// ValidateBuiltinSkillOwners checks the immutable owner projection against the
// runtime plugin catalog. The map is supplied by the composition root, so this
// package does not maintain a second ownership registry.
func (r *Registry) ValidateBuiltinSkillOwners(knownOwners map[string]struct{}) error {
	if r == nil {
		return fmt.Errorf("builtin registry is nil")
	}
	for _, skill := range r.BuiltinSkills() {
		ownerValidator := validateSkillOwner
		if skill.SourceRoot != "" {
			ownerValidator = validateExplicitSkillOwner
		}
		if err := ownerValidator(skill.Root, skill.OwnerPluginID); err != nil {
			return err
		}
		if skill.OwnerPluginID == "" {
			continue
		}
		if _, ok := knownOwners[skill.OwnerPluginID]; !ok {
			return fmt.Errorf("builtin skill %q has unknown plugin owner %q", skill.Name, skill.OwnerPluginID)
		}
	}
	return nil
}

// loadKind discovers resources of a single kind under subFS.
// Skills are multi-file directories (id = dir name, main = SKILL.md).
// Souls/delegates/templates are single files (id = basename without .md).
func loadKind(r *Registry, kind Kind, subFS fs.FS) error {
	entries, err := fs.ReadDir(subFS, ".")
	if err != nil {
		// Missing kind dir is fine — treat as empty (useful for test fixtures).
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("read %s: %w", kind.subdir(), err)
	}

	if kind == KindSkill {
		roots, err := listSkillRoots(subFS)
		if err != nil {
			return fmt.Errorf("discover skills: %w", err)
		}
		for _, root := range roots {
			raw, err := fs.ReadFile(subFS, path.Join(root.Path, "SKILL.md"))
			if err != nil {
				return fmt.Errorf("read %s/SKILL.md: %w", root.Path, err)
			}
			res, err := parseResource(kind, root.Leaf, string(raw))
			if err != nil {
				return fmt.Errorf("skill %s: %w", root.Path, err)
			}
			r.byKind[kind][res.ID] = res
		}
		return nil
	}

	for _, entry := range entries {

		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasSuffix(name, ".md") {
			continue
		}
		id := strings.TrimSuffix(name, ".md")
		raw, err := fs.ReadFile(subFS, name)
		if err != nil {
			return fmt.Errorf("read %s/%s: %w", kind.subdir(), name, err)
		}
		res, err := parseResource(kind, id, string(raw))
		if err != nil {
			return fmt.Errorf("%s %s: %w", kind, id, err)
		}
		r.byKind[kind][res.ID] = res
	}
	return nil
}

// List returns all resources of the given kind, sorted by ID for determinism.
func (r *Registry) List(kind Kind) []Resource {
	m := r.byKind[kind]
	out := make([]Resource, 0, len(m))
	for _, res := range m {
		out = append(out, res)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Get fetches a single resource by kind and ID.
func (r *Registry) Get(kind Kind, id string) (Resource, bool) {
	res, ok := r.byKind[kind][id]
	return res, ok
}

// Kinds returns the set of kinds that have at least one loaded resource.
func (r *Registry) Kinds() []Kind {
	out := make([]Kind, 0, len(r.byKind))
	for _, k := range AllKinds() {
		if len(r.byKind[k]) > 0 {
			out = append(out, k)
		}
	}
	return out
}

// BundleRevision identifies the exact builtin bundle loaded by this registry.
func (r *Registry) BundleRevision() string { return r.manifest.Revision }

// BuiltinSkills returns all release-owned skill descriptors in stable name order.
func (r *Registry) BuiltinSkills() []BuiltinSkillDescriptor {
	out := make([]BuiltinSkillDescriptor, 0, len(r.skills))
	for _, skill := range r.skills {
		out = append(out, cloneBuiltinSkillDescriptor(skill))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// BuiltinSkill returns one release-owned descriptor by its stable skill name.
func (r *Registry) BuiltinSkill(name string) (BuiltinSkillDescriptor, bool) {
	skill, ok := r.skills[name]
	return cloneBuiltinSkillDescriptor(skill), ok
}

func (r *Registry) readBuiltinSkillFile(skill BuiltinSkillDescriptor, filePath string) ([]byte, error) {
	if r.skillReader != nil {
		return r.skillReader(skill, filePath)
	}
	return fs.ReadFile(r.source, path.Join("skills", skill.Root, filePath))
}

// ReadBuiltinSkillFile returns an embedded skill file selected by canonical
// root-relative path, together with its manifest descriptor.
func (r *Registry) ReadBuiltinSkillFile(name, filePath string) ([]byte, BuiltinSkillFile, error) {
	skill, ok := r.skills[name]
	if !ok {
		return nil, BuiltinSkillFile{}, fmt.Errorf("builtin skill %q not found", name)
	}
	if !canonicalBuiltinPath(filePath) {
		return nil, BuiltinSkillFile{}, fmt.Errorf("invalid builtin skill file path %q", filePath)
	}
	for _, file := range skill.Files {
		if file.Path != filePath {
			continue
		}
		data, err := r.readBuiltinSkillFile(skill, file.Path)
		if err != nil {
			return nil, BuiltinSkillFile{}, fmt.Errorf("read builtin skill %q file %q: %w", name, filePath, err)
		}
		if int64(len(data)) != file.Size || sha256Hex(data) != file.Digest {
			return nil, BuiltinSkillFile{}, fmt.Errorf("builtin skill %q file %q does not match manifest", name, filePath)
		}
		return data, file, nil
	}
	return nil, BuiltinSkillFile{}, fmt.Errorf("builtin skill %q file %q not found", name, filePath)
}

func cloneBuiltinSkillDescriptor(skill BuiltinSkillDescriptor) BuiltinSkillDescriptor {
	cloned := skill
	cloned.SourceRoot = skill.SourceRoot
	cloned.Files = append([]BuiltinSkillFile(nil), skill.Files...)
	cloned.Tags = append([]string(nil), skill.Tags...)
	cloned.Metadata = cloneBuiltinMetadata(skill.Metadata)
	return cloned
}

func cloneBuiltinMetadata(metadata map[string]any) map[string]any {
	if metadata == nil {
		return nil
	}
	cloned := make(map[string]any, len(metadata))
	for key, value := range metadata {
		cloned[key] = cloneBuiltinMetadataValue(value)
	}
	return cloned
}

func cloneBuiltinMetadataValue(value any) any {
	switch value := value.(type) {
	case map[string]any:
		return cloneBuiltinMetadata(value)
	case []any:
		cloned := make([]any, len(value))
		for i, item := range value {
			cloned[i] = cloneBuiltinMetadataValue(item)
		}
		return cloned
	case []string:
		return append([]string(nil), value...)
	default:
		return value
	}
}
