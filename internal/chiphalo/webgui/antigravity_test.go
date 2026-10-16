package webgui

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAntigravityGUIQuotaIsIndependentAndAllowlisted(t *testing.T) {
	s, clock := fixtureServer(t)
	row := map[string]any{"id": strings.Repeat("a", 64), "prefix": "antigravity", "status": "ok", "checked_at": clock.Unix(), "updated_at": clock.Unix(), "models": []any{map[string]any{"model": "gemini-fixture", "remaining_percent": 75, "access_token": "never-export"}}, "access_token": "never-export"}
	if err := writeJSON(s.options.Quota, map[string]any{"accounts": []any{}, "antigravity_accounts": []any{row}}); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	s.accountResponse(w, httptest.NewRequest("GET", "/api/oauth-accounts?include_reset=0", nil))
	var value struct {
		Accounts    []any            `json:"accounts"`
		Antigravity []map[string]any `json:"antigravity_accounts"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &value) != nil || len(value.Accounts) != 0 || len(value.Antigravity) != 1 || strings.Contains(w.Body.String(), "never-export") {
		t.Fatalf("independent safe quota unavailable: %d %s", w.Code, w.Body)
	}
	row["models"] = []any{map[string]any{"model": "gemini-fixture", "remaining_percent": 101}}
	_ = writeJSON(s.options.Quota, map[string]any{"accounts": []any{}, "antigravity_accounts": []any{row}})
	w = httptest.NewRecorder()
	s.accountResponse(w, httptest.NewRequest("GET", "/api/oauth-accounts?include_reset=0", nil))
	if w.Code != 200 || strings.Contains(w.Body.String(), "101") {
		t.Fatal("malformed quota escaped boundary or broke Codex")
	}
}
