package access

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

type pinnedFixtureProvider struct{ name, key string }

func (p pinnedFixtureProvider) Identifier() string { return p.name }
func (p pinnedFixtureProvider) Authenticate(_ context.Context, r *http.Request) (*Result, *AuthError) {
	if r.Header.Get("Authorization") == "Bearer "+p.key {
		return &Result{Provider: p.name, Principal: p.name}, nil
	}
	return nil, NewInvalidCredentialError()
}

func TestPinnedHostProviderSurvivesConfigAndPluginReconciliation(t *testing.T) {
	m := NewManager()
	host := pinnedFixtureProvider{name: "host-fixture", key: "host-key"}
	fallback := pinnedFixtureProvider{name: "inline-fixture", key: "old-internal-key"}
	if err := m.PinProvider(host); err != nil {
		t.Fatal(err)
	}
	for _, providers := range [][]Provider{{fallback}, {}, nil, {host, fallback}} {
		m.SetProviders(providers)
		r := httptest.NewRequest("GET", "/v1/models", nil)
		r.Header.Set("Authorization", "Bearer old-internal-key")
		if _, err := m.Authenticate(context.Background(), r); err == nil {
			t.Fatal("reconciliation bypassed host policy")
		}
		r.Header.Set("Authorization", "Bearer host-key")
		if result, err := m.Authenticate(context.Background(), r); err != nil || result.Principal != host.name {
			t.Fatal("reconciliation lost host identity")
		}
	}
}
