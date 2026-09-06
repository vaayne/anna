package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/CherryHQ/stella/internal/authz"
	appdb "github.com/CherryHQ/stella/internal/db"
	"github.com/CherryHQ/stella/internal/platform/diagnostic"
	"github.com/CherryHQ/stella/internal/plugin"
	"github.com/CherryHQ/stella/pkg/tools"
)

// defaultDiscoveryConcurrency caps cold tools/list discovery. A bad
// system-wide MCP fleet should degrade by skipping servers, not by serially
// stalling runner creation for N*timeout.
const defaultDiscoveryConcurrency = 4

// defaultDiscoveryTimeout bounds the whole cold-discovery pass at session
// start; a persisted catalog needs no connection and is never subject to it.
const defaultDiscoveryTimeout = 20 * time.Second

// catalogMaxAge bounds how stale a persisted tool catalog may be before the
// next session start re-probes the server in the background of building tools.
const catalogMaxAge = 24 * time.Hour

// ToolProvider surfaces the tools of every MCP server visible to a (user,
// agent) context into the agent tool registry, proxying tools/call back to the
// server. It builds proxies from the persisted tool catalog (populated by
// Probe) so session startup does not have to connect; a stale or empty catalog
// triggers a cold discovery whose result is written back through the service.
// A down or misbehaving server is logged and skipped so it can never break an
// agent session.
type ToolProvider struct {
	svc         *Service
	log         *slog.Logger
	concurrency int
}

// NewToolProvider builds a provider over the registration service.
func NewToolProvider(svc *Service) *ToolProvider {
	return &ToolProvider{
		svc:         svc,
		log:         slog.With("component", "mcp"),
		concurrency: defaultDiscoveryConcurrency,
	}
}

// ToolsForSnapshot builds MCP tools from an already resolved plugin snapshot.
// The snapshot is the single source of truth for definition/config winners;
// this method never re-queries plugin configuration or falls back to a
// shadowed namespace. It reads observations internally for this snapshot's
// trusted authority, so callers never supply an arbitrary owner or cache.
func (p *ToolProvider) ToolsForSnapshot(ctx context.Context, snapshot plugin.Snapshot) ([]tools.Tool, error) {
	authority := snapshot.Authority()
	if !authority.Valid() {
		return nil, authz.ErrForbidden
	}
	if p == nil || p.svc == nil {
		return nil, nil
	}
	registrations, err := p.svc.RegistrationsForSnapshot(ctx, snapshot)
	if err != nil {
		return nil, err
	}
	return p.toolsForRegistrations(ctx, registrations, true, string(authority.UserID())), nil
}

// observationsForSnapshot reads only config IDs visible to this snapshot and
// selects the exact shared or trusted per-user owner. It deliberately leaves
// legacy per-user cache rows dormant because their provenance is unknown.
func (s *Service) observationsForSnapshot(ctx context.Context, snapshot plugin.Snapshot, authority authz.Authority) (map[string]PluginMCPObservation, error) {
	if s == nil || s.pool == nil {
		return nil, errPluginCredentialsUnavailable
	}
	ids := make([]string, 0)
	modes := make(map[string]string)
	seen := make(map[string]struct{})
	for _, def := range snapshot.Definitions() {
		if def.Backend != plugin.BackendMCP {
			continue
		}
		effective, err := snapshot.Resolve(def.ID)
		if err != nil || !effective.IsEffectivelyEnabled || effective.ConfigID == "" || len(effective.Payload) == 0 {
			continue
		}
		payload, err := decodeMCPPluginPayload(effective.Payload)
		if err != nil {
			return nil, fmt.Errorf("decode MCP config %q for observation lookup: %w", effective.ConfigID, err)
		}
		if _, ok := seen[effective.ConfigID]; !ok {
			seen[effective.ConfigID] = struct{}{}
			ids = append(ids, effective.ConfigID)
		}
		modes[effective.ConfigID] = payload.CredentialMode
	}
	var userID *string
	if authority.Kind() == authz.ActorUser || authority.Kind() == authz.ActorAgent {
		value := string(authority.UserID())
		if value != "" {
			userID = &value
		}
	}
	states, err := appdb.ListMCPConnectionStatesForConfigs(ctx, s.pool, ids, userID)
	if err != nil {
		return nil, err
	}
	out := make(map[string]PluginMCPObservation, len(states))
	for _, state := range states {
		mode := modes[state.ConfigID]
		if mode == CredentialModePerUser {
			if userID == nil || state.CredentialUserID == nil || *state.CredentialUserID != *userID {
				continue
			}
		} else if state.CredentialUserID != nil {
			continue
		}
		var tools []CatalogTool
		if len(state.Tools) != 0 {
			if err := json.Unmarshal(state.Tools, &tools); err != nil {
				return nil, fmt.Errorf("decode MCP observation %q: %w", state.ConfigID, err)
			}
		}
		observation := PluginMCPObservation{
			Status: state.Status, StatusError: state.StatusError,
			ConfigRevision: state.ConfigRevision, Tools: tools,
		}
		if state.ProbedAt != nil {
			observation.ProbedAt = state.ProbedAt.UTC()
		}
		if state.CredentialUserID != nil {
			observation.CredentialUserID = *state.CredentialUserID
		}
		out[state.ConfigID] = observation
	}
	return out, nil
}

