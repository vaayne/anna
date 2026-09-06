package channel

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/CherryHQ/stella/internal/agent"
	"github.com/CherryHQ/stella/internal/eventlog"
	pkgchannel "github.com/CherryHQ/stella/pkg/channel"
	"github.com/CherryHQ/stella/pkg/db/sqlc"
)

// groupPublishDriver owns egress for an accepted group reply: routing it to a
// publisher, recording the delivery on the canonical message, waking the peers
// it unblocks, and compensating when delivery is finally lost.
//
// It deliberately decides no retry policy. Attempt counting, requeue and the
// terminal state of a dispatch row belong to GroupDispatcher, which calls
// failDispatch/completeDispatch on what run returns. The dependency edge only
// ever points dispatcher -> driver.
// acceptedPublishRecoveryPrefix is persisted in last_error solely to preserve
// the distinct retry class across claim/requeue cycles without a schema change.
const acceptedPublishRecoveryPrefix = "accepted_publish_recovery:"

type acceptedPublishBookkeepingError struct{ err error }

func (e *acceptedPublishBookkeepingError) Error() string {
	return "accepted publish bookkeeping: " + e.err.Error()
}
func (e *acceptedPublishBookkeepingError) Unwrap() error { return e.err }

func isAcceptedPublishRecovery(row sqlc.CtxGroupDispatch, err error) bool {
	var bookkeeping *acceptedPublishBookkeepingError
	// Once a prior send outcome is ambiguous, a later explicit failure proves
	// only that later attempt failed; it cannot prove the earlier platform send
	// was absent. Preserve the wider recovery class until its own ceiling.
	return strings.HasPrefix(row.LastError, acceptedPublishRecoveryPrefix) || errors.As(err, &bookkeeping)
}

type groupPublishDriver struct {
	db         *pgxpool.Pool
	q          *sqlc.Queries
	publishers *PublisherRegistry
	coord      *Coordinator
	events     *GroupEventHub
	log        *slog.Logger
	// wake re-polls the dispatcher after a successor outbox is committed.
	wake func()
	// abort stops the turn still running behind a session key, for publishers
	// that expose a Cancel control. The session queue lives in the chat resolver.
	abort func(sessionKey string) bool
}

func newGroupPublishDriver(db *pgxpool.Pool, q *sqlc.Queries, publishers *PublisherRegistry, coord *Coordinator, log *slog.Logger, wake func(), abort func(string) bool) *groupPublishDriver {
	return &groupPublishDriver{db: db, q: q, publishers: publishers, coord: coord, log: log, wake: wake, abort: abort}
}

// publishJob is one egress attempt: the accepted reply, the trigger it answers,
// and the routing state it is delivered through.
type publishJob struct {
	row       sqlc.CtxGroupDispatch
	trigger   sqlc.CtxGroupMessage
	state     sqlc.CtxGroupState
	publisher pkgchannel.GroupPublisher
	response  groupResponse
	envelope  GroupOutboxEnvelope
	// acceptedMessageID is the canonical row this publish is rendering. It is
	// empty on the recovery path, where the row already carries the id.
	acceptedMessageID string
}

