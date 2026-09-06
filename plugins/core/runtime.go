// Package core owns Stella's release-provided runtime commands.
package core

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"

	"github.com/CherryHQ/stella/internal/plugin/manifest"
	"github.com/CherryHQ/stella/resources/binaries"
)

// RuntimeResource describes a release-owned command that is available to
// every session. Mise and Xberg are embedded in the release; fd and rg are
// installed into the release-owned mise cache at fixed versions. This catalog
// is independent of plugin state and snapshot identities.
type RuntimeResource struct {
	Name     string
	MiseTool string
	Version  string
	Embedded bool
	SkillRef string
}

// RuntimePlan is the immutable core command selection exposed to startup and
// sandbox backends. PublicBinDir is the only directory that needs to be added
// to a runner PATH.
type RuntimePlan struct {
	Identity     string
	PublicDir    string
	PublicBinDir string
	Runtimes     []Runtime
}

// Runtime is one command in a prepared core selection.
type Runtime struct {
	Name      string
	Version   string
	Path      string
	Available bool
}

// RuntimeResources returns the release-owned runtime commands in stable
// declaration order. Callers receive a fresh slice and may not mutate the
// process-wide declaration.
func RuntimeResources() []RuntimeResource {
	return []RuntimeResource{
		{Name: "mise", Embedded: true},
		{Name: "xberg", Embedded: true, SkillRef: "builtin:xberg"},
		{Name: "fd", MiseTool: "github:sharkdp/fd", Version: "10.4.2"},
		{Name: "rg", MiseTool: "github:BurntSushi/ripgrep", Version: "15.2.0"},
	}
}

// RuntimeIdentity returns the content identity of the core declaration and
// embedded release assets for this platform. Embedded digests prevent a
// changed release asset from reusing an older public selection directory.
func RuntimeIdentity() (string, error) { return cachedRuntimeIdentity() }

var embeddedRuntimeAssets = sync.OnceValues(binaries.EmbeddedRuntimeAssets)

var cachedRuntimeIdentity = sync.OnceValues(func() (string, error) {
	assets, err := embeddedRuntimeAssets()
	if err != nil {
		return "", err
	}
	return runtimeIdentity(RuntimeResources(), assets)
})

type runtimeIdentityInput struct {
	OS        string                          `json:"os"`
	Arch      string                          `json:"arch"`
	Resources []RuntimeResource               `json:"resources"`
	Assets    []binaries.EmbeddedRuntimeAsset `json:"assets"`
}

func runtimeIdentity(resources []RuntimeResource, assets []binaries.EmbeddedRuntimeAsset) (string, error) {
	canonicalAssets := slices.Clone(assets)
	slices.SortFunc(canonicalAssets, func(left, right binaries.EmbeddedRuntimeAsset) int {
		return strings.Compare(left.Name, right.Name)
	})
	payload, err := json.Marshal(runtimeIdentityInput{
		OS: runtime.GOOS, Arch: runtime.GOARCH, Resources: resources, Assets: canonicalAssets,
	})
	if err != nil {
		return "", fmt.Errorf("encode core runtime identity: %w", err)
	}
	digest := sha256.Sum256(payload)
	return "core-" + hex.EncodeToString(digest[:16]), nil
}

// Prepare extracts embedded runtimes and installs the fixed mise-owned core
// tools into one content-addressed public selection. It never creates plugin
// configuration or snapshot identity.
func Prepare(ctx context.Context, stellaHome string) (RuntimePlan, error) {
	if stellaHome == "" {
		return RuntimePlan{}, errors.New("core: stella home is required")
	}
	if err := binaries.EnsureTools(stellaHome); err != nil {
		return RuntimePlan{}, fmt.Errorf("core: ensure embedded runtimes: %w", err)
	}
	// Sandbox mounts resolve STELLA_HOME physically; publish in the same frame.
	resolvedHome, err := filepath.EvalSymlinks(stellaHome)
	if err != nil {
		return RuntimePlan{}, fmt.Errorf("core: resolve stella home: %w", err)
	}
	stellaHome = resolvedHome
	identity, err := RuntimeIdentity()
	if err != nil {
		return RuntimePlan{}, err
	}
	dataDir := filepath.Join(stellaHome, ".mise-tools")
	publicDir := filepath.Join(dataDir, "public", identity)
	tools := make([]manifest.NativeMiseTool, 0, 2)
	embeddedNames := make([]string, 0, 2)
	for _, resource := range RuntimeResources() {
		if resource.Embedded {
			embeddedNames = append(embeddedNames, resource.Name)
		}
		if resource.MiseTool == "" {
			continue
		}
		publicName := resource.Name
		if runtime.GOOS == "windows" {
			publicName += ".exe"
		}
		tools = append(tools, manifest.NativeMiseTool{
			Key: resource.MiseTool, Version: resource.Version,
			Lookup: resource.Name, PublicName: publicName,
		})
	}
	if err := manifest.InstallNativeMiseSelection(ctx, stellaHome, manifest.NativeSelectionPlan{
		DataDir: dataDir, PublicDir: publicDir, PublicBinDir: publicDir, EmbeddedNames: embeddedNames,
	}, tools); err != nil {
		return RuntimePlan{}, fmt.Errorf("core: prepare native selection: %w", err)
	}
	plan := runtimePlan(identity, publicDir)
	if err := Verify(plan); err != nil {
		return RuntimePlan{}, err
	}
	return plan, nil
}

