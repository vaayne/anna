package host

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"

	"github.com/CherryHQ/stella/internal/plugin"
	"github.com/CherryHQ/stella/pkg/hooks"
	pkgplugins "github.com/CherryHQ/stella/pkg/plugins"
	"github.com/CherryHQ/stella/pkg/tools"
)

func (h *Host) BuildEnabledTools(ctx context.Context, bc pkgplugins.ToolBuildContext, snapshot plugin.Snapshot) (_ []tools.Tool, resultErr error) {
	h.mu.RLock()
	regs := make([]pkgplugins.ToolSpec, 0, len(h.toolRegs))
	for _, reg := range h.toolRegs {
		regs = append(regs, reg)
	}
	h.mu.RUnlock()
	sort.Slice(regs, func(i, j int) bool { return regs[i].Name < regs[j].Name })
	var out []tools.Tool
	defer func() {
		if resultErr == nil {
			return
		}
		for _, built := range out {
			if closer, ok := built.(io.Closer); ok {
				resultErr = errors.Join(resultErr, closer.Close())
			}
		}
	}()
	for _, reg := range regs {
		if reg.Build == nil {
			continue
		}
		_, enabled, err := snapshotState(snapshot, reg.PluginID)
		if err != nil {
			return nil, err
		}
		if !enabled {
			continue
		}
		t, err := reg.Build(pkgplugins.ToolContext{
			Platform: h.platform(reg.PluginID),
			Runtime:  bc.Runtime,
		})
		if t != nil {
			out = append(out, t)
		}
		if err != nil {
			return nil, fmt.Errorf("build plugin tool %q: %w", reg.Name, err)
		}
	}
	return out, nil
}

func (h *Host) BuildEnabledHooks(ctx context.Context, toolsBinDir string, snapshot plugin.Snapshot) (_ []hooks.HookPlugin, resultErr error) {
	h.mu.RLock()
	regs := make([]pkgplugins.HookSpec, 0, len(h.hookRegs))
	for _, reg := range h.hookRegs {
		regs = append(regs, reg)
	}
	h.mu.RUnlock()
	sort.Slice(regs, func(i, j int) bool { return regs[i].Name < regs[j].Name })
	var out []hooks.HookPlugin
	defer func() {
		if resultErr == nil {
			return
		}
		for _, built := range out {
			if closer, ok := built.(io.Closer); ok {
				resultErr = errors.Join(resultErr, closer.Close())
			}
		}
	}()
	for _, reg := range regs {
		state, enabled, err := snapshotState(snapshot, reg.PluginID)
		if err != nil {
			return nil, err
		}
		if !enabled || reg.Build == nil {
			continue
		}
		item, err := reg.Build(pkgplugins.HookContext{
			Platform:    h.platform(reg.PluginID),
			State:       state,
			ToolsBinDir: toolsBinDir,
		})
		if item != nil {
			out = append(out, item)
		}
		if err != nil {
			return nil, fmt.Errorf("build plugin hook %q: %w", reg.Name, err)
		}
	}
	return out, nil
}
