package agent

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/CherryHQ/stella/internal/memory/memorytest"
	"github.com/CherryHQ/stella/internal/plugin"
	"github.com/CherryHQ/stella/pkg/hooks"
)

type countingHook struct {
	name   string
	closed atomic.Int32
}

func (h *countingHook) Name() string { return h.name }
func (*countingHook) Priority() int  { return 0 }
func (h *countingHook) Close() error { h.closed.Add(1); return nil }

func TestConcurrentReloadPluginHooksRetiresEachGenerationOnce(t *testing.T) {
	pm := NewPoolManager(nil, memorytest.New())
	initial := &countingHook{name: "initial"}
	pm.hookPlugins = []hooks.HookPlugin{initial}
	constructed := make(chan struct{}, 2)
	release := make(chan struct{})
	var next atomic.Int32
	created := make(chan *countingHook, 2)
	pm.pluginHooksBuilder = func(context.Context, plugin.Snapshot) ([]hooks.HookPlugin, error) {
		h := &countingHook{name: fmt.Sprintf("reload-%d", next.Add(1))}
		created <- h
		constructed <- struct{}{}
		<-release
		return []hooks.HookPlugin{h}, nil
	}
	results := make(chan error, 2)
	go func() { results <- pm.ReloadPluginHooks(context.Background()) }()
	go func() { results <- pm.ReloadPluginHooks(context.Background()) }()
	<-constructed
	<-constructed
	close(release)
	for range 2 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	first, second := <-created, <-created
	if err := pm.Close(); err != nil {
		t.Fatal(err)
	}
	for _, h := range []*countingHook{initial, first, second} {
		if got := h.closed.Load(); got != 1 {
			t.Fatalf("hook generation %q close count = %d, want 1", h.name, got)
		}
	}
}
