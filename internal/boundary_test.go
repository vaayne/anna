// Package-direction guards for the repo's layered trees. The rule text lives
// in each guarded package or tree (pkg is the extension contract;
// non-channel plugins are replaceable adapters; plugins/core owns fixed release
// runtimes; internal/core is the leaf
// kernel; internal/platform is infrastructure) and in
// web/content/docs/development/rules/go-patterns.md; this file only enforces it.
package internal_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const modulePrefix = "github.com/CherryHQ/stella/"

// boundary is one guarded tree. allowed lists the in-repo import paths (exact,
// or prefixes ending in "/") files under root may use; testOnly widens that for
// _test.go files. Standard library and third-party imports are unconstrained:
// the rule is about the direction of intra-repo dependencies.
type boundary struct {
	root     string
	allowed  []string
	testOnly []string
	skipDirs []string
}

var boundaries = []boundary{
	{root: "pkg", allowed: []string{"pkg/"}},
	{root: "plugins", allowed: []string{"pkg/", "plugins/"}, testOnly: []string{"internal/agent/prompt"}, skipDirs: []string{"channels", "core"}},
	{root: "plugins/core", allowed: []string{"internal/plugin/manifest", "resources/binaries"}, testOnly: []string{"resources"}},
	// Channel tests use the host and notifier fixtures to exercise registration;
	// production channel adapters remain under the same pkg-only guard.
	{root: "plugins/channels", allowed: []string{"pkg/", "plugins/"}, testOnly: []string{"internal/notify", "internal/platform/config", "internal/plugin/host"}},
	{root: "internal/core", allowed: []string{"pkg/", "internal/core/", "internal/authz", "internal/platform/config"}},
	{root: "internal/platform", allowed: []string{"pkg/", "internal/platform/"}, testOnly: []string{"internal/db/dbtest"}},
}

// forbidden returns the in-repo imports of f that fall outside b's whitelist.
func (b boundary) forbidden(f *ast.File, isTest bool) []string {
	allowed := b.allowed
	if isTest {
		allowed = append(append([]string{}, allowed...), b.testOnly...)
	}
	var out []string
	for _, imp := range f.Imports {
		path := strings.Trim(imp.Path.Value, "`\"")
		rel, ok := strings.CutPrefix(path, modulePrefix)
		if !ok {
			continue
		}
		permitted := false
		for _, prefix := range allowed {
			if rel == strings.TrimSuffix(prefix, "/") || (strings.HasSuffix(prefix, "/") && strings.HasPrefix(rel, prefix)) {
				permitted = true
				break
			}
		}
		if !permitted {
			out = append(out, path)
		}
	}
	return out
}

func TestPackageBoundaries(t *testing.T) {
	repo, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range boundaries {
		t.Run(b.root, func(t *testing.T) {
			root := filepath.Join(repo, filepath.FromSlash(b.root))
			seen := 0
			err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if d.IsDir() {
					rel, relErr := filepath.Rel(root, path)
					if relErr != nil {
						return relErr
					}
					if slices.Contains(b.skipDirs, filepath.ToSlash(rel)) {
						return filepath.SkipDir
					}
					if d.Name() == "testdata" || d.Name() == "node_modules" {
						return filepath.SkipDir
					}
					return nil
				}
				if !strings.HasSuffix(path, ".go") {
					return nil
				}
				seen++
				f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
				if err != nil {
					return err
				}
				rel, _ := filepath.Rel(repo, path)
				for _, bad := range b.forbidden(f, strings.HasSuffix(path, "_test.go")) {
					t.Errorf("%s imports %s, outside the %s whitelist %v; either the dependency is wrong or the package does not belong there", filepath.ToSlash(rel), bad, b.root, b.allowed)
				}
				return nil
			})
			if err != nil {
				t.Fatalf("walk %s: %v", b.root, err)
			}
			if seen == 0 {
				t.Fatalf("no Go files under %s; the guard would pass vacuously", b.root)
			}
		})
	}
}

func TestInternalDoesNotImportNonChannelPlugins(t *testing.T) {
	repo, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(repo, "internal")
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "testdata" || d.Name() == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		relPath, _ := filepath.Rel(repo, path)
		for _, imp := range file.Imports {
			importPath := strings.Trim(imp.Path.Value, "`\"")
			relImport, ok := strings.CutPrefix(importPath, modulePrefix)
			if !ok || !isReplaceablePluginImport(relImport) {
				continue
			}
			t.Errorf("%s imports replaceable plugin %s; wire it in cmd/stellad through a pkg contract", filepath.ToSlash(relPath), importPath)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestPackageBoundariesTripwire proves each guard bites: a synthetic file
// reaching into a domain package is flagged, a whitelisted one is not, and a
// test-only carve-out stays test-only.
func TestPackageBoundariesTripwire(t *testing.T) {
	parse := func(src string) *ast.File {
		t.Helper()
		f, err := parser.ParseFile(token.NewFileSet(), "synthetic.go", src, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		return f
	}
	const offender = modulePrefix + "internal/agent"
	for _, b := range boundaries {
		t.Run(b.root, func(t *testing.T) {
			allowed := b.allowed[0]
			if strings.HasSuffix(allowed, "/") {
				allowed += "ai"
			}
			bad := parse("package x\nimport (\n\t\"context\"\n\t\"" + offender + "\"\n\t\"" + modulePrefix + allowed + "\"\n)\n")
			if got := b.forbidden(bad, false); len(got) != 1 || got[0] != offender {
				t.Fatalf("counterexample not detected, got %v; the guard is vacuous", got)
			}
			for _, extra := range b.testOnly {
				f := parse("package x\nimport _ \"" + modulePrefix + extra + "\"\n")
				if got := b.forbidden(f, true); len(got) != 0 {
					t.Fatalf("test-only import %s rejected in a _test.go file: %v", extra, got)
				}
				if got := b.forbidden(f, false); len(got) != 1 {
					t.Fatalf("test-only import %s must stay test-only, got %v in a non-test file", extra, got)
				}
			}
		})
	}
}

// The fixed release runtime is the only non-channel plugin package that
// internal code may consume directly. Its subpackages gain no exception.
func isReplaceablePluginImport(name string) bool {
	return strings.HasPrefix(name, "plugins/") && !strings.HasPrefix(name, "plugins/channels/") && name != "plugins/core"
}

func TestCoreRuntimeBoundaryIsExact(t *testing.T) {
	for name, rejected := range map[string]bool{"plugins/core": false, "plugins/core/other": true, "plugins/core-extra": true, "plugins/tools": true} {
		if got := isReplaceablePluginImport(name); got != rejected {
			t.Errorf("%s rejected=%v, want %v", name, got, rejected)
		}
	}
	var coreBoundary boundary
	for _, b := range boundaries {
		if b.root == "plugins/core" {
			coreBoundary = b
		}
	}
	for name, rejected := range map[string]bool{"internal/plugin/manifest": false, "resources/binaries": false, "internal/plugin/manifest/other": true, "internal/agent": true, "resources": true} {
		f, err := parser.ParseFile(token.NewFileSet(), "core.go", "package core\nimport _ \""+modulePrefix+name+"\"", parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		if got := len(coreBoundary.forbidden(f, false)) > 0; got != rejected {
			t.Errorf("core -> %s rejected=%v, want %v", name, got, rejected)
		}
	}
}
