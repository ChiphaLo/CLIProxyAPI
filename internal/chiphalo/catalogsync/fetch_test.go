package catalogsync

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

	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/quota"
)

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func response(code int, body string) *http.Response {
	return &http.Response{StatusCode: code, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}
}
func TestOfficialCatalogProjectionPreservesCapabilitiesWithoutCredentials(t *testing.T) {
	value, err := Project([]byte(`{"models":[{"slug":"gpt-6-astra","display_name":"Astra","context_window":272000,"max_context_window":1000000,"default_reasoning_level":"ultra","supported_reasoning_levels":[{"effort":"high"},{"effort":"ultra"}],"input_modalities":["text","image"],"multi_agent_version":"v2","visibility":"list","access_token":"must-not-export","base_instructions":"private-extra"},{"slug":"gpt-image-2"},{"slug":"gpt-5.5"},{"slug":"gpt-5.6"},{"slug":"gpt-5.6-sol"},{"slug":"gpt-6-sol"}]}`))
	if err != nil || len(value) != 1 || value[0].DefaultThinking != "ultra" || value[0].Window != 272000 || value[0].MaxWindow != 1000000 || strings.Join(value[0].Inputs, ",") != "text,image" || value[0].MultiAgent != "v2" {
		t.Fatal("official capabilities lost or retired/internal names returned")
	}
	raw, _ := json.Marshal(value)
	if strings.Contains(string(raw), "must-not-export") || strings.Contains(string(raw), "private-extra") {
		t.Fatal("unapproved upstream fields were exported")
	}
}
func TestOfficialCatalogRejectsAmbiguitySizeAndInvalidBounds(t *testing.T) {
	for _, body := range []string{`{}`, `{"models":null}`, `{"models":[{"slug":"gpt-6-luna","slug":"gpt-6-astra"}]}`, `{"models":[{"slug":"gpt-6-luna"},{"slug":"gpt-6-luna"}]}`, `{"models":[{"slug":"gpt-6-luna","context_window":200000,"max_context_window":100000}]}`, strings.Repeat("x", 2*1024*1024+1)} {
		if _, err := Project([]byte(body)); err == nil {
			t.Fatal("invalid official catalog accepted")
		}
	}
}
func TestOfficialCatalogGETHasFixedOriginAndRegisteredCredential(t *testing.T) {
	calls := 0
	f := Fetcher{ClientVersion: "0.170.1", Transport: func(proxy string) (http.RoundTripper, error) {
		if proxy != "socks5h://172.17.0.1:10808" {
			t.Fatal("configured proxy lost")
		}
		return transportFunc(func(r *http.Request) (*http.Response, error) {
			calls++
			if r.Method != "GET" || r.URL.String() != OfficialModelsURL+"?client_version=0.170.1" || r.Header.Get("Authorization") != "Bearer token-fixture" || r.Header.Get("ChatGPT-Account-Id") != "shared-team" || r.Header.Get("Originator") != "codex_cli_rs" || r.Header.Get("User-Agent") != "codex_cli_rs/0.170.1" {
				t.Fatal("unexpected official catalog request")
			}
			return response(200, `{"models":[{"slug":"gpt-6.1-sol"}]}`), nil
		}), nil
	}}
	models, err := f.Fetch(t.Context(), quota.Credential{Token: "token-fixture", Workspace: "shared-team", Proxy: "socks5h://172.17.0.1:10808"})
	if err != nil || calls != 1 || len(models) != 1 {
		t.Fatal("catalog fetch did not use one direct read")
	}
	if ClientVersion("codex_cli_rs/0.170.1 (Windows)") != "0.170.1" || ClientVersion("other-agent") != DefaultClientVersion {
		t.Fatal("trusted client version selection changed")
	}
}
func TestOfficialCatalogFailuresDoNotFollowRedirectsRetryOrLeakBodies(t *testing.T) {
	for _, status := range []int{302, 401, 429, 500} {
		calls := 0
		f := Fetcher{Transport: func(string) (http.RoundTripper, error) {
			return transportFunc(func(*http.Request) (*http.Response, error) {
				calls++
				r := response(status, "private token-fixture body")
				r.Header.Set("Location", "https://different.example")
				return r, nil
			}), nil
		}}
		_, err := f.Fetch(t.Context(), quota.Credential{Token: "token-fixture", Workspace: "team"})
		if err == nil || calls != 1 || strings.Contains(err.Error(), "private") || strings.Contains(err.Error(), "token-fixture") {
			t.Fatal("catalog failure leaked a body or changed retry/redirect policy")
		}
	}
}
func TestOfficialDiscoveryPreservesTwoMembersAndPartialFailure(t *testing.T) {
	bindings := []quota.Binding{{ID: "one", Prefix: "codex", Workspace: "shared-team"}, {ID: "two", Prefix: "codex2", Workspace: "shared-team"}}
	mu, calls := sync.Mutex{}, map[string]int{}
	f := Fetcher{Transport: func(string) (http.RoundTripper, error) {
		return transportFunc(func(r *http.Request) (*http.Response, error) {
			key := r.Header.Get("Authorization")
			mu.Lock()
			calls[key]++
			mu.Unlock()
			if key == "Bearer two" {
				return response(401, "private failure"), nil
			}
			return response(200, `{"models":[{"slug":"gpt-6-astra"}]}`), nil
		}), nil
	}}
	value := Discover(t.Context(), bindings, func(_ context.Context, binding quota.Binding) (quota.Credential, error) {
		return quota.Credential{Token: binding.ID, Workspace: binding.Workspace}, nil
	}, f, func() time.Time { return time.Unix(100, 0) })
	if !value.OK || len(value.Accounts) != 2 || value.Accounts[0].Status != "ok" || value.Accounts[1].Status != "unavailable" || value.Accounts[1].ID != "two" || calls["Bearer one"] != 1 || calls["Bearer two"] != 1 {
		t.Fatal("catalog discovery rotated or reassigned a member")
	}
}
func TestOfficialCatalogCancellationAndCredentialValidation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	f := Fetcher{Transport: func(string) (http.RoundTripper, error) {
		return transportFunc(func(r *http.Request) (*http.Response, error) {
			if r.Context().Err() == nil {
				t.Fatal("catalog read ignored caller cancellation")
			}
			return nil, errors.New("private diagnostic")
		}), nil
	}}
	if _, err := f.Fetch(ctx, quota.Credential{Token: "fixture", Workspace: "team"}); err == nil || strings.Contains(err.Error(), "private") {
		t.Fatal("unsafe cancellation handling")
	}
	for _, token := range []string{"", "bad\nheader", strings.Repeat("x", 65537)} {
		if _, err := f.Fetch(t.Context(), quota.Credential{Token: token, Workspace: "team"}); err == nil {
			t.Fatal("invalid credential accepted")
		}
	}
}
