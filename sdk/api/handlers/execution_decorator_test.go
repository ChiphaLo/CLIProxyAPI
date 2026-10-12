package handlers

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

type admittedMetadataKey struct{}
type parentMetadataKey struct{}

func TestHandlerTransfersOnlyExplicitAdmissionSnapshot(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
	requestCtx := context.WithValue(c.Request.Context(), admittedMetadataKey{}, "not-implicitly-copied")
	c.Request = c.Request.WithContext(requestCtx)
	handler := &BaseAPIHandler{Cfg: &config.SDKConfig{}}
	parent := context.WithValue(context.Background(), parentMetadataKey{}, "stream-delivery")
	ctx, cancel := handler.GetContextWithCancel(nil, c, parent)
	if ctx.Value(admittedMetadataKey{}) != nil {
		t.Fatal("arbitrary request values copied without opting in")
	}
	cancel()
	c.Request = c.Request.WithContext(WithExecutionContextDecorator(requestCtx, func(ctx context.Context) context.Context {
		return context.WithValue(ctx, admittedMetadataKey{}, "immutable-snapshot")
	}))
	ctx, cancel = handler.GetContextWithCancel(nil, c, parent)
	if ctx.Value(admittedMetadataKey{}) != "immutable-snapshot" || ctx.Value(parentMetadataKey{}) != "stream-delivery" {
		t.Fatal("admission/stream-delivery metadata lost")
	}
	cancel()
	if ctx.Err() != context.Canceled {
		t.Fatal("decorator broke execution cancellation")
	}
}
