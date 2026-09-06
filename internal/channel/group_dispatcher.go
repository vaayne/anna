package channel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel/attribute"

	"github.com/CherryHQ/stella/internal/agent"
	"github.com/CherryHQ/stella/internal/eventlog"
	"github.com/CherryHQ/stella/internal/memory"
	"github.com/CherryHQ/stella/internal/platform/config"
	pkgchannel "github.com/CherryHQ/stella/pkg/channel"
	"github.com/CherryHQ/stella/pkg/db/sqlc"
)

const (
	defaultGroupDispatchLease       = 5 * time.Minute
	defaultGroupDispatchPoll        = 2 * time.Second
	defaultGroupDispatchMaxAttempts = int64(3)
	// acceptedPublishRecoveryMaxAttempts bounds the ambiguous window after a
	// lease crash or a post-publish bookkeeping outcome is unknown. Raise only
	// after platform idempotency keys make repeated egress provably harmless.
	acceptedPublishRecoveryMaxAttempts = int64(10)
	// global pool; per-(group, agent) serialization remains enforced by the
	// durable claim and session queue. Raise only after measured saturation.
	defaultGroupDispatchWorkers = 8
	// groupReplyBufferBytes bounds the complete result retained between model
	// completion and egress. Raise only after adding BlobStore spooling.
	defaultGroupReplyBufferBytes = 8 << 20
)

type dispatchChatFunc func(context.Context, sqlc.CtxGroupDispatch, sqlc.CtxGroupMessage, sqlc.CtxGroupState) (*pkgchannel.ChatStream, error)

// GroupDispatcher materializes durable group response decisions and executes
// one selected-agent dispatch at a time. Ingest owns facts; this owns work.
type GroupDispatcher struct {
	db    *pgxpool.Pool
	q     *sqlc.Queries
	coord *Coordinator
	log   *slog.Logger

	// publish owns egress for an accepted reply; chats owns the turn transport
	// and the per-(agent, group) session queue. Both are called from here and
	// never call back.
	publish *groupPublishDriver
	chats   *groupChatResolver

	leaseDuration time.Duration
	maxAttempts   int64
	wakeC         chan struct{}
	dispatchC     chan sqlc.CtxGroupDispatch
	chat          dispatchChatFunc
	committer     memory.TxGroupCommitter
	events        *GroupEventHub
}

// Coordination bundles the coordinator and its durable group dispatcher. The
// channel domain builds them together and closes the coordinator<->dispatcher
// cycle, so the composition root does not assemble the cycle by hand.
type Coordination struct {
	// Coordinator is the channel MessageHandler for all channels.
	Coordinator *Coordinator
	// GroupDispatcher is the durable group-dispatch runner. The HTTP layer needs
	// only this narrow port, not the whole coordinator.
	GroupDispatcher *GroupDispatcher
}

// NewCoordination constructs the coordinator and its group dispatcher together
// and closes the coordinator<->dispatcher cycle. The dispatcher reuses the
// coordinator's publisher registry (supplied via WithPublisherRegistry). The
// composition root receives the coordinator (as the channel Handler) and the
// narrow GroupDispatcher port without wiring the cycle itself.
func NewCoordination(
	db *pgxpool.Pool,
	pm interface {
		agent.ServiceManager
		userInvalidator
	},
	store config.Store,
	listFn func() []pkgchannel.ModelOption,
	switchFn func(provider, model string) error,
	opts ...CoordinatorOption,
) Coordination {
	coord := NewCoordinator(pm, store, listFn, switchFn, opts...)
	gd := NewGroupDispatcher(db, coord, nil)
	coord.SetGroupDispatcher(gd)
	return Coordination{Coordinator: coord, GroupDispatcher: gd}
}

func NewGroupDispatcher(db *pgxpool.Pool, coord *Coordinator, publishers *PublisherRegistry) *GroupDispatcher {
	if publishers == nil && coord != nil {
		publishers = coord.publisherRegistry
	}
	d := &GroupDispatcher{
		db:            db,
		q:             sqlc.New(db),
		coord:         coord,
		log:           slog.With("component", "group_dispatcher"),
		leaseDuration: defaultGroupDispatchLease,
		maxAttempts:   defaultGroupDispatchMaxAttempts,
		wakeC:         make(chan struct{}, 1),
		dispatchC:     make(chan sqlc.CtxGroupDispatch, 25),
	}
	d.chats = newGroupChatResolver(d.q, coord)
	d.publish = newGroupPublishDriver(db, d.q, publishers, coord, d.log, d.Wake, d.chats.abort)
	d.chat = d.chats.chatDispatch
	return d
}

// SetGroupTurnCommitter supplies the production memory capability that commits
// a deferred group turn inside the dispatcher's acceptance transaction.
func (d *GroupDispatcher) SetGroupTurnCommitter(committer memory.TxGroupCommitter) {
	if d != nil {
		d.committer = committer
	}
}

