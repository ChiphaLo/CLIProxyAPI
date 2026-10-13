package providerprobe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func fixtureFetcher(t *testing.T, run roundTripFunc) Fetcher {
	t.Helper()
	return Fetcher{Resolve: func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
	}, Transport: func(input Input, ips []netip.Addr) (http.RoundTripper, error) {
		if input.Secret != "fixture-secret" || len(ips) != 1 || ips[0].String() != "8.8.8.8" {
			t.Fatal("validated inputs not passed to transport")
		}
		return run, nil
	}}
}
func response(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}
}
func TestCatalogShapesAndRequest(t *testing.T) {
	for _, body := range []string{`{"data":[{"id":"alpha"},{"name":"beta"},{"model":"gamma"},"alpha"]}`, `{"models":["alpha","beta","gamma"]}`, `["alpha","beta","gamma"]`} {
		f := fixtureFetcher(t, func(r *http.Request) (*http.Response, error) {
			if r.Method != "GET" || r.URL.String() != "https://api.example/v1/models" || r.Header.Get("Authorization") != "Bearer fixture-secret" || len(r.Header) != 2 {
				t.Fatal("unexpected probe URL/method/headers")
			}
			return response(200, body), nil
		})
		value := f.Fetch(context.Background(), Input{Base: "https://api.example/v1/", Secret: "fixture-secret"})
		if !value.OK || value.Count != 3 || strings.Join(value.Models, ",") != "alpha,beta,gamma" {
			t.Fatal("catalog projection changed")
		}
	}
}
func TestOriginsRejectedBeforeSecretTransmission(t *testing.T) {
	for _, base := range []string{"http://api.example/v1", "https://127.0.0.1/v1", "https://user:pass@api.example/v1", "https://api.example/v1?q=x", "https://api.example/v1#secret", "https://api.example:99999/v1"} {
		t.Run(base, func(t *testing.T) {
			f := fixtureFetcher(t, func(*http.Request) (*http.Response, error) { t.Fatal("rejected origin contacted"); return nil, nil })
			if f.Fetch(context.Background(), Input{Base: base, Secret: "fixture-secret"}).Error != "provider_url_rejected" {
				t.Fatal("unsafe origin accepted")
			}
		})
	}
	for _, ip := range []string{"127.0.0.1", "10.0.0.1", "169.254.169.254", "::1", "::ffff:127.0.0.1", "198.18.0.1"} {
		t.Run(ip, func(t *testing.T) {
			f := fixtureFetcher(t, func(*http.Request) (*http.Response, error) { t.Fatal("private address contacted"); return nil, nil })
			f.Resolve = func(context.Context, string) ([]netip.Addr, error) {
				return []netip.Addr{netip.MustParseAddr("8.8.8.8"), netip.MustParseAddr(ip)}, nil
			}
			if f.Fetch(context.Background(), Input{Base: "https://api.example/v1", Secret: "fixture-secret"}).OK {
				t.Fatal("mixed private resolution accepted")
			}
		})
	}
}
func TestRedirectAndErrorBodyNeverLeak(t *testing.T) {
	for status, kind := range map[int]string{302: "provider_request_rejected", 401: "provider_auth_rejected", 403: "provider_auth_rejected", 429: "provider_rate_limited", 500: "provider_server_error"} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			calls := 0
			f := fixtureFetcher(t, func(*http.Request) (*http.Response, error) {
				calls++
				r := response(status, "secret diagnostic fixture-secret")
				r.Header.Set("Location", "https://different.example/private")
				return r, nil
			})
			result := f.Fetch(context.Background(), Input{Base: "https://api.example", Secret: "fixture-secret"})
			raw, _ := json.Marshal(result)
			if calls != 1 || result.Error != kind || result.Status != status || strings.Contains(string(raw), "fixture-secret") {
				t.Fatal("redirect followed or diagnostic leaked")
			}
		})
	}
}
func TestCatalogBoundsAndMalformedResponses(t *testing.T) {
	for _, body := range []string{`{"data":{}}`, `invalid`, `{"error":"fixture-secret"}`, strings.Repeat("x", 4*1024*1024+1)} {
		f := fixtureFetcher(t, func(*http.Request) (*http.Response, error) { return response(200, body), nil })
		if f.Fetch(context.Background(), Input{Base: "https://api.example", Secret: "fixture-secret"}).OK {
			t.Fatal("malformed or oversized catalog accepted")
		}
	}
	items := []string{}
	for i := 0; i < 600; i++ {
		items = append(items, fmt.Sprintf("model-%d", i))
	}
	items = append(items, "fixture-secret", "line\nfeed", strings.Repeat("x", 300))
	raw, _ := json.Marshal(items)
	f := fixtureFetcher(t, func(*http.Request) (*http.Response, error) { return response(200, string(raw)), nil })
	result := f.Fetch(context.Background(), Input{Base: "https://api.example", Secret: "fixture-secret"})
	if !result.OK || len(result.Models) != 500 || result.Count != 600 || !result.Truncated {
		t.Fatal("catalog output bound or filtering changed")
	}
}
func TestPinningDisablesEnvironmentProxyAndPostConnectTimeouts(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	rt, err := pinnedTransport(Input{Base: "https://api.example/v1"}, []netip.Addr{netip.MustParseAddr("8.8.8.8")})
	if err != nil {
		t.Fatal(err)
	}
	tr := rt.(*http.Transport)
	if tr.Proxy != nil || tr.TLSClientConfig.ServerName != "api.example" || tr.TLSHandshakeTimeout != 0 || tr.ResponseHeaderTimeout != 0 {
		t.Fatal("explicit pinned transport policy changed")
	}
	if _, err := tr.DialContext(context.Background(), "tcp", "different.example:443"); err == nil {
		t.Fatal("unexpected dial target allowed")
	}
	for _, scheme := range []string{"socks5", "socks5h"} {
		if _, err := pinnedTransport(Input{Base: "https://api.example", Proxy: scheme + "://172.17.0.1:10808"}, []netip.Addr{netip.MustParseAddr("8.8.8.8")}); err != nil {
			t.Fatal("allowlisted SOCKS transport unavailable")
		}
	}
}

