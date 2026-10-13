//go:build linux

package adminhelper

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/netip"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/providerprobe"
)

type probeRoundTrip func(*http.Request) (*http.Response, error)

func (f probeRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func probeFixture(t *testing.T) (*Service, *atomic.Int32) {
	t.Helper()
	s := fixture(t)
	if err := os.WriteFile(s.paths.Vault, []byte(`{"fixture":"fixture-secret","untouched":"old"}`), 0600); err != nil {
		t.Fatal(err)
	}
	s.inventory = &HTTPBackend{origin: "http://private", private: true, client: &http.Client{Transport: probeRoundTrip(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/seed" {
			t.Fatal("unexpected inventory route")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"ok":true,"providers":[{"id":"fixture","baseUrl":"https://api.example/v1","proxyUrl":""}]}`))}, nil
	})}}
	calls := &atomic.Int32{}
	s.probe = &providerprobe.Fetcher{Resolve: func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
	}, Transport: func(input providerprobe.Input, _ []netip.Addr) (http.RoundTripper, error) {
		calls.Add(1)
		return probeRoundTrip(func(r *http.Request) (*http.Response, error) {
			if r.Header.Get("Authorization") != "Bearer fixture-secret" {
				t.Fatal("unexpected credential")
			}
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"data":[{"id":"fixture-model"}]}`))}, nil
		}), nil
	}}
	return s, calls
}
func TestStoredProviderProbeRequiresPublishedOriginAndProxy(t *testing.T) {
	s, calls := probeFixture(t)
	vault, _ := os.ReadFile(s.paths.Vault)
	for _, payload := range []map[string]any{
		{"provider_id": "fixture", "base_url": "https://different.example/v1"},
		{"provider_id": "other", "base_url": "https://api.example/v1"},
		{"provider_id": "fixture", "base_url": "https://api.example/v1", "proxy_policy": "cpa-egress"},
		{"provider_id": "fixture", "base_url": "https://api.example/v1", "proxy_url": "socks5h://172.17.0.1:10808"},
		{"provider_id": "fixture", "base_url": "https://api.example/v1", "url": "https://other.example"},
	} {
		if result := call(s, "probe_provider_models", payload); result["ok"] != false || calls.Load() != 0 {
			t.Fatal("stored key sent outside published tuple")
		}
	}
	result := call(s, "probe_provider_models", map[string]any{"provider_id": "fixture", "base_url": "https://api.example/v1", "proxy_policy": "direct"})
	requireOK(t, result)
	if calls.Load() != 1 {
		t.Fatal("published origin was not probed")
	}
	after, _ := os.ReadFile(s.paths.Vault)
	if string(vault) != string(after) {
		t.Fatal("probe changed vault")
	}
}
func TestExplicitProbeKeyAllowsValidatedUnpublishedDraftWithoutPersisting(t *testing.T) {
	s, calls := probeFixture(t)
	before, _ := os.ReadFile(s.paths.Vault)
	result := call(s, "probe_provider_models", map[string]any{"provider_id": "draft", "base_url": "https://new.example/v1", "api_key": "fixture-secret"})
	requireOK(t, result)
	after, _ := os.ReadFile(s.paths.Vault)
	if calls.Load() != 1 || string(before) != string(after) {
		t.Fatal("explicit test key was persisted or probe skipped")
	}
}
func TestProviderProbeSlotsDoNotAcquireMutationGate(t *testing.T) {
	s, _ := probeFixture(t)
	entered, release := make(chan struct{}, 2), make(chan struct{})
	s.probe.Transport = func(providerprobe.Input, []netip.Addr) (http.RoundTripper, error) {
		return probeRoundTrip(func(*http.Request) (*http.Response, error) {
			entered <- struct{}{}
			<-release
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`[]`))}, nil
		}), nil
	}
	payload := map[string]json.RawMessage{"provider_id": set("draft"), "base_url": set("https://new.example/v1"), "api_key": set("fixture-secret")}
	var workers sync.WaitGroup
	for i := 0; i < 2; i++ {
		workers.Go(func() { _ = s.dispatchContext(context.Background(), "probe_provider_models", payload) })
	}
	<-entered
	<-entered
	if value := s.dispatchContext(context.Background(), "probe_provider_models", payload); value["ok"] != false || value["error"] != "provider probe busy" {
		t.Fatal("unbounded probes admitted")
	}
	select {
	case s.gate <- struct{}{}:
		<-s.gate
	default:
		t.Fatal("network probe monopolized mutation gate")
	}
	close(release)
	workers.Wait()
}