func mcpRegistrationsFromSnapshot(snapshot plugin.Snapshot, observations map[string]PluginMCPObservation, authority authz.Authority) ([]Registration, error) {
	defs := snapshot.Definitions()
	namespaces := make(map[string]struct{}, len(defs))
	for _, def := range defs {
		namespaces[def.Namespace] = struct{}{}
	}
	orderedNamespaces := slices.Sorted(maps.Keys(namespaces))

	registrations := make([]Registration, 0, len(orderedNamespaces))
	for _, namespace := range orderedNamespaces {
		effective, err := snapshot.ResolveNamespace(namespace)
		if errors.Is(err, plugin.ErrNotFound) {
			// A visible negative-only definition does not claim its namespace.
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("resolve MCP namespace %q: %w", namespace, err)
		}
		if effective.PluginID == "" {
			return nil, fmt.Errorf("resolve MCP namespace %q returned no plugin", namespace)
		}
		resolved, ok := snapshot.Get(effective.PluginID)
		if !ok {
			return nil, fmt.Errorf("resolve MCP namespace %q selected missing plugin %q", namespace, effective.PluginID)
		}
		if resolved.Definition.Namespace != namespace || resolved.Effective.ConfigID != effective.ConfigID {
			return nil, fmt.Errorf("resolve MCP namespace %q returned inconsistent winner", namespace)
		}
		if !effective.IsEffectivelyEnabled || resolved.Definition.Backend != plugin.BackendMCP || resolved.Config == nil || len(resolved.Config.Payload) == 0 {
			// A different backend owns this namespace, or a negative/no-payload
			// record won. Neither case may fall through to another definition.
			continue
		}
		observation := observations[resolved.Config.ID]
		registration, err := RegistrationFromPluginConfig(resolved.Definition, *resolved.Config, resolved.Effective, observation, authority)
		if err != nil {
			return nil, fmt.Errorf("convert MCP config %q: %w", resolved.Config.ID, err)
		}
		exportedNames := make(map[string]struct{}, len(registration.Tools))
		for _, catalogTool := range registration.Tools {
			name, err := plugin.ExportedToolName(registration.Namespace, SanitizeIdent(catalogTool.Name, "tool"))
			if err != nil {
				return nil, fmt.Errorf("convert MCP config %q tool %q: %w", resolved.Config.ID, catalogTool.Name, err)
			}
			if _, duplicate := exportedNames[name]; duplicate {
				return nil, fmt.Errorf("MCP config %q has duplicate exported tool name %q", resolved.Config.ID, name)
			}
			exportedNames[name] = struct{}{}
		}
		registrations = append(registrations, registration)
	}
	return registrations, nil
}