// SetGroupEventHub attaches the channel-owned post-commit projection feed.
func (d *GroupDispatcher) SetGroupEventHub(events *GroupEventHub) {
	if d != nil {
		d.events = events
		d.publish.events = events
	}
}

// ValidateStartup fails closed when the dispatcher could otherwise accept a
// group post without atomically committing its agent history.
func (d *GroupDispatcher) ValidateStartup() error {
	if d == nil || d.committer == nil {
		return errors.New("group dispatcher requires memory.TxGroupCommitter")
	}
	return nil
}

func (d *GroupDispatcher) Wake() {
	if d == nil {
		return
	}
	select {
	case d.wakeC <- struct{}{}:
	default:
	}
}

func (d *GroupDispatcher) heartbeatInterval() time.Duration {
	interval := d.leaseDuration / 3
	if interval <= 0 {
		return time.Minute
	}
	return interval
}

func (d *GroupDispatcher) startHeartbeat(ctx context.Context, label, id string, knownUntil time.Time, extend func(context.Context, time.Time) (int64, error), onLost func()) func() {
	if d == nil || d.leaseDuration <= 0 || extend == nil {
		return func() {}
	}
	interval := d.heartbeatInterval()
	hctx, cancel := context.WithCancel(ctx)
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		// The worker may continue only while it can prove its last committed lease
		// is still valid. Cancel one heartbeat interval before that proof expires,
		// giving the owner time to stop before the reaper makes the row claimable.
		knownUntil = knownUntil.UTC()
		if knownUntil.IsZero() {
			knownUntil = time.Now().UTC().Add(d.leaseDuration)
		}
		loseOwnership := func(reason string) {
			d.log.Debug("group dispatch heartbeat lost ownership", "type", label, "id", id, "reason", reason)
			if onLost != nil {
				onLost()
			}
		}
		for {
			select {
			case <-hctx.Done():
				return
			case <-ticker.C:
				now := time.Now().UTC()
				until := now.Add(d.leaseDuration)
				rows, err := extend(hctx, until)
				if err != nil {
					d.log.Warn("group dispatch heartbeat failed", "type", label, "id", id, "error", err)
					if !now.Add(interval).Before(knownUntil) {
						loseOwnership("lease extension unconfirmed")
						return
					}
					continue
				}
				if rows == 0 {
					loseOwnership("lease row no longer owned")
					return
				}
				knownUntil = until
			}
		}
	}()
	return cancel
}

func (d *GroupDispatcher) Run(ctx context.Context) error {
	if d == nil || d.q == nil {
		return errors.New("group dispatcher not configured")
	}
	for range defaultGroupDispatchWorkers {
		go d.runWorker(ctx)
	}
	ticker := time.NewTicker(defaultGroupDispatchPoll)
	defer ticker.Stop()
	for {
		if err := d.poll(ctx); err != nil {
			d.log.Warn("group dispatch poll failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-d.wakeC:
		case <-ticker.C:
		}
	}
}

func (d *GroupDispatcher) runWorker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case row := <-d.dispatchC:
			if err := d.ExecuteDispatch(ctx, row); err != nil {
				d.log.Warn("group dispatch worker failed", "dispatch_id", row.ID, "error", err)
			}
		}
	}
}

// AbortGroupTurn stops the active turn for one group member. It is intentionally
// idempotent: a completed or unknown turn has nothing left to cancel.
func (d *GroupDispatcher) AbortGroupTurn(groupID, agentID string) bool {
	if d == nil || groupID == "" || agentID == "" {
		return false
	}
	return d.chats.abort(agent.BuildGroupSessionKey(agentID, groupID))
}

func (d *GroupDispatcher) poll(ctx context.Context) error {
	if err := d.reapExpired(ctx); err != nil {
		return err
	}
	pending, err := d.q.ListPendingGroupOutbox(ctx, sqlc.ListPendingGroupOutboxParams{
		Now:        nullTime(time.Now().UTC()),
		LimitCount: 25,
	})
	if err != nil {
		return fmt.Errorf("list pending outbox: %w", err)
	}
	var errs []error
	for _, row := range pending {
		if err := d.processOutbox(ctx, row); err != nil {
			errs = append(errs, err)
		}
	}
	dueDispatch, err := d.q.ListPendingGroupWakePairs(ctx, sqlc.ListPendingGroupWakePairsParams{
		Now:        nullTime(time.Now().UTC()),
		LimitCount: 25,
	})
	if err != nil {
		return fmt.Errorf("list pending wake pairs: %w", err)
	}
	dueNudges, err := d.q.ListPendingGroupNudges(ctx, sqlc.ListPendingGroupNudgesParams{
		Now:        nullTime(time.Now().UTC()),
		LimitCount: 25,
	})
	if err != nil {
		return fmt.Errorf("list pending nudges: %w", err)
	}
	for _, row := range append(dueDispatch, dueNudges...) {
		select {
		case d.dispatchC <- row:
		case <-ctx.Done():
			return errors.Join(append(errs, ctx.Err())...)
		default:
			// Never block the Run goroutine: it also owns lease reaping and outbox
			// processing, and every worker can sit in a multi-minute model turn.
			// The rows stay pending and the next poll re-lists them.
			d.log.Debug("group dispatch queue full; deferring rows to the next poll", "dispatch_id", row.ID)
			return errors.Join(errs...)
		}
	}
	return errors.Join(errs...)
}

