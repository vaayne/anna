package core

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	builtinplugins "github.com/CherryHQ/stella/plugins"
)

func TestCoreSkillsHaveOneEmbeddedSource(t *testing.T) {
	assets := builtinplugins.BuiltinSkillAssets()
	coreCount := 0
	for _, asset := range assets {
		if asset.OwnerPluginID != "" {
			continue
		}
		coreCount++
		if asset.SourceRoot == "" || asset.LogicalRoot == "" {
			t.Fatalf("core skill %q has incomplete asset descriptor: %#v", asset.Name, asset)
		}
		root := filepath.Join("..", filepath.FromSlash(asset.SourceRoot))
		disk := os.DirFS(root)
		if err := fs.WalkDir(disk, ".", func(name string, entry fs.DirEntry, err error) error {
			if err != nil || entry.IsDir() {
				return err
			}
			want, err := fs.ReadFile(disk, name)
			if err != nil {
				return err
			}
			got, err := fs.ReadFile(builtinplugins.BuiltinSkillFS(), filepath.ToSlash(filepath.Join(asset.SourceRoot, name)))
			if err != nil {
				return err
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("embedded core skill %s differs", filepath.Join(asset.Name, name))
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	if coreCount != 2 {
		t.Fatalf("core asset count = %d, want stella and xberg only", coreCount)
	}
}
