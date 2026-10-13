package webgui

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/billing"
)

func waitResetWorker(t *testing.T, s *Server) {
	t.Helper()
	s.resetCache.mu.Lock()
	done := s.resetCache.done
	s.resetCache.mu.Unlock()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("reset GET worker did not finish")
	}
}
func TestResetCreditCacheIsAsyncAccountBoundAndReadOnly(t *testing.T) {
	s, clock := fixtureServer(t)
	accounts := []billing.Account{{ID: "one", Prefix: "codex"}}
	public := []map[string]any{{"workspace_id": "shared-team"}}
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	s.options.Helper = probeHelperFunc(func(_ context.Context, op string, _ map[string]any) (map[string]any, error) {
		if op != "oauth_reset_credits" {
			t.Error("unexpected non-read operation")
		}
		if calls.Add(1) == 1 {
			close(entered)
			<-release
		}
		return map[string]any{"ok": true, "accounts": map[string]any{"codex": map[string]any{"status": "ok", "available_count": float64(1), "credits": []any{}, "token": "must-not-export"}}}, nil
	})
	if value := s.resetReports(accounts, public, false); value["codex"]["status"] != "loading" {
		t.Fatal("cold credit GET blocked GUI")
	}
	<-entered
	for i := 0; i < 4; i++ {
		_ = s.resetReports(accounts, public, true)
	}
	if calls.Load() != 1 {
		t.Fatal("parallel reset lookups admitted")
	}
	close(release)
	waitResetWorker(t, s)
	value := s.resetReports(accounts, public, false)
	raw, _ := json.Marshal(value)
	if value["codex"]["status"] != "ok" || calls.Load() != 1 || string(raw) == "" || value["codex"]["token"] != nil {
		t.Fatal("cached GET not reused or credential leaked")
	}
	*clock = clock.Add(31 * time.Second)
	_ = s.resetReports(accounts, public, false)
	waitResetWorker(t, s)
	if calls.Load() != 2 {
		t.Fatal("credit TTL not refreshed")
	}
	accounts[0].ID = "different-member"
	if value = s.resetReports(accounts, public, false); value["codex"]["status"] != "loading" {
		t.Fatal("credits crossed an account identity change")
	}
	waitResetWorker(t, s)
}
func TestResetCreditUnknownFailuresStayNullable(t *testing.T) {
	value := publicResetCredits(map[string]any{"status": "unavailable", "error": "private credential diagnostic", "available_count": 0, "access_token": "private"})
	if value["available_count"] != nil || value["status"] != "unavailable" || value["error"] != "official reset credits unavailable" {
		t.Fatal("credit failure reported a fake count or private diagnostic")
	}
}
func TestAccountFastPathDoesNotFetchResetCredits(t *testing.T) {
	s, _ := fixtureServer(t)
	if err := writeJSON(s.options.Quota, map[string]any{"accounts": []any{map[string]any{"id": "one", "prefix": "codex", "status": "ok", "quota": map[string]any{}}}}); err != nil {
		t.Fatal(err)
	}
	s.options.Helper = probeHelperFunc(func(context.Context, string, map[string]any) (map[string]any, error) {
		t.Error("fast account path performed an official lookup")
		return nil, nil
	})
	w := httptest.NewRecorder()
	s.accountResponse(w, httptest.NewRequest("GET", "/api/oauth-accounts?include_reset=0", nil))
	if w.Code != 200 {
		t.Fatal("fast account page failed")
	}
}
