package handlers

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/session"
	translator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func TestVerifiedRequestIdentityStaysInsideExecutionMetadata(t *testing.T) {
	body := []byte(`{"model":"claude-opus-5-5","input":"hello"}`)
	id := session.DeriveID(translator.FormatOpenAIResponse, body, session.CallerScope("fixture"))
	for _, valid := range []bool{true, false} {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest("POST", "/v1/responses", strings.NewReader(string(body)))
		c.Set("userApiKey", "fixture")
		key := "fixture"
		if !valid {
			key = "external-client"
		}
		c.Request.Header.Set(session.BridgeHeader, session.SignBridge(key, body, id, time.Now()))
		if _, err := ReadRequestBody(c); err != nil {
			t.Fatal(err)
		}
		meta := requestExecutionMetadata(context.WithValue(context.Background(), "gin", c))
		got, _ := meta[coreexecutor.DerivedSessionIDMetadataKey].(string)
		if (got == id) != valid {
			t.Fatal("unverified identity accepted or valid identity dropped")
		}
		if c.Request.Header.Get(session.BridgeHeader) != "" {
			t.Fatal("private identity header retained")
		}
	}
}
