package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	ucli "github.com/urfave/cli/v2"
	"github.com/vaayne/anna/channel"
	"github.com/vaayne/anna/ai/providers/anthropic"
	"github.com/vaayne/anna/ai/providers/openai"
	openairesponse "github.com/vaayne/anna/ai/providers/openai-response"
	"github.com/vaayne/anna/ai/stream"
)

// CachedModel is the on-disk representation of a model in models.json.
type CachedModel struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

// ModelsCache is the top-level structure for models.json in the workspace.
type ModelsCache struct {
	UpdatedAt time.Time     `json:"updated_at"`
	Models    []CachedModel `json:"models"`
}

func modelsCachePath() string {
	return filepath.Join(cachePath(), "models.json")
}

// LoadModelsCache reads the cached models from the workspace models.json.
func LoadModelsCache() (*ModelsCache, error) {
	data, err := os.ReadFile(modelsCachePath())
	if err != nil {
		return nil, err
	}
	var cache ModelsCache
	if err := json.Unmarshal(data, &cache); err != nil {
		return nil, fmt.Errorf("parse models cache: %w", err)
	}
	return &cache, nil
}

// SaveModelsCache writes the models cache to the cache directory.
func SaveModelsCache(cache *ModelsCache) error {
	path := modelsCachePath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create cache dir: %w", err)
	}
	data, err := json.MarshalIndent(cache, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal models cache: %w", err)
	}
	return os.WriteFile(path, data, 0o644)
}

// fetchModelsFromAPIs queries all configured providers for their model lists.
func fetchModelsFromAPIs(cfg *Config) []CachedModel {
	seen := make(map[string]bool)
	var models []CachedModel

	add := func(provider, model string) {
		key := provider + "/" + model
		if seen[key] {
			return
		}
		seen[key] = true
		models = append(models, CachedModel{Provider: provider, Model: model})
	}

	provNames := make([]string, 0, len(cfg.Providers))
	for name := range cfg.Providers {
		provNames = append(provNames, name)
	}
	sort.Strings(provNames)

	for _, provName := range provNames {
		prov := cfg.Providers[provName]

		// Explicitly listed models from config.
		for _, m := range prov.Models {
			add(provName, m.ID)
		}

		// Fetch from provider API.
		if provider := newStreamProvider(provName, prov); provider != nil {
			if lister, ok := provider.(stream.ModelLister); ok {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				listed, err := lister.ListModels(ctx)
				cancel()
				if err != nil {
					slog.Warn("failed to list models from provider", "provider", provName, "error", err)
					continue
				}
				for _, m := range listed {
					add(provName, m.ID)
				}
			}
		}
	}

	return models
}

// collectModels builds the list of available provider/model pairs.
// It reads from the models cache, falling back to config-only models
// if the cache doesn't exist. Run "anna models update" to populate the cache.
func collectModels(cfg *Config) []channel.ModelOption {
	seen := make(map[string]bool)
	var models []channel.ModelOption

	add := func(provider, model string) {
		key := provider + "/" + model
		if seen[key] {
			return
		}
		seen[key] = true
		models = append(models, channel.ModelOption{Provider: provider, Model: model})
	}

	// Current model first.
	add(cfg.Provider, cfg.Model)

	// Load from cache.
	if cache, err := LoadModelsCache(); err == nil {
		for _, m := range cache.Models {
			add(m.Provider, m.Model)
		}
		return models
	}

	// Fallback: config-only models (no API calls).
	provNames := make([]string, 0, len(cfg.Providers))
	for name := range cfg.Providers {
		provNames = append(provNames, name)
	}
	sort.Strings(provNames)

	for _, provName := range provNames {
		prov := cfg.Providers[provName]
		add(provName, cfg.Model)
		for _, m := range prov.Models {
			add(provName, m.ID)
		}
	}

	return models
}

// newStreamProvider creates a stream.Provider for the given provider name and config.
func newStreamProvider(name string, cfg ProviderConfig) stream.Provider {
	switch name {
	case "anthropic":
		return anthropic.New(anthropic.Config{BaseURL: cfg.BaseURL, APIKey: cfg.APIKey})
	case "openai":
		return openai.New(openai.Config{BaseURL: cfg.BaseURL, APIKey: cfg.APIKey})
	case "openai-response":
		return openairesponse.New(openairesponse.Config{BaseURL: cfg.BaseURL, APIKey: cfg.APIKey})
	default:
		return nil
	}
}

