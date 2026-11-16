//go:build linux

package adminhelper

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCodexModelDiscoveryIsAccountLocalReadOnlyAndDoesNotHoldApplyGate(t *testing.T) {
	s, opts, _, old := codexSettingsFixture(t)
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != "GET" || r.URL.Path != "/codex-models" || r.URL.Query().Get("account_id") != "two" || len(r.URL.Query()) != 1 {
			t.Errorf("unexpected discovery: %s %s", r.Method, r.URL)
		}
		select {
		case s.gate <- struct{}{}:
			<-s.gate
		default:
			t.Error("read-only discovery holds configuration gate")
		}
		_, _ = w.Write([]byte(`{"ok":true,"accounts":[{"id":"two","prefix":"codex2","status":"ok","models":[{"name":"gpt-6.1-sol","context_window":272000,"access_token":"must-not-export"}]}]}`))
	}))
	defer server.Close()
	s.inventory = &HTTPBackend{private: true, origin: server.URL, client: server.Client()}
	result := s.dispatch("codex_account_models", map[string]json.RawMessage{"account_id": json.RawMessage(`"two"`)})
	requireOK(t, result)
	raw, _ := json.Marshal(result)
	if calls != 1 || result["published"] != false || strings.Contains(string(raw), "must-not-export") || strings.Contains(string(raw), "access_token") {
		t.Fatal("discovery was not a safe list read", string(raw))
	}
	for n, want := range old {
		got, _ := os.ReadFile(filepath.Join(opts.ConfigDir, n))
		if string(got) != string(want) {
			t.Fatal("discovery changed configuration", n)
		}
	}
	bad := s.dispatch("codex_account_models", map[string]json.RawMessage{"account_id": json.RawMessage(`"unknown"`)})
	if bad["ok"] == true || calls != 1 {
		t.Fatal("unknown member queried upstream")
	}
}
