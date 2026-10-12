package clientkeys

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGeminiHeaderOnlyOnAuditedNativeRoutes(t *testing.T) {
	p, err := New("admin-test-only", keyRaw(t, "client-test-only", nil))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		method, path string
		header       http.Header
		allowed      bool
	}{
		{"GET", "/v1beta/models", http.Header{"X-Goog-Api-Key": {"client-test-only"}}, true},
		{"POST", "/v1beta/models/gemini-fixture:generateContent", http.Header{"X-Goog-Api-Key": {"admin-test-only"}}, true},
		{"GET", "/v1beta/models", http.Header{"X-Goog-Api-Key": {"inner-test-key"}}, false},
		{"GET", "/v1/models", http.Header{"X-Goog-Api-Key": {"admin-test-only"}}, false},
		{"POST", "/v1beta/interactions", http.Header{"X-Goog-Api-Key": {"admin-test-only"}}, false},
		{"GET", "/v1beta/models?key=admin-test-only", http.Header{"Authorization": {"Bearer admin-test-only"}}, false},
		{"GET", "/v1beta/models", http.Header{"X-Goog-Api-Key": {"admin-test-only"}, "Authorization": {"Bearer admin-test-only"}}, false},
		{"GET", "/v1beta/models", http.Header{"X-Goog-Api-Key": {"admin-test-only", "client-test-only"}}, false},
		{"GET", "/v1beta/models/google%2Fa", http.Header{"X-Goog-Api-Key": {"admin-test-only"}}, false},
	} {
		t.Run(tc.method+tc.path, func(t *testing.T) {
			r := httptest.NewRequest(tc.method, tc.path, nil)
			r.Header = tc.header
			result, err := p.Authenticate(context.Background(), r)
			if (err == nil) != tc.allowed {
				t.Fatalf("wrong auth decision: %v", err)
			}
			if tc.allowed && (result.Principal != "admin" && result.Principal != "client-1") {
				t.Fatal("secret leaked as principal")
			}
		})
	}
}
