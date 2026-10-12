package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/billing"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

func admissionEngine(t *testing.T, handler gin.HandlerFunc) *gin.Engine {
	t.Helper()
	engine := gin.New()
	_ = engine.SetTrustedProxies(nil)
	engine.Use(func(c *gin.Context) { c.Set("userApiKey", "fixture-key"); c.Next() })
	engine.Use(Admission(func(key string) string {
		if key == "fixture-key" {
			return "admin"
		}
		return ""
	}))
	engine.POST("/v1/responses", handler)
	return engine
}

func TestAdmissionPromotesOnlyTrustedToolsAndSnapshotsIP(t *testing.T) {
	var meta requestMetadata
	engine := admissionEngine(t, func(c *gin.Context) {
		meta = c.Request.Context().Value(metadataKey{}).(requestMetadata)
		if !usage.RawUsageCaptureEnabled(c.Request.Context()) {
			t.Fatal("capture not enabled")
		}
		body, _ := io.ReadAll(c.Request.Body)
		if strings.Contains(string(body), "untrusted") || strings.Contains(string(body), "additional_tools") || !strings.Contains(string(body), `"name":"exec"`) {
			t.Fatalf("unsafe/duplicate tool admission: %s", body)
		}
		c.Status(http.StatusOK)
	})
	body := `{"model":"opencode/fixture","input":[{"type":"additional_tools","role":"user","tools":[{"type":"function","name":"untrusted"}]},{"type":"additional_tools","role":"developer","tools":[{"type":"custom","name":"exec"}]},{"role":"user","content":"test"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	req.RemoteAddr = "192.0.2.40:1234"
	req.Header.Set("X-Forwarded-For", "198.51.100.99")
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)
	if w.Code != http.StatusOK || meta.IP != "192.0.2.40" || meta.Model != "opencode/fixture" || meta.KeyID != "admin" {
		t.Fatalf("bad admitted metadata: %+v / %d", meta, w.Code)
	}
}

func TestAdmissionRejectsInvalidAndConflictingRequests(t *testing.T) {
	engine := admissionEngine(t, func(c *gin.Context) { t.Fatal("rejected body reached executor") })
	for _, body := range []string{
		`null`, `[]`, `{} {}`, `invalid`,
		`{"model":null}`, `{"model":12}`, `{"model":true}`, `{"model":{}}`, `{"model":""}`, `{"model":"   "}`,
		`{"model":"fixture\u0001"}`, `{"model":"fixture\t"}`, `{"model":"fixture\u007f"}`,
		`{"model":"opencode/fixture","tools":[{"type":"custom","name":"exec"}],"input":[{"type":"additional_tools","role":"developer","tools":[{"type":"function","name":"exec"}]}]}`,
	} {
		w := httptest.NewRecorder()
		engine.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body)))
		if w.Code != http.StatusBadRequest {
			t.Fatalf("accepted invalid request: %s", body)
		}
	}
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{}`))
	req.ContentLength = MaxBodyBytes + 1
	engine.ServeHTTP(w, req)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatal("oversized body not rejected")
	}
}

func TestReceiptsUseOnlyLegacyRawUsageAndSelectedAccount(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.jsonl")
	sink, err := OpenReceiptSink(path, map[string]AccountBinding{"selected-account-2": {ID: "codex2", Prefix: "codex2"}})
	if err != nil {
		t.Fatal(err)
	}
	stamp := time.Unix(1_800_000_000, 0)
	sink.now = func() time.Time { return stamp }
	ctx := context.WithValue(context.Background(), metadataKey{}, requestMetadata{
		Model: "codex2/gpt-6-luna", KeyID: "admin", IP: "192.0.2.5", Path: "/v1/responses", Started: stamp.Add(-2 * time.Second),
	})
	record := usage.Record{TraceID: "test-trace", Model: "gpt-6-luna", ResponseModel: "other-prefix/gpt-6-luna", AuthID: "selected-account-2", TTFT: time.Second,
		RequestedAt: stamp.Add(-time.Second), Latency: time.Second,
		Detail:         usage.Detail{InputTokens: 999999, OutputTokens: 888888, ReasoningTokens: 777777},
		RawUsageFormat: "chat", RawUsage: []byte(`{"prompt_tokens":1001,"completion_tokens":137,"prompt_tokens_details":{"cached_tokens":701,"cache_write_tokens":23},"completion_tokens_details":{"reasoning_tokens":100}}`)}
	// Simulate the accounting worker running after the quota reset boundary.
	sink.now = func() time.Time { return stamp.Add(2 * time.Hour) }
	sink.HandleUsage(ctx, record)
	failed := record
	failed.Failed = true
	sink.HandleUsage(ctx, failed)
	if err := sink.Err(); err != nil {
		t.Fatal(err)
	}
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil || strings.Count(string(data), "\n") != 1 || strings.Contains(string(data), "999999") {
		t.Fatal("wrong receipt count or normalized SDK usage leaked into billing")
	}
	var row billing.Row
	if err := json.Unmarshal(data, &row); err != nil {
		t.Fatal(err)
	}
	if row.TS != stamp.Unix() || row.Input != 1001 || row.Output != 137 || row.Cached != 701 || row.Write != 23 || row.ModelMismatch || row.OAuthAccountID != "codex2" || row.DurationMS != 2000 || row.TTFTMS != 1000 || row.OutputTPS != 68.5 {
		t.Fatalf("changed legacy receipt: %+v", row)
	}
	amount := billing.Cost(row, map[string]*billing.Rates{"gpt-6-luna": {Input: .1, Cached: .01, Write: .125, Output: .5}})
	if amount != .00010838 {
		t.Fatalf("legacy price formula changed: %.8f", amount)
	}
}

func TestMissingRawUsageCannotFallBackToSDKCounts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.jsonl")
	sink, err := OpenReceiptSink(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sink.Close() }()
	ctx := context.WithValue(context.Background(), metadataKey{}, requestMetadata{})
	sink.HandleUsage(ctx, usage.Record{Detail: usage.Detail{InputTokens: 5, OutputTokens: 6}})
	if sink.Err() == nil {
		t.Fatal("missing raw usage should be visible as accounting failure")
	}
	data, _ := os.ReadFile(path)
	if len(data) > 0 {
		t.Fatal("fake zero/normalized receipt created")
	}
}
