package providerprobe

import (
	"context"
	"net/http"
	"testing"
)

func TestNativeClaudeCatalogPathAndAuthentication(t *testing.T) {
	for _, host := range []string{"api.anthropic.com", "relay.example"} {
		t.Run(host, func(t *testing.T) {
			f := fixtureFetcher(t, func(r *http.Request) (*http.Response, error) {
				if r.URL.Path != "/v1/models" || r.Header.Get("Anthropic-Version") != "2023-06-01" {
					t.Fatal("wrong Messages catalog request")
				}
				if host == "api.anthropic.com" {
					if r.Header.Get("x-api-key") != "fixture-secret" || r.Header.Get("Authorization") != "" {
						t.Fatal("wrong official API authentication")
					}
				} else if r.Header.Get("Authorization") != "Bearer fixture-secret" {
					t.Fatal("relay authentication changed")
				}
				return response(200, `{"data":[{"id":"claude-fixture"}]}`), nil
			})
			v := f.Fetch(context.Background(), Input{Base: "https://" + host + "/v1/", Secret: "fixture-secret", Format: "messages"})
			if !v.OK || v.Count != 1 {
				t.Fatalf("native catalog failed: %+v", v)
			}
		})
	}
}
