package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/CherryHQ/stella/internal/agent/session"
	"github.com/CherryHQ/stella/internal/authz"
	"github.com/CherryHQ/stella/internal/core/agentctx"
	"github.com/CherryHQ/stella/internal/core/agenterr"
	"github.com/CherryHQ/stella/internal/eventlog"
	"github.com/CherryHQ/stella/internal/memory"
	"github.com/CherryHQ/stella/internal/platform/observability"
	"github.com/CherryHQ/stella/internal/sessionmedia"
	"github.com/CherryHQ/stella/pkg/ai"
	"github.com/CherryHQ/stella/pkg/hooks"
	"github.com/CherryHQ/stella/pkg/tools"
)

// ErrChatTimeout is emitted when a chat turn exceeds its deadline.
var ErrChatTimeout = agenterr.ErrChatTimeout

const autoCompactionTimeout = 2 * time.Minute

// BeforeRunFunc is called before each chat turn to inject/override the system
// prompt using the runner's admission-time plugin snapshot.
type BeforeRunFunc func(ctx context.Context, info session.Info, model, msgText, system string, history []ai.Message, pluginContext PluginContext) (systemOut string, err error)

// SnapshotPromptFunc builds a system prompt from the session's snapshot version
// and the plugin context captured with the admitted runner.
type SnapshotPromptFunc func(ctx context.Context, info session.Info, snap memory.SessionSnapshot, pluginContext PluginContext) (string, error)

// chat is retained for direct internal callers and tests. Runtime.ChatAdmitted
// uses chatWithRunner after synchronously selecting a runner at admission.
func (rt *Runtime) chat(ctx context.Context, out chan<- Event, info session.Info, msg MessageContent, co chatOptions) {
	ctx = withSessionIdentity(ctx, info)
	selection, err := rt.getOrCreateReservedRunner(ctx, info, co.model, co.extraTools)
	if err != nil {
		out <- Event{Err: fmt.Errorf("get runner: %w", err)}
		close(out)
		return
	}
	rt.capturePromptBuilders(&selection)
	rt.chatWithRunner(ctx, out, info, msg, co, selection)
}

