package host

import (
	"context"
	"sync"

	pkgchannel "github.com/CherryHQ/stella/pkg/channel"
	pkgplugins "github.com/CherryHQ/stella/pkg/plugins"
)

// WithNotificationService injects the narrow notification service available to plugins.
func WithNotificationService(service pkgplugins.Notifier) Option {
	return func(h *Host) {
		h.notifications = service
	}
}

// SetNotificationService updates the notification service extension after host construction.
func (h *Host) SetNotificationService(service pkgplugins.Notifier) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.requireUnsealedLocked("SetNotificationService")
	h.notifications = service
}

// Notifications returns the injected notification service, if any.
func (h *Host) Notifications() pkgplugins.Notifier {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.notifications
}

// WithStateStore injects the host plugin state backend available to plugins.
func WithStateStore(store StateStoreBackend) Option {
	return func(h *Host) {
		h.stateStore = store
	}
}

// SetStateStore updates the plugin state backend after host construction.
func (h *Host) SetStateStore(store StateStoreBackend) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.requireUnsealedLocked("SetStateStore")
	h.stateStore = store
}

// StateStore returns the injected plugin state backend, if any.
func (h *Host) StateStore() StateStoreBackend {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.stateStore
}

// WithAuthService injects the narrow auth directory available to plugins.
func WithAuthService(service pkgplugins.Auth) Option {
	return func(h *Host) {
		h.authService = service
	}
}

// SetAuthService updates the auth service extension after host construction.
func (h *Host) SetAuthService(service pkgplugins.Auth) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.requireUnsealedLocked("SetAuthService")
	h.authService = service
}

// Auth returns the injected auth service, if any.
func (h *Host) Auth() pkgplugins.Auth {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.authService
}

// WithChannelRuntimeServices injects the narrow services used by managed channel runtimes.
func WithChannelRuntimeServices(services pkgplugins.ChannelPlatform) Option {
	return func(h *Host) {
		h.channelRuntime = services
	}
}

// SetChannelRuntimeServices updates the channel runtime service extension after host construction.
func (h *Host) SetChannelRuntimeServices(services pkgplugins.ChannelPlatform) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.requireUnsealedLocked("SetChannelRuntimeServices")
	h.channelRuntime = services
}

// ChannelRuntime returns the injected channel runtime service extension, if any.
func (h *Host) ChannelRuntime() pkgplugins.ChannelPlatform {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.channelRuntime
}

// WithListenerCap injects the common scope gate used by channel runtimes.
// The gate is deliberately a function: the host only needs a boolean decision
// and must not own policy resolution or credential data.
func WithListenerCap(cap ListenerCap) Option {
	return func(h *Host) {
		h.listenerCap = cap
	}
}

// SetListenerCap updates the listener gate before the host is sealed.
func (h *Host) SetListenerCap(cap ListenerCap) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.requireUnsealedLocked("SetListenerCap")
	h.listenerCap = cap
}

// ListenerCap returns the currently injected listener gate.
func (h *Host) ListenerCap() ListenerCap {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.listenerCap
}

// ChannelPlatform is a mutable host extension bag for managed channel runtimes.
type ChannelPlatform struct {
	mu            sync.RWMutex
	parent        context.Context
	handler       pkgchannel.Handler
	notifications pkgplugins.ChannelRegistry
	wrapHandler   pkgplugins.HandlerWrapper
	buildVersion  string
}

func NewChannelRuntimeServices() *ChannelPlatform {
	return &ChannelPlatform{}
}

func (s *ChannelPlatform) Set(parent context.Context, handler pkgchannel.Handler, notifications pkgplugins.ChannelRegistry, wrapper pkgplugins.HandlerWrapper) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.parent = parent
	s.handler = handler
	s.notifications = notifications
	s.wrapHandler = wrapper
}

// AccountEnrollmentBackend accepts a host-selected namespace separately from
// plugin-provided profile data.
type AccountEnrollmentBackend interface {
	EnrollAccount(context.Context, string, pkgchannel.EnrollmentRequest) error
}

func (h *Host) SetAccountEnrollment(backend AccountEnrollmentBackend) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.requireUnsealedLocked("SetAccountEnrollment")
	h.enrollment = backend
}

func (s *ChannelPlatform) ParentContext() context.Context {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.parent
}

func (s *ChannelPlatform) Handler() pkgchannel.Handler {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.handler
}

func (s *ChannelPlatform) Notifications() pkgplugins.ChannelRegistry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.notifications
}

func (s *ChannelPlatform) WrapHandler() pkgplugins.HandlerWrapper {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.wrapHandler
}

func (s *ChannelPlatform) SetBuildVersion(version string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.buildVersion = version
}

func (s *ChannelPlatform) BuildVersion() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.buildVersion
}
