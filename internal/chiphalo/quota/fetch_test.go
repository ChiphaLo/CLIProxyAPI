package quota

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func response(code int, body string) *http.Response {
	return &http.Response{StatusCode: code, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}
func TestFixedOfficialEndpointAndSecretProjection(t *testing.T) {
	calls := 0
	fetcher := Fetcher{Transport: func(proxy string) (http.RoundTripper, error) {
		if proxy != "direct" {
			t.Fatal("proxy lost")
		}
		return transportFunc(func(r *http.Request) (*http.Response, error) {
			calls++
			if r.Method != "GET" || r.URL.String() != OfficialUsageURL || r.Header.Get("Authorization") != "Bearer token-fixture" || r.Header.Get("ChatGPT-Account-Id") != "same-team" {
				t.Fatal("incorrect official request")
			}
			return response(200, `{"plan_type":"team","token":"token-fixture","rate_limit":{"primary_window":{"used_percent":25,"limit_window_seconds":604800,"reset_at":2000000}}}`), nil
		}), nil
	}}
	value, err := fetcher.Fetch(t.Context(), Credential{Token: "token-fixture", Workspace: "same-team", Proxy: "direct"})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(value)
	if calls != 1 || strings.Contains(string(raw), "token-fixture") {
		t.Fatal("credentials leaked or incorrect request count")
	}
}
func TestOfficialErrorsNeverExposeBodiesOrCredentials(t *testing.T) {
	for _, code := range []int{302, 401, 429, 500} {
		fetcher := Fetcher{Transport: func(string) (http.RoundTripper, error) {
			return transportFunc(func(*http.Request) (*http.Response, error) {
				r := response(code, "private upstream body")
				r.Header.Set("Location", "https://other.example/steal")
				return r, nil
			}), nil
		}}
		_, err := fetcher.Fetch(t.Context(), Credential{Token: "token-fixture", Workspace: "same-team"})
		if err == nil || strings.Contains(err.Error(), "private") || strings.Contains(err.Error(), "token-fixture") {
			t.Fatal("unsafe error")
		}
	}
}
func TestOfficialResponseBoundedAndCallerCancellation(t *testing.T) {
	fetcher := Fetcher{Transport: func(string) (http.RoundTripper, error) {
		return transportFunc(func(*http.Request) (*http.Response, error) { return response(200, strings.Repeat("x", 262145)), nil }), nil
	}}
	if _, err := fetcher.Fetch(t.Context(), Credential{Token: "fixture", Workspace: "team"}); err == nil {
		t.Fatal("oversized response accepted")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	fetcher.Transport = func(string) (http.RoundTripper, error) {
		return transportFunc(func(r *http.Request) (*http.Response, error) {
			if r.Context().Err() == nil {
				t.Fatal("caller cancellation lost")
			}
			return nil, errors.New("token-fixture secret transport diagnostic")
		}), nil
	}
	if _, err := fetcher.Fetch(ctx, Credential{Token: "fixture", Workspace: "team"}); err == nil || strings.Contains(err.Error(), "token-fixture") {
		t.Fatal("unsafe transport error")
	}
}
func TestProxyAcquisitionSupportsSocks5hAndIgnoresEnvironment(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:9")
	r, err := defaultTransport("")
	if err != nil || r.(*http.Transport).Proxy != nil {
		t.Fatal("environment proxy inherited")
	}
	r, err = defaultTransport("socks5h://172.17.0.1:10808")
	if err != nil || r.(*http.Transport).DialContext == nil {
		t.Fatalf("socks5h unsupported: %v", err)
	}
}
func TestTwoMembersInOneWorkspaceNeverRotateOrFallback(t *testing.T) {
	bindings := []Binding{{ID: "one", Prefix: "codex", Workspace: "same-team"}, {ID: "two", Prefix: "codex2", Workspace: "same-team"}}
	mu := sync.Mutex{}
	seen := map[string]int{}
	fetcher := Fetcher{Transport: func(string) (http.RoundTripper, error) {
		return transportFunc(func(r *http.Request) (*http.Response, error) {
			mu.Lock()
			seen[r.Header.Get("Authorization")]++
			mu.Unlock()
			if r.Header.Get("Authorization") == "Bearer member-one" {
				return response(401, "secret"), nil
			}
			return response(200, `{"rate_limit":{"primary_window":{"used_percent":4,"limit_window_seconds":604800,"reset_at":2000000}}}`), nil
		}), nil
	}}
	snapshot := Collect(t.Context(), bindings, func(_ context.Context, b Binding) (Credential, error) {
		token := "member-one"
		if b.ID == "two" {
			token = "member-two"
		}
		return Credential{Token: token, Workspace: b.Workspace}, nil
	}, fetcher, func() time.Time { return time.Unix(1000000, 0) })
	if snapshot.Accounts[0].Status != "unavailable" || snapshot.Accounts[1].Status != "ok" || seen["Bearer member-one"] != 1 || seen["Bearer member-two"] != 1 {
		t.Fatal("account fallback/rotation or attribution occurred")
	}
}
func TestCarryStaleQuotaOnlyForMatchingIdentity(t *testing.T) {
	old := Snapshot{Accounts: []Account{{ID: "one", Prefix: "codex", Workspace: "team", Updated: new(int64(100)), Quota: &Usage{Plan: "team"}}}}
	current := Snapshot{Accounts: []Account{{ID: "one", Prefix: "codex", Workspace: "team", Status: "unavailable"}}}
	result := CarryPrevious(current, old)
	if result.Accounts[0].Quota == nil || *result.Accounts[0].Updated != 100 || result.Accounts[0].Status != "unavailable" {
		t.Fatal("stale quota lost or false success")
	}
	current.Accounts[0].Workspace = "other"
	current.Accounts[0].Quota = nil
	current.Accounts[0].Updated = nil
	if CarryPrevious(current, old).Accounts[0].Quota != nil {
		t.Fatal("stale quota crossed identity")
	}
}
