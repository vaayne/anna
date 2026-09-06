package manifest

import (
	"bytes"
	"cmp"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

var builtinPluginIdentifier = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// BuiltinOAuthProviderIDs returns the provider identities from the central
// oauth.yaml document so plugin generation can validate cross-file references.
func BuiltinOAuthProviderIDs(data []byte) ([]string, error) {
	raw, err := parseRawYAML(data)
	if err != nil {
		return nil, fmt.Errorf("decode builtin OAuth providers: %w", err)
	}
	ids := make([]string, 0, len(raw.OAuthProviders))
	seen := make(map[string]struct{}, len(raw.OAuthProviders))
	for _, provider := range raw.OAuthProviders {
		if provider.ID == "" {
			return nil, fmt.Errorf("builtin OAuth provider has empty ID")
		}
		if _, exists := seen[provider.ID]; exists {
			return nil, fmt.Errorf("duplicate builtin OAuth provider ID %q", provider.ID)
		}
		seen[provider.ID] = struct{}{}
		ids = append(ids, provider.ID)
	}
	return ids, nil
}

// GenerateBuiltinPlugins recursively loads plugin.yaml files beneath sourceRoot.
// Directory names are packaging only; declared IDs and namespaces are the identity.
func GenerateBuiltinPlugins(sourceRoot string, reservedRuntimeNames, oauthProviderIDs []string) (*Manifest, error) {
	info, err := os.Lstat(sourceRoot)
	if err != nil {
		return nil, fmt.Errorf("stat builtin plugins root: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return nil, fmt.Errorf("builtin plugins root must be a directory: %s", sourceRoot)
	}

	var rawPlugins []rawManifestPlugin
	err = filepath.WalkDir(sourceRoot, func(filename string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(sourceRoot, filename)
		if err != nil {
			return fmt.Errorf("relative plugin path %q: %w", filename, err)
		}
		if rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("builtin plugin path %q is a symlink", rel)
		}
		if entry.IsDir() {
			if path.Base(rel) == "plugin.yaml" {
				return fmt.Errorf("builtin plugin path %q has unsupported type directory", rel)
			}
			return nil
		}
		fileInfo, err := entry.Info()
		if err != nil {
			return fmt.Errorf("stat builtin plugin path %q: %w", rel, err)
		}
		if !fileInfo.Mode().IsRegular() {
			return fmt.Errorf("builtin plugin path %q has unsupported type %s", rel, fileInfo.Mode().Type())
		}
		if path.Base(rel) != "plugin.yaml" {
			return nil
		}
		if strings.HasPrefix(rel, "core/") || rel == "core/plugin.yaml" {
			return fmt.Errorf("builtin plugin %q is forbidden beneath plugins/core", rel)
		}
		plugin, err := loadBuiltinPlugin(filename, rel)
		if err != nil {
			return err
		}
		rawPlugins = append(rawPlugins, plugin)
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(rawPlugins) == 0 {
		return nil, fmt.Errorf("builtin plugins root contains no plugin.yaml: %s", sourceRoot)
	}

	manifest := rawToManifest(rawManifest{Plugins: rawPlugins})
	if err := validateBuiltinPlugins(manifest.Plugins, reservedRuntimeNames, oauthProviderIDs); err != nil {
		return nil, err
	}
	slices.SortFunc(manifest.Plugins, func(a, b ManifestPlugin) int { return cmp.Compare(a.ID, b.ID) })
	return manifest, nil
}

func loadBuiltinPlugin(filename, relative string) (rawManifestPlugin, error) {
	data, err := os.ReadFile(filename)
	if err != nil {
		return rawManifestPlugin{}, fmt.Errorf("read builtin plugin %q: %w", relative, err)
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	var plugin rawManifestPlugin
	if err := decoder.Decode(&plugin); err != nil {
		return rawManifestPlugin{}, fmt.Errorf("decode builtin plugin %q: %w", relative, err)
	}
	if yamlDocumentHasField(data, "essential") || yamlDocumentHasField(data, "bundled_binaries") {
		return rawManifestPlugin{}, fmt.Errorf("builtin plugin %q cannot declare essential or bundled_binaries", relative)
	}
	var extra yaml.Node
	if err := decoder.Decode(&extra); err == nil {
		return rawManifestPlugin{}, fmt.Errorf("builtin plugin %q contains more than one YAML document", relative)
	} else if !errors.Is(err, io.EOF) {
		return rawManifestPlugin{}, fmt.Errorf("decode trailing builtin plugin document %q: %w", relative, err)
	}
	return plugin, nil
}

func yamlDocumentHasField(data []byte, field string) bool {
	var document yaml.Node
	if err := yaml.Unmarshal(data, &document); err != nil || len(document.Content) != 1 {
		return false
	}
	node := document.Content[0]
	if node.Kind != yaml.MappingNode {
		return false
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == field {
			return true
		}
	}
	return false
}

func validateBuiltinPlugins(plugins []ManifestPlugin, reservedRuntimeNames, oauthProviderIDs []string) error {
	providerIDs := make(map[string]struct{}, len(oauthProviderIDs))
	for _, id := range oauthProviderIDs {
		providerIDs[id] = struct{}{}
	}
	if errs := validatePlugins(plugins, providerIDs); len(errs) > 0 {
		return errors.Join(errs...)
	}
	seenIDs := make(map[string]struct{}, len(plugins))
	seenNamespaces := make(map[string]string, len(plugins))
	seenResources := make(map[string]string)
	reservedIDs := make(map[string]struct{}, len(reservedRuntimeNames))
	reservedBinaryNames := make(map[string]struct{}, len(reservedRuntimeNames))
	for _, name := range reservedRuntimeNames {
		reservedIDs["tool/"+name] = struct{}{}
		reservedBinaryNames[name] = struct{}{}
	}
	for _, plugin := range plugins {
		if plugin.Kind == "" || !builtinPluginIdentifier.MatchString(plugin.Kind) || plugin.Name == "" || !builtinPluginIdentifier.MatchString(plugin.Name) || strings.Contains(plugin.Name, "__") || plugin.DisplayName == "" {
			return fmt.Errorf("builtin plugin %q has invalid kind, namespace, or display_name", plugin.ID)
		}
		if _, reserved := reservedIDs[plugin.ID]; reserved {
			return fmt.Errorf("builtin plugin %q uses reserved core ID %q", plugin.ID, plugin.ID)
		}
		if _, exists := seenIDs[plugin.ID]; exists {
			return fmt.Errorf("duplicate builtin plugin ID %q", plugin.ID)
		}
		seenIDs[plugin.ID] = struct{}{}
		if previous, exists := seenNamespaces[plugin.Name]; exists {
			return fmt.Errorf("duplicate builtin plugin namespace %q in %q and %q", plugin.Name, previous, plugin.ID)
		}
		seenNamespaces[plugin.Name] = plugin.ID
		for _, binary := range plugin.Binaries {
			if _, reserved := reservedBinaryNames[binary.Name]; reserved {
				return fmt.Errorf("builtin plugin %q uses reserved core binary name %q", plugin.ID, binary.Name)
			}
			if previous, exists := seenResources["binary:"+binary.Name]; exists {
				return fmt.Errorf("duplicate builtin plugin resource binary %q in %q and %q", binary.Name, previous, plugin.ID)
			}
			seenResources["binary:"+binary.Name] = plugin.ID
		}
		for _, skill := range plugin.Skills {
			if previous, exists := seenResources["skill:"+skill.Name]; exists {
				return fmt.Errorf("duplicate builtin plugin resource skill %q in %q and %q", skill.Name, previous, plugin.ID)
			}
			seenResources["skill:"+skill.Name] = plugin.ID
		}
		for _, env := range plugin.SessionEnvs {
			if previous, exists := seenResources["session_env:"+env.EnvVar]; exists {
				return fmt.Errorf("duplicate builtin plugin resource session env %q in %q and %q", env.EnvVar, previous, plugin.ID)
			}
			seenResources["session_env:"+env.EnvVar] = plugin.ID
		}
	}
	return nil
}

func renderBuiltinPlugins(manifest *Manifest) ([]byte, error) {
	data, err := yaml.Marshal(manifest)
	if err != nil {
		return nil, fmt.Errorf("encode builtin plugins: %w", err)
	}
	return []byte("// Code generated by internal/tools/generatebuiltinmanifest; DO NOT EDIT.\n\npackage resources\n\nconst builtinPluginsYAML = " + strconv.Quote(string(data)) + "\n"), nil
}

// WriteBuiltinPlugins writes the generated source after the complete input tree
// validates, using a same-directory rename so a failed scan cannot publish a
// partial catalog.
func WriteBuiltinPlugins(sourceRoot, output string, reservedRuntimeNames, oauthProviderIDs []string) error {
	manifest, err := GenerateBuiltinPlugins(sourceRoot, reservedRuntimeNames, oauthProviderIDs)
	if err != nil {
		return err
	}
	rendered, err := renderBuiltinPlugins(manifest)
	if err != nil {
		return err
	}
	if current, err := os.ReadFile(output); err == nil && bytes.Equal(current, rendered) {
		return nil
	}
	tmp, err := os.CreateTemp(filepath.Dir(output), ".builtin-plugins-*.tmp")
	if err != nil {
		return fmt.Errorf("create builtin plugin output: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod builtin plugin output: %w", err)
	}
	if _, err := tmp.Write(rendered); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write builtin plugin output: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close builtin plugin output: %w", err)
	}
	if err := os.Rename(tmpName, output); err != nil {
		return fmt.Errorf("install builtin plugin output: %w", err)
	}
	return nil
}
