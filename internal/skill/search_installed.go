package skill

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/CherryHQ/stella/internal/searchrank"
	pkgplugins "github.com/CherryHQ/stella/pkg/plugins"
)

type installedSkillSearchResult struct {
	Name          string   `json:"name"`
	Description   string   `json:"description"`
	Status        string   `json:"status"`
	Scope         string   `json:"scope"`
	MatchedFields []string `json:"matched_fields"`
	Snippet       string   `json:"snippet"`
	Score         float64  `json:"score"`
}

// Search ranks the skills installed and visible to this agent against a task
// query. It returns names and descriptions only; content comes from skill_load.
func (t *Tool) Search(ctx context.Context, in SkillSearchInput) (any, error) {
	query := in.Q
	if strings.TrimSpace(query) == "" {
		return nil, fmt.Errorf("q must not be empty")
	}
	limit := installedSkillSearchLimit(in.Limit)

	merged, err := t.identityMerged(ctx, t.viewContext(ctx))
	if err != nil {
		return nil, fmt.Errorf("search installed skills: %w", err)
	}
	merged, err = t.hydrateAuthorized(ctx, merged)
	if err != nil {
		return nil, fmt.Errorf("search installed skills: %w", err)
	}

	skills := t.visibleSearchableSkills(merged)
	if len(skills) == 0 {
		return noInstalledSkills, nil
	}

	docs := make([]searchrank.Document, 0, len(skills))
	byName := make(map[string]Skill, len(skills))
	for _, skill := range skills {
		byName[skill.Name] = skill
		docs = append(docs, searchrank.Document{
			ID: skill.Name,
			Fields: []searchrank.Field{
				{Name: "description", Text: skill.Description, Weight: 3},
				{Name: "name", Text: skill.Name, Weight: 1.5},
			},
		})
	}

	ranked := searchrank.Rank(query, docs, len(docs))
	if len(ranked) == 0 {
		return noInstalledSkills, nil
	}
	boostSkillNameMatches(query, ranked)
	if len(ranked) > limit {
		ranked = ranked[:limit]
	}

	results := make([]installedSkillSearchResult, 0, len(ranked))
	for _, hit := range ranked {
		skill := byName[hit.ID]
		results = append(results, installedSkillSearchResult{
			Name:          skill.Name,
			Description:   skill.Description,
			Status:        skill.Status,
			Scope:         skill.Scope,
			MatchedFields: hit.MatchedFields,
			Snippet:       hit.Snippet,
			Score:         hit.Score,
		})
	}

	out, _ := json.MarshalIndent(results, "", "  ")
	return string(out), nil
}

// noInstalledSkills is prose, not an empty list: an empty result set here means
// this agent can see no skill at all, which is worth saying plainly.
const noInstalledSkills = "No installed skills found."

func (t *Tool) visibleSearchableSkills(merged []ResolvedSkill) []Skill {
	visible := filterVisibleResolvedSkills(merged, pkgplugins.SystemPromptContext{
		RegisteredPluginIDs: t.registeredPluginIDs,
		EnabledPluginIDs:    t.enabledPluginIDs,
	})
	all := make([]Skill, 0, len(visible))
	for _, rs := range visible {
		all = append(all, rs.Skill)
	}

	out := make([]Skill, 0, len(all))
	for _, skill := range all {
		if skill.Status == SkillStatusDeprecated || skill.DisableModelInvocation {
			continue
		}
		out = append(out, skill)
	}
	return out
}

// installedSkillSearchLimit clamps to the same maximum the schema declares, so
// a limit the model could not have sent still cannot widen the result set.
func installedSkillSearchLimit(requested int) int {
	const (
		defaultLimit = 10
		maxLimit     = 100
	)
	if requested <= 0 {
		return defaultLimit
	}
	if requested > maxLimit {
		return maxLimit
	}
	return requested
}

func boostSkillNameMatches(query string, hits []searchrank.Result) {
	q := strings.ToLower(strings.TrimSpace(query))
	for i := range hits {
		name := strings.ToLower(hits[i].ID)
		switch {
		case name == q:
			hits[i].Score += 10
		case strings.Contains(name, q) || strings.Contains(q, name):
			hits[i].Score += 1
		}
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].Score == hits[j].Score {
			return hits[i].ID < hits[j].ID
		}
		return hits[i].Score > hits[j].Score
	})
}