func (d *GroupDispatcher) reapExpired(ctx context.Context) error {
	now := time.Now().UTC()
	// A stale owner gets one heartbeat interval to observe its failed lease CAS
	// and cancel before another worker may claim the row.
	nextClaimAt := now.Add(d.heartbeatInterval())
	outboxRows, err := d.q.ListExpiredRunningGroupOutbox(ctx, sqlc.ListExpiredRunningGroupOutboxParams{
		Now:        nullTime(now),
		LimitCount: 50,
	})
	if err != nil {
		return fmt.Errorf("list expired outbox: %w", err)
	}
	for _, row := range outboxRows {
		if row.AttemptCount >= d.maxAttempts {
			if _, err := d.q.MarkExpiredGroupOutboxFailed(ctx, sqlc.MarkExpiredGroupOutboxFailedParams{
				ID: row.ID, AttemptCount: row.AttemptCount, LastError: "lease expired", Now: nullTime(now),
			}); err != nil {
				return fmt.Errorf("fail expired outbox: %w", err)
			}
			continue
		}
		if _, err := d.q.RequeueExpiredGroupOutbox(ctx, sqlc.RequeueExpiredGroupOutboxParams{
			ID: row.ID, AttemptCount: row.AttemptCount, NextAttemptAt: nullTime(nextClaimAt), LastError: "lease expired", Now: nullTime(now),
		}); err != nil {
			return fmt.Errorf("requeue expired outbox: %w", err)
		}
	}
	dispatchRows, err := d.q.ListExpiredRunningGroupDispatch(ctx, sqlc.ListExpiredRunningGroupDispatchParams{
		Now:        nullTime(now),
		LimitCount: 50,
	})
	if err != nil {
		return fmt.Errorf("list expired dispatch: %w", err)
	}
	for _, row := range dispatchRows {
		if row.ResultMessageID != "" {
			if row.PublishedAt.Valid {
				// Platform success is durable, but its local outbox/delivery
				// finalization may have crashed. Re-run only that idempotent DB work,
				// never complete the row or repeat the platform side effect here.
				if _, err := d.markAndAnnounce(ctx, row, "failed", "published finalize lease expired", "requeue expired published finalization", func(ctx context.Context) (int64, error) {
					return d.q.RequeueExpiredGroupDispatch(ctx, sqlc.RequeueExpiredGroupDispatchParams{
						ID: row.ID, AttemptCount: row.AttemptCount, NextAttemptAt: nullTime(nextClaimAt), LastError: "published finalize lease expired", Now: nullTime(now),
					})
				}); err != nil {
					return err
				}
				continue
			}
			// Accepted but never published: the canonical message is committed
			// and egress is still owed. A lease crash is ambiguous rather than a
			// returned publisher failure, so it gets its explicit higher ceiling.
			if row.AttemptCount >= acceptedPublishRecoveryMaxAttempts {
				// The expiry fence and delivery compensation share one transaction.
				// A heartbeat that renewed after the list snapshot makes the fence lose
				// and rolls the message/peer updates back.
				if err := d.publish.failAcceptedPublishWithExpiryFence(ctx, row, errors.New("accepted publish lease recovery exhausted"), now); err != nil {
					d.log.Warn("fail expired accepted dispatch", "dispatch_id", row.ID, "error", err)
				}
				continue
			}
			if _, err := d.markAndAnnounce(ctx, row, "failed", "lease expired before publish", "requeue expired accepted dispatch", func(ctx context.Context) (int64, error) {
				return d.q.RequeueExpiredGroupDispatch(ctx, sqlc.RequeueExpiredGroupDispatchParams{
					ID: row.ID, AttemptCount: row.AttemptCount, NextAttemptAt: nullTime(nextClaimAt), LastError: acceptedPublishRecoveryPrefix + "lease expired before publish", Now: nullTime(now),
				})
			}); err != nil {
				return err
			}
			continue
		}
		if row.AttemptCount >= d.maxAttempts {
			if _, err := d.markAndAnnounce(ctx, row, "failed", "lease expired", "fail expired dispatch", func(ctx context.Context) (int64, error) {
				return d.q.MarkExpiredGroupDispatchFailed(ctx, sqlc.MarkExpiredGroupDispatchFailedParams{
					ID: row.ID, AttemptCount: row.AttemptCount, LastError: "lease expired", Now: nullTime(now),
				})
			}); err != nil {
				return err
			}
			continue
		}
		if _, err := d.markAndAnnounce(ctx, row, "failed", "lease expired", "requeue expired dispatch", func(ctx context.Context) (int64, error) {
			return d.q.RequeueExpiredGroupDispatch(ctx, sqlc.RequeueExpiredGroupDispatchParams{
				ID: row.ID, AttemptCount: row.AttemptCount, NextAttemptAt: nullTime(nextClaimAt), LastError: "lease expired", Now: nullTime(now),
			})
		}); err != nil {
			return err
		}
	}
	return nil
}

