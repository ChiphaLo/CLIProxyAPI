package webgui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
)

type providerKeyFixture struct {
	secrets map[string]string
	binding map[string]any
}

func (h *providerKeyFixture) Call(_ context.Context, op string, p map[string]any) (map[string]any, error) {
	switch op {
	case "set_provider_secret":
		h.secrets[p["provider_id"].(string)] = p["secret"].(string)
		h.binding = p
	case "delete_provider_secret":
		delete(h.secrets, p["provider_id"].(string))
	}
	status := map[string]any{}
	for k := range h.secrets {
		status[k] = true
	}
	return map[string]any{"ok": true, "provider_secrets": status}, nil
}
func TestNewProviderKeyRetainedOnBlankEdit(t *testing.T) {
	for _, protocol := range []string{"chat", "messages"} {
		t.Run(protocol, func(t *testing.T) { testProviderKeyRetainedOnBlankEdit(t, protocol) })
	}
}

func testProviderKeyRetainedOnBlankEdit(t *testing.T, protocol string) {
	t.Helper()
	s, _ := fixtureServer(t)
	h := &providerKeyFixture{secrets: map[string]string{}}
	s.options.Helper = h
	s.options.Resolve = func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
	}
	token, err := s.auth.login("local", "fixture-password")
	if err != nil {
		t.Fatal(err)
	}
	call := func(method, path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Origin", s.options.Origin)
		r.Header.Set("X-CSRF-Token", s.auth.csrf(token))
		r.AddCookie(&http.Cookie{Name: "cpa_session", Value: token})
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		return w
	}
	w := call("POST", "/api/providers", `{"name":"fixture","prefix":"fixture","baseUrl":"https://provider.example/v1","upstreamProtocol":"`+protocol+`","apiKey":"secret-fixture","enabled":true,"models":[]}`)
	if w.Code != 200 {
		t.Fatalf("create %d %s", w.Code, w.Body.String())
	}
	wantBase := "https://provider.example/v1"
	if protocol == "messages" {
		wantBase = "https://provider.example"
	}
	if h.binding["base_url"] != wantBase || h.binding["api_format"] != protocol || h.binding["proxy_url"] != "" {
		t.Fatal("explicit key save did not bind its normalized origin and protocol")
	}
	w = call("PUT", "/api/providers/fixture", `{"name":"renamed","apiKey":"","models":[]}`)
	if w.Code != 200 {
		t.Fatalf("edit %d %s", w.Code, w.Body.String())
	}
	if h.secrets["fixture"] != "secret-fixture" {
		t.Fatal("blank edit changed secret")
	}
	w = call("GET", "/api/state", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"hasApiKey":true`) || strings.Contains(w.Body.String(), "secret-fixture") {
		t.Fatalf("bad projection %d %s", w.Code, w.Body.String())
	}
	if protocol == "messages" && (!strings.Contains(w.Body.String(), `"type":"anthropic"`) || !strings.Contains(w.Body.String(), `"baseUrl":"https://provider.example"`)) {
		t.Fatal("native provider edit lost type or root URL")
	}
}
