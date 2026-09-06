package host

import (
	"context"
	"sort"

	"github.com/CherryHQ/stella/internal/plugin"

	pkgplugins "github.com/CherryHQ/stella/pkg/plugins"
)

func (h *Host) BeforeToolCall(ctx context.Context, build pkgplugins.BeforeToolCallContext, snapshot plugin.Snapshot) (pkgplugins.BeforeToolCallResult, error) {
	h.mu.RLock()
	regs := make([]pkgplugins.BeforeToolCallSpec, 0, len(h.beforeToolRegs))
	for _, reg := range h.beforeToolRegs {
		regs = append(regs, reg)
	}
	h.mu.RUnlock()
	sort.Slice(regs, func(i, j int) bool { return regs[i].Name < regs[j].Name })

	args := cloneMap(build.Arguments)
	var blocked bool
	var blockMsg string
	for _, reg := range regs {
		if reg.Run == nil {
			continue
		}

		state, enabled, err := snapshotState(snapshot, reg.PluginID)
		if err != nil {
			return pkgplugins.BeforeToolCallResult{}, err
		}
		if !enabled {
			continue
		}

		result, err := reg.Run(ctx, pkgplugins.BeforeToolCallContext{
			Platform:   h.platform(reg.PluginID),
			State:      state,
			SessionID:  build.SessionID,
			Channel:    build.Channel,
			UserID:     build.UserID,
			AgentID:    build.AgentID,
			ToolName:   build.ToolName,
			ToolCallID: build.ToolCallID,
			Arguments:  cloneMap(args),
		})
		if err != nil {
			return pkgplugins.BeforeToolCallResult{}, err
		}
		if result.Arguments != nil {
			args = cloneMap(result.Arguments)
		}
		if result.Block {
			blocked = true
			blockMsg = result.BlockMessage
			break
		}
	}

	return pkgplugins.BeforeToolCallResult{
		Arguments:    args,
		Block:        blocked,
		BlockMessage: blockMsg,
	}, nil
}

func (h *Host) AfterToolResult(ctx context.Context, build pkgplugins.AfterToolResultContext, snapshot plugin.Snapshot) (pkgplugins.AfterToolResult, error) {
	h.mu.RLock()
	regs := make([]pkgplugins.AfterToolResultSpec, 0, len(h.afterToolRegs))
	for _, reg := range h.afterToolRegs {
		regs = append(regs, reg)
	}
	h.mu.RUnlock()
	sort.Slice(regs, func(i, j int) bool { return regs[i].Name < regs[j].Name })

	resultText := build.Result
	isError := build.IsError
	resultChanged := false
	errorChanged := false
	for _, reg := range regs {
		if reg.Run == nil {
			continue
		}

		state, enabled, err := snapshotState(snapshot, reg.PluginID)
		if err != nil {
			return pkgplugins.AfterToolResult{}, err
		}
		if !enabled {
			continue
		}

		mutation, err := reg.Run(ctx, pkgplugins.AfterToolResultContext{
			Platform:   h.platform(reg.PluginID),
			State:      state,
			SessionID:  build.SessionID,
			Channel:    build.Channel,
			UserID:     build.UserID,
			AgentID:    build.AgentID,
			ToolName:   build.ToolName,
			ToolCallID: build.ToolCallID,
			Arguments:  cloneMap(build.Arguments),
			Result:     resultText,
			IsError:    isError,
			Duration:   build.Duration,
		})
		if err != nil {
			return pkgplugins.AfterToolResult{}, err
		}
		if mutation.Result != nil {
			resultText = *mutation.Result
			resultChanged = true
		}
		if mutation.IsError != nil {
			isError = *mutation.IsError
			errorChanged = true
		}
	}

	var result *string
	if resultChanged {
		result = &resultText
	}
	var isErr *bool
	if errorChanged {
		isErr = &isError
	}
	return pkgplugins.AfterToolResult{
		Result:  result,
		IsError: isErr,
	}, nil
}
