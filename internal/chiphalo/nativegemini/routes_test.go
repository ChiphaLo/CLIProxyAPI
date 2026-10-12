package nativegemini

import (
	"net/http"
	"strings"
	"testing"
)

func TestAuditedPublicSurface(t *testing.T) {
	for _, path := range []string{"/v1beta/models", "/v1beta/models/gemini-3.0-pro", "/v1beta/models/google-a/gemini-3.0-pro"} {
		if _, _, ok := Parse(http.MethodGet, path); !ok {
			t.Fatalf("rejected valid resource %s", path)
		}
	}
	for _, method := range []string{"generateContent", "streamGenerateContent", "countTokens"} {
		model, action, ok := Parse(http.MethodPost, "/v1beta/models/google-a/gemini-3.0-pro:"+method)
		if !ok || model != "google-a/gemini-3.0-pro" || action != method {
			t.Fatal("native resource namespace changed")
		}
	}
	for _, path := range []string{"/v1beta/interactions", "/v1beta/models/", "/v1beta/models//x:generateContent", "/v1beta/models/x:unknown", "/v1beta/models/x:generateContent:countTokens", "/v1beta/models/../x:generateContent", "/v1beta/models/x%2fy:generateContent", "/v1beta/models/" + strings.Repeat("a", 1025) + ":generateContent"} {
		if _, _, ok := Parse(http.MethodPost, path); ok {
			t.Fatalf("accepted unaudited resource %s", path)
		}
	}
	if Provider("codex") || Provider("claude") || !Provider("gemini-cli") || !Provider("gemini") {
		t.Fatal("native provider boundary changed")
	}
}
