package main

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	maxAssets = 128
	maxFiles  = 4096
	maxBytes  = 32 << 20
)

type assetFile struct {
	Name          string
	SourceRoot    string
	LogicalRoot   string
	OwnerPluginID string
	Files         []string
	Bytes         int64
}

type assetDocument struct {
	Assets []assetEntry `yaml:"assets"`
}

type assetEntry struct {
	Name          string `yaml:"name"`
	Source        string `yaml:"source"`
	LogicalRoot   string `yaml:"logical_root"`
	OwnerPluginID string `yaml:"owner_plugin_id"`
}

func main() {
	root, err := os.Getwd()
	if err != nil {
		fatal(err)
	}
	assets, err := discover(filepath.Join(root, "plugins"))
	if err != nil {
		fatal(err)
	}
	if err := writeGenerated(filepath.Join(root, "plugins", "builtin_assets_gen.go"), assets); err != nil {
		fatal(err)
	}
}

func discover(pluginsRoot string) ([]assetFile, error) {
	var paths []string
	err := filepath.WalkDir(pluginsRoot, func(filename string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("plugin asset tree contains symlink %q", filename)
		}
		if entry.Name() == "assets.yaml" && !entry.Type().IsRegular() {
			return fmt.Errorf("assets.yaml %q is not a regular file", filename)
		}
		if !entry.IsDir() && entry.Name() == "assets.yaml" {
			paths = append(paths, filename)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)
	assets := make([]assetFile, 0)
	seenNames := map[string]struct{}{}
	seenSources := map[string]struct{}{}
	seenLogical := map[string]struct{}{}
	for _, filename := range paths {
		entries, err := decode(filename)
		if err != nil {
			return nil, err
		}
		dir := filepath.Dir(filename)
		relDir, err := filepath.Rel(pluginsRoot, dir)
		if err != nil {
			return nil, err
		}
		relDir = filepath.ToSlash(relDir)
		for _, entry := range entries {
			if err := validateEntry(relDir, entry); err != nil {
				return nil, fmt.Errorf("%s: %w", filename, err)
			}
			source := path.Join(relDir, entry.Source)
			if relDir == "." {
				source = entry.Source
			}
			if _, ok := seenNames[entry.Name]; ok {
				return nil, fmt.Errorf("duplicate builtin skill name %q", entry.Name)
			}
			if _, ok := seenSources[source]; ok {
				return nil, fmt.Errorf("duplicate builtin skill source root %q", source)
			}
			if _, ok := seenLogical[entry.LogicalRoot]; ok {
				return nil, fmt.Errorf("duplicate builtin skill logical root %q", entry.LogicalRoot)
			}
			files, bytes, err := collectFiles(filepath.Join(pluginsRoot, filepath.FromSlash(source)))
			if err != nil {
				return nil, fmt.Errorf("asset %q: %w", entry.Name, err)
			}
			seenNames[entry.Name] = struct{}{}
			seenSources[source] = struct{}{}
			seenLogical[entry.LogicalRoot] = struct{}{}
			assets = append(assets, assetFile{Name: entry.Name, SourceRoot: source, LogicalRoot: entry.LogicalRoot, OwnerPluginID: entry.OwnerPluginID, Files: files, Bytes: bytes})
		}
	}
	if len(assets) == 0 || len(assets) > maxAssets {
		return nil, fmt.Errorf("builtin asset count %d outside 1..%d", len(assets), maxAssets)
	}
	if files, bytes := assetTotals(assets); files > maxFiles || bytes > maxBytes {
		return nil, fmt.Errorf("builtin assets exceed ceilings: files=%d/%d bytes=%d/%d", files, maxFiles, bytes, maxBytes)
	}
	sort.Slice(assets, func(i, j int) bool { return assets[i].Name < assets[j].Name })
	return assets, nil
}

func decode(filename string) ([]assetEntry, error) {
	f, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	decoder := yaml.NewDecoder(f)
	decoder.KnownFields(true)
	var document assetDocument
	if err := decoder.Decode(&document); err != nil {
		return nil, fmt.Errorf("decode assets: %w", err)
	}
	var extra assetDocument
	if err := decoder.Decode(&extra); err == io.EOF {
		return document.Assets, nil
	} else if err != nil {
		return nil, fmt.Errorf("assets must contain one YAML document: %w", err)
	}
	return nil, fmt.Errorf("assets must contain one YAML document")
}

func validateEntry(dir string, entry assetEntry) error {
	if entry.Name == "" || !validPathComponent(entry.Name) || strings.Contains(entry.Name, "/") {
		return fmt.Errorf("invalid asset name %q", entry.Name)
	}
	if !validRelativePath(entry.Source) || !validRelativePath(entry.LogicalRoot) {
		return fmt.Errorf("invalid asset path for %q", entry.Name)
	}
	if path.Base(entry.LogicalRoot) != entry.Name {
		return fmt.Errorf("logical root %q does not end in asset name %q", entry.LogicalRoot, entry.Name)
	}
	if dir == "core" {
		if entry.OwnerPluginID != "" || !strings.HasPrefix(entry.LogicalRoot, "core/") || (entry.Name != "stella" && entry.Name != "xberg") {
			return fmt.Errorf("core asset %q must use the explicit empty-owner core allowlist", entry.Name)
		}
		return nil
	}
	if entry.OwnerPluginID == "" {
		return fmt.Errorf("asset %q has no owner", entry.Name)
	}
	return nil
}

func validRelativePath(value string) bool {
	return value != "" && value != "." && fs.ValidPath(value)
}

func validPathComponent(value string) bool {
	return value != "" && value != "." && fs.ValidPath(value)
}

func collectFiles(root string) ([]string, int64, error) {
	info, err := os.Lstat(root)
	if err != nil {
		return nil, 0, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return nil, 0, fmt.Errorf("source must be a non-symlink directory")
	}
	var files []string
	var bytes int64
	err = filepath.WalkDir(root, func(filename string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("source contains symlink %q", filename)
		}
		rel, err := filepath.Rel(root, filename)
		if err != nil {
			return err
		}
		if rel == "." || entry.IsDir() {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if !validRelativePath(rel) {
			return fmt.Errorf("source contains non-canonical path %q", rel)
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("source contains unsupported file %q", rel)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		mode := info.Mode().Perm()
		if mode != 0o644 && mode != 0o755 {
			return fmt.Errorf("source file %q has unsupported mode %04o", rel, mode)
		}
		files = append(files, rel)
		bytes += info.Size()
		return nil
	})
	if err != nil {
		return nil, 0, err
	}
	if len(files) == 0 {
		return nil, 0, fmt.Errorf("source is empty")
	}
	sort.Strings(files)
	return files, bytes, nil
}

func assetTotals(assets []assetFile) (int, int64) {
	var files int
	var bytes int64
	for _, asset := range assets {
		files += len(asset.Files)
		bytes += asset.Bytes
	}
	return files, bytes
}

func writeGenerated(output string, assets []assetFile) error {
	var b strings.Builder
	b.WriteString("// Code generated by internal/tools/generatepluginassets; DO NOT EDIT.\n\npackage plugins\n\nimport \"embed\"\n\n")
	for _, asset := range assets {
		for _, file := range asset.Files {
			fmt.Fprintf(&b, "//go:embed %s\n", strconv.Quote(filepath.ToSlash(filepath.Join(asset.SourceRoot, file))))
		}
	}
	b.WriteString("var builtinAssetFS embed.FS\n\nvar builtinSkillAssets = []BuiltinSkillAsset{\n")
	for _, asset := range assets {
		fmt.Fprintf(&b, "\t{Name: %q, SourceRoot: %q, LogicalRoot: %q, OwnerPluginID: %q},\n", asset.Name, asset.SourceRoot, asset.LogicalRoot, asset.OwnerPluginID)
	}
	b.WriteString("}\n")
	return publish(output, []byte(b.String()))
}

func publish(output string, content []byte) error {
	if current, err := os.ReadFile(output); err == nil && string(current) == string(content) {
		return nil
	}
	temporary, err := os.CreateTemp(filepath.Dir(output), ".builtin-assets-*.tmp")
	if err != nil {
		return err
	}
	temporaryName := temporary.Name()
	defer func() { _ = os.Remove(temporaryName) }()
	if err := temporary.Chmod(0o644); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(content); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryName, output)
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "generate plugin assets:", err)
	os.Exit(1)
}
