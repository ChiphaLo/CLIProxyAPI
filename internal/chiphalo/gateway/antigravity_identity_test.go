package gateway

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/billing"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

func TestAntigravityReportedCatalogHidesOnlyLegacyListing(t *testing.T) {
	f := newProfileFixture(t, false)
	const old = "antigravity/gemini-3.8-flash-high"
	const canonical = "antigravity/gemini-3.8-flash-n"
	auth := &coreauth.Auth{ID: "antigravity-catalog-fixture", Provider: "antigravity", Prefix: "antigravity", Status: coreauth.StatusActive}
	if _, err := f.manager.Register(t.Context(), auth); err != nil {
		t.Fatal(err)
	}
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(auth.ID, "antigravity", []*registry.ModelInfo{
		{ID: old, Type: "antigravity", Thinking: &registry.ThinkingSupport{Levels: []string{"low", "medium", "high"}}},
		{ID: canonical, Type: "antigravity", MetadataModelID: "gemini-3.8-flash-high", Thinking: &registry.ThinkingSupport{Levels: []string{"low", "medium", "high"}}},
	})
	t.Cleanup(func() { reg.UnregisterClient(auth.ID) })
	for _, path := range []string{"/v1/models", "/v1/models?client_version=0.170.1"} {
		r := httptest.NewRequest("GET", path, nil)
		r.Header.Set("Authorization", "Bearer admin-test-key")
		w := httptest.NewRecorder()
		f.engine.ServeHTTP(w, r)
		if w.Code != 200 || strings.Contains(w.Body.String(), `"`+old+`"`) || !strings.Contains(w.Body.String(), `"`+canonical+`"`) {
			t.Fatalf("wrong public catalog: %d %s", w.Code, w.Body.String())
		}
	}
	if !slices.Contains(reg.GetModelProviders(old), "antigravity") {
		t.Fatal("legacy route removed")
	}
	engine := gin.New()
	engine.Use(func(c *gin.Context) { c.Set("userApiKey", "admin") }, f.profile.GeminiCatalog(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, f.manager)))
	engine.Any("/v1beta/*path", func(c *gin.Context) { c.Status(204) })
	for _, path := range []string{"/v1beta/models", "/v1beta/models/" + old, "/v1beta/models/" + canonical} {
		w := httptest.NewRecorder()
		engine.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 200 || path == "/v1beta/models" && (strings.Contains(w.Body.String(), `"models/`+old+`"`) || !strings.Contains(w.Body.String(), `"models/`+canonical+`"`)) {
			t.Fatalf("wrong native catalog or compatibility: %d %s", w.Code, w.Body.String())
		}
	}
	for _, id := range []string{old, canonical} {
		w := httptest.NewRecorder()
		engine.ServeHTTP(w, httptest.NewRequest("POST", "/v1beta/models/"+id+":streamGenerateContent?alt=sse", nil))
		if w.Code != 204 {
			t.Fatalf("native alias not admitted: %d %s", w.Code, w.Body.String())
		}
	}
}

func TestAntigravityReceiptKeepsRequestRouteAndRawResponse(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.jsonl")
	sink, err := OpenReceiptSink(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sink.Close() })
	stamp := time.Unix(1800000000, 0)
	ctx := context.WithValue(t.Context(), metadataKey{}, requestMetadata{Model: "antigravity/gemini-3.8-flash-high", Path: "/v1/responses", Started: stamp})
	sink.HandleUsage(ctx, usage.Record{Provider: "antigravity", Model: "gemini-3.8-flash-high", ResponseModel: "gemini-3.8-flash-n", RequestedAt: stamp, Latency: time.Second, RawUsageFormat: "gemini", RawUsage: []byte(geminiUsageFixture)})
	if sink.Err() != nil {
		t.Fatal(sink.Err())
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var row billing.Row
	if json.Unmarshal(raw, &row) != nil || row.Model != "antigravity/gemini-3.8-flash-high" || row.RoutedModel != "gemini-3.8-flash-high" || row.UpstreamReportedModel != "gemini-3.8-flash-n" || row.ModelMismatch || row.Input != 1020 || row.Output != 200 {
		t.Fatalf("identity or tokens changed: %s", raw)
	}
}
