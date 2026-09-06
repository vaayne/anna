package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	ucli "github.com/urfave/cli/v2"

	"github.com/CherryHQ/stella/internal/platform/config"
	pluginmanifest "github.com/CherryHQ/stella/internal/plugin/manifest"
	pkgplugins "github.com/CherryHQ/stella/pkg/plugins"
	"github.com/CherryHQ/stella/plugins/core"
	"github.com/CherryHQ/stella/resources"
)

func systemBundleCommand() *ucli.Command {
	return &ucli.Command{
		Name:     "system-bundle",
		Usage:    "Inspect and install the builtin skill bundle",
		Category: "System",
		Subcommands: []*ucli.Command{
			systemBundleRevisionCommand(),
			systemBundleInstallCommand(),
			systemBundleVerifyCommand(),
		},
	}
}

func systemBundleRevisionCommand() *ucli.Command {
	return &ucli.Command{
		Name:  "revision",
		Usage: "Print the builtin skill bundle revision",
		Action: func(c *ucli.Context) error {
			registry, err := resources.Default()
			if err != nil {
				return fmt.Errorf("load builtin skill bundle: %w", err)
			}
			if _, err := fmt.Fprintln(c.App.Writer, registry.BundleRevision()); err != nil {
				return fmt.Errorf("write builtin skill bundle revision: %w", err)
			}
			return nil
		},
	}
}

func systemBundleInstallCommand() *ucli.Command {
	return &ucli.Command{
		Name:  "install",
		Usage: "Install the verified builtin skill bundle into $STELLA_HOME",
		Flags: []ucli.Flag{
			&ucli.StringFlag{Name: "core-path", Usage: "publish the prepared core runtime selection at this image-local path"},
			&ucli.BoolFlag{Name: "prepare-builtin-artifacts", Usage: "prepare shipped plugin binaries for image-local reuse"},
		},
		Action: func(c *ucli.Context) error {
			plan, err := core.Prepare(c.Context, config.StellaHome())
			if err != nil {
				return fmt.Errorf("prepare core runtimes: %w", err)
			}
			if target := c.String("core-path"); target != "" {
				if err := publishCoreRuntimePath(plan, target); err != nil {
					return fmt.Errorf("publish core runtimes: %w", err)
				}
			}
			if c.Bool("prepare-builtin-artifacts") {
				if err := prepareBuiltinArtifacts(c.Context, config.StellaHome()); err != nil {
					return fmt.Errorf("prepare builtin artifacts: %w", err)
				}
			}
			registry, err := resources.Default()
			if err != nil {
				return fmt.Errorf("load builtin skill bundle: %w", err)
			}
			bundlePath, err := registry.InstallBuiltinBundle(config.StellaHome())
			if err != nil {
				return fmt.Errorf("install builtin skill bundle: %w", err)
			}
			if _, err := fmt.Fprintf(c.App.Writer, "installed builtin skill bundle %s at %s\n", registry.BundleRevision(), bundlePath); err != nil {
				return fmt.Errorf("write builtin skill bundle install result: %w", err)
			}
			return nil
		},
	}
}

func prepareBuiltinArtifacts(ctx context.Context, stellaHome string) error {
	if stellaHome == "" {
		return fmt.Errorf("stella home is required")
	}
	builtin, err := pluginmanifest.LoadBuiltin()
	if err != nil {
		return fmt.Errorf("load builtin manifest: %w", err)
	}
	artifactRoot := filepath.Join(stellaHome, ".mise-tools", "builtin-artifacts")
	if err := os.MkdirAll(artifactRoot, 0o755); err != nil {
		return fmt.Errorf("create builtin artifact directory: %w", err)
	}
	for _, plugin := range builtin.Plugins {
		for _, binary := range plugin.Binaries {
			spec := pkgplugins.PluginBinarySpec{
				Name: binary.Name, Tool: binary.Tool, Version: binary.Version, Options: binary.Options,
			}
			fingerprint, err := pkgplugins.BinaryArtifactIdentity(spec)
			if err != nil {
				return fmt.Errorf("identity for builtin binary %q: %w", binary.Name, err)
			}
			artifactDir := filepath.Join(artifactRoot, fingerprint)
			if err := pluginmanifest.InstallNativeMiseSelection(ctx, stellaHome, pluginmanifest.NativeSelectionPlan{
				DataDir:   filepath.Join(stellaHome, ".mise-tools"),
				PublicDir: artifactDir, PublicBinDir: artifactDir,
			}, []pluginmanifest.NativeMiseTool{{
				Key: binary.Tool, Version: binary.Version, Options: binary.Options,
				Lookup: pluginmanifest.BinaryLookupName(binary), PublicName: binary.Name,
			}}); err != nil {
				return fmt.Errorf("install builtin binary %q: %w", binary.Name, err)
			}
		}
	}
	return nil
}

func publishCoreRuntimePath(plan core.RuntimePlan, target string) error {
	if !filepath.IsAbs(target) {
		return fmt.Errorf("path %q must be absolute", target)
	}
	target = filepath.Clean(target)
	if target == string(filepath.Separator) {
		return fmt.Errorf("path %q is not a publication target", target)
	}
	if existing, err := os.Readlink(target); err == nil {
		if filepath.Clean(existing) == filepath.Clean(plan.PublicDir) {
			return nil
		}
		return fmt.Errorf("path %q already points to %q", target, existing)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("inspect %q: %w", target, err)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return fmt.Errorf("create publication parent: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(target), ".core-runtime-*")
	if err != nil {
		return fmt.Errorf("stage publication: %w", err)
	}
	tmpPath := tmp.Name()
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("close publication stage: %w", err)
	}
	if err := os.Remove(tmpPath); err != nil {
		return fmt.Errorf("remove publication stage: %w", err)
	}
	if err := os.Symlink(plan.PublicDir, tmpPath); err != nil {
		return fmt.Errorf("create publication symlink: %w", err)
	}
	if err := os.Rename(tmpPath, target); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("publish core runtime symlink: %w", err)
	}
	return nil
}

func systemBundleVerifyCommand() *ucli.Command {
	return &ucli.Command{
		Name:  "verify",
		Usage: "Verify the builtin skill bundle installed in $STELLA_HOME",
		Action: func(c *ucli.Context) error {
			registry, err := resources.Default()
			if err != nil {
				return fmt.Errorf("load builtin skill bundle: %w", err)
			}
			if err := registry.VerifyBuiltinBundle(config.StellaHome()); err != nil {
				return fmt.Errorf("verify builtin skill bundle: %w", err)
			}
			if _, err := fmt.Fprintf(c.App.Writer, "verified builtin skill bundle %s\n", registry.BundleRevision()); err != nil {
				return fmt.Errorf("write builtin skill bundle verification result: %w", err)
			}
			return nil
		},
	}
}
