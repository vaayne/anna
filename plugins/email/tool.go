package email

import (
	"context"
	"fmt"
	"time"

	"github.com/CherryHQ/stella/pkg/tools"
)

const (
	defaultToolPageSize = 20
	maxToolPageSize     = 100
)

// ListTool is the email action that lists what this agent can reach. Error
// prose points at it, so a rename shows up here rather than in a string.
const ListTool = "email__message_list"

// actionDescriptions is the model-facing description per generated tool. A
// split tool's schema is exact, so each description only says what the call
// does and what it costs.
var actionDescriptions = map[string]string{
	"account_list": "List this user's configured email accounts and which one is the default. Never returns passwords or any other EMAIL_CONFIG contents.",
	"message_list": "List message envelopes in one folder, filtered by sender, subject, unread state, or date. Envelopes only; call email__message_read for a body.",
	"message_read": "Read one message by uid and folder. Long bodies are truncated for token safety.",
	"message_send": "Send one mail as this user. It leaves the server immediately and cannot be recalled, so idempotency_key is required: reuse a key only when retrying the exact same send.",
}

type ToolDeps struct {
	Authorize func(context.Context, string) (context.Context, error)
	MapError  func(string, string, error) error
}

// Tool is one generated email action. The tool name carries the action, so the
// provider validates arguments against an exact schema before dispatch.
type Tool struct {
	spec ActionTool
	svc  *Service
	deps ToolDeps
}

// NewTool builds one email action tool. Authorization is supplied by the
// system adapter, so the plugin never constructs or accepts an Authority.
func NewTool(svc *Service, spec ActionTool, deps ToolDeps) *Tool {
	return &Tool{spec: spec, svc: svc, deps: deps}
}

func (t *Tool) Definition() tools.Definition {
	return t.spec.Definition(actionDescriptions[t.spec.Action])
}

func (t *Tool) Execute(ctx context.Context, args map[string]any) (string, error) {
	if t == nil || t.svc == nil {
		return "", fmt.Errorf("email service is unavailable — try again later")
	}
	if t.deps.Authorize == nil {
		return "", fmt.Errorf("email authorization is unavailable — try again later")
	}
	authorizedCtx, err := t.deps.Authorize(ctx, t.spec.Name)
	if err != nil {
		return "", err
	}
	out, err := Dispatch(authorizedCtx, emailHandler{svc: t.svc, ctx: authorizedCtx}, t.spec.Action, args)
	if err != nil {
		return "", t.mapError(err)
	}
	return tools.MarshalResult(out)
}

func (t *Tool) mapError(err error) error {
	if t.deps.MapError == nil {
		return err
	}
	return t.deps.MapError(t.spec.Name, ListTool, err)
}

type emailHandler struct {
	svc *Service
	ctx context.Context
}

func (h emailHandler) access() (Access, error) {
	return h.svc.Access(h.ctx)
}

func (h emailHandler) AccountList(ctx context.Context, _ AccountListInput) (any, error) {
	acc, err := h.access()
	if err != nil {
		return nil, err
	}
	accounts, err := acc.Accounts(ctx)
	if err != nil {
		return nil, err
	}
	return map[string]any{"accounts": accounts.Accounts, "default": accounts.Default}, nil
}

func (h emailHandler) MessageList(ctx context.Context, in MessageListInput) (any, error) {
	limit := in.Limit
	if limit == 0 {
		limit = defaultToolPageSize
	}
	if limit < 1 || limit > maxToolPageSize {
		return nil, fmt.Errorf("invalid limit — use a value between 1 and %d", maxToolPageSize)
	}
	opts := ListOptions{Limit: limit, Folder: in.Folder, From: in.From, Subject: in.Subject}
	if in.Unread != nil {
		opts.Unread = *in.Unread
	}
	if in.Since != "" {
		if t, err := time.Parse("2006-01-02", in.Since); err == nil {
			opts.Since = &t
		}
	}
	if in.Before != "" {
		if t, err := time.Parse("2006-01-02", in.Before); err == nil {
			opts.Before = &t
		}
	}
	acc, err := h.access()
	if err != nil {
		return nil, err
	}
	msgs, err := acc.List(ctx, in.Account, opts)
	if err != nil {
		return nil, err
	}
	items := make([]emailEnvelopeResponse, 0, len(msgs))
	for _, msg := range msgs {
		items = append(items, emailEnvelopeSummary(msg))
	}
	return listResponse[emailEnvelopeResponse]{Items: items, HasMore: false}, nil
}

func (h emailHandler) MessageRead(ctx context.Context, in MessageReadInput) (any, error) {
	acc, err := h.access()
	if err != nil {
		return nil, err
	}
	msg, err := acc.Read(ctx, in.Account, in.Folder, uint32(in.Uid))
	if err != nil {
		return nil, err
	}
	return emailMessageSummary(msg), nil
}

func (h emailHandler) MessageSend(ctx context.Context, in MessageSendInput) (any, error) {
	opts := SendOptions{To: stringItems(in.To), Cc: stringItems(in.Cc), Bcc: stringItems(in.Bcc), Subject: in.Subject, Body: in.Body, From: in.From, ReplyTo: in.ReplyTo, InReplyTo: in.InReplyTo}
	if in.Html != nil {
		opts.HTML = *in.Html
	}
	acc, err := h.access()
	if err != nil {
		return nil, err
	}
	result, err := acc.Send(ctx, in.Account, opts, in.IdempotencyKey)
	if err != nil {
		return nil, err
	}
	if result.Duplicate {
		return map[string]any{"status": result.Status, "duplicate_suppressed": true}, nil
	}
	return map[string]any{"status": result.Status}, nil
}

type emailEnvelopeResponse struct {
	UID         uint32 `json:"uid"`
	From        string `json:"from"`
	Subject     string `json:"subject"`
	Date        string `json:"date"`
	Snippet     string `json:"snippet,omitempty"`
	Attachments bool   `json:"has_attachments,omitempty"`
}
type emailMessageResponse struct {
	Envelope  emailEnvelopeResponse `json:"envelope"`
	Body      string                `json:"body"`
	Truncated bool                  `json:"truncated"`
	Note      string                `json:"note,omitempty"`
}
type listResponse[T any] struct {
	Items         []T    `json:"items"`
	HasMore       bool   `json:"has_more"`
	NextPageToken string `json:"next_page_token,omitempty"`
}

func emailEnvelopeSummary(msg Envelope) emailEnvelopeResponse {
	return emailEnvelopeResponse{UID: msg.UID, From: msg.From, Subject: msg.Subject, Date: msg.Date.UTC().Format(time.RFC3339), Attachments: msg.HasAttachments}
}

func emailMessageSummary(msg *Message) emailMessageResponse {
	body := msg.TextBody
	if body == "" {
		body = msg.HTMLBody
	}
	body, truncated := tools.TruncateText(body, 50*1024)
	out := emailMessageResponse{Envelope: emailEnvelopeSummary(msg.Envelope), Body: body, Truncated: truncated}
	if truncated {
		out.Note = "truncated — use the web UI or email client for the full message"
	}
	return out
}

func stringItems(items []any) []string {
	out := make([]string, 0, len(items))
	for _, item := range items {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}
