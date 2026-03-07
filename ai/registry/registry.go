package registry

import (
	"sync"

	"github.com/vaayne/anna/ai/stream"
)

// Registry stores providers by API name.
type Registry struct {
	mu        sync.RWMutex
	providers map[string]stream.Provider
}

// New creates an empty provider registry.
func New() *Registry {
	return &Registry{providers: make(map[string]stream.Provider)}
}

// Register stores a provider by API key.
func (r *Registry) Register(provider stream.Provider) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.providers[provider.API()] = provider
}

// Get resolves a provider by API key.
func (r *Registry) Get(api string) (stream.Provider, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	provider, ok := r.providers[api]
	return provider, ok
}
