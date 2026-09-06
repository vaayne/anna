package binaries

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

const shellEnvFilename = ".stella-shell-env"

// Every runtime Stella installs under $STELLA_HOME/bin obeys one permission
// contract, whether it is a single file (mise) or a versioned bundle directory
// (Xberg): anything reachable from bin/ must stay readable, and executables
// runnable, by any UID that has bin/ on PATH. The installing UID is not always
// the running one — the sandbox image takes its UID as a build arg — and the
// creating syscalls do not honor these modes on their own: os.MkdirTemp always
// creates 0700 and ignores umask, and OpenFile's mode is masked by umask. Both
// install paths therefore set the mode explicitly rather than trusting defaults,
// so neither can drift into being privately owned while the other works.
const (
	toolDirMode  = 0o755
	toolExecMode = 0o755
	toolDataMode = 0o644
)

//go:embed shell_env.sh
var shellEnv []byte

type ensureState struct {
	once sync.Once
	err  error
}

var (
	ensureMu     sync.Mutex
	ensureStates = make(map[string]*ensureState)
)

// EnsureTools extracts Stella's embedded runtimes to stellaHome/bin/. mise is a
// single compressed executable; Xberg is kept as a directory because its
// official Linux and macOS bundles include adjacent dynamic libraries.
// Already-extracted runtimes are skipped. Safe for concurrent calls.
func EnsureTools(stellaHome string) error {
	destDir := BinDir(stellaHome)

	ensureMu.Lock()
	state := ensureStates[destDir]
	if state == nil {
		state = &ensureState{}
		ensureStates[destDir] = state
	}
	ensureMu.Unlock()

	state.once.Do(func() {
		state.err = extractTools(destDir)
	})
	return state.err
}

// BinDir returns the tool binaries directory path.
func BinDir(stellaHome string) string {
	return filepath.Join(stellaHome, "bin")
}

// ToolPath returns the full path to a named tool binary, or empty if not embedded.
func ToolPath(stellaHome, name string) string {
	p := filepath.Join(BinDir(stellaHome), name)
	if _, err := os.Stat(p); err == nil {
		return p
	}
	return ""
}

// ToolNames returns the names of all embedded tools for the current platform.
func ToolNames() []string {
	var names []string
	for _, rt := range platformRuntimes() {
		names = append(names, rt.name)
	}
	return names
}

// VerifyTools checks that every runtime embedded for this platform was extracted
// to stellaHome/bin and is usable there. It no longer tolerates an empty embedded
// FS: embed_*.go names each archive exactly, so a build that skipped
// `go generate` fails to compile rather than producing a stellad that silently
// ships no runtimes. If this binary exists, its runtimes exist.
func VerifyTools(stellaHome string) error {
	names := ToolNames()
	var missing []string
	for _, name := range names {
		if ToolPath(stellaHome, name) == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("embedded tools missing in %s after extraction: %s",
			BinDir(stellaHome), strings.Join(missing, ", "))
	}
	// Present is not the same as usable. Windows has no POSIX mode bits, so the
	// contract is only meaningful — and only enforceable — elsewhere.
	if runtime.GOOS == "windows" {
		return nil
	}
	for _, name := range names {
		if err := walkToolInstall(BinDir(stellaHome), name, requirePerm); err != nil {
			return err
		}
	}
	return nil
}

// walkToolInstall yields every path belonging to one installed runtime together
// with the mode the contract requires for it, so repair and verification can
// never disagree about what "correct" means. A runtime that is not installed yet
// yields nothing; that is a skip, not an error.
func walkToolInstall(binDir, name string, fn func(path string, want os.FileMode) error) error {
	// Compare resolved against resolved. A symlinked ancestor — /var → /private/var
	// on macOS, or an operator's symlinked STELLA_HOME — otherwise makes every
	// single-file runtime look like it lives in a bundle directory.
	root, err := filepath.EvalSymlinks(binDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("resolve embedded tool dir %s: %w", binDir, err)
	}
	// A dangling launcher symlink resolves to ErrNotExist too, which is the same
	// "nothing installed here yet" case: extraction, not repair, will fix it.
	target, err := filepath.EvalSymlinks(filepath.Join(binDir, name))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("resolve embedded tool %s: %w", name, err)
	}
	dir := filepath.Dir(target)
	if dir == root {
		// Single-file runtime (mise): the executable is the entire install.
		return fn(target, toolExecMode)
	}
	// Bundle runtime (Xberg): the dynamic linker reads the adjacent libraries
	// through this directory, so the directory and every file in it count.
	if err := fn(dir, toolDirMode); err != nil {
		return err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("read embedded tool bundle %s: %w", dir, err)
	}
	for _, entry := range entries {
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		want := os.FileMode(toolDataMode)
		if path == target {
			want = toolExecMode
		}
		if err := fn(path, want); err != nil {
			return err
		}
	}
	return nil
}

