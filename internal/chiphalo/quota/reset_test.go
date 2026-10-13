package quota

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestResetCreditProjectionIsAllowlistedAndBounded(t *testing.T) {
	for _, body := range []string{
		`{"credits":[{"id":"fixture","status":"available","reset_type":"codex","granted_at":100,"expires_at":"200","access_token":"must-not-export"}],"token":"must-not-export"}`,
		`{"rate_limit_reset_credits":{"items":[{"id":"fixture","status":"available"}]}}`,
		`{"items":[{"id":"fixture","status":"AVAILABLE"}],"available_count":"1"}`,
	} {
		value, err := ProjectResetCredits([]byte(body))
		if err != nil || value.Available == nil || *value.Available != 1 || len(value.Credits) != 1 {
			t.Fatal("credit projection failed")
		}
		raw, _ := json.Marshal(value)
		if strings.Contains(string(raw), "must-not-export") || strings.Contains(string(raw), "access_token") {
			t.Fatal("unknown upstream fields leaked")
		}
	}
	value, err := ProjectResetCredits([]byte(`{"credits":[],"available_count":"not-a-number"}`))
	if err != nil || value.Available != nil {
		t.Fatal("unknown count fabricated as zero")
	}
	for _, body := range []string{`{}`, `[]`, `{"available_count":null}`, `{"credits":"invalid"}`, strings.Repeat("x", 262145)} {
		if _, err := ProjectResetCredits([]byte(body)); err == nil {
			t.Fatal("invalid reset response accepted")
		}
	}
}
func TestResetCreditFetchOnlyGETsFixedOfficialEndpoint(t *testing.T) {
	calls := 0
	fetcher := Fetcher{Transport: func(proxy string) (http.RoundTripper, error) {
		if proxy != "direct" {
			t.Fatal("proxy changed")
		}
		return transportFunc(func(r *http.Request) (*http.Response, error) {
			calls++
			if r.Method != "GET" || r.URL.String() != OfficialResetCreditsURL || r.Header.Get("Authorization") != "Bearer token-fixture" || r.Header.Get("ChatGPT-Account-Id") != "shared-team" || r.Body != nil {
				t.Fatal("unexpected endpoint, method or account")
			}
			return response(200, `{"credits":[],"available_count":0}`), nil
		}), nil
	}}
	value, err := fetcher.FetchResetCredits(t.Context(), Credential{Token: "token-fixture", Workspace: "shared-team", Proxy: "direct"})
	if err != nil || value.Status != "ok" || calls != 1 || value.Available == nil || *value.Available != 0 {
		t.Fatal("reset GET failed")
	}
}
func TestResetCreditErrorsAreRedactedAndDoNotRetry(t *testing.T) {
	for _, status := range []int{302, 401, 429, 500} {
		calls := 0
		fetcher := Fetcher{Transport: func(string) (http.RoundTripper, error) {
			return transportFunc(func(*http.Request) (*http.Response, error) {
				calls++
				r := response(status, "private token-fixture error body")
				r.Header.Set("Location", "https://other.example/consume")
				return r, nil
			}), nil
		}}
		_, err := fetcher.FetchResetCredits(t.Context(), Credential{Token: "token-fixture", Workspace: "shared-team"})
		if err == nil || calls != 1 || strings.Contains(err.Error(), "private") || strings.Contains(err.Error(), "token-fixture") {
			t.Fatal("unsafe reset failure or redirect/retry")
		}
	}
}
func TestResetCreditBoundAndCallerCancellation(t *testing.T) {
	fetcher := Fetcher{Transport: func(string) (http.RoundTripper, error) {
		return transportFunc(func(*http.Request) (*http.Response, error) { return response(200, strings.Repeat("x", 262145)), nil }), nil
	}}
	if _, err := fetcher.FetchResetCredits(t.Context(), Credential{Token: "fixture", Workspace: "team"}); err == nil {
		t.Fatal("unbounded reset response")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	fetcher.Transport = func(string) (http.RoundTripper, error) {
		return transportFunc(func(r *http.Request) (*http.Response, error) {
			if r.Context().Err() == nil {
				t.Fatal("caller cancellation ignored")
			}
			return nil, errors.New("private fixture transport error")
		}), nil
	}
	if _, err := fetcher.FetchResetCredits(ctx, Credential{Token: "fixture", Workspace: "team"}); err == nil || strings.Contains(err.Error(), "private") {
		t.Fatal("transport diagnostic leaked")
	}
}
func TestResetCreditCollectNeverRotatesSharedWorkspaceMembers(t *testing.T) {
	bindings := []Binding{{ID: "one", Prefix: "codex", Workspace: "shared-team"}, {ID: "two", Prefix: "codex2", Workspace: "shared-team"}}
	mu, calls := sync.Mutex{}, map[string]int{}
	fetcher := Fetcher{Transport: func(string) (http.RoundTripper, error) {
		return transportFunc(func(r *http.Request) (*http.Response, error) {
			if r.Method != "GET" || r.URL.String() != OfficialResetCreditsURL {
				t.Error("a reset page read attempted a non-GET operation")
			}
			key := r.Header.Get("Authorization")
			mu.Lock()
			calls[key]++
			mu.Unlock()
			if key == "Bearer two" {
				return response(401, "private failure"), nil
			}
			return response(200, `{"credits":[{"id":"credit-one","status":"available"}]}`), nil
		}), nil
	}}
	result := CollectResetCredits(t.Context(), bindings, func(_ context.Context, b Binding) (Credential, error) {
		return Credential{Token: b.ID, Workspace: b.Workspace}, nil
	}, fetcher, func() time.Time { return time.Unix(100, 0) })
	if !result.OK || result.Accounts["codex"].Status != "ok" || result.Accounts["codex2"].Status != "unavailable" || result.Accounts["codex2"].Available != nil || calls["Bearer one"] != 1 || calls["Bearer two"] != 1 {
		t.Fatal("failure fabricated zero credits or caused member fallback")
	}
}
