package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	recallyplugin "github.com/CherryHQ/stella/internal/library/recally"
	"github.com/CherryHQ/stella/internal/plugin/host"
	_ "github.com/CherryHQ/stella/internal/plugin/host/catalogimports"
	"github.com/CherryHQ/stella/internal/plugin/manifest"
	schedulerplugin "github.com/CherryHQ/stella/internal/scheduler"
	"github.com/CherryHQ/stella/pkg/toolmeta"
	builtinplugins "github.com/CherryHQ/stella/plugins"
	coreplugins "github.com/CherryHQ/stella/plugins/core"
	emailplugin "github.com/CherryHQ/stella/plugins/email"
	"github.com/CherryHQ/stella/resources"
)

func main() {
	root, err := os.Getwd()
	if err != nil {
		fatal(err)
	}
	assets := builtinplugins.BuiltinSkillAssets()
	if len(assets) == 0 {
		fatal(fmt.Errorf("generated builtin asset table is empty; run generatepluginassets first"))
	}
	sources := make([]resources.BuiltinSkillSource, 0, len(assets))
	for _, asset := range assets {
		sources = append(sources, resources.BuiltinSkillSource{
			Name:          asset.Name,
			SourceRoot:    asset.SourceRoot,
			LogicalRoot:   asset.LogicalRoot,
			OwnerPluginID: asset.OwnerPluginID,
		})
	}
	reservedRuntimeNames := make([]string, 0, len(coreplugins.RuntimeResources()))
	for _, resource := range coreplugins.RuntimeResources() {
		reservedRuntimeNames = append(reservedRuntimeNames, resource.Name)
	}
	oauthProviderIDs, err := manifest.BuiltinOAuthProviderIDs(resources.BuiltinOAuthYAML())
	if err != nil {
		fatal(err)
	}
	owners := map[string]struct{}{}
	runtimeHost := host.New(nil)
	if err := runtimeHost.LoadDefaultCatalog(); err != nil {
		fatal(fmt.Errorf("load runtime plugin catalog: %w", err))
	}
	for _, plugin := range runtimeHost.ListRegisteredPlugins() {
		owners[plugin.ID] = struct{}{}
	}
	var generatedTools []toolmeta.ActionTool
	for _, specs := range [][]toolmeta.ActionTool{emailplugin.ActionTools(), recallyplugin.ActionTools(), schedulerplugin.ActionTools()} {
		generatedTools = append(generatedTools, specs...)
	}
	goDefinitions, err := host.BuiltinToolDefinitions(toolmeta.NewRegistry(generatedTools...))
	if err != nil {
		fatal(err)
	}
	for _, definition := range goDefinitions {
		owners[definition.ID] = struct{}{}
	}
	cliManifest, err := manifest.GenerateBuiltinPlugins(filepath.Join(root, "plugins"), reservedRuntimeNames, oauthProviderIDs)
	if err != nil {
		fatal(err)
	}
	if err := validateBuiltinSkillDeclarations(assets, cliManifest.Plugins); err != nil {
		fatal(err)
	}
	for _, plugin := range cliManifest.Plugins {
		owners[plugin.ID] = struct{}{}
	}
	for _, asset := range assets {
		if asset.OwnerPluginID == "" {
			continue
		}
		if _, ok := owners[asset.OwnerPluginID]; !ok {
			fatal(fmt.Errorf("builtin skill %q has unknown owner %q", asset.Name, asset.OwnerPluginID))
		}
	}
	if err := resources.WriteBuiltinManifestFromAssets(root, filepath.Join(root, "resources", "builtin_manifest_gen.go"), sources); err != nil {
		fatal(err)
	}
	if err := manifest.WriteBuiltinPlugins(filepath.Join(root, "plugins"), filepath.Join(root, "resources", "builtin_plugins_gen.go"), reservedRuntimeNames, oauthProviderIDs); err != nil {
		fatal(err)
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "generate builtin manifest:", err)
	os.Exit(1)
}

func validateBuiltinSkillDeclarations(assets []builtinplugins.BuiltinSkillAsset, plugins []manifest.ManifestPlugin) error {
	expected := make(map[string][]string)
	for _, asset := range assets {
		if asset.OwnerPluginID != "" {
			expected[asset.OwnerPluginID] = append(expected[asset.OwnerPluginID], asset.Name)
		}
	}
	for owner := range expected {
		sort.Strings(expected[owner])
	}
	for _, plugin := range plugins {
		if _, ok := expected[plugin.ID]; !ok && len(plugin.Skills) == 0 {
			continue
		}
		if err := manifest.ValidateBundledSkillNames(plugin.Skills, expected[plugin.ID]); err != nil {
			return fmt.Errorf("builtin plugin %q skill declarations: %w", plugin.ID, err)
		}
		delete(expected, plugin.ID)
	}
	return nil
}