func (d *GroupDispatcher) processOutbox(ctx context.Context, outbox sqlc.CtxGroupOutbox) error {
	claimed, ok, err := d.claimOutbox(ctx, outbox)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	ownedCtx, cancelOwned := context.WithCancel(ctx)
	defer cancelOwned()
	stopHeartbeat := d.startHeartbeat(ownedCtx, "outbox", claimed.ID, claimed.LeaseUntil.Time, func(ctx context.Context, until time.Time) (int64, error) {
		return d.q.ExtendRunningGroupOutboxLease(ctx, sqlc.ExtendRunningGroupOutboxLeaseParams{
			ID:           claimed.ID,
			LeaseUntil:   nullTime(until),
			AttemptCount: claimed.AttemptCount,
		})
	}, cancelOwned)
	defer stopHeartbeat()
	_, err = d.q.GetGroupMessage(ownedCtx, claimed.GroupMessageID)
	if err != nil {
		return d.failOutbox(ctx, claimed, fmt.Errorf("get outbox message: %w", err))
	}
	count, err := d.q.CountGroupDispatchByMessage(ownedCtx, claimed.GroupMessageID)
	if err != nil {
		return d.failOutbox(ctx, claimed, fmt.Errorf("count dispatch rows: %w", err))
	}
	if count == 0 {
		if err := d.materializeDispatchRowsTx(ownedCtx, claimed); err != nil {
			return d.failOutbox(ctx, claimed, err)
		}
	}
	stopHeartbeat()
	rows, err := d.q.MarkGroupOutboxCompleted(ctx, sqlc.MarkGroupOutboxCompletedParams{ID: claimed.ID, AttemptCount: claimed.AttemptCount})
	if err != nil {
		return fmt.Errorf("mark outbox completed: %w", err)
	}
	if rows == 0 {
		return nil
	}
	d.Wake()
	return nil
}

func (d *GroupDispatcher) claimOutbox(ctx context.Context, row sqlc.CtxGroupOutbox) (sqlc.CtxGroupOutbox, bool, error) {
	switch row.Status {
	case "running":
		return row, true, nil
	case "pending":
		claimed, err := d.q.ClaimPendingGroupOutbox(ctx, sqlc.ClaimPendingGroupOutboxParams{
			ID:         row.ID,
			Now:        nullTime(time.Now().UTC()),
			LeaseUntil: nullTime(time.Now().UTC().Add(d.leaseDuration)),
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return sqlc.CtxGroupOutbox{}, false, nil
		}
		if err != nil {
			return sqlc.CtxGroupOutbox{}, false, fmt.Errorf("claim outbox: %w", err)
		}
		return claimed, true, nil
	default:
		return sqlc.CtxGroupOutbox{}, false, nil
	}
}

func (d *GroupDispatcher) materializeDispatchRowsTx(ctx context.Context, outbox sqlc.CtxGroupOutbox) error {
	message, err := d.q.GetGroupMessage(ctx, outbox.GroupMessageID)
	if err != nil {
		return err
	}
	if d.db == nil {
		return d.materializeWakeRows(ctx, d.q, outbox, message)
	}
	tx, err := d.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin materialize dispatch rows: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()
	if err := d.materializeWakeRows(ctx, d.q.WithTx(tx), outbox, message); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit materialize dispatch rows: %w", err)
	}
	committed = true
	return nil
}

// materializeWakeRows creates a durable per-member triage opportunity for
// every canonical message. The author has already acted, so never wake it.
func (d *GroupDispatcher) materializeWakeRows(ctx context.Context, q *sqlc.Queries, outbox sqlc.CtxGroupOutbox, message sqlc.CtxGroupMessage) error {
	envelope, err := DecodeGroupOutboxEnvelope(outbox.Envelope)
	if err != nil {
		return fmt.Errorf("decode group outbox envelope: %w", err)
	}
	members, err := q.ListGroupMembers(ctx, outbox.GroupID)
	if err != nil {
		return fmt.Errorf("list group members: %w", err)
	}
	for _, member := range members {
		if envelope.NudgeTarget != "" && member.AgentID != envelope.NudgeTarget {
			continue
		}
		if message.ActorType == string(eventlog.ActorAgent) && member.AgentID == message.ActorID {
			continue
		}
		if envelope.NudgeTarget != "" {
			if err := q.CreateGroupNudge(ctx, sqlc.CreateGroupNudgeParams{
				ID: uuid.Must(uuid.NewV7()).String(), GroupMessageID: outbox.GroupMessageID,
				GroupID: outbox.GroupID, AgentID: member.AgentID, ReplyChannelID: member.ReplyChannelID,
			}); err != nil {
				return fmt.Errorf("create nudge row: %w", err)
			}
			continue
		}
		if err := q.CreateGroupWake(ctx, sqlc.CreateGroupWakeParams{
			ID: uuid.Must(uuid.NewV7()).String(), GroupMessageID: outbox.GroupMessageID,
			GroupID: outbox.GroupID, AgentID: member.AgentID, ReplyChannelID: member.ReplyChannelID,
		}); err != nil {
			return fmt.Errorf("create wake row: %w", err)
		}
	}
	return nil
}

