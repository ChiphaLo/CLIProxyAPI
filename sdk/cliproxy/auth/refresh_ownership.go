package auth

import (
	"context"
	"errors"
)

// SetCredentialRefreshGuard installs a host ownership barrier before loading
// credentials. The returned release function is held across the upstream
// refresh AND its disk persistence, including successful refresh after cancel.
// A denied guard must not mutate credential state. Hosts must acquire their
// process lease before loading shared tokens; this hook does not coordinate
// legacy processes that do not participate in the same ownership protocol.
func (m *Manager) SetCredentialRefreshGuard(guard func(context.Context) (func(), error)) error {
	if m == nil || guard == nil {
		return errors.New("credential refresh guard is required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.credentialRefreshGuard != nil || len(m.auths) != 0 || m.refreshLoop != nil {
		return errors.New("credential refresh guard must be installed once before loading credentials")
	}
	m.credentialRefreshGuard = guard
	return nil
}

// DisableCredentialRefresh is a startup-only opt-in for shadow validation.
// It prevents scheduled, 401-triggered and explicitly forced refresh calls.
// Call before starting the service; it does not cancel already-running refresh.
// Normal SDK runtimes retain upstream's refresh behavior by default.
func (m *Manager) DisableCredentialRefresh() {
	if m != nil {
		m.credentialRefreshDisabled.Store(true)
	}
}
