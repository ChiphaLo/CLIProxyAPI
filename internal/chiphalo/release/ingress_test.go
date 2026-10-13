package release

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestIngressClientTrustBoundary(t *testing.T) {
	policy, err := parseIngress([]string{"127.0.0.1", "::1"})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, peer, xff, want string
		invalid               bool
	}{
		{"direct spoof", "192.0.2.2:9000", "198.51.100.8", "192.0.2.2", false},
		{"direct invalid ignored", "192.0.2.2:9000", "not-an-ip", "192.0.2.2", false},
		{"caddy", "127.0.0.1:9000", "198.51.100.8", "198.51.100.8", false},
		{"spoof before caddy peer", "127.0.0.1:9000", "203.0.113.99, 198.51.100.8", "198.51.100.8", false},
		{"trusted inner hop", "[::1]:9000", "203.0.113.99, 198.51.100.8, 127.0.0.1", "198.51.100.8", false},
		{"mapped ipv4", "[::ffff:127.0.0.1]:9000", "::ffff:198.51.100.8", "198.51.100.8", false},
		{"ipv6 client", "[::1]:9000", "2001:db8::8", "2001:db8::8", false},
		{"missing", "127.0.0.1:9000", "", "127.0.0.1", false},
		{"invalid", "127.0.0.1:9000", "unknown, 198.51.100.8", "", true},
		{"empty entry", "127.0.0.1:9000", "198.51.100.8,", "", true},
		{"port in header", "127.0.0.1:9000", "198.51.100.8:90", "", true},
		{"unspecified", "127.0.0.1:9000", "0.0.0.0", "", true},
		{"multicast", "127.0.0.1:9000", "ff02::1", "", true},
		{"zone", "127.0.0.1:9000", "fe80::1%eth0", "", true},
		{"oversized", "127.0.0.1:9000", strings.Repeat("1", 2049), "", true},
		{"too many hops", "127.0.0.1:9000", strings.Repeat("127.0.0.1,", 16) + "198.51.100.8", "", true},
		{"invalid peer", "unknown", "198.51.100.8", "", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/v1/models", nil)
			r.RemoteAddr = test.peer
			r.Header.Set("X-Forwarded-For", test.xff)
			got, err := policy.client(r)
			if (err != nil) != test.invalid || got != test.want {
				t.Fatalf("client=%q, error=%v; want %q, invalid=%v", got, err, test.want, test.invalid)
			}
		})
	}
	for _, value := range []string{"*", "127.0.0.0/8", "0.0.0.0", "192.0.2.2", "localhost", "::1%lo", ""} {
		if _, err := parseIngress([]string{value}); err == nil {
			t.Fatalf("unsafe trust policy accepted: %q", value)
		}
	}
	defaultPolicy, _ := parseIngress(nil)
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "127.0.0.1:9000"
	r.Header.Set("X-Forwarded-For", "198.51.100.8")
	if got, _ := defaultPolicy.client(r); got != "127.0.0.1" {
		t.Fatal("default policy trusted forwarding metadata")
	}
}

func TestIngressProxyPublishesCanonicalHeaders(t *testing.T) {
	var got http.Header
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		_, _ = io.WriteString(w, "ok")
	}))
	defer upstream.Close()
	endpoint, _ := url.Parse(upstream.URL)
	policy, _ := parseIngress([]string{"127.0.0.1"})
	proxy := newProxy(endpoint, http.DefaultTransport, policy)
	r := httptest.NewRequest("GET", "http://cpa.chipha.de/v1/models", nil)
	r.RemoteAddr = "127.0.0.1:9000"
	r.Header.Add("X-Forwarded-For", "203.0.113.99")
	r.Header.Add("X-Forwarded-For", "198.51.100.8")
	r.Header.Set("X-Forwarded-Proto", "https")
	r.Header.Set("X-Forwarded-Host", "attacker.invalid")
	for _, header := range []string{"Forwarded", "X-Real-IP", "CF-Connecting-IP", "True-Client-IP"} {
		r.Header.Set(header, "203.0.113.99")
	}
	w := httptest.NewRecorder()
	proxy.ServeHTTP(w, r)
	if w.Code != 200 || got.Get("X-Forwarded-For") != "198.51.100.8" || got.Get("X-Forwarded-Proto") != "https" || got.Get("X-Forwarded-Host") != "cpa.chipha.de" {
		t.Fatalf("bad canonical forwarding headers: status=%d headers=%v", w.Code, got)
	}
	for _, header := range []string{"Forwarded", "X-Real-IP", "CF-Connecting-IP", "True-Client-IP"} {
		if got.Get(header) != "" {
			t.Fatalf("spoofable header forwarded: %s", header)
		}
	}
	got = nil
	r.Header.Set("X-Forwarded-For", "invalid")
	w = httptest.NewRecorder()
	proxy.ServeHTTP(w, r)
	if w.Code != 400 || got != nil {
		t.Fatal("invalid trusted metadata reached backend")
	}
	r.RemoteAddr = "192.0.2.2:9000"
	w = httptest.NewRecorder()
	proxy.ServeHTTP(w, r)
	if w.Code != 200 || got.Get("X-Forwarded-For") != "192.0.2.2" || got.Get("X-Forwarded-Proto") != "http" {
		t.Fatal("untrusted peer spoofed client or TLS metadata")
	}
}