func (d *GroupDispatcher) ExecuteDispatch(ctx context.Context, row sqlc.CtxGroupDispatch) error {
	ctx, _ = startIngress(ctx, "channel.ingress",
		attribute.String("stella.channel.name", "group"),
		attribute.String("stella.channel.dispatch_id", row.ID),
		attribute.String("stella.channel.group_id", row.GroupID),
		attribute.String("stella.agent_id", row.AgentID),
		attribute.String("stella.channel.dispatch_kind", row.Kind),
	)
	defer finishIngress(ctx)

	claimed, ok, err := d.claimDispatch(ctx, row)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	ownedCtx, cancelOwned := context.WithCancel(ctx)
	defer cancelOwned()
	stopHeartbeat := d.startHeartbeat(ownedCtx, "dispatch", claimed.ID, claimed.LeaseUntil.Time, func(ctx context.Context, until time.Time) (int64, error) {
		return d.q.ExtendRunningGroupDispatchLease(ctx, sqlc.ExtendRunningGroupDispatchLeaseParams{
			ID:           claimed.ID,
			LeaseUntil:   nullTime(until),
			AttemptCount: claimed.AttemptCount,
		})
	}, cancelOwned)
	defer stopHeartbeat()
	message, state, err := d.messageAndState(ownedCtx, d.q, claimed.GroupMessageID)
	if err != nil {
		return d.failDispatch(ctx, claimed, err)
	}
	outbox, err := d.q.GetGroupOutboxByMessage(ownedCtx, claimed.GroupMessageID)
	if err != nil {
		return d.failDispatch(ctx, claimed, fmt.Errorf("get group outbox metadata: %w", err))
	}
	envelope, err := DecodeGroupOutboxEnvelope(outbox.Envelope)
	if err != nil {
		// Dispatch rows were already materialized from this envelope. Optional
		// publish metadata must not make an otherwise executable legacy row fail.
		d.log.Debug("ignoring invalid group outbox publish metadata", "dispatch_id", claimed.ID, "error", err)
		envelope = GroupOutboxEnvelope{}
	}
	// A published marker means egress already succeeded. Its retry only runs
	// idempotent DB finalization, so a missing publisher must not block it.
	var publisher pkgchannel.GroupPublisher
	if claimed.ResultMessageID == "" || !claimed.PublishedAt.Valid {
		publisher, err = d.publish.publisherFor(state, claimed)
		if err != nil {
			return d.failDispatch(ctx, claimed, err)
		}
	}
	// Egress compensation runs before triage on purpose. The reply is already
	// committed and visible to peers; re-triaging it would count that very post
	// (hard_cap, agent_lap) and go silent, leaving the message readable by
	// agents and never delivered to the humans.
	if claimed.ResultMessageID != "" {
		if claimed.PublishedAt.Valid {
			return d.publishAccepted(ownedCtx, publishJob{
				row: claimed, trigger: message, state: state,
			})
		}
		accepted, err := d.q.GetGroupMessage(ownedCtx, claimed.ResultMessageID)
		if err != nil {
			return d.failDispatch(ctx, claimed, fmt.Errorf("get accepted group result: %w", err))
		}
		d.log.Warn("replaying accepted group reply from canonical text after buffer loss", "dispatch_id", claimed.ID, "result_message_id", accepted.ID, "upgrade_trigger", "cross-process rich replay requires BlobStore event spooling")
		return d.publishAccepted(ownedCtx, publishJob{
			row: claimed, trigger: message, state: state, publisher: publisher,
			response: groupResponseFromMessage(accepted),
		})
	}
	// Nudges pass the gate too: recovery may hand an agent the floor, but it
	// must not bypass the hard caps that keep a stalled group from flooding.
	var reason string
	if claimed.Kind == "wake" || claimed.Kind == "nudge" {
		act, degraded := false, false
		act, reason, degraded = d.triageWake(ownedCtx, claimed, message, state, envelope)
		if act {
			ownedCtx = memory.WithGroupWake(ownedCtx, d.groupWake(ownedCtx, claimed, reason))
		}
		if !act {
			if degraded && claimed.AttemptCount < d.maxAttempts {
				// Only a DB read failure lands here. Silence is not a verdict we
				// can trust from a failed read, so requeue without a terminal frame.
				return d.failDispatch(ctx, claimed, fmt.Errorf("triage unavailable: %s", reason))
			}
			updated, err := d.markAndAnnounce(ctx, claimed, "silent", reason, "", func(ctx context.Context) (int64, error) {
				return d.q.MarkGroupDispatchSilent(ctx, sqlc.MarkGroupDispatchSilentParams{ID: claimed.ID, AttemptCount: claimed.AttemptCount, Reason: reason})
			})
			if err != nil {
				return err
			}
			if updated == 0 {
				return errors.New("mark gated dispatch silent: lost dispatch ownership")
			}
			return nil
		}
	}
	// The sink is deliberately installed before Chat. The runtime finalizes it
	// before closing its output, so draining the stream is the handoff barrier.
	sink := memory.NewGroupTurnSink()
	chatCtx := memory.WithGroupTurnSink(ownedCtx, sink)
	stream, err := d.chat(chatCtx, claimed, message, state)
	if errors.Is(err, errGroupNudgeMoot) {
		// The re-check runs after the session queue grants this slot. Do not emit
		// a running frame for work the wake ahead of it already completed.
		updated, markErr := d.markAndAnnounce(ctx, claimed, "silent", "nudge_moot", "", func(ctx context.Context) (int64, error) {
			return d.q.MarkGroupDispatchSilent(ctx, sqlc.MarkGroupDispatchSilentParams{ID: claimed.ID, AttemptCount: claimed.AttemptCount, Reason: "nudge_moot"})
		})
		if markErr != nil {
			return markErr
		}
		if updated == 0 {
			return errors.New("mark moot nudge silent: lost dispatch ownership")
		}
		return nil
	}
	if errors.Is(err, errGroupTurnSuperseded) {
		// The trigger was consumed by a session boundary while this row waited
		// (restart after `/new`, or a redundant re-execution). Running it now
		// would leak a pre-reset message into the successor session; there is
		// nothing left to do but retire the row.
		//
		// Every path below the running frame owes a terminal frame: without one
		// the browser keeps a presence badge lit until it reconnects.
		d.announceTurn(claimed, "silent", "superseded")
		return d.completeDispatch(ctx, claimed)
	}
	if err != nil {
		return d.failDispatch(ctx, claimed, err)
	}
	// chat returns only after its per-(group, agent) session queue gives this
	// turn the slot. This is the first truthful point to project it as running.
	d.announceTurn(claimed, "running", reason)
	response := d.bufferGroupResponse(ownedCtx, stream)
	turn, delivered := sink.Result()
	if response.err != nil || !response.complete || !delivered || !turn.Complete {
		cause := response.err
		if cause == nil {
			cause = errors.New("group turn ended without a complete deferred result")
		}
		return d.failDispatch(ctx, claimed, cause)
	}
	// The model's own decision to stay quiet, checked before the accept gates:
	// nothing was written, so there is nothing for them to judge.
	if isModelPass(response.text) {
		return d.retireModelPass(ownedCtx, claimed, turn)
	}
	outcome, err := d.acceptGroupResponse(ownedCtx, claimed, response, turn)
	if err != nil {
		return d.failDispatch(ctx, claimed, err)
	}
	if outcome.Status != groupTurnAccepted {
		d.announceTurn(claimed, string(outcome.Status), outcome.Reason)
		return nil
	}
	return d.publishAccepted(ownedCtx, publishJob{
		row: claimed, trigger: message, state: state, publisher: publisher,
		response: response, envelope: envelope, acceptedMessageID: outcome.Accepted.Message.ID,
	})
}

