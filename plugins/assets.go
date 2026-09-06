// Package plugins contains the generated release asset projection for builtin
// plugins. The generated declarations keep source paths explicit; runtime
// code never walks the repository to discover plugin assets.
package plugins

import (
	"fmt"
	"io/fs"
	"path"

	pkgplugins "github.com/CherryHQ/stella/pkg/plugins"
)

// BuiltinSkillAsset describes one release-owned skill tree. SourceRoot is the
// physical path embedded below, while LogicalRoot is the stable bundle path.
type BuiltinSkillAsset = pkgplugins.BuiltinSkillAsset

// BuiltinSkillAssets returns a defensive copy of the generated asset table.
func BuiltinSkillAssets() []BuiltinSkillAsset {
	return append([]BuiltinSkillAsset(nil), builtinSkillAssets...)
}

// BuiltinSkillFS returns the embedded physical asset tree.
func BuiltinSkillFS() fs.FS { return builtinAssetFS }

// ReadBuiltinSkillFile reads one file from an explicit physical asset root.
func ReadBuiltinSkillFile(sourceRoot, file string) ([]byte, error) {
	if !fs.ValidPath(sourceRoot) || !fs.ValidPath(file) || sourceRoot == "." || file == "." {
		return nil, fmt.Errorf("invalid builtin asset path")
	}
	return fs.ReadFile(builtinAssetFS, path.Join(sourceRoot, file))
}
