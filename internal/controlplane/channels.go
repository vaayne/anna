package controlplane

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/CherryHQ/stella/internal/authz"
	"github.com/CherryHQ/stella/internal/platform/config"
)

// ChannelManagement is one channel-management operation. Its Save method owns the
// channel row plus the required channel plugin enable/apply as a single unit, so
// callers never need a second plugin call.
type ChannelManagement struct {
	access *Access
	id     string
}

// ManageChannel opens a channel-management operation. The admin gate already ran
// at Begin, so this only hands back the operation handle; id is retained for
// call-site symmetry (registration flows keep this handle across their platform
// handshake, then call Save after credentials arrive). The id is part of the
// authorization boundary: a caller cannot begin for one row and save another.
func (a *Access) ManageChannel(id string) (*ChannelManagement, error) {
	if a == nil || a.svc == nil {
		return nil, ErrUnavailable
	}
	if id == "" {
		return nil, invalid("channel id is required")
	}
	return &ChannelManagement{access: a, id: id}, nil
}

// ID is the row this operation is authorized to read or write.
func (m *ChannelManagement) ID() string {
	if m == nil {
		return ""
	}
	return m.id
}

// ListChannels returns every configured channel.
// ValidateBinding checks the durable channel-binding invariant before an
// enrollment flow performs an external handshake. Save repeats the check and the
// database unique index closes the concurrent-write race.
func (m *ChannelManagement) ValidateBinding(ctx context.Context, ch config.Channel) error {
	return m.access.svc.validateBinding(ctx, ch)
}

func (s *Service) validateBinding(ctx context.Context, ch config.Channel) error {
	conflict, err := s.channelAgentPlatformBindingConflict(ctx, ch)
	if err != nil {
		return err
	}
	if conflict != "" {
		return invalid(conflict)
	}
	return nil
}

func (a *Access) ListChannels(ctx context.Context) ([]config.Channel, error) {
	return a.svc.store.ListChannels(ctx)
}

// LookupAgent reads an agent for a channel-binding precondition: a registration
// flow binds a channel to an agent that must exist and be enabled. It is an
// admin-gated read (the Access already passed Begin), so a non-admin never
// reaches it; the caller inspects the returned agent's Enabled flag.
func (a *Access) LookupAgent(ctx context.Context, id string) (config.Agent, error) {
	return a.svc.store.GetAgent(ctx, id)
}

// GetChannel returns one channel by id (opaque 404 when missing).
func (a *Access) GetChannel(ctx context.Context, id string) (config.Channel, error) {
	ch, err := a.svc.store.GetChannel(ctx, id)
	if err != nil {
		return config.Channel{}, notFound("channel not found")
	}
	return ch, nil
}

// SaveChannel validates and persists a channel, ensures its plugin is enabled,
// and applies its runtime. create=true rejects an id that already exists (the
// create-only POST contract). It returns the reloaded channel.
func (a *Access) SaveChannel(ctx context.Context, ch config.Channel, cfgMap map[string]any, create bool) (config.Channel, error) {
	operation, err := a.ManageChannel(ch.ID)
	if err != nil {
		return config.Channel{}, err
	}
	return operation.Save(ctx, ch, cfgMap, create)
}

// Channel reads the current row for this operation's channel so a caller can
// merge a partial update onto it. It is an admin-gated read (Begin already ran);
// a missing channel returns an error the caller treats as "start from a fresh
// row" rather than a hard failure.
func (m *ChannelManagement) Channel(ctx context.Context, id string) (config.Channel, error) {
	if m == nil || m.access == nil || id != m.id {
		return config.Channel{}, invalid("channel operation targets a different channel")
	}
	return m.access.svc.store.GetChannel(ctx, id)
}

// Save persists the already-authorized channel and applies its plugin as one
// control-plane operation. It intentionally makes no further authorization call.
func (m *ChannelManagement) Save(ctx context.Context, ch config.Channel, cfgMap map[string]any, create bool) (config.Channel, error) {
	if m == nil || m.access == nil || ch.ID != m.id {
		return config.Channel{}, invalid("channel operation targets a different channel")
	}
	return m.access.svc.saveChannel(ctx, ch, cfgMap, create)
}