// run performs one egress attempt and returns the dispatch row it worked on.
// Platform success is committed before any local finalization, so a retry that
// sees published_at repairs only DB bookkeeping and never repeats the side effect.
func (p *groupPublishDriver) run(ctx context.Context, job publishJob) (sqlc.CtxGroupDispatch, error) {
	row := job.row
	if row.ResultMessageID == "" && job.acceptedMessageID != "" {
		row.ResultMessageID = job.acceptedMessageID
	}
	if row.PublishedAt.Valid {
		if err := p.finalizeAcceptedPublished(ctx, row); err != nil {
			return row, &acceptedPublishBookkeepingError{err: err}
		}
		return row, nil
	}
	if job.publisher == nil {
		return row, errors.New("publish: publisher unavailable")
	}
	// A response carrying acceptedMessageID completed its admission before the
	// publish call. Let that admitted turn finish under its captured decision;
	// recovery of an older accepted row is a new admission and must recheck the
	// exact channel boundary before retrying external egress.
	if p.coord != nil && job.acceptedMessageID == "" {
		allowed, err := p.coord.channelListenerAllowed(ctx, job.state.Platform, row.ReplyChannelID)
		if err != nil {
			return row, fmt.Errorf("publish channel admission: %w", err)
		}
		if !allowed {
			return row, errChannelPluginDisabled
		}
	}
	sessionKey := agent.BuildGroupSessionKey(row.AgentID, row.GroupID)
	if row.ResultMessageID != "" {
		if row.PublishStartedAt.Valid {
			// The previous attempt reached the platform and never reported back,
			// so this reply may already be visible. Publishers receive row.ID as a
			// stable delivery key; channels without native idempotency still prefer
			// a recoverable duplicate over silently dropping the answer.
			p.log.Warn("republishing an accepted group reply whose delivery outcome is unknown", "dispatch_id", row.ID, "result_message_id", row.ResultMessageID)
		} else if _, err := p.q.MarkGroupDispatchPublishStarted(ctx, sqlc.MarkGroupDispatchPublishStartedParams{ID: row.ID, AttemptCount: row.AttemptCount}); err != nil {
			return row, fmt.Errorf("mark publish started: %w", err)
		}
	}
	err := job.publisher.Publish(ctx, pkgchannel.GroupPublishRequest{
		Platform:        job.state.Platform,
		PlatformGroupID: job.state.PlatformGroupID, PlatformThreadID: job.state.PlatformThreadID,
		ReplyTo: nullStringValue(job.trigger.PlatformMessageID), Stream: replayGroupResponse(job.response),
		DeliveryID:  row.ID,
		RequesterID: job.trigger.ActorID, LifecycleFeedback: job.envelope.LifecycleFeedback,
		Abort: func() bool { return p.abort(sessionKey) },
	})
	if err != nil {
		// A returned publisher error is a known platform outcome and stays on the
		// ordinary three-attempt policy. A bookkeeping error after success does not.
		if _, clearErr := p.q.ClearGroupDispatchPublishStarted(ctx, sqlc.ClearGroupDispatchPublishStartedParams{ID: row.ID, AttemptCount: row.AttemptCount}); clearErr != nil {
			p.log.Warn("clear publish start marker failed", "dispatch_id", row.ID, "error", clearErr)
		}
		return row, fmt.Errorf("publish: %w", err)
	}
	if err := p.markPublished(ctx, row); err != nil {
		return row, &acceptedPublishBookkeepingError{err: err}
	}
	// MarkGroupDispatchPublished is a committed standalone statement. Carry the
	// durable fact locally so a finalization error cannot fall back into the
	// ordinary publish-failure path merely because a follow-up read failed.
	row.PublishedAt = nullTime(time.Now().UTC())
	if err := p.finalizeAcceptedPublished(ctx, row); err != nil {
		return row, &acceptedPublishBookkeepingError{err: err}
	}
	return row, nil
}

func (p *groupPublishDriver) publisherFor(state sqlc.CtxGroupState, row sqlc.CtxGroupDispatch) (pkgchannel.GroupPublisher, error) {
	if publisher, ok := p.publishers.Get(row.ReplyChannelID); ok {
		return publisher, nil
	}
	if state.Platform == webGroupPlatform {
		// Web is a platform whose egress is the event log the browser already
		// reads, so its publisher does nothing. Everything else about the turn
		// -- publish markers, delivery state, compensation -- stays identical.
		return NoopGroupPublisher(), nil
	}
	return nil, fmt.Errorf("publisher %q not registered", row.ReplyChannelID)
}

// markPublished is deliberately one statement after the publisher returns.
// It is the durable boundary between an externally successful send and local
// recovery work; do not fold it into finalizeAcceptedPublished below.
func (p *groupPublishDriver) markPublished(ctx context.Context, row sqlc.CtxGroupDispatch) error {
	updated, err := p.q.MarkGroupDispatchPublished(ctx, sqlc.MarkGroupDispatchPublishedParams{ID: row.ID, AttemptCount: row.AttemptCount, ResultMessageID: row.ResultMessageID})
	if err != nil {
		return fmt.Errorf("mark dispatch published: %w", err)
	}
	if updated == 0 {
		return errors.New("mark dispatch published: lost dispatch ownership")
	}
	return nil
}

