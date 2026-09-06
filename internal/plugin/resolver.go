package plugin

import "fmt"

// Resolve applies the common winner-first rule for a trusted user/agent tuple.
// A selected false or unavailable record never falls back to a broader record.
func Resolve(def Definition, configs []Config, userID, agentID string) (Effective, error) {
	if err := def.Validate(); err != nil {
		return Effective{}, err
	}
	byScope := make(map[Scope]Config, len(configs))
	for _, config := range configs {
		if config.PluginID != def.ID || config.Namespace != def.Namespace {
			return Effective{}, fmt.Errorf("%w: definition mismatch", ErrInvalidConfig)
		}
		if err := config.Validate(); err != nil {
			return Effective{}, err
		}
		// A catalog read can contain every user's row. Only an exact owner tuple
		// is eligible for this trusted context; unrelated rows must not collide
		// with the selected scope or disclose their state.
		if !matchesContext(config, userID, agentID) {
			continue
		}
		if _, exists := byScope[config.Scope]; exists {
			return Effective{}, fmt.Errorf("%w: duplicate scope %q", ErrInvalidConfig, config.Scope)
		}
		byScope[config.Scope] = cloneConfig(config)
	}

	// These two records are upper bounds, so they are checked before the
	// more-specific records. A matching false can never be bypassed by a true
	// user or user_agent record.
	for _, scope := range []Scope{ScopeSystem, ScopeSystemAgent} {
		config, ok := byScope[scope]
		if ok && matchesContext(config, userID, agentID) && config.Enabled != nil && !*config.Enabled {
			return effectiveFrom(def, config), nil
		}
	}
	for _, scope := range scopePrecedence {
		config, ok := byScope[scope]
		if ok && matchesContext(config, userID, agentID) {
			return effectiveFrom(def, config), nil
		}
	}
	return Effective{
		PluginID:             def.ID,
		Namespace:            def.Namespace,
		IsEffectivelyEnabled: def.DefaultEnabled,
		AvailabilityReason:   "shipped_default",
		Payload:              cloneRaw(def.Spec),
	}, nil
}

// ResolveNamespace first selects the highest-precedence payload-bearing
// definition that owns a namespace for this context, then resolves that
// definition's caps. Negative records never claim a namespace; a disabled
// winning payload-bearing candidate still terminates resolution.
func ResolveNamespace(catalog *Catalog, configs []Config, namespace, userID, agentID string) (Effective, error) {
	if catalog == nil {
		return Effective{}, ErrInvalidDefinition
	}
	defs := make([]Definition, 0)
	for _, def := range catalog.Definitions() {
		if def.Namespace == namespace {
			defs = append(defs, def)
		}
	}
	if len(defs) == 0 {
		return Effective{}, ErrInvalidDefinition
	}
	var winner Config
	var winnerDef Definition
	winnerRank := len(scopePrecedence)
	for _, config := range configs {
		if config.Namespace != namespace || len(config.Payload) == 0 || !matchesContext(config, userID, agentID) {
			continue
		}
		var def Definition
		found := false
		for _, candidate := range defs {
			if candidate.ID == config.PluginID {
				def, found = candidate, true
				break
			}
		}
		if !found {
			return Effective{}, fmt.Errorf("%w: config %q has unknown definition", ErrInvalidConfig, config.ID)
		}
		if err := config.Validate(); err != nil {
			return Effective{}, err
		}
		rank := scopeRank(config.Scope)
		if rank > winnerRank {
			continue
		}
		if rank == winnerRank && winner.ID != "" && winner.PluginID != config.PluginID {
			return Effective{}, fmt.Errorf("%w: namespace %q has same-scope owners", ErrInvalidDefinition, namespace)
		}
		winner, winnerDef, winnerRank = cloneConfig(config), def, rank
	}
	if winner.ID == "" {
		if len(defs) == 1 {
			return Resolve(defs[0], configsFor(defs[0], configs), userID, agentID)
		}
		return Effective{}, fmt.Errorf("%w: namespace %q has no unambiguous owner", ErrInvalidDefinition, namespace)
	}
	return Resolve(winnerDef, configsFor(winnerDef, configs), userID, agentID)
}

func configsFor(def Definition, configs []Config) []Config {
	owned := make([]Config, 0)
	for _, config := range configs {
		if config.PluginID == def.ID && config.Namespace == def.Namespace {
			owned = append(owned, config)
		}
	}
	return owned
}

func scopeRank(scope Scope) int {
	for rank, candidate := range scopePrecedence {
		if scope == candidate {
			return rank
		}
	}
	return len(scopePrecedence)
}

func effectiveFrom(def Definition, config Config) Effective {
	enabled := def.DefaultEnabled
	if config.Enabled != nil {
		enabled = *config.Enabled
	}
	reason := "scope_enabled"
	if !enabled {
		reason = "scope_disabled"
	}
	payload := cloneRaw(config.Payload)
	if len(config.Payload) != 0 {
		if resolved, err := mergeObjects(def.Spec, config.Payload); err == nil {
			payload = resolved
		}
	}
	return Effective{
		PluginID:             def.ID,
		Namespace:            def.Namespace,
		ConfigID:             config.ID,
		SourceScope:          config.Scope,
		IsEffectivelyEnabled: enabled,
		AvailabilityReason:   reason,
		Payload:              payload,
	}
}

func matchesContext(config Config, userID, agentID string) bool {
	switch config.Scope {
	case ScopeSystem:
		return config.UserID == "" && config.AgentID == ""
	case ScopeSystemAgent:
		return agentID != "" && config.UserID == "" && config.AgentID == agentID
	case ScopeUser:
		return userID != "" && config.UserID == userID && config.AgentID == ""
	case ScopeUserAgent:
		return userID != "" && agentID != "" && config.UserID == userID && config.AgentID == agentID
	default:
		return false
	}
}

// IsDenied reports whether a resolved plugin is explicitly unavailable.
func (e Effective) IsDenied() bool { return !e.IsEffectivelyEnabled }
