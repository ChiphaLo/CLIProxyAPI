package webgui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCodexManagementRoutesRequireLoginAndCSRF(t *testing.T) {
	s, _ := fixtureServer(t)
	calls := []string{}
	s.options.Helper = probeHelperFunc(func(_ context.Context, op string, payload map[string]any) (map[string]any, error) {
		calls = append(calls, op)
		return map[string]any{"ok": true, "accounts": []any{}}, nil
	})
	token, err := s.auth.login("local", "fixture-password")
	if err != nil {
		t.Fatal(err)
	}
	request := func(method string, login, csrf bool) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "/api/codex-accounts", strings.NewReader(`{"account_id":"one","enabled":false}`))
		r.Header.Set("Origin", s.options.Origin)
		if login {
			r.AddCookie(&http.Cookie{Name: "cpa_session", Value: token})
		}
		if csrf {
			r.Header.Set("X-CSRF-Token", s.auth.csrf(token))
		}
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		return w
	}
	if w := request("GET", false, false); w.Code != 401 {
		t.Fatal("unauthenticated inventory accepted", w.Code)
	}
	if w := request("PUT", true, false); w.Code != 403 {
		t.Fatal("CSRF bypass", w.Code)
	}
	if w := request("GET", true, false); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if w := request("PUT", true, true); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	r := httptest.NewRequest("GET", "/api/codex-accounts/models?account_id=two", nil)
	r.AddCookie(&http.Cookie{Name: "cpa_session", Value: token})
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if strings.Join(calls, ",") != "codex_accounts,update_codex_account,codex_account_models" {
		t.Fatal(calls)
	}
}
