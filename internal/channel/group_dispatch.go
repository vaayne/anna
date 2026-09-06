package channel

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/google/uuid"

	"github.com/CherryHQ/stella/internal/agent"
	"github.com/CherryHQ/stella/internal/agent/session"
	agentaccess "github.com/CherryHQ/stella/internal/core/access"
	"github.com/CherryHQ/stella/internal/eventlog"
	"github.com/CherryHQ/stella/internal/memory"
	"github.com/CherryHQ/stella/internal/sessionmedia"
	"github.com/CherryHQ/stella/pkg/ai"
	pkgchannel "github.com/CherryHQ/stella/pkg/channel"
	"github.com/CherryHQ/stella/pkg/db/sqlc"
)

// GroupMember represents a bot's membership in a group chat.
type GroupMember struct {
	AgentID        string
	ReplyChannelID string
}

// handleGroupIncoming ingests a group message and wakes the durable dispatcher.
func (c *Coordinator) handleGroupIncoming(ctx context.Context, msg pkgchannel.IncomingMessage, command, args string) (string, bool, *pkgchannel.ChatStream, error) {
	log := slog.With("component", "group_dispatch", "platform", msg.Platform, "chat_id", msg.ChatID)

	// CR-007: /config may contain secrets — block it in groups before event log write.
	if strings.EqualFold(command, "/config") {
		return "⚠️ /config is not available in group chats. Please use it in a direct message.", true, nil, nil
	}
	// A group's context is shared by every member, so `/new` cannot reset it.
	// Answer before the event-log append so the refused command does not become
	// group context either — it is an instruction to Stella, not something the
	// group said.
	if strings.EqualFold(command, "/new") {
		return pkgchannel.GroupNewSessionUnsupportedMessage, true, nil, nil
	}
	allowed, err := c.channelListenerAllowed(ctx, msg.Platform, msg.ChannelID)
	if err != nil {
		return "", false, nil, fmt.Errorf("group channel admission: %w", err)
	}
	if !allowed {
		return "", false, nil, errChannelPluginDisabled
	}

	result, err := c.appendGroupMessage(ctx, msg)
	if err != nil {
		return "", false, nil, fmt.Errorf("group event log: %w", err)
	}
	if !result.Inserted {
		log.Debug("group message deduplicated, skipping", "seq", result.Seq)
		return "", false, nil, nil
	}

	log.With("group_id", result.GroupID, "seq", result.Seq).Debug("group message appended")
	if c.groupDispatcher != nil {
		c.groupDispatcher.Wake()
	}
	return "", false, nil, nil
}

// appendGroupMessage writes the incoming message to the event log.
func (c *Coordinator) appendGroupMessage(ctx context.Context, msg pkgchannel.IncomingMessage) (eventlog.AppendResult, error) {
	// Identity first: the stored text names Stella agents, and the wake fan-out
	// reads the same resolved mentions out of the outbox envelope.
	c.resolveMentionAgents(ctx, msg.Platform, msg.Mentions)
	msg.Content = c.rewriteMentionsToAgentNames(ctx, msg.Mentions, msg.Content)
	event, err := c.groupEventMessage(ctx, msg)
	if err != nil {
		return eventlog.AppendResult{}, err
	}
	return c.eventLog.AppendGroupMessage(ctx, event, eventlog.WithOnInserted(func(ctx context.Context, q *sqlc.Queries, result eventlog.AppendResult) error {
		members, err := q.ListGroupMembers(ctx, result.GroupID)
		if err != nil {
			return fmt.Errorf("list group members: %w", err)
		}
		groupMembers := make([]GroupMember, len(members))
		for i, m := range members {
			groupMembers[i] = GroupMember{AgentID: m.AgentID, ReplyChannelID: m.ReplyChannelID}
		}
		c.clearNonMemberMentions(msg.Platform, msg.Mentions, groupMembers)
		msg.Mentions = mergeResolvedMentions(msg.Mentions, parseGroupMentions(ctx, q, ai.FlattenCanonicalText(msg.Content), members))
		envelope, err := EncodeGroupOutboxEnvelopeWithFeedback(msg.Mentions, msg.LifecycleFeedback)
		if err != nil {
			return fmt.Errorf("encode outbox envelope: %w", err)
		}
		if err := createPendingGroupOutbox(ctx, q, result.Message.ID, result.GroupID, envelope); err != nil {
			return fmt.Errorf("create group outbox: %w", err)
		}
		return nil
	}))
}

