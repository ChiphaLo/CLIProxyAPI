package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/catalogsync"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/quota"
)

func TestPrivateOfficialCatalogReadUsesExactSDKMembers(t *testing.T) {
	live, f, _ := liveFixture(t)
	configPath := filepath.Join(filepath.Dir(f.path), "config/cliproxyapi.yaml")
	if err := os.WriteFile(configPath, []byte("api-keys: [inner-fixture]\nproxy-url: socks5h://172.17.0.1:10808\ncodex-header-defaults:\n  user-agent: codex_cli_rs/0.170.1\n"), 0600); err != nil {
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
			t.Error("global proxy changed")
		}
		return quotaRoundTrip(func(r *http.Request) (*http.Response, error) {
			if r.Method != "GET" || r.URL.String() != catalogsync.OfficialModelsURL+"?client_version=0.170.1" || r.Header.Get("ChatGPT-Account-Id") != "same-team" {
				t.Error("catalog read changed its fixed origin or acquisition version")
			}
			key := r.Header.Get("Authorization")
			mu.Lock()
			calls[key]++
			mu.Unlock()
			name := "gpt-6-astra"
			if strings.HasSuffix(key, "codex2") {
				name = "gpt-6-luna"
			}
			return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"models":[{"slug":"` + name + `","access_token":"must-not-export"}]}`))}, nil
		}), nil
	}}
	handler := live.PrivateManagement(configPath, f.manager, func() (int, map[string]any) { return 200, map[string]any{"ready": true} }, fetcher)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest("GET", "/codex-models", nil))
	var value catalogsync.Snapshot
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &value) != nil || len(value.Accounts) != 2 || value.Accounts[0].Models[0].Name == value.Accounts[1].Models[0].Name || calls["Bearer private-token-codex"] != 1 || calls["Bearer private-token-codex2"] != 1 || f.executor.results != 0 {
		t.Fatal("official account catalogs lost exact member attribution")
	}
	if strings.Contains(w.Body.String(), "private-token") || strings.Contains(w.Body.String(), "must-not-export") || strings.Contains(w.Body.String(), "auth_file") {
		t.Fatal("private catalog exposed credential material")
	}
	selected := value.Accounts[1]
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest("GET", "/codex-models?account_id="+url.QueryEscape(selected.ID), nil))
	var one catalogsync.Snapshot
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &one) != nil || len(one.Accounts) != 1 || one.Accounts[0].ID != selected.ID || calls["Bearer private-token-codex"] != 1 || calls["Bearer private-token-codex2"] != 2 || f.executor.results != 0 {
		t.Fatal("single-member list read crossed accounts or invoked inference", w.Body.String())
	}
	for _, query := range []string{"account_id=unknown", "account_id=" + url.QueryEscape(selected.ID) + "&account_id=unknown", "account_id=" + url.QueryEscape(selected.ID) + "&extra=1"} {
		w = httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest("GET", "/codex-models?"+query, nil))
		if w.Code != 400 {
			t.Fatal("invalid catalog selection accepted", query)
		}
	}

	w = httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest("GET", "/codex-models?account=other", nil))
	if w.Code != 400 {
		t.Fatal("caller-controlled catalog account accepted")
	}
}

func TestOfficialModelReadDoesNotCompeteWithQuotaRefresh(t *testing.T) {
	live, f, _ := liveFixture(t)
	cfg := filepath.Join(filepath.Dir(f.path), "config/cliproxyapi.yaml")
	if err := os.WriteFile(cfg, []byte("api-keys: [fixture]\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for prefix, auth := range f.auths {
		a := auth.Clone()
		a.Metadata["access_token"] = "token-" + prefix
		if _, err := f.manager.Update(t.Context(), a); err != nil {
			t.Fatal(err)
		}
	}
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var enterOnce, releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	transport := quota.Fetcher{Transport: func(string) (http.RoundTripper, error) {
		return quotaRoundTrip(func(r *http.Request) (*http.Response, error) {
			body := `{"models":[{"slug":"gpt-6-luna"}]}`
			if r.URL.String() == quota.OfficialUsageURL {
				enterOnce.Do(func() { close(entered) })
				<-release
				body = `{"rate_limit":{"primary_window":{"used_percent":1,"limit_window_seconds":604800,"reset_at":2000000}}}`
			}
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
		}), nil
	}}
	h := live.PrivateManagement(cfg, f.manager, func() (int, map[string]any) { return 200, map[string]any{"ready": true} }, transport)
	go func() {
		defer close(done)
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/quota", nil))
	}()
	<-entered
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/codex-models", nil))
	unblock()
	<-done
	if w.Code != 200 {
		t.Fatalf("quota refresh blocked independent model-list read: %d", w.Code)
	}
}