// chatWithRunner is the goroutine body for Runtime.Chat. The runner was selected and
// reserved synchronously by ChatAdmitted, so a policy invalidation cannot slip
// between admission and runner selection.
func (rt *Runtime) chatWithRunner(ctx context.Context, out chan<- Event, info session.Info, msg MessageContent, co chatOptions, selection runnerSelection) {
	defer rt.cache.releaseReservation(selection.session)

	isGuest := info.GuestID != ""
	ctx = withSessionIdentity(ctx, info)
	// Admission contexts may originate at HTTP or a durable worker. Never let a
	// caller's Authority bleed into this turn or the cached runner it selects.
	ctx = authz.ClearAuthority(ctx)
	if co.hasAuthority {
		ctx = authz.WithAuthority(ctx, co.turnAuthority)
	}
	if info.GroupID != "" {
		// Group turns: carry the group id (not a user) so trusted adapters can mint
		// a confined GroupAgentActor. authz.WithUserID stays unset so runtime
		// identity remains the group (D9).
		if co.hasSpeaker {
			// Attach the speaker as a personalization target only.
			ctx = memory.WithCurrentSpeaker(ctx, co.currentSpeaker)
		}
	}
	ctx = authz.WithAgentID(ctx, info.AgentID)
	ctx = agentctx.WithSessionCallBudget(ctx)
	inputActor := co.inputActor
	if !inputActor.Valid() {
		// Runtime callers predating provenance are human ingress. Keeping the
		// fallback here makes the trusted runtime, not model text, choose it.
		inputActor = eventlog.MessageActor{Type: eventlog.ActorHuman, ID: info.UserID}
		if isGuest {
			inputActor.ID = info.GuestID
		}
	}
	ctx = eventlog.WithMessageActor(ctx, inputActor)
	if info.ProjectID != "" {
		ctx = memory.WithProjectID(ctx, info.ProjectID)
	}
	if co.channel != "" {
		ctx = withChannel(ctx, co.channel)
	}

	memSess, err := info.MemoryScope()
	if err != nil {
		out <- Event{Err: fmt.Errorf("session scope: %w", err)}
		close(out)
		return
	}
	groupSink, hasGroupSink := memory.GroupTurnSinkFrom(ctx)
	// A sink only owns this turn when the turn really is a group turn; a stray
	// sink on a direct session must not swallow the ordinary persist path.
	deferredGroupTurn := hasGroupSink && memSess.GroupID != ""
	deferred := memory.DeferredGroupTurn{
		Session:              memSess,
		TriggerSeq:           memory.GroupSeqFromContext(ctx),
		OriginGroupMessageID: memory.GroupMessageIDFromContext(ctx),
	}
	// This is the only owner of out for a valid turn. Deliver before close gives
	// the dispatcher a happens-before edge after it finishes draining the stream.
	defer func() {
		if deferredGroupTurn {
			groupSink.Deliver(deferred)
		}
		close(out)
	}()

	msgText := MessageText(msg)
	rt.log.Debug("chat started", "session_id", info.ID, "message_len", len(msgText))

	// Fire PreAgentCall hooks for authenticated sessions only.
	chatStart := time.Now()
	var hookPlugins []hooks.HookPlugin
	if !isGuest {
		hookPlugins = rt.hookPlugins()
	}
	hs := hooks.NewHookSet(hookPlugins)
	channelName := co.channel
	bindingID := co.bindingID
	hookMeta := hooks.HookMeta{
		SessionID: info.ID,
		UserID:    info.UserID,
		AgentID:   info.AgentID,
		Channel:   channelName,
		BindingID: bindingID,
	}
	ctx = hooks.WithTelemetryMeta(ctx, hookMeta.Channel, hookMeta.BindingID)
	if !isGuest {
		hs.RunPreAgentCall(ctx, &hooks.PreAgentCallContext{
			HookMeta:   hookMeta,
			MessageLen: len(msgText),
			Channel:    hookMeta.Channel,
		})
	}

	// Auto-compact.
	if rt.needsCompaction(ctx, memSess) {
		rt.log.Info("auto-compaction triggered", "session_id", info.ID)
		compactParent := context.WithoutCancel(ctx)
		if _, synchronous := agentctx.SessionCallFromContext(ctx); synchronous {
			// A Session/delegate call holds its caller and target admission until
			// completion, so source cancellation must also stop compaction.
			compactParent = ctx
		}
		compactCtx, cancel := context.WithTimeout(compactParent, autoCompactionTimeout)
		if summary, err := rt.compact_(compactCtx, memSess); err != nil {
			cancel()
			rt.log.Warn("auto-compaction failed", "session_id", info.ID, "error", err)
		} else {
			cancel()
			rt.log.Info("auto-compaction succeeded", "session_id", info.ID, "summary_len", len(summary))
			// The admission lease owns this runner and its metadata for the
			// full turn. Re-selecting through mutable cache state here would let
			// a concurrent non-terminal reset change beforeRun's model midway.
		}
	}

	// Update last-active and title via SessionManager if available.
	now := time.Now()
	if sm, ok := rt.mem.(memory.SessionManager); ok {
		updated := info
		updated.LastActive = now
		if updated.Title == "" && len(msgText) > 0 {
			updated.Title = autoTitle(msgText)
		}
		saveCtx := ctx
		if !isGuest {
			saveCtx = authz.WithUserID(saveCtx, info.UserID)
		}
		saveCtx = authz.WithAgentID(saveCtx, info.AgentID)
		// TouchActiveInfo, not SaveInfo: a `/new` rotation can archive this
		// session after the turn resolved it, and
		// auto-compaction above widens that window to minutes. SaveInfo would
		// replay the turn-start snapshot's `archived = false` and un-archive a
		// session the chat has already left; only main sessions have a
		// unique-active index to catch that, so a resurrected kind=chat row would
		// silently win its binding's newest-match lookup and pull the chat
		// backwards. TouchActiveInfo makes the check and the write one statement.
		if rec, err := updated.Record(); err != nil {
			rt.log.Warn("skip saving invalid session info", "session_id", info.ID, "error", err)
		} else if applied, err := sm.TouchActiveInfo(saveCtx, rec); err != nil {
			rt.log.Warn("failed to save session info", "session_id", info.ID, "error", err)
		} else if !applied {
			rt.log.Info("session archived mid-turn; skipping session info save", "session_id", info.ID)
		}
	}

	// Assemble history before appending the new user message.
	var history []ai.Message
	assembledOK := false
	assembled, err := rt.mem.Assemble(ctx, memSess, rt.compact.MaxTokens, rt.compact.KeepTail)
	if err != nil {
		rt.log.Warn("memory assemble failed", "session_id", info.ID, "error", err)
		if memSess.GroupID != "" {
			out <- Event{Err: fmt.Errorf("assemble group memory: %w", err)}
			return
		}
	} else {
		history = assembled
		assembledOK = true
	}

	// Resolve system prompt.
	baseSystem := co.systemOverride
	if isGuest {
		// A caller override is another capability surface. Guest runners always
		// use the minimal prompt selected from durable GuestID at construction.
		baseSystem = selection.runner.SystemPrompt()
	}
	if baseSystem == "" {
		baseSystem = selection.runner.SystemPrompt()
		if !isGuest && info.GroupID == "" && selection.snapshotPrompt != nil && info.UserID != "" && info.AgentID != "" {
			// DM per-turn snapshot prompt: rebuild system with frozen memory
			// version. Skipped when systemOverride is set (e.g. delegate custom
			// system).
			sss, ok := rt.mem.(memory.SessionSnapshotStore)
			if ok {
				snap, err := sss.GetOrCreateSessionSnapshot(ctx, info.ID, info.UserID, info.AgentID)
				if err != nil {
					rt.log.Warn("snapshot lookup failed, using base system", "session_id", info.ID, "error", err)
				} else if rebuilt, err := selection.snapshotPrompt(ctx, info, snap, selection.pluginContext); err != nil {
					out <- Event{Err: fmt.Errorf("snapshot prompt: %w", err)}
					return
				} else {
					baseSystem = rebuilt
				}
			}
		}
	}
	if !isGuest && selection.beforeRun != nil {
		systemOut, err := selection.beforeRun(ctx, info, selection.model, msgText, baseSystem, history, selection.pluginContext)
		if err != nil {
			out <- Event{Err: fmt.Errorf("before run: %w", err)}
			return
		}
		if systemOut != "" {
			baseSystem = systemOut
		}
	}
	if baseSystem != "" {
		ctx = withSystemOverride(ctx, baseSystem)
	}

	// Exclude tools if requested.
	if len(co.excludedTools) > 0 {
		ctx = withExcludedTools(ctx, co.excludedTools...)
	}

	// Persist group trigger messages only after the turn succeeds. Otherwise a
	// failed durable dispatch retry would leave the same trigger in history and
	// duplicate it on the next attempt.
	userMsg := ai.UserMessage{Content: msg, Timestamp: time.Now()}
	modelMsg := userMsg
	modelMsg.Content = eventlog.RenderInput(modelMsg.Content, inputActor)
	var storePrefix []ai.Message
	if memSess.GroupID != "" {
		// A group trigger arrives already canonical: ingestion persisted its
		// images as group-owned references, so there is nothing to enrich here.
		// Only the append timing differs, deferred until the turn succeeds.
		if co.hasSpeaker {
			modelMsg.Content = withCurrentSpeakerContext(msg, co.currentSpeaker)
		}
		// The wake reason goes closest to the trigger, above the speaker block:
		// it frames everything that follows.
		modelMsg.Content = withGroupWakeContext(modelMsg.Content, co.groupWake)
		storePrefix = []ai.Message{userMsg}
	} else {
		// Direct internal callers may supply either one image block or the usual
		// ordered block list. Existing canonical refs are not re-enriched.
		var blocks []ai.ContentBlock
		hasCanonicalImage := false
		switch content := userMsg.Content.(type) {
		case ai.ImageContent:
			blocks = []ai.ContentBlock{content}
		case ai.ImageRefContent:
			blocks = []ai.ContentBlock{content}
		case []ai.ContentBlock:
			blocks = content
		}
		if blocks != nil && !isGuest {
			if ai.HasImage(blocks) {
				if rt.sessionImages == nil {
					out <- Event{Err: errors.New("session image enrichment is not configured")}
					return
				}
				owner, err := sessionmedia.SessionOwner(info.UserID, info.GroupID)
				if err != nil {
					out <- Event{Err: fmt.Errorf("resolve session media owner: %w", err)}
					return
				}
				enriched, err := rt.sessionImages.Enrich(ctx, owner, info.AgentID, blocks)
				if err != nil {
					out <- Event{Err: fmt.Errorf("enrich user images: %w", err)}
					return
				}
				blocks = enriched
			}
			hasCanonicalImage = ai.HasImageRef(blocks)
			userMsg.Content = blocks
			modelMsg = userMsg
			modelMsg.Content = eventlog.RenderInput(modelMsg.Content, inputActor)
		}
		if co.inboxID != "" {
			appender, ok := rt.mem.(memory.InboxAppender)
			if !ok {
				out <- Event{Err: errors.New("memory provider does not support durable Session inbox")}
				return
			}
			if err := appender.AppendInboxInput(ctx, memSess, co.inboxID, userMsg); err != nil {
				out <- Event{Err: fmt.Errorf("persist Session inbox input: %w", err)}
				return
			}
		} else if err := rt.mem.Append(ctx, memSess, userMsg); err != nil {
			if hasCanonicalImage {
				out <- Event{Err: fmt.Errorf("persist canonical user message: %w", err)}
				return
			}
			rt.log.Warn("memory append user message failed", "session_id", info.ID, "error", err)
		}
	}

	stream := selection.runner.Chat(ctx, history, modelMsg)
	ownRows, chatErr := rt.streamEvents(ctx, info.ID, memSess, stream, out, hs, hookMeta, chatStart, storePrefix...)
	if deferredGroupTurn {
		deferred.OwnRows = ownRows
		deferred.Complete = chatErr == nil && assembledOK && ctx.Err() == nil
		return
	}
	if chatErr == nil && assembledOK && ctx.Err() == nil && memSess.GroupID != "" {
		if committer, ok := rt.mem.(memory.GroupCursorCommitter); ok {
			commitCtx := context.WithoutCancel(ctx)
			if err := committer.CommitGroupCursor(commitCtx, memSess, memory.GroupSeqFromContext(ctx)); err != nil {
				rt.log.Warn("group cursor commit failed", "session_id", info.ID, "group_id", memSess.GroupID, "error", err)
			}
		}
	}
}