// announceTurn projects one live turn state to the group's subscribers. The hub
// is optional (tests, headless deployments), so the nil check lives here rather
// than at every call site.
func (d *GroupDispatcher) announceTurn(row sqlc.CtxGroupDispatch, state, reason string) {
	if d.events == nil {
		return
	}
	d.events.AnnounceTurn(row.GroupID, row.AgentID, state, reason)
}

// markAndAnnounce executes one CAS-backed dispatch transition and projects its
// frame only when this process won the row. An empty wrap preserves callers'
// existing unwrapped query errors when ownership loss needs a distinct error.
func (d *GroupDispatcher) markAndAnnounce(ctx context.Context, row sqlc.CtxGroupDispatch, state, reason, wrap string, mark func(context.Context) (int64, error)) (int64, error) {
	updated, err := mark(ctx)
	if err != nil {
		if wrap == "" {
			return 0, err
		}
		return 0, fmt.Errorf("%s: %w", wrap, err)
	}
	if updated > 0 {
		d.announceTurn(row, state, reason)
	}
	return updated, nil
}

// publishAccepted sequences one egress attempt: the driver delivers, this
// applies the row's terminal state. Retry policy stays with the row's owner.
func (d *GroupDispatcher) publishAccepted(ctx context.Context, job publishJob) error {
	row, err := d.publish.run(ctx, job)
	if err != nil {
		return d.failDispatch(ctx, row, err)
	}
	return d.completeDispatch(ctx, row)
}