// ImportGroupHistory appends platform history as canonical context without
// creating outbox work. Platform message IDs make repeated lazy imports safe.
func (c *Coordinator) ImportGroupHistory(ctx context.Context, messages []pkgchannel.IncomingMessage) error {
	if c.eventLog == nil {
		return errors.New("group event log is not configured")
	}
	for _, msg := range messages {
		if !msg.IsGroup || msg.MessageID == "" {
			return errors.New("group history message is missing group identity")
		}
		event, err := c.groupEventMessage(ctx, msg)
		if err != nil {
			return err
		}
		if _, err := c.eventLog.AppendGroupMessage(ctx, event); err != nil {
			return fmt.Errorf("append imported group history: %w", err)
		}
	}
	return nil
}

func (c *Coordinator) groupEventMessage(ctx context.Context, msg pkgchannel.IncomingMessage) (eventlog.Message, error) {
	channelID := msg.ChannelID
	if channelID == "" {
		channelID = msg.Platform
	}
	if _, err := validatePlatformChannel(ctx, c.store, msg.Platform, channelID); err != nil {
		return eventlog.Message{}, fmt.Errorf("validate source channel: %w", err)
	}
	content := c.canonicalGroupContent(ctx, msg)
	return eventlog.Message{
		Platform:          msg.Platform,
		PlatformGroupID:   msg.ChatID,
		PlatformThreadID:  msg.ThreadID,
		SourceChannelID:   channelID,
		ActorType:         eventlog.ActorHuman,
		ActorID:           msg.SenderID,
		ActorDisplayName:  msg.SenderName,
		PlatformMessageID: msg.MessageID,
		PlatformTimestamp: msg.Timestamp,
		ReplyTo:           msg.ReplyTo,
		Content:           ai.FlattenCanonicalText(content),
		ContentBlocks:     marshalGroupContentBlocks(content),
	}, nil
}

// resolveGroupChat builds a ResolvedChat for a specific agent in a group,
// bypassing the normal ResolveAgent flow.
// replyChannelID is the agent's registered reply channel from group membership;
// when non-empty it overrides msg.ChannelID for session context (CR-009).
func (c *Coordinator) resolveGroupChat(ctx context.Context, msg pkgchannel.IncomingMessage, groupID, agentID, replyChannelID string) (*ResolvedChat, error) {
	candidates := orderedIDs(msg.SenderID)
	if len(msg.SenderIDs) > 0 {
		candidates = orderedIDs(append([]string{msg.SenderID}, msg.SenderIDs...)...)
	}
	resolved, match, err := ResolveUserCandidates(ctx, c.auth, msg.Platform, candidates)
	if err != nil {
		return nil, fmt.Errorf("resolve user: %w", err)
	}
	if err := maybeCanonicalizeIdentity(ctx, c.auth, msg.Platform, msg.SenderID, match); err != nil {
		return nil, fmt.Errorf("canonicalize user identity: %w", err)
	}

	// Membership selects a candidate, not an authority. Every group turn gets a
	// fresh roleless GroupAgentActor bound to this exact group/member.
	if c.agentAccess == nil {
		return nil, ErrAgentAccessDenied
	}
	authority, err := agentaccess.GroupAgentAuthority(groupID, agentID)
	if err != nil {
		return nil, ErrAgentAccessDenied
	}
	if _, err := c.agentAccess.Use(ctx, authority, agentID); err != nil {
		return nil, ErrAgentAccessDenied
	}

	svc := c.serviceManager.GetService(agentID)
	if svc == nil {
		return nil, fmt.Errorf("agent service %q not found", agentID)
	}

	channelID := replyChannelID
	if channelID == "" {
		channelID = msg.ChannelID
	}
	if channelID == "" {
		channelID = msg.Platform
	}
	channelCtx := "group:" + msg.ChatID
	if channelID != "" && channelID != msg.Platform {
		channelCtx = "channel:" + channelID + ":" + channelCtx
	}

	return &ResolvedChat{
		Service:        svc,
		User:           resolved.User,
		AgentID:        agentID,
		SessionKey:     agent.BuildGroupSessionKey(agentID, groupID),
		Channel:        session.Channel(channelCtx),
		ChatCtx:        ChatContext{Platform: msg.Platform, ChannelID: channelID, ChatID: msg.ChatID, GroupID: groupID, IsGroup: true},
		GroupID:        groupID,
		Authority:      authority,
		CurrentSpeaker: platformGroupSpeaker(msg, resolved.User.ID, resolved.User.Name, c.transcriptSpeakerName(ctx, groupID, msg.SenderID)),
	}, nil
}

