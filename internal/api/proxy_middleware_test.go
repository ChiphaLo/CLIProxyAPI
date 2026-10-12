package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestProxyMiddlewareRunsAfterAuthenticationAndExcludesManagement(t *testing.T) {
	count := 0
	s := newTestServerWithOptions(t, WithProxyMiddleware(func(c *gin.Context) {
		count++
		if c.GetString("userApiKey") == "" {
			t.Fatal("admission ran before authentication")
		}
		c.AbortWithStatus(http.StatusAccepted)
	}))
	for _, path := range []string{"/v1/responses", "/backend-api/codex/responses", "/v1beta/interactions"} {
		for _, authorized := range []bool{false, true} {
			req := httptest.NewRequest(http.MethodPost, path, strings.NewReader("{}"))
			if authorized {
				req.Header.Set("Authorization", "Bearer test-key")
			}
			w := httptest.NewRecorder()
			s.engine.ServeHTTP(w, req)
			if authorized && w.Code != http.StatusAccepted {
				t.Fatalf("authenticated request did not reach admission: %d %s", w.Code, w.Body.String())
			}
			if !authorized && w.Code != http.StatusUnauthorized {
				t.Fatalf("unauthenticated request reached admission: %d", w.Code)
			}
		}
	}
	if count != 3 {
		t.Fatalf("admission count=%d, want 3", count)
	}
	w := httptest.NewRecorder()
	s.engine.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if w.Code != http.StatusOK || count != 3 {
		t.Fatal("health route entered proxy admission")
	}
}