// saveChannel is the shared, already-authorized channel write: validate the
// binding invariant and config, persist, enable the plugin, apply the runtime,
// and return the reloaded row. Every authorization gate (the admin Access or the
// per-user ChannelBinding) runs before this and none runs inside it.
func (s *Service) saveChannel(ctx context.Context, ch config.Channel, cfgMap map[string]any, create bool) (config.Channel, error) {
	// POST is create-only and PATCH is update-only. The store uses INSERT/UPDATE,
	// rather than a read-then-upsert, so concurrent creates cannot overwrite an
	// existing channel and a missing PATCH target cannot be created by accident.
	if err := s.validateBinding(ctx, ch); err != nil {
		return config.Channel{}, err
	}
	pluginID := config.PluginID(config.PluginKindChannel, ch.Type)
	if err := s.plugins.ValidateConfig(pluginID, cfgMap); err != nil {
		return config.Channel{}, invalid("invalid request")
	}
	cfgJSON, err := json.Marshal(cfgMap)
	if err != nil {
		return config.Channel{}, invalid("invalid config JSON")
	}
	ch.Config = string(cfgJSON)
	var saveErr error
	if create {
		saveErr = s.store.CreateChannel(ctx, ch)
	} else {
		saveErr = s.store.UpdateChannel(ctx, ch)
	}
	if saveErr != nil {
		var conflict *config.ChannelBindingConflictError
		switch {
		case errors.Is(saveErr, config.ErrChannelExists):
			return config.Channel{}, &ConflictError{Msg: "channel already exists"}
		case errors.Is(saveErr, config.ErrChannelNotFound):
			return config.Channel{}, notFound("channel not found")
		case errors.As(saveErr, &conflict):
			return config.Channel{}, invalid(conflict.Error())
		default:
			return config.Channel{}, saveErr
		}
	}
	if err := s.plugins.ReconcileChannel(ctx, ch.ID); err != nil {
		s.log.Error("failed to apply channel runtime", "channel_id", ch.ID, "channel_type", ch.Type, "error", err)
	}
	saved, err := s.store.GetChannel(ctx, ch.ID)
	if err != nil {
		return config.Channel{}, err
	}
	return saved, nil
}

// ChannelBinding is one authorized channel→agent binding operation.
//
// Channel administration — credentials, naming, enablement, deletion — stays
// admin-only behind Begin. Choosing which agent answers on a channel is a
// per-user action instead, so it gets its own narrow gate here rather than
// widening the admin Access or letting the transport write the row directly. The
// operation only ever moves AgentID: the stored config (channel credentials) is
// carried through untouched, and it reuses the same persist + plugin-apply path
// as an admin save, including the (agent, platform) binding invariant.
type ChannelBinding struct {
	svc *Service
	ch  config.Channel
}

// BeginChannelBinding authorizes rebinding one channel's agent for a signed-in
// caller and loads the target row. Any valid user principal may bind, but a
// non-admin may only touch an enabled channel; a disabled channel is as opaque to
// them as a missing one, so both fail as 404.
//
// The Agent decisions are deliberately not made here: the caller must clear the
// Agent PEP for CurrentAgentID (when set) and for the agent it binds, so the
// "which agents may this user use" rule stays in the one domain that owns it.
func (s *Service) BeginChannelBinding(ctx context.Context, authority authz.Authority, channelID string) (*ChannelBinding, error) {
	if s == nil {
		return nil, ErrUnavailable
	}
	if err := requireCatalogReader(authority); err != nil {
		return nil, err
	}
	ch, err := s.store.GetChannel(ctx, channelID)
	if err != nil {
		return nil, notFound("channel not found")
	}
	if !authority.IsAdmin() && !ch.Enabled {
		return nil, notFound("channel not found")
	}
	return &ChannelBinding{svc: s, ch: ch}, nil
}

// CurrentAgentID is the agent this channel is bound to right now, empty when it
// is unbound. The caller needs it to authorize taking the channel away from its
// current owner.
func (b *ChannelBinding) CurrentAgentID() string { return b.ch.AgentID }