func TestCatalogByteBudgetFitsPrivateControlEnvelope(t *testing.T) {
	items := []string{}
	for i := 0; i < 600; i++ {
		items = append(items, fmt.Sprintf("%03d-", i)+strings.Repeat("\"", 180))
	}
	raw, _ := json.Marshal(items)
	f := fixtureFetcher(t, func(*http.Request) (*http.Response, error) { return response(200, string(raw)), nil })
	result := f.Fetch(context.Background(), Input{Base: "https://api.example", Secret: "fixture-secret"})
	encoded, _ := json.Marshal(result)
	if !result.OK || result.Count != 600 || !result.Truncated || len(encoded) >= 60*1024 {
		t.Fatal("escaped model IDs overflow private reply budget")
	}
}
func TestCredentialAndCancellationFailuresAreRedacted(t *testing.T) {
	f := fixtureFetcher(t, func(r *http.Request) (*http.Response, error) {
		if r.Context().Err() == nil {
			t.Fatal("caller cancellation not propagated")
		}
		return nil, errors.New("private fixture-secret transport diagnostic")
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	value := f.Fetch(ctx, Input{Base: "https://api.example", Secret: "fixture-secret"})
	if value.OK || strings.Contains(value.Error, "fixture-secret") {
		t.Fatal("cancellation ignored or diagnostic leaked")
	}
	for _, secret := range []string{"", "key\r\ninjected", strings.Repeat("x", 4097)} {
		if f.Fetch(context.Background(), Input{Base: "https://api.example", Secret: secret}).Error != "invalid provider secret" {
			t.Fatal("bad credential accepted")
		}
	}
}
