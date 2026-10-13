package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/quota"
)

func TestPrivateResetGETUsesExactRegisteredMembersAndDisabledFailsClosed(t *testing.T) {
	live, f, _ := liveFixture(t)
	configPath := filepath.Join(filepath.Dir(f.path), "config/cliproxyapi.yaml")
	if err := os.WriteFile(configPath, []byte("api-keys: [inner-fixture]\nproxy-url: socks5h://172.17.0.1:10808\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for prefix, auth := range f.auths {
		auth = auth.Clone()
		auth.Metadata["access_token"] = "private-token-" + prefix
		if _, err := f.manager.Update(context.Background(), auth); err != nil {
			t.Fatal(err)
		}
	}
	mu, calls := sync.Mutex{}, map[string]int{}
	fetcher := quota.Fetcher{Transport: func(proxy string) (http.RoundTripper, error) {
		if proxy != "socks5h://172.17.0.1:10808" {
			t.Error("account's configured proxy lost")
		}
		return quotaRoundTrip(func(r *http.Request) (*http.Response, error) {
			if r.Method != "GET" || r.URL.String() != quota.OfficialResetCreditsURL || r.Header.Get("ChatGPT-Account-Id") != "same-team" {
				t.Error("reset page attempted an unexpected or non-read endpoint")
			}
			mu.Lock()
			calls[r.Header.Get("Authorization")]++
			mu.Unlock()
			return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"credits":[{"id":"fixture-credit","status":"available","access_token":"must-not-export"}],"refresh_token":"must-not-export"}`))}, nil
		}), nil
	}}
	handler := live.PrivateManagement(configPath, f.manager, func() (int, map[string]any) { return 200, map[string]any{"ready": true} }, fetcher)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest("GET", "/reset-credits", nil))
	var result quota.ResetSnapshot
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || len(result.Accounts) != 2 || calls["Bearer private-token-codex"] != 1 || calls["Bearer private-token-codex2"] != 1 || f.executor.results != 0 {
		t.Fatal("reset reads rotated/fell back or lost member attribution")
	}
	if strings.Contains(w.Body.String(), "private-token") || strings.Contains(w.Body.String(), "must-not-export") || strings.Contains(w.Body.String(), "auth_index") {
		t.Fatal("reset response leaked credential fields")
	}
	auth, _ := f.manager.GetByID(f.auths["codex2"].ID)
	auth.Disabled = true
	if _, err := f.manager.Update(context.Background(), auth); err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest("GET", "/reset-credits", nil))
	_ = json.Unmarshal(w.Body.Bytes(), &result)
	if w.Code != 200 || result.Accounts["codex2"].Status != "unavailable" || result.Accounts["codex2"].Available != nil || calls["Bearer private-token-codex2"] != 1 || calls["Bearer private-token-codex"] != 2 {
		t.Fatal("disabled account used a different member or fake zero")
	}
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest("GET", "/reset-credits?account=codex", nil))
	if w.Code != 400 {
		t.Fatal("caller-controlled account selection allowed")
	}
}
