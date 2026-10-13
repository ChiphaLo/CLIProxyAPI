package gateway

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

func TestAuditAdmissionAndFailedStreamRetainHistoryWithoutSecrets(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	history := "{\"historical\":true}\n"
	if err := os.WriteFile(path, []byte(history), 0640); err != nil {
		t.Fatal(err)
	}
	sink, err := OpenAuditSink(path)
	if err != nil {
		t.Fatal(err)
	}
	engine := gin.New()
	_ = engine.SetTrustedProxies(nil)
	engine.Use(sink.Middleware())
	engine.GET("/v1/models", func(c *gin.Context) { c.AbortWithStatusJSON(401, gin.H{"error": "fixture"}) })
	req := httptest.NewRequest("GET", "/v1/models?private=SECRET_QUERY", nil)
	req.RemoteAddr = "192.0.2.1:1"
	req.Header.Set("Authorization", "Bearer SECRET_BEARER")
	engine.ServeHTTP(httptest.NewRecorder(), req)
	stamp := time.Now().Add(-time.Second)
	ctx := context.WithValue(context.Background(), metadataKey{}, requestMetadata{Model: "opencode/fixture", KeyID: "admin", Path: "/v1/responses", IP: "192.0.2.1", Started: stamp})
	sink.HandleUsage(ctx, usage.Record{Failed: true, Fail: usage.Failure{StatusCode: 429, Body: "SECRET_PROVIDER_BODY"}, TraceID: "fixture-id", Provider: "opencode", RequestedAt: stamp, Latency: time.Second})
	if err := sink.Err(); err != nil {
		t.Fatal(err)
	}
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	if !strings.HasPrefix(string(raw), history) || strings.Contains(string(raw), "SECRET_") {
		t.Fatal("audit altered history or leaked credential material")
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 3 {
		t.Fatal("admission or committed-stream failure lost")
	}
	var row AuditRow
	if err := json.Unmarshal([]byte(lines[2]), &row); err != nil {
		t.Fatal(err)
	}
	if row.Status != 429 || row.KeyID != "admin" || row.Model != "opencode/fixture" || row.ErrorClass != "rate_limit_error" || row.RequestID != "fixture-id" {
		t.Fatalf("lost failure metadata: %+v", row)
	}
}
