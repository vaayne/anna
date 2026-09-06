package mcp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/CherryHQ/stella/internal/plugin"
	"github.com/CherryHQ/stella/pkg/ai"
	pkgtools "github.com/CherryHQ/stella/pkg/tools"
)

// Per-call timeout bounds. metadata.call_timeout_seconds on the registration
// overrides the default within [1, 300].
const (
	defaultCallTimeout     = 60 * time.Second
	maxCallTimeoutSeconds  = 300
	credentialRejectedHint = "credential rejected; reconnect in the Web UI"
)

// serverConn is the one lazily opened session shared by every proxy of a
// server, so a 30-tool server costs one MCP session, not thirty. Close is
// idempotent because the tool registry closes each proxy on teardown.
type serverConn struct {
	mu     sync.Mutex
	svc    *Service
	reg    Registration
	owner  CredentialOwner
	client RemoteClient
}

func (c *serverConn) get(ctx context.Context) (RemoteClient, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.client != nil {
		return c.client, nil
	}
	client, err := c.svc.connectClient(ctx, c.reg, c.owner)
	if err != nil {
		return nil, err
	}
	c.client = client
	return client, nil
}

func (c *serverConn) Close() error {
	c.mu.Lock()
	client := c.client
	c.client = nil
	c.mu.Unlock()
	if client == nil {
		return nil
	}
	return client.Close()
}

// toolProxy adapts one remote MCP tool to Stella's Tool interface. The
// agent-facing name is namespaced by the plugin namespace (<namespace>__<tool>); calls are
// proxied to the tool's original remote name. A persisted catalog can create
// the proxy without an open session, so Execute lazily connects on first use
// through the server's shared connection.
type toolProxy struct {
	mu sync.Mutex

	svc  *Service
	reg  Registration
	conn *serverConn // nil until first use when not injected by the provider

	def        pkgtools.Definition
	remoteName string
}

func (t *toolProxy) Definition() pkgtools.Definition { return t.def }

// PluginToolIdentity exposes the durable owner pair to the runner. The
// runner still checks the pair against its authority-bound snapshot and the
// proxy's exported definition before registering it.
func (t *toolProxy) PluginToolIdentity() (pluginID, localToolName string, ok bool) {
	if t == nil || t.reg.PluginID == "" || t.reg.Namespace == "" {
		return "", "", false
	}
	localToolName = SanitizeIdent(t.remoteName, "tool")
	if _, err := plugin.ExportedToolName(t.reg.Namespace, localToolName); err != nil {
		return "", "", false
	}
	return t.reg.PluginID, localToolName, true
}

// ExecuteContent runs the call and converts MCP content blocks to ai blocks:
// text stays text, images become ai.ImageContent, anything else is JSON-encoded
// as text so nothing is silently lost.
func (t *toolProxy) ExecuteContent(ctx context.Context, args map[string]any) ([]ai.ContentBlock, error) {
	res, err := t.call(ctx, args)
	if err != nil {
		return nil, err
	}
	return contentBlocks(res), nil
}

// Execute keeps the string Tool contract via the same content path.
func (t *toolProxy) Execute(ctx context.Context, args map[string]any) (string, error) {
	blocks, err := t.ExecuteContent(ctx, args)
	return flattenBlocks(blocks), err
}

func (t *toolProxy) call(ctx context.Context, args map[string]any) (*mcpsdk.CallToolResult, error) {
	client, err := t.ensureClient(ctx)
	if err != nil {
		return nil, err
	}
	callCtx, cancel := context.WithTimeout(ctx, callTimeout(t.reg))
	defer cancel()
	res, err := client.CallTool(callCtx, t.remoteName, args)
	if err != nil {
		// A plain timeout is the model's problem to retry; only a credential
		// rejection is a durable server state worth persisting.
		if isCredentialRejection(err) {
			owner := CredentialOwner{}
			if t.conn != nil {
				owner = t.conn.owner
			}
			_ = t.svc.setStatusForRegistration(ctx, t.reg, owner, StatusNeedsAuth, credentialRejectedHint)
			return nil, fmt.Errorf("mcp: call tool %q: %s", t.remoteName, credentialRejectedHint)
		}
		return nil, err
	}
	if res.IsError {
		text := flattenBlocks(contentBlocks(res))
		return nil, fmt.Errorf("mcp: tool %q returned an error: %s", t.remoteName, text)
	}
	return res, nil
}

func (t *toolProxy) ensureClient(ctx context.Context) (RemoteClient, error) {
	t.mu.Lock()
	if t.conn == nil {
		t.conn = &serverConn{svc: t.svc, reg: t.reg, owner: t.svc.CredentialOwner(t.reg, "")}
	}
	conn := t.conn
	t.mu.Unlock()
	return conn.get(ctx)
}

// Close ends the shared server session if one was opened. Idempotent, so the
// registry may call it once per tool sharing the session.
func (t *toolProxy) Close() error {
	t.mu.Lock()
	conn := t.conn
	t.mu.Unlock()
	if conn == nil {
		return nil
	}
	return conn.Close()
}

// callTimeout resolves the per-call timeout from the registration's metadata
// JSONB. Values outside [1, 300] fall back to the default / cap.
func callTimeout(reg Registration) time.Duration {
	v, ok := reg.Metadata["call_timeout_seconds"]
	if !ok {
		return defaultCallTimeout
	}
	f, ok := v.(float64) // JSON numbers decode as float64
	if !ok {
		return defaultCallTimeout
	}
	seconds := int(f)
	if seconds < 1 {
		return defaultCallTimeout
	}
	// Clamp instead of rejecting so a typo in metadata degrades to the cap, not to a broken tool.
	if seconds > maxCallTimeoutSeconds {
		seconds = maxCallTimeoutSeconds
	}
	return time.Duration(seconds) * time.Second
}

// contentBlocks converts MCP result content to ai content blocks.
func contentBlocks(res *mcpsdk.CallToolResult) []ai.ContentBlock {
	var out []ai.ContentBlock
	for _, block := range res.Content {
		switch c := block.(type) {
		case *mcpsdk.TextContent:
			out = append(out, ai.TextContent{Text: c.Text})
		case *mcpsdk.ImageContent:
			// The SDK base64-decodes the wire data into c.Data, so re-encode to
			// the base64 string ai.ImageContent carries.
			out = append(out, ai.ImageContent{Data: base64.StdEncoding.EncodeToString(c.Data), MimeType: c.MIMEType})
		default:
			if raw, err := json.Marshal(block); err == nil {
				out = append(out, ai.TextContent{Text: string(raw)})
			}
		}
	}
	return out
}

// flattenBlocks renders content blocks as plain text: text joined with
// newlines, images summarized, anything else JSON-encoded.
func flattenBlocks(blocks []ai.ContentBlock) string {
	var b strings.Builder
	for _, block := range blocks {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		switch c := block.(type) {
		case ai.TextContent:
			b.WriteString(c.Text)
		case ai.ImageContent:
			fmt.Fprintf(&b, "[image: %s]", c.MimeType)
		default:
			if raw, err := json.Marshal(block); err == nil {
				b.Write(raw)
			}
		}
	}
	return b.String()
}