// groupWake describes this turn to the agent about to run it: which gate let it
// through, and whether it is recovering from a HOLD. Reading the transcript
// cannot answer either question, and the answer changes what a good reply is.
func (d *GroupDispatcher) groupWake(ctx context.Context, row sqlc.CtxGroupDispatch, reason string) memory.GroupWake {
	wake := memory.GroupWake{Reason: reason}
	heldUpTo, err := d.q.MaxHeldUpToSeqInChain(ctx, sqlc.MaxHeldUpToSeqInChainParams{
		GroupID: row.GroupID, AgentID: row.AgentID, TriggerSeq: row.TriggerSeq,
		Pipeline: memory.GroupIngestPipeline(row.AgentID),
	})
	if err != nil {
		// A missing HOLD note costs the model one hint; failing the turn over it
		// would cost the group the reply.
		d.log.Debug("wake hold lookup failed", "dispatch_id", row.ID, "error", err)
		return wake
	}
	wake.HeldUpToSeq = heldUpTo
	mentionSeq, err := d.q.GetEarliestGroupMentionSinceCursor(ctx, sqlc.GetEarliestGroupMentionSinceCursorParams{
		GroupID: row.GroupID, AgentID: row.AgentID, Pipeline: memory.GroupIngestPipeline(row.AgentID), TriggerSeq: row.TriggerSeq,
	})
	if err != nil {
		d.log.Debug("wake mention lookup failed", "dispatch_id", row.ID, "error", err)
		return wake
	}
	wake.MentionSeq = mentionSeq
	return wake
}

