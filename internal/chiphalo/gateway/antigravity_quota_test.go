package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/quota"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestAntigravityQuotaUsesLiveSDKIdentityAndPublishedModels(t *testing.T) {
	f := newProfileFixture(t, false)
	auth := &coreauth.Auth{ID: "antigravity-quota-fixture", Provider: "antigravity", Prefix: "antigravity", ProxyURL: "socks5h://route:10808", Metadata: map[string]any{"type": "antigravity", "project_id": "project-fixture", "email": "fixture@example.invalid", "access_token": "private-fixture"}}
	if _, err := f.manager.Register(context.Background(), auth); err != nil {
		t.Fatal(err)
	}
	registry.GetGlobalRegistry().RegisterClient(auth.ID, "antigravity", []*registry.ModelInfo{{ID: "antigravity/gemini-fixture"}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
	calls := 0
	fetcher := quota.Fetcher{Transport: func(proxy string) (http.RoundTripper, error) {
		if proxy != auth.ProxyURL {
			t.Fatal("account proxy ignored")
		}
		return quotaRoundTrip(func(r *http.Request) (*http.Response, error) {
			calls++
			body, _ := io.ReadAll(r.Body)
			if r.URL.String() != quota.AntigravityModelsURL || r.Method != "POST" || r.Header.Get("Authorization") != "Bearer private-fixture" || string(body) != `{"project":"project-fixture"}` {
				t.Fatal("wrong account or non-metadata request")
			}
			return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"models":{"gemini-fixture":{"quotaInfo":{"remainingFraction":0.75}},"not-published":{"quotaInfo":{"remainingFraction":1}}},"access_token":"never-export"}`))}, nil
		}), nil
	}}
	rows := collectAntigravityQuota(context.Background(), f.manager, "global-proxy", fetcher)
	raw, _ := json.Marshal(rows)
	if len(rows) != 1 || rows[0].Status != "ok" || len(rows[0].Models) != 1 || *rows[0].Models[0].Remaining != 75 || calls != 1 || f.executor.results != 0 {
		t.Fatal("incorrect account quota projection")
	}
	if strings.Contains(string(raw), "private") || strings.Contains(string(raw), "project-fixture") || strings.Contains(string(raw), "example.invalid") || strings.Contains(string(raw), "not-published") {
		t.Fatal("private or unpublished metadata leaked")
	}
	auth = auth.Clone()
	auth.Disabled = true
	if _, err := f.manager.Update(context.Background(), auth); err != nil {
		t.Fatal(err)
	}
	if len(collectAntigravityQuota(context.Background(), f.manager, "global-proxy", fetcher)) != 0 || calls != 1 {
		t.Fatal("disabled account fetched")
	}
}
