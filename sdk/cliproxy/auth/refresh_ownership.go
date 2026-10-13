package auth

// DisableCredentialRefresh is a startup-only opt-in for shadow validation.
// It prevents scheduled, 401-triggered and explicitly forced refresh calls.
// Call before starting the service; it does not cancel already-running refresh.
// Normal SDK runtimes retain upstream's refresh behavior by default.
func (m *Manager) DisableCredentialRefresh() {
	if m != nil {
		m.credentialRefreshDisabled.Store(true)
	}
}