func modelsCommand() *ucli.Command {
	return &ucli.Command{
		Name:  "models",
		Usage: "Manage available models",
		Subcommands: []*ucli.Command{
			modelsListCommand(),
			modelsUpdateCommand(),
			modelsCurrentCommand(),
			modelsSetCommand(),
			modelsSearchCommand(),
		},
		Action: modelsListAction,
	}
}

func modelsListCommand() *ucli.Command {
	return &ucli.Command{
		Name:   "list",
		Usage:  "List all available models grouped by provider",
		Action: modelsListAction,
	}
}

func modelsListAction(c *ucli.Context) error {
	cfg, err := LoadConfig()
	if err != nil {
		return err
	}

	models := collectModels(cfg)
	printModelsGrouped(models, cfg.Provider, cfg.Model)
	return nil
}

func modelsUpdateCommand() *ucli.Command {
	return &ucli.Command{
		Name:  "update",
		Usage: "Fetch models from all provider APIs and update the cache",
		Action: func(c *ucli.Context) error {
			cfg, err := LoadConfig()
			if err != nil {
				return err
			}

			fmt.Fprintf(os.Stderr, "Fetching models from %d provider(s)...\n", len(cfg.Providers))

			cached := fetchModelsFromAPIs(cfg)
			cache := &ModelsCache{
				UpdatedAt: time.Now().UTC(),
				Models:    cached,
			}

			if err := SaveModelsCache(cache); err != nil {
				return fmt.Errorf("save models cache: %w", err)
			}

			fmt.Fprintf(os.Stderr, "Cached %d models to %s\n", len(cached), cfg.ModelsPath())
			return nil
		},
	}
}

func modelsCurrentCommand() *ucli.Command {
	return &ucli.Command{
		Name:  "current",
		Usage: "Show the active provider and model",
		Action: func(c *ucli.Context) error {
			cfg, err := LoadConfig()
			if err != nil {
				return err
			}
			fmt.Printf("%s/%s\n", cfg.Provider, cfg.Model)
			return nil
		},
	}
}

func modelsSetCommand() *ucli.Command {
	return &ucli.Command{
		Name:      "set",
		Usage:     "Switch the active model (e.g. anna models set openai/gpt-4o)",
		ArgsUsage: "<provider/model>",
		Action: func(c *ucli.Context) error {
			arg := c.Args().First()
			if arg == "" {
				return fmt.Errorf("usage: anna models set <provider/model>")
			}

			provider, model, ok := strings.Cut(arg, "/")
			if !ok || provider == "" || model == "" {
				return fmt.Errorf("invalid format %q, expected provider/model", arg)
			}

			cfg, err := LoadConfig()
			if err != nil {
				return err
			}
			if err := SaveModelSelection(cfg.Workspace, provider, model); err != nil {
				return err
			}
			fmt.Printf("Switched to %s/%s\n", provider, model)
			return nil
		},
	}
}

func modelsSearchCommand() *ucli.Command {
	return &ucli.Command{
		Name:      "search",
		Usage:     "Search models by name (e.g. anna models search gpt)",
		ArgsUsage: "<query>",
		Action: func(c *ucli.Context) error {
			query := strings.ToLower(c.Args().First())
			if query == "" {
				return fmt.Errorf("usage: anna models search <query>")
			}

			cfg, err := LoadConfig()
			if err != nil {
				return err
			}

			models := collectModels(cfg)
			var matched []channel.ModelOption
			for _, m := range models {
				label := strings.ToLower(m.Provider + "/" + m.Model)
				if strings.Contains(label, query) {
					matched = append(matched, m)
				}
			}

			if len(matched) == 0 {
				fmt.Fprintf(os.Stderr, "No models matching %q\n", query)
				return nil
			}

			printModelsGrouped(matched, cfg.Provider, cfg.Model)
			return nil
		},
	}
}

// printModelsGrouped prints models grouped by provider, marking the active one.
func printModelsGrouped(models []channel.ModelOption, activeProvider, activeModel string) {
	grouped := make(map[string][]string)
	var provOrder []string
	seen := make(map[string]bool)

	for _, m := range models {
		if !seen[m.Provider] {
			seen[m.Provider] = true
			provOrder = append(provOrder, m.Provider)
		}
		grouped[m.Provider] = append(grouped[m.Provider], m.Model)
	}

	for _, prov := range provOrder {
		fmt.Printf("%s:\n", prov)
		for _, model := range grouped[prov] {
			marker := "  "
			suffix := ""
			if prov == activeProvider && model == activeModel {
				marker = "* "
				suffix = " (current)"
			}
			fmt.Printf("  %s%s%s\n", marker, model, suffix)
		}
	}
}
