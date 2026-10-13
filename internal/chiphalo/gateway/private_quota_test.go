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

type quotaRoundTrip func(*http.Request) (*http.Response, error)

func (f quotaRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestPrivateQuotaUsesRegisteredSDKMembersWithoutRefresh(t *testing.T) {
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
	mu := sync.Mutex{}
	calls := map[string]int{}
	fetcher := quota.Fetcher{Transport: func(proxy string) (http.RoundTripper, error) {
		if proxy != "socks5h://172.17.0.1:10808" {
			t.Error("global proxy not retained")
		}
		return quotaRoundTrip(func(r *http.Request) (*http.Response, error) {
			if r.URL.String() != quota.OfficialUsageURL || r.Method != "GET" || r.Header.Get("ChatGPT-Account-Id") != "same-team" {
				t.Error("wrong official endpoint")
			}
			mu.Lock()
			calls[r.Header.Get("Authorization")]++
			mu.Unlock()
			body := `{"rate_limit":{"primary_window":{"used_percent":4,"limit_window_seconds":604800,"reset_at":2000000}},"access_token":"must-not-export"}`
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
		}), nil
	}}
	handler := live.PrivateManagement(configPath, f.manager, func() (int, map[string]any) { return 200, map[string]any{"ready": true} }, fetcher)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest("GET", "/quota", nil))
	var value quota.Snapshot
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &value) != nil || len(value.Accounts) != 2 {
		t.Fatal("quota projection failed")
	}
	if strings.Contains(w.Body.String(), "private-token") || strings.Contains(w.Body.String(), "must-not-export") || strings.Contains(w.Body.String(), "auth_file") {
		t.Fatal("credential data leaked")
	}
	if calls["Bearer private-token-codex"] != 1 || calls["Bearer private-token-codex2"] != 1 || f.executor.results != 0 {
		t.Fatal("SDK member rotation, inference or refresh occurred")
	}
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest("GET", "/quota?url=https://other.example", nil))
	if w.Code != 400 {
		t.Fatal("arbitrary quota query accepted")
	}
}