func (rt *Runtime) getOrCreateReservedRunner(ctx context.Context, info session.Info, model string, extraTools []tools.Tool) (runnerSelection, error) {
	attrs := []attribute.KeyValue{
		attribute.String("gen_ai.conversation.id", info.ID),
		attribute.String("stella.agent_id", info.AgentID),
	}
	if channel, ok := agentctx.ChannelFromContext(ctx); ok {
		attrs = append(attrs, attribute.String("stella.chat.channel", observability.ChannelName(channel)))
	}
	if binding, ok := agentctx.ChatBindingFromContext(ctx); ok {
		attrs = append(attrs, attribute.String("stella.chat.binding_id", binding.Channel))
	}
	if info.GuestID == "" {
		attrs = append(attrs, attribute.String("user_id", info.UserID))
	} else {
		attrs = append(attrs, attribute.String("guest_id", info.GuestID))
	}
	if info.ProjectID != "" {
		attrs = append(attrs, attribute.String("project_id", info.ProjectID))
	}
	if model != "" {
		attrs = append(attrs, attribute.String("gen_ai.request.model", model))
	}

	spanCtx, span := otel.Tracer("stella").Start(ctx, "agent.runner_get_or_create", trace.WithAttributes(attrs...))
	defer span.End()

	selection, err := rt.cache.getOrCreateReserved(spanCtx, info, model, "", extraTools...)
	if err != nil {
		observability.RecordSpanError(span, err, "runner lookup failed")
		return runnerSelection{}, err
	}
	if selection.model != "" {
		span.SetAttributes(attribute.String("gen_ai.response.model", selection.model))
	}
	return selection, nil
}

