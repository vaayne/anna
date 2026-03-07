package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	ucli "github.com/urfave/cli/v2"
	"github.com/vaayne/anna/agent/runner"
	"github.com/vaayne/anna/skills"
)

func skillsCommand() *ucli.Command {
	return &ucli.Command{
		Name:  "skills",
		Usage: "Manage agent skills",
		Subcommands: []*ucli.Command{
			skillsSearchCommand(),
			skillsInstallCommand(),
			skillsListCommand(),
			skillsRemoveCommand(),
		},
		Action: func(c *ucli.Context) error {
			// Default: list
			return skillsListAction()
		},
	}
}

func skillsSearchCommand() *ucli.Command {
	return &ucli.Command{
		Name:      "search",
		Usage:     "Search for skills (e.g. anna skills search react)",
		ArgsUsage: "<query>",
		Flags: []ucli.Flag{
			&ucli.IntFlag{
				Name:  "limit",
				Usage: "Max results to return",
				Value: 10,
			},
		},
		Action: func(c *ucli.Context) error {
			query := c.Args().First()
			if query == "" {
				return fmt.Errorf("usage: anna skills search <query>")
			}

			ctx, cancel := context.WithTimeout(c.Context, 10*time.Second)
			defer cancel()

			results, err := skills.Search(ctx, query, c.Int("limit"))
			if err != nil {
				return err
			}

			if len(results) == 0 {
				fmt.Println("No skills found.")
				return nil
			}

			fmt.Printf("Found %d skills:\n\n", len(results))
			for _, s := range results {
				fmt.Printf("  %s@%s\n", s.Source, s.SkillID)
				fmt.Printf("    %s (%d installs)\n", s.Name, s.Installs)
				fmt.Println()
			}
			fmt.Println("Install with: anna skills install <owner/repo@skill-name>")
			return nil
		},
	}
}

func skillsInstallCommand() *ucli.Command {
	return &ucli.Command{
		Name:      "install",
		Aliases:   []string{"add"},
		Usage:     "Install a skill (e.g. anna skills install owner/repo@skill-name)",
		ArgsUsage: "<source>",
		Action: func(c *ucli.Context) error {
			source := c.Args().First()
			if source == "" {
				return fmt.Errorf("usage: anna skills install <owner/repo@skill-name>")
			}

			cfg, err := LoadConfig()
			if err != nil {
				return err
			}
			targetDir := cfg.SkillsPath()

			fmt.Fprintf(os.Stderr, "Installing from %s...\n", source)

			name, err := skills.Install(c.Context, source, targetDir)
			if err != nil {
				return err
			}

			fmt.Printf("Skill %q installed to %s\n", name, filepath.Join(targetDir, name))
			return nil
		},
	}
}

func skillsListCommand() *ucli.Command {
	return &ucli.Command{
		Name:  "list",
		Usage: "List installed skills",
		Flags: []ucli.Flag{
			&ucli.BoolFlag{
				Name:  "json",
				Usage: "Output as JSON",
			},
		},
		Action: func(c *ucli.Context) error {
			if c.Bool("json") {
				return skillsListJSON()
			}
			return skillsListAction()
		},
	}
}

func skillsListAction() error {
	cfg, err := LoadConfig()
	if err != nil {
		return err
	}
	cwd, _ := os.Getwd()
	loaded := runner.LoadSkills(cfg.Workspace, cwd)
	if len(loaded) == 0 {
		fmt.Println("No skills installed.")
		return nil
	}

	// Group by source
	grouped := map[string][]runner.Skill{}
	var sourceOrder []string
	seen := map[string]bool{}
	for _, s := range loaded {
		if !seen[s.Source] {
			seen[s.Source] = true
			sourceOrder = append(sourceOrder, s.Source)
		}
		grouped[s.Source] = append(grouped[s.Source], s)
	}

	for _, src := range sourceOrder {
		fmt.Printf("%s:\n", src)
		for _, s := range grouped[src] {
			desc := s.Description
			if len(desc) > 80 {
				desc = desc[:77] + "..."
			}
			desc = strings.ReplaceAll(desc, "\n", " ")
			fmt.Printf("  %-25s %s\n", s.Name, desc)
		}
		fmt.Println()
	}
	return nil
}

func skillsListJSON() error {
	cfg, err := LoadConfig()
	if err != nil {
		return err
	}
	cwd, _ := os.Getwd()
	loaded := runner.LoadSkills(cfg.Workspace, cwd)

	type entry struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Source      string `json:"source"`
		Path        string `json:"path"`
	}

	entries := make([]entry, len(loaded))
	for i, s := range loaded {
		entries[i] = entry{
			Name:        s.Name,
			Description: s.Description,
			Source:      s.Source,
			Path:        s.FilePath,
		}
	}

	out, _ := json.MarshalIndent(entries, "", "  ")
	fmt.Println(string(out))
	return nil
}

func skillsRemoveCommand() *ucli.Command {
	return &ucli.Command{
		Name:      "remove",
		Aliases:   []string{"rm"},
		Usage:     "Remove an installed skill (e.g. anna skills remove my-skill)",
		ArgsUsage: "<name>",
		Action: func(c *ucli.Context) error {
			name := c.Args().First()
			if name == "" {
				return fmt.Errorf("usage: anna skills remove <name>")
			}

			cfg, err := LoadConfig()
			if err != nil {
				return err
			}
			skillDir := filepath.Join(cfg.SkillsPath(), name)

			if err := skills.Remove(name, skillDir); err != nil {
				return err
			}

			fmt.Printf("Skill %q removed.\n", name)
			return nil
		},
	}
}
