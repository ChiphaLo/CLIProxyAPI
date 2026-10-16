package providerprobe

import (
	"context"
	"net/http"
	"net/netip"
	"testing"
)

func TestWorkBuddyProbePinsExactLocalDestination(t *testing.T) {
	f := Fetcher{Resolve: func(context.Context, string) ([]netip.Addr, error) { t.Fatal("local worker used DNS"); return nil, nil }, Transport: func(in Input, ips []netip.Addr) (http.RoundTripper, error) {
		if in.Proxy != "direct" || len(ips) != 1 || ips[0].String() != "127.0.0.1" {
			t.Fatal("worker destination not pinned")
		}
		return roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if r.URL.String() != "http://127.0.0.1:8331/v1/models" || r.Header.Get("Authorization") != "Bearer fixture-secret" {
				t.Fatal("unexpected worker request")
			}
			return response(200, `{"data":[{"id":"model-fixture"}]}`), nil
		}), nil
	}}
	v := f.Fetch(t.Context(), Input{Base: "http://127.0.0.1:8331/v1", Proxy: "direct", Secret: "fixture-secret"})
	if !v.OK || v.Count != 1 {
		t.Fatalf("local worker probe failed: %+v", v)
	}
}