func withSessionIdentity(ctx context.Context, info session.Info) context.Context {
	switch {
	case info.GuestID != "":
		// Guest mode is derived exclusively from durable session metadata. Never
		// mint a Stella user identity for the guest's UUID-shaped owner key.
		return authz.WithGuestID(ctx, info.GuestID)
	case info.GroupID != "":
		// A confined GroupAgentActor needs both ids, and the runner (with its
		// skills prompt) is built at admission, before chatWithRunner attaches
		// the agent id. Carrying it here is what keeps a platform-group turn from
		// failing its skill authorization with authz: invalid actor.
		return authz.WithAgentID(authz.WithGroupID(ctx, info.GroupID), info.AgentID)
	default:
		return authz.WithUserID(ctx, info.UserID)
	}
}

func (rt *Runtime) hookPlugins() []hooks.HookPlugin {
	rt.cache.mu.Lock()
	fn := rt.cache.hooksFn
	rt.cache.mu.Unlock()
	if fn == nil {
		return nil
	}
	return fn()
}

func (rt *Runtime) needsCompaction(ctx context.Context, sess memory.Session) bool {
	if rt.compact.MaxTokens <= 0 {
		return false
	}
	c, ok := rt.mem.(memory.Compactor)
	if !ok {
		return false
	}
	return c.NeedsCompaction(ctx, sess, float64(rt.compact.MaxTokens))
}