// finalizeAcceptedPublished contains only idempotent local DB work. A retry
// after published_at is set may call it any number of times without publishing.
func (p *groupPublishDriver) finalizeAcceptedPublished(ctx context.Context, row sqlc.CtxGroupDispatch) error {
	tx, err := p.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("finalize accepted publish: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := p.q.WithTx(tx)
	// Every platform gets the successor outbox: it is what wakes the peers, and
	// without it agent-to-agent collaboration silently exists on web only. No
	// platform echoes a bot's own message back through ingest, so this is the
	// only path that carries an agent post to its peers. Chain length stays
	// bounded by triage (agent_lap, agent_chain_hard_limit) and the accept caps.
	if err := p.createAgentReplyOutbox(ctx, q, row); err != nil {
		return err
	}
	// Delivery is a fact about the publisher returning, not about which one ran:
	// the noop web publisher earns 'delivered' the same way a platform API call
	// does, and the browser gets the pending -> delivered frame every other
	// surface already emits.
	message, err := q.SetGroupMessageDeliveryState(ctx, sqlc.SetGroupMessageDeliveryStateParams{ID: row.ResultMessageID, DeliveryState: "delivered"})
	if err != nil {
		return fmt.Errorf("mark group message delivered: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("finalize accepted publish: commit: %w", err)
	}
	p.wake()
	if p.events != nil {
		p.events.Announce(eventlog.AppendResult{GroupID: row.GroupID, Seq: message.Seq, Message: message})
		// The terminal frame for an accepted turn, emitted after the message so a
		// subscriber never retires the presence badge before the reply it was
		// waiting for lands. Without it the browser would have to guess when the
		// dispatcher's 'running' frame stopped being true.
		p.events.AnnounceTurn(row.GroupID, row.AgentID, "done", "")
	}
	return nil
}

func (p *groupPublishDriver) createAgentReplyOutbox(ctx context.Context, q *sqlc.Queries, row sqlc.CtxGroupDispatch) error {
	message, err := q.GetGroupMessage(ctx, row.ResultMessageID)
	if err != nil {
		return fmt.Errorf("get accepted agent reply: %w", err)
	}
	members, err := q.ListGroupMembers(ctx, row.GroupID)
	if err != nil {
		return fmt.Errorf("list group members: %w", err)
	}
	mentions := parseGroupMentions(ctx, q, message.Content, members)
	envelope, err := encodeGroupOutboxEnvelope(GroupOutboxEnvelope{Mentions: mentions})
	if err != nil {
		return fmt.Errorf("encode agent reply outbox: %w", err)
	}
	err = q.CreateGroupOutboxIfAbsent(ctx, sqlc.CreateGroupOutboxIfAbsentParams{
		ID: uuid.Must(uuid.NewV7()).String(), GroupMessageID: row.ResultMessageID, GroupID: row.GroupID,
		Envelope: envelope, Status: "pending", LastError: "",
	})
	if err != nil {
		return fmt.Errorf("create agent reply outbox: %w", err)
	}
	return nil
}

func (p *groupPublishDriver) failAcceptedPublishWithExpiryFence(ctx context.Context, row sqlc.CtxGroupDispatch, cause error, expiredAt time.Time) error {
	tx, err := p.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("fail accepted publish: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := p.q.WithTx(tx)
	var updated int64
	if expiredAt.IsZero() {
		updated, err = q.MarkGroupDispatchFailed(ctx, sqlc.MarkGroupDispatchFailedParams{ID: row.ID, AttemptCount: row.AttemptCount, LastError: cause.Error()})
	} else {
		updated, err = q.MarkExpiredGroupDispatchFailed(ctx, sqlc.MarkExpiredGroupDispatchFailedParams{
			ID: row.ID, AttemptCount: row.AttemptCount, LastError: cause.Error(), Now: nullTime(expiredAt),
		})
	}
	if err != nil {
		return fmt.Errorf("mark dispatch failed: %w", err)
	}
	if updated == 0 {
		return errors.New("mark dispatch failed: lost dispatch ownership")
	}
	message, err := q.SetGroupMessageDeliveryState(ctx, sqlc.SetGroupMessageDeliveryStateParams{ID: row.ResultMessageID, DeliveryState: "failed"})
	if err != nil {
		return fmt.Errorf("mark group message failed: %w", err)
	}
	if _, err := q.RequeueHeldGroupDispatchesAfterAcceptedPost(ctx, sqlc.RequeueHeldGroupDispatchesAfterAcceptedPostParams{
		GroupID: row.GroupID, AcceptedSeq: message.Seq,
	}); err != nil {
		return fmt.Errorf("requeue held peers after failed delivery: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("fail accepted publish: commit: %w", err)
	}
	p.log.Warn("group reply delivery failed permanently", "dispatch_id", row.ID, "result_message_id", row.ResultMessageID, "error", cause)
	if p.events != nil {
		p.events.Announce(eventlog.AppendResult{GroupID: row.GroupID, Seq: message.Seq, Message: message})
		p.events.AnnounceTurn(row.GroupID, row.AgentID, "failed", cause.Error())
	}
	return cause
}

func groupResponseFromMessage(message sqlc.CtxGroupMessage) groupResponse {
	// Ceiling: event envelopes live only in this process. After a restart, an
	// accepted unpublished reply replays canonical text/reasoning only. Spool the
	// buffered event sequence to BlobStore when cross-process rich replay matters.
	events := make([]pkgchannel.Event, 0, 2)
	if message.Reasoning != "" {
		events = append(events, pkgchannel.Event{Reasoning: message.Reasoning})
	}
	if message.Content != "" {
		events = append(events, pkgchannel.Event{Text: message.Content})
	}
	return groupResponse{
		text: message.Content, reasoning: message.Reasoning, sessionID: message.AgentSessionID,
		events: events, complete: true,
	}
}

func replayGroupResponse(response groupResponse) *pkgchannel.ChatStream {
	events := make(chan pkgchannel.Event, len(response.events))
	for _, evt := range response.events {
		events <- evt
	}
	close(events)
	return &pkgchannel.ChatStream{Events: events, SessionID: response.sessionID}
}