// Bind points the channel at agentID, or unbinds it when agentID is empty, and
// returns the secret-free projection of the saved row. A bound agent must exist
// and be enabled; the caller's authority over that agent was already decided.
func (b *ChannelBinding) Bind(ctx context.Context, agentID string) (PublicChannel, error) {
	if agentID != "" {
		agent, err := b.svc.store.GetAgent(ctx, agentID)
		if err != nil {
			return PublicChannel{}, invalid("agent not found")
		}
		if !agent.Enabled {
			return PublicChannel{}, invalid("agent is disabled")
		}
	}
	ch := b.ch
	// Legacy singleton rows store the type in the id; resolve it so the binding
	// invariant and the plugin lookup both see a real platform.
	ch.Type = effectiveChannelType(ch)
	ch.AgentID = agentID
	cfgMap, err := decodeChannelConfig(ch.Config)
	if err != nil {
		return PublicChannel{}, invalid("invalid config JSON")
	}
	saved, err := b.svc.saveChannel(ctx, ch, cfgMap, false)
	if err != nil {
		return PublicChannel{}, err
	}
	return PublicChannel{
		ID:      saved.ID,
		Type:    effectiveChannelType(saved),
		AgentID: saved.AgentID,
		Enabled: saved.Enabled,
	}, nil
}

// decodeChannelConfig parses a stored channel config JSON blob back into the map
// shape saveChannel round-trips, so a binding write re-persists the existing
// credentials byte-equivalently instead of clearing them.
func decodeChannelConfig(raw string) (map[string]any, error) {
	cfgMap := map[string]any{}
	if raw == "" {
		return cfgMap, nil
	}
	if err := json.Unmarshal([]byte(raw), &cfgMap); err != nil {
		return nil, err
	}
	if cfgMap == nil {
		cfgMap = map[string]any{}
	}
	return cfgMap, nil
}

// DeleteChannel stops a channel's runtime and removes it.
func (a *Access) DeleteChannel(ctx context.Context, id string) error {
	if _, err := a.svc.store.GetChannel(ctx, id); err != nil {
		return notFound("channel not found")
	}
	if err := a.svc.store.DeleteChannel(ctx, id); err != nil {
		return err
	}
	if err := a.svc.plugins.ReconcileChannel(ctx, id); err != nil {
		a.svc.log.Error("failed to stop channel runtime", "channel_id", id, "error", err)
	}
	return nil
}

// ChannelAccess is the channel-management surface the transport talks to. Two
// authorized implementations satisfy it: the admin Access, which reaches every
// channel in the deployment, and AgentChannels, which reaches only the channels
// of the agents its caller manages. The handlers are identical either way — the
// scope decision is made once, when the access is opened.
type ChannelAccess interface {
	ListChannels(ctx context.Context) ([]config.Channel, error)
	GetChannel(ctx context.Context, id string) (config.Channel, error)
	SaveChannel(ctx context.Context, ch config.Channel, cfgMap map[string]any, create bool) (config.Channel, error)
	DeleteChannel(ctx context.Context, id string) error
}

// AgentChannels is channel management confined to one caller's agents.
//
// A channel is an agent's phone number, so the person who owns the agent owns
// its channels: they hold the bot token, they answer for what it says. The
// deployment-wide control plane stays admin-only — this reaches exactly the
// channels bound to agents the caller may manage, and a channel outside that set
// is as opaque as a missing one.
type AgentChannels struct {
	svc      *Service
	agentIDs map[string]bool
}

// BeginAgentChannels opens agent-scoped channel management for the agents the
// caller was already found to manage.
//
// The Agent decision is deliberately not made here: the caller must clear the
// Agent PEP for every id it passes, so the "which agents may this user manage"
// rule stays in the one domain that owns it — the same split BeginChannelBinding
// uses. An empty set is forbidden rather than an empty listing, so a user with
// no agents of their own cannot probe channel ids at all.
func (s *Service) BeginAgentChannels(_ context.Context, authority authz.Authority, managedAgentIDs []string) (*AgentChannels, error) {
	if s == nil {
		return nil, ErrUnavailable
	}
	if err := requireCatalogReader(authority); err != nil {
		return nil, err
	}
	if len(managedAgentIDs) == 0 {
		return nil, authz.ErrForbidden
	}
	ids := make(map[string]bool, len(managedAgentIDs))
	for _, id := range managedAgentIDs {
		if id != "" {
			ids[id] = true
		}
	}
	if len(ids) == 0 {
		return nil, authz.ErrForbidden
	}
	return &AgentChannels{svc: s, agentIDs: ids}, nil
}

