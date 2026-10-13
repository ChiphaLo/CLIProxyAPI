package access

import (
	"context"
	"fmt"
	"net/http"
	"sync"
)

// Manager coordinates authentication providers.
type Manager struct {
	mu        sync.RWMutex
	providers []Provider
	// pinnedProvider is a host-owned authentication boundary. Plugin/config
	// registry reconciliation cannot add fallback providers around this policy.
	pinnedProvider Provider
}

// NewManager constructs an empty manager.
func NewManager() *Manager {
	return &Manager{}
}

// SetProviders replaces the active provider list.
func (m *Manager) SetProviders(providers []Provider) {
	if m == nil {
		return
	}
	cloned := make([]Provider, len(providers))
	copy(cloned, providers)
	m.mu.Lock()
	if m.pinnedProvider != nil {
		cloned = []Provider{m.pinnedProvider}
	}
	m.providers = cloned
	m.mu.Unlock()
}

// PinProvider selects one host-owned provider for all future reconciliations.
// Normal managers remain registry-driven. This opt-in must be configured by
// the embedding host, never by client headers, bodies or provider YAML.
func (m *Manager) PinProvider(provider Provider) error {
	if m == nil || provider == nil {
		return fmt.Errorf("a host authentication provider is required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pinnedProvider = provider
	m.providers = []Provider{provider}
	return nil
}

// Providers returns a snapshot of the active providers.
func (m *Manager) Providers() []Provider {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	snapshot := make([]Provider, len(m.providers))
	copy(snapshot, m.providers)
	return snapshot
}

// Authenticate evaluates providers until one succeeds.
func (m *Manager) Authenticate(ctx context.Context, r *http.Request) (*Result, *AuthError) {
	if m == nil {
		return nil, nil
	}
	providers := m.Providers()
	if len(providers) == 0 {
		return nil, nil
	}

	var (
		missing bool
		invalid bool
	)

	for _, provider := range providers {
		if provider == nil {
			continue
		}
		res, authErr := provider.Authenticate(ctx, r)
		if authErr == nil {
			return res, nil
		}
		if IsAuthErrorCode(authErr, AuthErrorCodeNotHandled) {
			continue
		}
		if IsAuthErrorCode(authErr, AuthErrorCodeNoCredentials) {
			missing = true
			continue
		}
		if IsAuthErrorCode(authErr, AuthErrorCodeInvalidCredential) {
			invalid = true
			continue
		}
		return nil, authErr
	}

	if invalid {
		return nil, NewInvalidCredentialError()
	}
	if missing {
		return nil, NewNoCredentialsError()
	}
	return nil, NewNoCredentialsError()
}
