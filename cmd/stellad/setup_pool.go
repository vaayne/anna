package main

import (
	"context"

	"github.com/CherryHQ/stella/internal/agent"
	"github.com/CherryHQ/stella/internal/plugin"
	"github.com/CherryHQ/stella/internal/plugin/host"
	coreagent "github.com/CherryHQ/stella/pkg/agent"
	pkgplugins "github.com/CherryHQ/stella/pkg/plugins"
)

// lazyServiceManager resolves the underlying ServiceManager at call time.
// Used when ServiceManager is not yet set at struct-initialization time.
type lazyServiceManager struct {
	get func() agent.ServiceManager
}

func (l *lazyServiceManager) GetService(agentID string) *agent.Service {
	sm := l.get()
	if sm == nil {
		return nil
	}
	return sm.GetService(agentID)
}

func (l *lazyServiceManager) Default() *agent.Service {
	sm := l.get()
	if sm == nil {
		return nil
	}
	return sm.Default()
}

func buildToolLifecycle(phost *host.Host, snapshot plugin.Snapshot) *coreagent.ToolLifecycle {
	return &coreagent.ToolLifecycle{
		BeforeCall: func(ctx context.Context, call coreagent.ToolCallContext) (coreagent.ToolCallMutation, error) {
			result, err := phost.BeforeToolCall(ctx, pkgplugins.BeforeToolCallContext{
				SessionID:  call.SessionID,
				Channel:    call.Channel,
				UserID:     call.UserID,
				AgentID:    call.AgentID,
				ToolName:   call.ToolName,
				ToolCallID: call.ToolCallID,
				Arguments:  call.Arguments,
			}, snapshot)
			if err != nil {
				return coreagent.ToolCallMutation{}, err
			}
			return coreagent.ToolCallMutation{
				Arguments:    result.Arguments,
				Block:        result.Block,
				BlockMessage: result.BlockMessage,
			}, nil
		},
		AfterCall: func(ctx context.Context, result coreagent.ToolResultContext) (coreagent.ToolResultMutation, error) {
			mutation, err := phost.AfterToolResult(ctx, pkgplugins.AfterToolResultContext{
				SessionID:  result.SessionID,
				Channel:    result.Channel,
				UserID:     result.UserID,
				AgentID:    result.AgentID,
				ToolName:   result.ToolName,
				ToolCallID: result.ToolCallID,
				Arguments:  result.Arguments,
				Result:     result.Result,
				IsError:    result.IsError,
				Duration:   result.Duration,
			}, snapshot)
			if err != nil {
				return coreagent.ToolResultMutation{}, err
			}
			return coreagent.ToolResultMutation{
				Result:  mutation.Result,
				IsError: mutation.IsError,
			}, nil
		},
	}
}
