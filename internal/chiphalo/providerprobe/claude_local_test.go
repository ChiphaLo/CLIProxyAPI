package providerprobe

import (
	"context"
	"net/http"
	"net/netip"
	"testing"
)

func TestClaudeLocalCatalogUsesPinnedLoopback(t *testing.T) {
	f := Fetcher{
		Resolve: func(context.Context, string) ([]netip.Addr, error) {
			t.Fatal("exact local core must not resolve through DNS")
			return nil, nil
		},
		Transport: func(input Input, ips []netip.Addr) (http.RoundTripper, error) {
			if input.Proxy != "direct" || len(ips) != 1 || ips[0].String() != "127.0.0.1" {
				t.Fatal("local core transport not pinned")
			}
			return roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.String() != "http://127.0.0.1:8334/v1/models" || r.Header.Get("Authorization") != "Bearer fixture-secret" {
					t.Fatal("unexpected local catalog request")
				}
				return response(200, `{"data":[{"id":"claude-fixture"}]}`), nil
			}), nil
		},
	}
	result := f.Fetch(t.Context(), Input{Base: "http://127.0.0.1:8334/v1", Proxy: "direct", Secret: "fixture-secret", Format: "responses"})
	if !result.OK || result.Count != 1 {
		t.Fatalf("local catalog failed: %+v", result)
	}
}