// Verify checks that a prepared plan still names the current release assets
// and exposes every core runtime available on this platform.
func Verify(plan RuntimePlan) error {
	if plan.Identity == "" || plan.PublicDir == "" || plan.PublicBinDir == "" {
		return errors.New("core: incomplete runtime plan")
	}
	identity, err := RuntimeIdentity()
	if err != nil {
		return err
	}
	if plan.Identity != identity {
		return fmt.Errorf("core: runtime plan identity %q is stale", plan.Identity)
	}
	assets, err := embeddedRuntimeAssets()
	if err != nil {
		return err
	}
	assetNames := make(map[string]struct{}, len(assets))
	for _, asset := range assets {
		assetNames[asset.Name] = struct{}{}
	}
	preparedByName := make(map[string]Runtime, len(plan.Runtimes))
	for _, prepared := range plan.Runtimes {
		if _, exists := preparedByName[prepared.Name]; exists {
			return fmt.Errorf("core: duplicate runtime %q", prepared.Name)
		}
		preparedByName[prepared.Name] = prepared
	}
	if len(preparedByName) != len(RuntimeResources()) {
		return errors.New("core: runtime plan is incomplete")
	}
	for _, resource := range RuntimeResources() {
		prepared, ok := preparedByName[resource.Name]
		if !ok {
			return fmt.Errorf("core: runtime %q is not declared in plan", resource.Name)
		}
		if !prepared.Available && resource.MiseTool == "" {
			assetName := resource.Name
			if resource.Name == "mise" && runtime.GOOS == "windows" {
				assetName = "mise.exe"
			}
			if _, embedded := assetNames[assetName]; !embedded {
				continue
			}
		}
		publicName := prepared.Name
		if runtime.GOOS == "windows" {
			publicName += ".exe"
		}
		if prepared.Path != filepath.Join(plan.PublicBinDir, publicName) {
			return fmt.Errorf("core: runtime %q has invalid path", prepared.Name)
		}
		info, err := os.Stat(prepared.Path)
		if err != nil {
			return fmt.Errorf("core: runtime %q is unavailable: %w", prepared.Name, err)
		}
		if info.IsDir() || (runtime.GOOS != "windows" && info.Mode()&0o111 == 0) {
			return fmt.Errorf("core: runtime %q is not executable", prepared.Name)
		}
	}
	return nil
}

// UnavailableSkillRefs returns core skill references whose release runtime is
// absent on this platform. Embedded assets are the only source of truth; a
// missing metadata read fails closed by hiding the affected runtime skill.
func UnavailableSkillRefs() []string {
	assets, err := embeddedRuntimeAssets()
	available := make(map[string]struct{}, len(assets))
	if err == nil {
		for _, asset := range assets {
			available[asset.Name] = struct{}{}
		}
	}
	var unavailable []string
	for _, resource := range RuntimeResources() {
		if resource.SkillRef == "" {
			continue
		}
		assetName := resource.Name
		if resource.Name == "mise" && runtime.GOOS == "windows" {
			assetName = "mise.exe"
		}
		if _, ok := available[assetName]; !ok {
			unavailable = append(unavailable, resource.SkillRef)
		}
	}
	return unavailable
}

func runtimePlan(identity, publicDir string) RuntimePlan {
	plan := RuntimePlan{
		Identity: identity, PublicDir: publicDir, PublicBinDir: publicDir,
		Runtimes: make([]Runtime, 0, len(RuntimeResources())),
	}
	for _, resource := range RuntimeResources() {
		name := resource.Name
		publicName := name
		if runtime.GOOS == "windows" {
			publicName += ".exe"
		}
		path := filepath.Join(publicDir, publicName)
		_, err := os.Stat(path)
		plan.Runtimes = append(plan.Runtimes, Runtime{
			Name: name, Version: resource.Version, Path: path, Available: err == nil,
		})
	}
	return plan
}
