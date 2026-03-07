package registry

import "sync"

var (
	defaultRegistry *Registry
	defaultOnce     sync.Once
)

// Default returns the process-wide provider registry singleton.
func Default() *Registry {
	defaultOnce.Do(func() {
		defaultRegistry = New()
	})
	return defaultRegistry
}