// transcriptSpeakerName is the name the injected transcript will print for this
// sender, or "" when the namer has none and would fall back to the raw actor id.
func (c *Coordinator) transcriptSpeakerName(ctx context.Context, groupID, senderID string) string {
	if c.db == nil || senderID == "" {
		return ""
	}
	name := eventlog.NewParticipantNamer(sqlc.New(c.db)).Name(ctx, groupID, string(eventlog.ActorHuman), senderID)
	if name == senderID {
		return ""
	}
	return name
}

// platformGroupSpeaker builds the per-turn speaker for a platform group sender.
// A linked sender carries the resolved auth user id (profile target); an unlinked
// sender carries an empty UserID, so no profile is ever injected for them.
//
// transcriptName wins when the namer produced one, so a person is spelled the
// same way in <current_speaker> as on their own transcript line -- the whole
// point of routing every participant name through one function. When the namer
// has nothing and would print the raw actor id, the live platform sender name is
// the friendlier choice and cannot contradict a name the transcript never shows.
func platformGroupSpeaker(msg pkgchannel.IncomingMessage, userID, userName, transcriptName string) memory.CurrentSpeaker {
	displayName := transcriptName
	if displayName == "" {
		displayName = msg.SenderName
	}
	if displayName == "" {
		displayName = userName
	}
	return memory.CurrentSpeaker{
		Platform:       msg.Platform,
		PlatformUserID: msg.SenderID,
		DisplayName:    displayName,
		UserID:         userID,
	}
}

// canonicalGroupContent turns raw platform images into group-owned canonical
// references before the event log stores them, so a group message reaches
// history through the same media path a direct session uses.
//
// Ingestion persists only: a group image is stored, not described. The baseline
// costs a VLM call and most group images never wake an agent, so it is rendered
// lazily by the first turn that actually reads the message.
//
// Ingestion must never lose a group message because media handling failed: an
// image that cannot be canonicalized degrades to the stable unavailable
// projection and the message still lands, exactly as the old inline codec did
// for images too large to keep.
func (c *Coordinator) canonicalGroupContent(ctx context.Context, msg pkgchannel.IncomingMessage) []ai.ContentBlock {
	blocks := ai.CloneContentBlocks(msg.Content)
	if !ai.HasImage(blocks) {
		return blocks
	}
	persisted, err := c.persistGroupImages(ctx, msg, blocks)
	if err != nil {
		slog.Warn("group image canonicalization failed; storing unavailable projections",
			"platform", msg.Platform, "chat_id", msg.ChatID, "error", err)
		return unavailableImages(blocks)
	}
	return persisted
}

func (c *Coordinator) persistGroupImages(ctx context.Context, msg pkgchannel.IncomingMessage, blocks []ai.ContentBlock) ([]ai.ContentBlock, error) {
	if c.sessionImages == nil {
		return nil, errors.New("group image pipeline is not configured")
	}
	if c.eventLog == nil {
		return nil, errors.New("group event log is not configured")
	}
	// Media is owned by the group, so the group registry row must exist before
	// its first image does. This is the same get-or-create the append below
	// performs, under the same advisory lock, just one step earlier.
	//
	// Storing ahead of the append means a failed or duplicate delivery can leave
	// an unreferenced media row and blob behind. Accepted: the bytes are inert
	// and content-addressed, and the same follow-up that purges an owner's blob
	// prefix on delete owns this cleanup.
	groupID, err := c.eventLog.ResolveGroupID(ctx, msg.Platform, msg.ChatID, msg.ThreadID)
	if err != nil {
		return nil, err
	}
	id, err := uuid.Parse(groupID)
	if err != nil {
		return nil, fmt.Errorf("parse group owner %q: %w", groupID, err)
	}
	return c.sessionImages.Persist(ctx, sessionmedia.GroupOwner(id), blocks)
}

// unavailableImages is the degraded projection: the message keeps its text and
// its image positions, but the bytes are gone rather than stored raw.
func unavailableImages(blocks []ai.ContentBlock) []ai.ContentBlock {
	out := ai.CloneContentBlocks(blocks)
	for i, block := range out {
		if _, ok := block.(ai.ImageContent); ok {
			out[i] = ai.TextContent{Text: ai.UnavailableImageProjection}
		}
	}
	return out
}

// marshalGroupContentBlocks serializes message content for event-log storage
// when it carries more than text. Text-only messages store nothing and replay
// from the text projection; a marshal failure degrades the same way rather
// than dropping the message.
func marshalGroupContentBlocks(blocks []ai.ContentBlock) []byte {
	if !ai.HasImageRef(blocks) && !ai.HasImage(blocks) {
		return nil
	}
	data, err := ai.MarshalContentBlocks(blocks)
	if err != nil {
		return nil
	}
	return data
}
