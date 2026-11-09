package gateway

import (
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/session"
)

func TestPublicAdmissionStripsPrivateIdentityHeader(t *testing.T) {
	router := gin.New()
	router.Use(Admission(nil))
	router.GET("/v1/models", func(c *gin.Context) {
		if c.Request.Header.Get(session.BridgeHeader) != "" {
			t.Error("client identity claim reached SDK")
		}
		c.Status(200)
	})
	r := httptest.NewRequest("GET", "/v1/models", nil)
	r.Header.Set(session.BridgeHeader, "forged-by-client")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
}