func (d *GroupDispatcher) claimDispatch(ctx context.Context, row sqlc.CtxGroupDispatch) (sqlc.CtxGroupDispatch, bool, error) {
	switch row.Status {
	case "running":
		return row, true, nil
	case "pending":
		if row.Kind == "wake" {
			claimed, err := d.q.ClaimNewestGroupWake(ctx, sqlc.ClaimNewestGroupWakeParams{
				GroupID: row.GroupID, AgentID: row.AgentID, Now: nullTime(time.Now().UTC()),
				LeaseUntil: nullTime(time.Now().UTC().Add(d.leaseDuration)),
			})
			if errors.Is(err, pgx.ErrNoRows) {
				return sqlc.CtxGroupDispatch{}, false, nil
			}
			if err != nil {
				return sqlc.CtxGroupDispatch{}, false, fmt.Errorf("claim newest wake: %w", err)
			}
			return claimed, true, nil
		}
		claimed, err := d.q.ClaimPendingGroupNudge(ctx, sqlc.ClaimPendingGroupNudgeParams{
			ID:         row.ID,
			GroupID:    row.GroupID,
			AgentID:    row.AgentID,
			Now:        nullTime(time.Now().UTC()),
			LeaseUntil: nullTime(time.Now().UTC().Add(d.leaseDuration)),
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return sqlc.CtxGroupDispatch{}, false, nil
		}
		if err != nil {
			return sqlc.CtxGroupDispatch{}, false, fmt.Errorf("claim dispatch: %w", err)
		}
		return claimed, true, nil
	default:
		return sqlc.CtxGroupDispatch{}, false, nil
	}
}

// completeDispatch retires the row. Losing the CAS (0 rows) means another
// worker already retired it, which is the same terminal state.
func (d *GroupDispatcher) completeDispatch(ctx context.Context, row sqlc.CtxGroupDispatch) error {
	if _, err := d.q.MarkGroupDispatchCompleted(ctx, sqlc.MarkGroupDispatchCompletedParams{ID: row.ID, AttemptCount: row.AttemptCount}); err != nil {
		return fmt.Errorf("mark dispatch completed: %w", err)
	}
	return nil
}

func (d *GroupDispatcher) failOutbox(ctx context.Context, row sqlc.CtxGroupOutbox, cause error) error {
	if row.AttemptCount >= d.maxAttempts {
		if _, err := d.q.MarkGroupOutboxFailed(ctx, sqlc.MarkGroupOutboxFailedParams{ID: row.ID, AttemptCount: row.AttemptCount, LastError: cause.Error()}); err != nil {
			return fmt.Errorf("mark outbox failed: %w", err)
		}
		return cause
	}
	if _, err := d.q.RequeueGroupOutbox(ctx, sqlc.RequeueGroupOutboxParams{
		ID:            row.ID,
		AttemptCount:  row.AttemptCount,
		NextAttemptAt: nullTime(time.Now().UTC().Add(backoff(row.AttemptCount))),
		LastError:     cause.Error(),
	}); err != nil {
		return fmt.Errorf("requeue outbox: %w", err)
	}
	return cause
}

func (d *GroupDispatcher) failDispatch(ctx context.Context, row sqlc.CtxGroupDispatch, cause error) error {
	if row.ResultMessageID != "" && row.PublishedAt.Valid {
		// The platform already accepted this reply. Retrying its local finalize is
		// safe and must never reclassify the delivered message as a platform fail.
		if _, err := d.markAndAnnounce(ctx, row, "failed", cause.Error(), "requeue published dispatch finalization", func(ctx context.Context) (int64, error) {
			return d.q.RequeueGroupDispatch(ctx, sqlc.RequeueGroupDispatchParams{
				ID: row.ID, AttemptCount: row.AttemptCount,
				NextAttemptAt: nullTime(time.Now().UTC().Add(backoff(row.AttemptCount))), LastError: cause.Error(),
			})
		}); err != nil {
			return err
		}
		return cause
	}
	limit := d.dispatchAttemptLimit(row, cause)
	if row.AttemptCount >= limit {
		// Giving up on a row that already carries an accepted reply is not the
		// same as giving up on a wake: the message is committed and visible to
		// peers, so it must be marked undelivered and the peers it held must be
		// released, or it stays 'pending' forever and holds them with it.
		if row.ResultMessageID != "" {
			return d.publish.failAcceptedPublishWithExpiryFence(ctx, row, cause, time.Time{})
		}
		if _, err := d.markAndAnnounce(ctx, row, "failed", cause.Error(), "mark dispatch failed", func(ctx context.Context) (int64, error) {
			return d.q.MarkGroupDispatchFailed(ctx, sqlc.MarkGroupDispatchFailedParams{ID: row.ID, AttemptCount: row.AttemptCount, LastError: cause.Error()})
		}); err != nil {
			return err
		}
		return cause
	}
	if _, err := d.markAndAnnounce(ctx, row, "failed", cause.Error(), "requeue dispatch", func(ctx context.Context) (int64, error) {
		return d.q.RequeueGroupDispatch(ctx, sqlc.RequeueGroupDispatchParams{
			ID:            row.ID,
			AttemptCount:  row.AttemptCount,
			NextAttemptAt: nullTime(time.Now().UTC().Add(backoff(row.AttemptCount))),
			LastError:     d.dispatchFailureLastError(row, cause),
		})
	}); err != nil {
		return err
	}
	return cause
}

func (d *GroupDispatcher) dispatchAttemptLimit(row sqlc.CtxGroupDispatch, cause error) int64 {
	if row.ResultMessageID != "" && (isAcceptedPublishRecovery(row, cause) || row.PublishedAt.Valid) {
		return acceptedPublishRecoveryMaxAttempts
	}
	return d.maxAttempts
}

func (d *GroupDispatcher) dispatchFailureLastError(row sqlc.CtxGroupDispatch, cause error) string {
	if isAcceptedPublishRecovery(row, cause) {
		return acceptedPublishRecoveryPrefix + cause.Error()
	}
	return cause.Error()
}

func (d *GroupDispatcher) messageAndState(ctx context.Context, q *sqlc.Queries, messageID string) (sqlc.CtxGroupMessage, sqlc.CtxGroupState, error) {
	message, err := q.GetGroupMessage(ctx, messageID)
	if err != nil {
		return sqlc.CtxGroupMessage{}, sqlc.CtxGroupState{}, fmt.Errorf("get group message: %w", err)
	}
	state, err := q.GetGroupStateByID(ctx, message.GroupID)
	if err != nil {
		return sqlc.CtxGroupMessage{}, sqlc.CtxGroupState{}, fmt.Errorf("get group state: %w", err)
	}
	return message, state, nil
}

type groupResponse struct {
	text      string
	reasoning string
	sessionID string
	events    []pkgchannel.Event
	complete  bool
	err       error
}

// bufferGroupResponse drains the runtime completely before any platform side
// effect. The ceiling is an intentional in-memory limit: use BlobStore spooling
// when a deployment needs responses larger than this.
func (d *GroupDispatcher) bufferGroupResponse(ctx context.Context, stream *pkgchannel.ChatStream) groupResponse {
	response := groupResponse{sessionID: stream.SessionID, complete: true}
	limit := defaultGroupReplyBufferBytes
	var used int
	for evt := range stream.Events {
		if evt.Err != nil {
			response.complete = false
			if response.err == nil {
				response.err = evt.Err
			}
			continue
		}
		encoded, err := json.Marshal(evt)
		if err != nil {
			response.complete = false
			if response.err == nil {
				response.err = fmt.Errorf("encode group reply event: %w", err)
			}
			continue
		}
		used += len(encoded)
		if used > limit {
			response.complete = false
			if response.err == nil {
				response.err = fmt.Errorf("group reply exceeded %d-byte buffer", limit)
			}
			continue
		}
		response.events = append(response.events, evt)
		response.text += evt.Text
		response.reasoning += evt.Reasoning
	}
	if ctx.Err() != nil {
		response.complete = false
		if response.err == nil {
			response.err = ctx.Err()
		}
	}
	return response
}

func backoff(attempts int64) time.Duration {
	if attempts < 1 {
		attempts = 1
	}
	d := time.Duration(attempts) * time.Second
	if d > time.Minute {
		return time.Minute
	}
	return d
}

func nullTime(t time.Time) pgtype.Timestamptz {
	if t.IsZero() {
		return pgtype.Timestamptz{}
	}
	return pgtype.Timestamptz{Time: t.UTC(), Valid: true}
}

func nullStringValue(v pgtype.Text) string {
	if !v.Valid {
		return ""
	}
	return v.String
}