// ListChannels returns the channels bound to the caller's agents.
func (a *AgentChannels) ListChannels(ctx context.Context) ([]config.Channel, error) {
	if err := a.svc.requireListenerCap(); err != nil {
		return nil, err
	}
	channels, err := a.svc.store.ListChannels(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]config.Channel, 0, len(channels))
	for _, ch := range channels {
		allowed, err := a.svc.channelListenerAllowed(ctx, ch)
		if err != nil {
			return nil, err
		}
		if a.agentIDs[ch.AgentID] && allowed {
			out = append(out, ch)
		}
	}
	return out, nil
}

// GetChannel returns one of the caller's channels; anything else is a 404.
func (a *AgentChannels) GetChannel(ctx context.Context, id string) (config.Channel, error) {
	if err := a.svc.requireListenerCap(); err != nil {
		return config.Channel{}, err
	}
	ch, err := a.svc.store.GetChannel(ctx, id)
	if err != nil || !a.agentIDs[ch.AgentID] {
		return config.Channel{}, notFound("channel not found")
	}
	allowed, capErr := a.svc.channelListenerAllowed(ctx, ch)
	if capErr != nil || !allowed {
		return config.Channel{}, notFound("channel not found")
	}
	return ch, nil
}

// SaveChannel persists a channel that stays bound to one of the caller's agents.
// Both ends are checked: an update must target a channel they already own, and
// the saved row must still name an agent they manage — so a channel can neither
// be taken from another owner nor handed to an agent the caller cannot manage.
func (a *AgentChannels) SaveChannel(ctx context.Context, ch config.Channel, cfgMap map[string]any, create bool) (config.Channel, error) {
	// Ownership of the target is decided before the candidate is judged, so
	// editing someone else's channel stays a 404 rather than leaking its
	// existence through a validation error.
	if !create {
		if _, err := a.GetChannel(ctx, ch.ID); err != nil {
			return config.Channel{}, err
		}
	}
	if !a.agentIDs[ch.AgentID] {
		return config.Channel{}, invalid("channel must belong to an agent you manage")
	}
	allowed, err := a.svc.channelListenerAllowed(ctx, ch)
	if err != nil {
		return config.Channel{}, err
	}
	if !allowed {
		return config.Channel{}, notFound("channel not found")
	}
	return a.svc.saveChannel(ctx, ch, cfgMap, create)
}

// DeleteChannel removes one of the caller's channels.
func (a *AgentChannels) DeleteChannel(ctx context.Context, id string) error {
	ch, err := a.GetChannel(ctx, id)
	if err != nil {
		return err
	}
	if err := a.svc.store.DeleteChannel(ctx, ch.ID); err != nil {
		return err
	}
	if err := a.svc.plugins.ReconcileChannel(ctx, ch.ID); err != nil {
		a.svc.log.Error("failed to stop channel runtime", "channel_id", ch.ID, "error", err)
	}
	return nil
}

// channelAgentPlatformBindingConflict enforces one bidirectional-channel binding
// per (agent, platform).
// A non-empty string is the client-facing conflict message.
//
// This mirrors the server-side helper that the feishu/weixin registration
// handlers still use; the admin channel CRUD path owns its own copy so those
// out-of-scope ingress handlers are untouched.
func (s *Service) channelAgentPlatformBindingConflict(ctx context.Context, ch config.Channel) (string, error) {
	if ch.AgentID == "" || ch.Type == "" {
		return "", nil
	}
	channels, err := s.store.ListChannels(ctx)
	if err != nil {
		return "", err
	}
	for _, existing := range channels {
		if existing.ID == ch.ID {
			continue
		}
		existingType := existing.Type
		if existingType == "" {
			existingType = existing.ID
		}
		if existingType == ch.Type && existing.AgentID == ch.AgentID {
			return "agent is already bound to " + ch.Type + " channel " + existing.ID, nil
		}
	}
	return "", nil
}
