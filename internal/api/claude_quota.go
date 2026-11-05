package api

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/quota"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// Only explicitly enabled isolated cores register this route. OAuth material
// stays in the live native auth manager, including refresh ownership.
func (s *Server) claudeQuotaHandler() gin.HandlerFunc {
	var mu sync.Mutex
	var cached quota.ClaudeSnapshot
	return func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		mu.Lock()
		defer mu.Unlock()
		var selected *auth.Auth
		for _, a := range s.handlers.AuthManager.List() {
			if a.Provider != "claude" || a.Disabled {
				continue
			}
			if token, _ := a.Metadata["access_token"].(string); token == "" {
				continue
			}
			if selected != nil {
				c.JSON(503, gin.H{"error": "Claude account mapping is ambiguous"})
				return
			}
			selected = a
		}
		if selected == nil {
			c.JSON(503, gin.H{"error": "Claude OAuth account unavailable"})
			return
		}
		identity, _ := selected.Metadata["account_uuid"].(string)
		if identity == "" {
			c.JSON(503, gin.H{"error": "Claude account identity unavailable"})
			return
		}
		h := sha256.Sum256([]byte(identity))
		id := fmt.Sprintf("claude_%x", h[:12])
		now := time.Now().Unix()
		fresh := cached.ID == id && now-cached.Updated < 60
		for _, w := range []*quota.ClaudeWindow{cached.FiveHour, cached.Weekly} {
			if w != nil && w.Reset <= now {
				fresh = false
			}
		}
		if fresh {
			c.JSON(200, cached)
			return
		}
		proxy := selected.ProxyURL
		if proxy == "" {
			proxy = s.cfg.ProxyURL
		}
		token, _ := selected.Metadata["access_token"].(string)
		ctx, cancel := context.WithTimeout(c.Request.Context(), 6*time.Second)
		defer cancel()
		value, err := (quota.Fetcher{}).FetchClaude(ctx, token, proxy, id, now)
		if err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Claude official quota unavailable"})
			return
		}
		cached = value
		c.JSON(200, value)
	}
}
