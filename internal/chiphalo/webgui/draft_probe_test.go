package webgui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"reflect"
	"strings"
	"testing"
)

func TestUnsavedProviderProbeUsesExplicitKeyWithoutSaving(t *testing.T) {
	for _, protocol := range []string{"chat", "responses", "messages"} {
		t.Run(protocol, func(t *testing.T) {
			s, _ := fixtureServer(t)
			s.options.Resolve = func(context.Context, string) ([]netip.Addr, error) {
				return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
			}
			before, err := s.state()
			if err != nil {
				t.Fatal(err)
			}
			calls := 0
			s.options.Helper = probeHelperFunc(func(_ context.Context, op string, payload map[string]any) (map[string]any, error) {
				calls++
				if op != "probe_provider_models" || payload["api_key"] != "draft-secret" || payload["provider_id"] != "draft-probe" || payload["api_format"] != protocol || payload["proxy_url"] != "socks5h://172.17.0.1:10808" {
					t.Fatal("draft probe did not preserve the explicit connection tuple")
				}
				if !s.mu.TryLock() {
					t.Fatal("probe holds state lock")
				}
				s.mu.Unlock()
				return map[string]any{"ok": true, "models": []string{"test-model"}, "count": 1}, nil
			})
			token, err := s.auth.login("local", "fixture-password")
			if err != nil {
				t.Fatal(err)
			}
			request := func(body string, csrf bool) *httptest.ResponseRecorder {
				r := httptest.NewRequest("POST", "/api/providers/probe", strings.NewReader(body))
				r.Header.Set("Origin", s.options.Origin)
				if csrf {
					r.Header.Set("X-CSRF-Token", s.auth.csrf(token))
				}
				r.AddCookie(&http.Cookie{Name: "cpa_session", Value: token})
				w := httptest.NewRecorder()
				s.ServeHTTP(w, r)
				return w
			}
			body := `{"baseUrl":"https://provider.example/v1","proxyUrl":"socks5h://172.17.0.1:10808","upstreamProtocol":"` + protocol + `","apiKey":"draft-secret"}`
			w := request(body, true)
			if w.Code != 200 || !strings.Contains(w.Body.String(), "test-model") || strings.Contains(w.Body.String(), "draft-secret") {
				t.Fatalf("probe status=%d response=%s", w.Code, w.Body.String())
			}
			if w = request(body, false); w.Code != 403 {
				t.Fatalf("probe bypassed CSRF: %d", w.Code)
			}
			if w = request(strings.ReplaceAll(body, "draft-secret", ""), true); w.Code != 400 {
				t.Fatalf("missing explicit key: %d", w.Code)
			}
			if w = request(strings.ReplaceAll(body, "https://provider.example/v1", "http://127.0.0.1:8317"), true); w.Code != 400 {
				t.Fatalf("unsafe origin: %d", w.Code)
			}
			if calls != 1 {
				t.Fatalf("unexpected helper calls: %d", calls)
			}
			after, err := s.state()
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatal("draft probe changed provider state")
			}
		})
	}
}

func TestNewProviderEditorAllowsProbeBeforeSaving(t *testing.T) {
	page := string(index)
	for _, want := range []string{`"/providers/probe"`, `addEventListener("input",updateProbeAvailability)`, `p.id||"draft"`, `data-role="upstream-options"`} {
		if !strings.Contains(page, want) {
			t.Fatalf("missing draft UI contract: %s", want)
		}
	}
	if strings.Contains(page, "保存订阅后可获取上游模型") || strings.Contains(page, `["test","fetch-models","save-prices"]`) {
		t.Fatal("new provider still requires saving before probing")
	}
}
