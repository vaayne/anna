package host

import (
	"context"
	"sort"

	"github.com/CherryHQ/stella/internal/plugin"

	pkgplugins "github.com/CherryHQ/stella/pkg/plugins"
)

// EnabledToolSpecs returns metadata for plugin tools that are visible in the
// current server state. It does not build tools or touch runtime sandboxes.
func (h *Host) EnabledToolSpecs(ctx context.Context, snapshot plugin.Snapshot) ([]pkgplugins.ToolSpec, error) {
	h.mu.RLock()
	regs := make([]pkgplugins.ToolSpec, 0, len(h.toolRegs))
	for _, reg := range h.toolRegs {
		regs = append(regs, reg)
	}
	h.mu.RUnlock()
	sort.Slice(regs, func(i, j int) bool { return regs[i].Name < regs[j].Name })

	out := make([]pkgplugins.ToolSpec, 0, len(regs))
	for _, reg := range regs {
		_, enabled, err := snapshotState(snapshot, reg.PluginID)
		if err != nil {
			return nil, err
		}
		if !enabled {
			continue
		}
		out = append(out, reg)
	}
	return out, nil
}