func (rt *Runtime) compact_(ctx context.Context, sess memory.Session) (string, error) {
	c, ok := rt.mem.(memory.Compactor)
	if !ok {
		return "", fmt.Errorf("memory provider does not support compaction")
	}
	result, err := c.Compact(ctx, sess, memory.CompactionFull)
	if err != nil {
		return "", fmt.Errorf("memory compact: %w", err)
	}
	rt.log.Info("memory compaction complete",
		"session_id", sess.ID,
		"leaf_summaries", result.LeafSummariesCreated,
		"condensed_summaries", result.CondensedSummariesCreated,
		"tokens_before", result.TokensBefore,
		"tokens_after", result.TokensAfter)
	return fmt.Sprintf("compacted: %d leaf + %d condensed summaries, %d→%d tokens",
		result.LeafSummariesCreated, result.CondensedSummariesCreated,
		result.TokensBefore, result.TokensAfter), nil
}

// sendEvent forwards evt to out unless ctx is done. It returns false when the
// consumer has gone away (ctx cancelled), letting the forwarder stop instead of
// blocking on a channel nobody drains — which would otherwise wedge the upstream
// runner goroutine indefinitely.
func sendEvent(ctx context.Context, out chan<- Event, evt Event) bool {
	if ctx.Err() != nil {
		return false
	}
	select {
	case out <- evt:
		return true
	case <-ctx.Done():
		return false
	}
}

