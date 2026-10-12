package gateway

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/nativegemini"
)

// PublicSurface keeps the legacy gateway's small API boundary when embedded
// into a newer server with additional native routes. Internal management and
// OAuth callbacks must use a separately protected administrative surface.
func PublicSurface() gin.HandlerFunc {
	return func(c *gin.Context) {
		path, method := c.Request.URL.Path, c.Request.Method
		if method == http.MethodGet && (path == "/v1/responses" || path == "/backend-api/codex/responses") && !strings.EqualFold(c.Request.Header.Get("Upgrade"), "websocket") {
			abort(c, 404, "unsupported public CPA endpoint")
			return
		}
		if _, _, ok := nativegemini.Parse(method, path); ok && c.Request.URL.RawPath == "" {
			c.Next()
			return
		}
		if method == http.MethodGet && path == "/health" {
			c.AbortWithStatusJSON(http.StatusOK, gin.H{"status": "ok", "service": "unified-ai-gateway"})
			return
		}
		if method == http.MethodGet && (path == "/healthz" || path == "/readyz" || path == "/v1/models" || path == "/v1/responses" || path == "/backend-api/codex/responses") ||
			method == http.MethodPost && (path == "/v1/responses" || path == "/v1/chat/completions" || path == "/backend-api/codex/responses") {
			c.Next()
			return
		}
		abort(c, http.StatusNotFound, "unsupported public CPA endpoint")
	}
}
