package webgui

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"
)

type probeHelperFunc func(context.Context, string, map[string]any) (map[string]any, error)

func (f probeHelperFunc) Call(ctx context.Context, op string, payload map[string]any) (map[string]any, error) {
	return f(ctx, op, payload)
}
func TestProviderProbePreservesExactProxyAndReleasesStateLock(t *testing.T) {
	s, _ := fixtureServer(t)
	state, err := s.state()
	if err != nil {
		t.Fatal(err)
	}
	state["providers"] = []any{map[string]any{"id": "fixture", "baseUrl": "https://api.example/v1", "proxyUrl": "socks5h://172.17.0.1:10808", "models": []any{}}}
	if err := s.saveState(state); err != nil {
		t.Fatal(err)
	}
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	s.options.Helper = probeHelperFunc(func(_ context.Context, op string, payload map[string]any) (map[string]any, error) {
		if op != "probe_provider_models" || payload["proxy_url"] != "socks5h://172.17.0.1:10808" || payload["proxy_policy"] != "cpa-egress" {
			t.Error("exact validated proxy tuple was not sent to helper")
		}
		close(entered)
		<-release
		return map[string]any{"ok": true, "models": []string{"fixture-model"}}, nil
	})
	go func() {
		defer close(done)
		s.providers(httptest.NewRecorder(), httptest.NewRequest("POST", "/api/providers/fixture/models", nil), "/api/providers/fixture/models", map[string]any{})
	}()
	<-entered
	if !s.mu.TryLock() {
		close(release)
		<-done
		t.Fatal("slow provider probe blocks subscription state access")
	}
	s.mu.Unlock()
	close(release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("probe handler did not finish")
	}
}