// repairToolPermissions widens an install written by an earlier Stella that left
// paths owner-only. It runs on every startup because the archive fingerprint
// makes such an install byte-identical to a correct one, so the extraction fast
// path would skip it forever. Without this, tightening VerifyTools would turn a
// merely-degraded deployment into one that refuses to start.
func repairToolPermissions(binDir string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	for _, name := range ToolNames() {
		err := walkToolInstall(binDir, name, func(path string, want os.FileMode) error {
			info, err := os.Lstat(path)
			if err != nil {
				return fmt.Errorf("stat embedded tool path %s: %w", path, err)
			}
			if info.Mode().Perm() == want {
				return nil
			}
			return os.Chmod(path, want)
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func requirePerm(path string, want os.FileMode) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("stat embedded tool path %s: %w", path, err)
	}
	if got := info.Mode().Perm(); got != want {
		return fmt.Errorf("embedded tool path %s has mode %o, want %o: a UID other than the installing one cannot use it", path, got, want)
	}
	return nil
}

// embeddedRuntime describes one bundled runtime as it exists in the embedded FS.
// The list stays local to runtime extraction on purpose. internal/tools/
// syncembeddedbinaries cannot import this package — it produces the artifacts
// that embed_*.go names exactly, so this package does not compile until that
// program has run — and a "shared" registry would mean a third package that
// hides nothing from either side.
type embeddedRuntime struct {
	name    string // installed name under bin/
	archive string // filename inside toolsDir
	extract func(archivePath, destDir string) error
}

// EmbeddedRuntimeAsset identifies one release asset embedded for this
// platform. Digest is over the compressed artifact, so an asset replacement
// cannot reuse a previous public selection directory by version alone.
type EmbeddedRuntimeAsset struct {
	Name    string
	Version string
	Digest  string
}

// EmbeddedRuntimeAssets returns the embedded runtime assets for this platform.
// The result is derived from the embedded bytes and carries no mutable install
// state.
func EmbeddedRuntimeAssets() ([]EmbeddedRuntimeAsset, error) {
	runtimes := platformRuntimes()
	assets := make([]EmbeddedRuntimeAsset, 0, len(runtimes))
	for _, rt := range runtimes {
		archivePath := toolsDir + "/" + rt.archive
		version, err := archiveVersion(archivePath)
		if err != nil {
			return nil, fmt.Errorf("read embedded %s version: %w", rt.name, err)
		}
		digest, err := embeddedArchiveDigest(archivePath)
		if err != nil {
			return nil, fmt.Errorf("digest embedded %s: %w", rt.name, err)
		}
		assets = append(assets, EmbeddedRuntimeAsset{Name: rt.name, Version: version, Digest: digest})
	}
	return assets, nil
}

func embeddedArchiveDigest(path string) (string, error) {
	file, err := toolsFS.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = file.Close() }()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func knownRuntimes() []embeddedRuntime {
	mise := embeddedRuntime{name: "mise", archive: "mise.gz", extract: extractSingleFile}
	if runtime.GOOS == "windows" {
		mise = embeddedRuntime{name: "mise.exe", archive: "mise.exe.gz", extract: extractSingleFile}
	}
	return []embeddedRuntime{
		mise,
		{name: "xberg", archive: "xberg.tar.gz", extract: extractXbergBundle},
	}
}

// platformRuntimes returns the runtimes actually embedded for this platform.
// A runtime with no asset for the target — Xberg on Windows — is simply absent.
func platformRuntimes() []embeddedRuntime {
	var out []embeddedRuntime
	for _, rt := range knownRuntimes() {
		if _, err := fs.Stat(toolsFS, toolsDir+"/"+rt.archive); err == nil {
			out = append(out, rt)
		}
	}
	return out
}

// archiveVersion reads the version the sync program stamped into the archive's
// gzip header. Carrying it in the artifact rather than in a Go constant is what
// removes the hand-synchronized version pair this package used to keep with the
// generator: a bumped version cannot disagree with the bytes it describes.
func archiveVersion(archivePath string) (string, error) {
	f, err := toolsFS.Open(archivePath)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	gr, err := gzip.NewReader(f)
	if err != nil {
		return "", fmt.Errorf("read %s header: %w", archivePath, err)
	}
	defer func() { _ = gr.Close() }()
	return gr.Comment, nil
}

func allToolsExtracted(destDir string) bool {
	for _, rt := range platformRuntimes() {
		if _, err := os.Stat(filepath.Join(destDir, rt.name)); err != nil {
			return false
		}
	}
	return true
}

func extractTools(destDir string) error {
	if err := os.MkdirAll(destDir, toolDirMode); err != nil {
		return fmt.Errorf("create bin dir: %w", err)
	}
	// This file is not part of the platform archive fingerprint. Always refresh
	// it so an upgrade that changes only shell startup behavior cannot be skipped
	// by an already-current embedded tool installation.
	shellEnvPath := filepath.Join(destDir, shellEnvFilename)
	if err := writeFileAtomic(shellEnvPath, shellEnv, toolDataMode); err != nil {
		return fmt.Errorf("write managed shell environment: %w", err)
	}

	// Also not part of the archive fingerprint: the modes of an already-installed
	// runtime. Reassert them before the fast path below can skip extraction.
	if err := repairToolPermissions(destDir); err != nil {
		return fmt.Errorf("repair embedded tool permissions: %w", err)
	}

	runtimes := platformRuntimes()
	fp, err := fingerprint(runtimes)
	if err != nil {
		return err
	}
	fpFile := filepath.Join(destDir, ".embedded-version")
	if old, err := os.ReadFile(fpFile); err == nil && string(old) == fp {
		if allToolsExtracted(destDir) {
			return nil // already up to date
		}
	}

	for _, rt := range runtimes {
		if err := rt.extract(toolsDir+"/"+rt.archive, destDir); err != nil {
			return fmt.Errorf("extract %s: %w", rt.name, err)
		}
	}

	return writeFileAtomic(fpFile, []byte(fp), toolDataMode)
}

// fingerprint identifies the embedded artifact set by name, stamped version, and
// size. Version matters because a rebuilt archive of the same tool can land on
// the same size, and size alone would then skip a genuine upgrade.
func fingerprint(runtimes []embeddedRuntime) (string, error) {
	var b strings.Builder
	for _, rt := range runtimes {
		version, err := archiveVersion(toolsDir + "/" + rt.archive)
		if err != nil {
			return "", fmt.Errorf("fingerprint %s: %w", rt.name, err)
		}
		info, err := fs.Stat(toolsFS, toolsDir+"/"+rt.archive)
		if err != nil {
			return "", fmt.Errorf("fingerprint %s: %w", rt.name, err)
		}
		fmt.Fprintf(&b, "%s@%s:%d,", rt.archive, version, info.Size())
	}
	return b.String(), nil
}

// writeFileAtomic publishes content by rename so a concurrent reader — a sandbox
// shell sourcing the managed shell environment, for one — never observes a
// half-written file.
func writeFileAtomic(destPath string, content []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(destPath), ".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if _, err := tmp.Write(content); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpPath, mode); err != nil {
		return err
	}
	return os.Rename(tmpPath, destPath)
}

// extractSingleFile installs a runtime shipped as one static executable.
func extractSingleFile(srcPath, destDir string) error {
	destPath := filepath.Join(destDir, strings.TrimSuffix(filepath.Base(srcPath), ".gz"))
	data, err := toolsFS.ReadFile(srcPath)
	if err != nil {
		return err
	}

	gr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return err
	}
	defer func() { _ = gr.Close() }()

	// Publish by rename. Rewriting the live binary in place risks ETXTBSY when a
	// sandbox shell is executing it, and leaves a half-written executable behind
	// if the copy fails partway.
	tmp, err := os.CreateTemp(destDir, ".tool-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()

	const maxBinarySize = 200 << 20 // 200 MB safety cap
	// Read one byte past the cap: io.Copy against a LimitReader returns a nil
	// error at the limit, which would install a truncated binary and record it as
	// a successful, fingerprinted extraction.
	written, err := io.Copy(tmp, io.LimitReader(gr, maxBinarySize+1))
	if err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if written > maxBinarySize {
		return fmt.Errorf("%s exceeds %d bytes", destPath, maxBinarySize)
	}
	if err := os.Chmod(tmpPath, toolExecMode); err != nil {
		return err
	}
	return os.Rename(tmpPath, destPath)
}

func extractXbergBundle(srcPath, destDir string) error {
	// The version names the install directory, so read it from the artifact the
	// same way the fingerprint does rather than taking a caller's word for it.
	version, err := archiveVersion(srcPath)
	if err != nil {
		return err
	}
	data, err := toolsFS.ReadFile(srcPath)
	if err != nil {
		return err
	}
	gr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return err
	}
	defer func() { _ = gr.Close() }()

	if version == "" {
		return fmt.Errorf("bundle carries no version stamp")
	}
	runtimeDir := filepath.Join(destDir, "xberg-v"+version)
	tmpDir, err := os.MkdirTemp(destDir, ".xberg-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()
	// Set the mode on the staging dir so the atomic rename publishes a directory
	// that is already correct, never a briefly-private one.
	if err := os.Chmod(tmpDir, toolDirMode); err != nil {
		return err
	}

	const maxBundleSize = 300 << 20
	var written int64
	tr := tar.NewReader(gr)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		switch h.Typeflag {
		case tar.TypeReg:
		case tar.TypeDir, tar.TypeXGlobalHeader, tar.TypeXHeader:
			continue
		default:
			// Shared-library bundles commonly carry symlinks (libfoo.so ->
			// libfoo.so.1). Skipping them would extract "successfully" and then
			// fail at dlopen, so refuse rather than install something broken.
			return fmt.Errorf("bundle entry %q has unsupported type %q", h.Name, h.Typeflag)
		}
		// Base cleans on its own and drops every directory component, so a
		// traversing entry flattens instead of escaping. ".." survives that and
		// would otherwise only be stopped by O_EXCL failing on the parent dir,
		// which is too subtle to rely on.
		name := filepath.Base(h.Name)
		if name == "." || name == ".." || name == string(filepath.Separator) {
			return fmt.Errorf("invalid bundle entry %q", h.Name)
		}
		if h.Size < 0 || written+h.Size > maxBundleSize {
			return fmt.Errorf("bundle exceeds %d bytes", maxBundleSize)
		}
		mode := os.FileMode(toolDataMode)
		if name == "xberg" {
			mode = toolExecMode
		}
		path := filepath.Join(tmpDir, name)
		out, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_EXCL, mode)
		if err != nil {
			return err
		}
		_, copyErr := io.CopyN(out, tr, h.Size)
		closeErr := out.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
		if err := os.Chmod(path, mode); err != nil {
			return err
		}
		written += h.Size
	}
	if _, err := os.Stat(filepath.Join(tmpDir, "xberg")); err != nil {
		return fmt.Errorf("bundle executable missing: %w", err)
	}
	if err := os.RemoveAll(runtimeDir); err != nil {
		return err
	}
	if err := os.Rename(tmpDir, runtimeDir); err != nil {
		return err
	}
	launcher := filepath.Join(destDir, "xberg")
	launcherTmp := launcher + ".tmp"
	_ = os.Remove(launcherTmp)
	if err := os.Symlink(filepath.Join(filepath.Base(runtimeDir), "xberg"), launcherTmp); err != nil {
		return err
	}
	if err := os.Rename(launcherTmp, launcher); err != nil {
		return err
	}
	// Only after the launcher points at the new bundle: an upgrade otherwise
	// leaves every superseded version on disk forever, and each is ~140 MB.
	stale, err := filepath.Glob(filepath.Join(destDir, "xberg-v*"))
	if err != nil {
		return err
	}
	for _, dir := range stale {
		if dir == runtimeDir {
			continue
		}
		if err := os.RemoveAll(dir); err != nil {
			return fmt.Errorf("remove superseded bundle %s: %w", dir, err)
		}
	}
	return nil
}