// streamEvents reads runner events, persists messages, and forwards them to the
// caller. A deferred group turn gets its own rows back instead of having them
// persisted here. The caller owns closing out.
func (rt *Runtime) streamEvents(
	ctx context.Context,
	sessionID string,
	memSess memory.Session,
	stream <-chan Event,
	out chan<- Event,
	hs *hooks.HookSet,
	hookMeta hooks.HookMeta,
	chatStart time.Time,
	storePrefix ...ai.Message,
) ([]ai.Message, error) {
	persistCtx := context.WithoutCancel(ctx)
	isGroup := memSess.GroupID != ""
	var chatErr error
	var pendingStores []ai.Message
	var textBuf strings.Builder
	var reasoningBuf strings.Builder
	appendWithPrefix := func(msgs ...ai.Message) error {
		storeMessages := make([]ai.Message, 0, len(storePrefix)+len(msgs))
		storeMessages = append(storeMessages, storePrefix...)
		storeMessages = append(storeMessages, msgs...)
		storePrefix = nil
		if isGroup {
			return rt.mem.Append(persistCtx, memSess, storeMessages...)
		}
		return rt.mem.Append(persistCtx, memSess, storeMessages...)
	}
	storeCurrent := func(msgs ...ai.Message) error {
		if isGroup {
			pendingStores = append(pendingStores, msgs...)
			return nil
		}
		return appendWithPrefix(msgs...)
	}
	flushInterruptedAssistant := func() {
		if isGroup || (textBuf.Len() == 0 && reasoningBuf.Len() == 0) {
			return
		}
		if err := appendWithPrefix(bufferedAssistantMessage(textBuf.String(), reasoningBuf.String())); err != nil {
			rt.log.Warn("memory append interrupted assistant failed", "session_id", sessionID, "error", err)
		}
		textBuf.Reset()
		reasoningBuf.Reset()
	}
	defer func() {
		hs.RunPostAgentCall(ctx, &hooks.PostAgentCallContext{
			HookMeta: hookMeta,
			Duration: time.Since(chatStart),
			Error:    chatErr,
		})
	}()

	for evt := range stream {
		if evt.Err != nil {
			chatErr = evt.Err
			flushInterruptedAssistant()
			// Explicit stop and lifecycle shutdown are normal cancellation paths,
			// not failed turns to surface as an in-band chat error.
			if ctx.Err() != nil && errors.Is(evt.Err, context.Canceled) {
				return nil, ctx.Err()
			}
			if errors.Is(evt.Err, ErrChatTimeout) {
				notice := "I've been working on this for a while and have reached the time limit. Here's where things stand — feel free to send a message to continue or change direction."
				if !isGroup {
					noticeMsg := ai.AssistantMessage{Content: []ai.ContentBlock{ai.TextContent{Text: notice}}}
					if err := rt.mem.Append(persistCtx, memSess, noticeMsg); err != nil {
						rt.log.Warn("memory append timeout notice failed", "session_id", sessionID, "error", err)
					}
				}
				sendEvent(ctx, out, Event{Text: notice})
				return nil, chatErr
			}
			sendEvent(ctx, out, evt)
			return nil, chatErr
		}

		if evt.Store != nil {
			if _, ok := evt.Store.(ai.AssistantMessage); ok {
				textBuf.Reset()
				reasoningBuf.Reset()
			} else if textBuf.Len() > 0 || reasoningBuf.Len() > 0 {
				flush := bufferedAssistantMessage(textBuf.String(), reasoningBuf.String())
				if err := storeCurrent(flush); err != nil {
					rt.log.Warn("memory append text-flush failed", "session_id", sessionID, "error", err)
					return nil, fmt.Errorf("memory append text-flush: %w", err)
				}
				textBuf.Reset()
				reasoningBuf.Reset()
			}
			if err := storeCurrent(evt.Store); err != nil {
				rt.log.Warn("memory append store message failed", "session_id", sessionID, "error", err)
				return nil, fmt.Errorf("memory append store message: %w", err)
			}
		}

		if evt.ToolUse != nil {
			if !sendEvent(ctx, out, evt) {
				chatErr = ctx.Err()
				flushInterruptedAssistant()
				return nil, chatErr
			}
			continue
		}

		if evt.Reasoning != "" {
			reasoningBuf.WriteString(evt.Reasoning)
		}
		if evt.Text != "" {
			textBuf.WriteString(evt.Text)
		}
		if !sendEvent(ctx, out, evt) {
			chatErr = ctx.Err()
			flushInterruptedAssistant()
			return nil, chatErr
		}
	}

	if ctx.Err() != nil {
		flushInterruptedAssistant()
		return nil, ctx.Err()
	}
	if textBuf.Len() > 0 || reasoningBuf.Len() > 0 {
		pendingStores = append(pendingStores, bufferedAssistantMessage(textBuf.String(), reasoningBuf.String()))
	}
	if isGroup {
		if _, deferred := memory.GroupTurnSinkFrom(ctx); deferred {
			ownRows := make([]ai.Message, 0, len(storePrefix)+len(pendingStores))
			ownRows = append(ownRows, storePrefix...)
			ownRows = append(ownRows, pendingStores...)
			return ownRows, nil
		}
		if len(storePrefix) > 0 || len(pendingStores) > 0 {
			if err := appendWithPrefix(pendingStores...); err != nil {
				rt.log.Warn("memory append final message failed", "session_id", sessionID, "error", err)
				return nil, fmt.Errorf("memory append final message: %w", err)
			}
		}
		return nil, nil
	}
	if len(pendingStores) > 0 {
		if err := appendWithPrefix(pendingStores...); err != nil {
			rt.log.Warn("memory append final message failed", "session_id", sessionID, "error", err)
			return nil, fmt.Errorf("memory append final message: %w", err)
		}
	}
	return nil, nil
}

func bufferedAssistantMessage(text, reasoning string) ai.AssistantMessage {
	blocks := make([]ai.ContentBlock, 0, 2)
	if reasoning != "" {
		blocks = append(blocks, ai.ThinkingContent{Thinking: reasoning})
	}
	if text != "" {
		blocks = append(blocks, ai.TextContent{Text: text})
	}
	return ai.AssistantMessage{Content: blocks}
}

// --- context helpers --------------------------------------------------------

func withSystemOverride(ctx context.Context, system string) context.Context {
	return agentctx.WithSystemOverride(ctx, system)
}

func withChannel(ctx context.Context, channel string) context.Context {
	return agentctx.WithChannel(ctx, channel)
}

func withExcludedTools(ctx context.Context, names ...string) context.Context {
	// Child runs must retain every exclusion their ancestor applied. In
	// particular, delegate adds its recursion guard here without restoring a
	// goal worker's control-plane tools.
	names = append(agentctx.ExcludedToolsFromContext(ctx), names...)
	return agentctx.WithExcludedTools(ctx, names...)
}