func (p *ToolProvider) toolsForRegistrations(ctx context.Context, regs []Registration, allowDiscovery bool, userID string) []tools.Tool {
	type result struct {
		index int
		tools []tools.Tool
	}
	limit := p.concurrency
	if limit <= 0 {
		limit = defaultDiscoveryConcurrency
	}
	discoveryCtx, cancel := context.WithTimeout(ctx, defaultDiscoveryTimeout)
	defer cancel()
	sem := make(chan struct{}, limit)
	results := make(chan result, len(regs))
	var wg sync.WaitGroup
	for i, reg := range regs {
		if !reg.Enabled {
			continue
		}
		owner := p.svc.CredentialOwner(reg, userID)
		if reg.Status == StatusNeedsAuth || !p.svc.HasUserCredential(ctx, reg, userID) {
			// Skip without connecting: needs_auth means the last credential was
			// rejected; a per_user registration without this user's bundle has
			// nothing to authenticate with. Only a reconnect from the Web UI
			// fixes either.
			continue
		}
		if catalog, ok := freshCatalog(reg); ok {
			if err := validateCatalogTools(reg, catalog); err != nil {
				p.log.Warn("mcp cached catalog is invalid; skipping server", "server", reg.Name, "error", err)
				continue
			}
			results <- result{index: i, tools: p.catalogProxies(reg, catalog, owner)}
			continue
		}
		if !allowDiscovery {
			continue
		}
		wg.Go(func() {
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-discoveryCtx.Done():
				return
			}
			results <- result{index: i, tools: p.discover(discoveryCtx, reg, owner)}
		})
	}
	go func() {
		wg.Wait()
		close(results)
	}()

	// Collect by registration index so name collisions resolve by scope
	// precedence (regs is ordered most-specific-first), not by arrival order.
	byIndex := make([][]tools.Tool, len(regs))
	for res := range results {
		byIndex[res.index] = res.tools
	}
	seen := map[string]struct{}{}
	var out []tools.Tool
	for _, serverTools := range byIndex {
		for _, tool := range serverTools {
			name := tool.Definition().Name
			if _, ok := seen[name]; ok {
				p.log.Warn("mcp tool name collision; skipping duplicate", "tool", name)
				continue
			}
			seen[name] = struct{}{}
			out = append(out, tool)
		}
	}
	return out
}

// freshCatalog reports whether the registration carries a persisted catalog
// good enough to build proxies without connecting.
func freshCatalog(reg Registration) ([]CatalogTool, bool) {
	if reg.Status != StatusOK || len(reg.Tools) == 0 {
		return nil, false
	}
	if reg.ProbedAt.IsZero() || time.Since(reg.ProbedAt) > catalogMaxAge {
		return nil, false
	}
	return reg.Tools, true
}

// discover cold-probes one server via the service, which persists both success
// and failure, then returns proxies from the refreshed catalog.
func (p *ToolProvider) discover(ctx context.Context, reg Registration, owner CredentialOwner) []tools.Tool {
	updated, err := p.svc.Probe(ctx, reg, owner)
	if err != nil {
		p.log.Warn("mcp cold discovery failed; skipping server", "server", reg.Name, "url", diagnostic.Endpoint(reg.URL), "error", err)
		return nil
	}
	if updated.Status != StatusOK {
		p.log.Warn("mcp probe failed; skipping server", "server", reg.Name, "url", diagnostic.Endpoint(reg.URL), "status", updated.Status, "reason", updated.StatusError)
		return nil
	}
	catalog, ok := freshCatalog(updated)
	if !ok {
		// ok with an empty catalog: the server advertised no tools.
		return nil
	}
	return p.catalogProxies(updated, catalog, owner)
}

func (p *ToolProvider) catalogProxies(reg Registration, catalog []CatalogTool, owner CredentialOwner) []tools.Tool {
	out := make([]tools.Tool, 0, len(catalog))
	conn := &serverConn{svc: p.svc, reg: reg, owner: owner}
	for _, ct := range catalog {
		name := exportedToolName(reg, ct.Name)
		if name == "" {
			// A registration without a trusted plugin namespace is legacy state.
			// It cannot enter the model-facing registry under a guessed name.
			p.log.Warn("mcp catalog has no exported namespace; skipping tool", "server", reg.Name, "tool", ct.Name)
			continue
		}
		out = append(out, &toolProxy{
			svc:        p.svc,
			reg:        reg,
			conn:       conn,
			remoteName: ct.Name,
			def: tools.Definition{
				Name:        name,
				Description: ct.Description,
				InputSchema: cloneSchema(ct.InputSchema),
			},
		})
	}
	return out
}

func exportedToolName(reg Registration, remoteName string) string {
	name, _ := plugin.ExportedToolName(reg.Namespace, SanitizeIdent(remoteName, "tool"))
	return name
}

func cloneSchema(in map[string]any) map[string]any {
	if len(in) == 0 {
		return map[string]any{"type": "object"}
	}
	raw, err := json.Marshal(in)
	if err != nil {
		return map[string]any{"type": "object"}
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil || out == nil {
		return map[string]any{"type": "object"}
	}
	return out
}
