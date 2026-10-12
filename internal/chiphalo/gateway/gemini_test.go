package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/api"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/billing"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v8/sdk/access"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	"github.com/tidwall/gjson"
)

const geminiFixtureModel = "gemini-native-fixture"
const geminiUsageFixture = `{"promptTokenCount":1000,"cachedContentTokenCount":700,"toolUsePromptTokenCount":20,"candidatesTokenCount":60,"thoughtsTokenCount":140,"totalTokenCount":1200}`

type geminiCapture struct {
	sink    *ReceiptSink
	records chan usage.Record
}

type disabledGeminiCapture struct{}

func (disabledGeminiCapture) HandleUsage(context.Context, usage.Record) {}

func (s *geminiCapture) HandleUsage(ctx context.Context, r usage.Record) {
	if _, ok := ctx.Value(metadataKey{}).(requestMetadata); !ok || r.Provider != "gemini" {
		return
	}
	s.sink.HandleUsage(ctx, r)
	s.records <- r
}

func nativeRequest(t *testing.T, server *api.Server, method, path, body string, header http.Header) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header = header
	r.RemoteAddr = "192.0.2.50:1234"
	w := httptest.NewRecorder()
	server.Handler().ServeHTTP(w, r)
	return w
}

func TestNativeGeminiActualServerExecutorToolsStreamAndBilling(t *testing.T) {
	f := newProfileFixture(t, false)
	var mu sync.Mutex
	var upstreamRequests []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			w.WriteHeader(500)
			return
		}
		mu.Lock()
		upstreamRequests = append(upstreamRequests, r.URL.Path)
		mu.Unlock()
		if r.Header.Get("X-Goog-Api-Key") != "upstream-fixture-secret" || r.Header.Get("Authorization") != "" {
			t.Error("downstream credential leaked to upstream")
		}
		if !strings.HasPrefix(r.URL.Path, "/v1beta/models/"+geminiFixtureModel+":") {
			t.Errorf("account prefix leaked into upstream URL: %s", r.URL.Path)
		}
		if strings.HasSuffix(r.URL.Path, ":countTokens") {
			if nested := gjson.GetBytes(raw, "generateContentRequest.model"); nested.Exists() && nested.String() != "models/"+geminiFixtureModel {
				t.Errorf("public account prefix leaked into embedded upstream model: %s", nested.String())
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"totalTokens":123}`))
			return
		}
		if !strings.Contains(string(raw), `"contents"`) {
			t.Error("native contents lost")
		}
		if strings.HasSuffix(r.URL.Path, ":streamGenerateContent") {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"modelVersion\":\"gemini-native-fixture\",\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"Hello\"}]}}],\"usageMetadata\":{\"promptTokenCount\":1000,\"candidatesTokenCount\":1}}\n\n"))
			w.(http.Flusher).Flush()
			_, _ = w.Write([]byte(`data: {"modelVersion":"gemini-native-fixture","candidates":[{"content":{"role":"model","parts":[{"text":" world"}]},"finishReason":"STOP"}],"usageMetadata":` + geminiUsageFixture + "}\n\n"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"modelVersion":"gemini-native-fixture","candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"lookup","args":{"city":"Singapore"}}}]},"finishReason":"STOP"}],"usageMetadata":` + geminiUsageFixture + `}`))
	}))
	defer upstream.Close()
	cfg := &config.Config{}
	f.manager.RegisterExecutor(executor.NewGeminiExecutor(cfg))
	for _, prefix := range []string{"google-a", "google-b"} {
		auth := &coreauth.Auth{ID: prefix + "-fixture", Provider: "gemini", Prefix: prefix, Status: coreauth.StatusActive, Attributes: map[string]string{"api_key": "upstream-fixture-secret", "base_url": upstream.URL}}
		if _, err := f.manager.Register(context.Background(), auth); err != nil {
			t.Fatal(err)
		}
		registry.GetGlobalRegistry().RegisterClient(auth.ID, "gemini", []*registry.ModelInfo{{ID: prefix + "/" + geminiFixtureModel, Name: geminiFixtureModel, Type: "gemini", SupportedGenerationMethods: []string{"generateContent", "countTokens"}, SupportedInputModalities: []string{"TEXT", "IMAGE"}, InputTokenLimit: 100000}})
		id := auth.ID
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
	}
	sink, err := OpenReceiptSink(filepath.Join(t.TempDir(), "native-usage.jsonl"), nil)
	if err != nil {
		t.Fatal(err)
	}
	capture := &geminiCapture{sink: sink, records: make(chan usage.Record, 10)}
	usage.RegisterNamedPlugin("native-gemini-e2e", capture)
	t.Cleanup(func() { usage.RegisterNamedPlugin("native-gemini-e2e", disabledGeminiCapture{}); _ = sink.Close() })
	access := sdkaccess.NewManager()
	access.SetProviders(sdkaccess.RegisteredProviders())
	server := api.NewServer(cfg, f.manager, access, filepath.Join(t.TempDir(), "config.yaml"), f.profile.ServerOptions()...)
	header := http.Header{"X-Goog-Api-Key": {"issued-test-key"}}
	for _, path := range []string{"/v1beta/models", "/v1beta/models/google-a/" + geminiFixtureModel} {
		w := nativeRequest(t, server, "GET", path, "", header)
		if w.Code != 200 || !strings.Contains(w.Body.String(), "models/google-a/"+geminiFixtureModel) || strings.Contains(w.Body.String(), identityModel) {
			t.Fatalf("wrong native catalog: %d %s", w.Code, w.Body.String())
		}
	}
	body := `{"contents":[{"role":"user","parts":[{"text":"lookup"}]}],"tools":[{"functionDeclarations":[{"name":"lookup","parameters":{"type":"OBJECT","properties":{"city":{"type":"STRING"}}}}]}]}`
	for _, tc := range []struct {
		prefix, action, body string
		bill                 bool
	}{
		{"google-a", "generateContent", body, true},
		{"google-b", "generateContent", `{"contents":[{"role":"user","parts":[{"text":"lookup"}]},{"role":"model","parts":[{"functionCall":{"name":"lookup","args":{"city":"Singapore"}}}]},{"role":"user","parts":[{"functionResponse":{"name":"lookup","response":{"temperature":30}}}]}]}`, true},
		{"google-a", "streamGenerateContent?alt=sse", body, true},
		{"google-a", "countTokens", `{"contents":[{"parts":[{"text":"count me"}]}]}`, false},
		{"google-b", "countTokens", `{"generateContentRequest":{"model":"models/google-b/gemini-native-fixture","contents":[{"parts":[{"text":"count nested request"}]}]}}`, false},
	} {
		w := nativeRequest(t, server, "POST", "/v1beta/models/"+tc.prefix+"/"+geminiFixtureModel+":"+tc.action, tc.body, header)
		if w.Code != 200 {
			t.Fatalf("native action failed: %d %s", w.Code, w.Body.String())
		}
		if !tc.bill {
			if !strings.Contains(w.Body.String(), `"totalTokens":123`) {
				t.Fatal("token count lost")
			}
			continue
		}
		if tc.action == "streamGenerateContent?alt=sse" {
			if !strings.Contains(w.Body.String(), "data: ") || !strings.Contains(w.Body.String(), "Hello") || !strings.Contains(w.Body.String(), "world") {
				t.Fatal("SSE frames lost")
			}
		} else if !strings.Contains(w.Body.String(), `"functionCall"`) {
			t.Fatal("native tool call lost")
		}
		select {
		case rec := <-capture.records:
			if rec.AuthID != tc.prefix+"-fixture" || rec.RawUsageFormat != "gemini" || string(rec.RawUsage) != geminiUsageFixture {
				t.Fatalf("wrong account or final raw usage: %+v", rec)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("native usage not published")
		}
	}
	if sink.Err() != nil {
		t.Fatal(sink.Err())
	}
	raw, _ := os.ReadFile(sink.file.Name())
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 3 {
		t.Fatalf("countTokens or SSE double-billed: %s", raw)
	}
	for _, line := range lines {
		var row billing.Row
		if json.Unmarshal([]byte(line), &row) != nil {
			t.Fatal("bad receipt")
		}
		if row.Input != 1020 || row.Output != 200 || row.Cached != 700 || row.KeyID != "issued-fixture" || row.ClientIP != "192.0.2.50" || row.ModelMismatch {
			t.Fatalf("incorrect receipt: %+v", row)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(upstreamRequests) != 5 || len(f.executor.selected) != 0 {
		t.Fatal("native traffic crossed into Codex or retries")
	}
}

func TestNativeGeminiRejectsAmbiguityAndUnauditedRoutesBeforeDispatch(t *testing.T) {
	f := newProfileFixture(t, false)
	access := sdkaccess.NewManager()
	access.SetProviders(sdkaccess.RegisteredProviders())
	server := api.NewServer(&config.Config{}, f.manager, access, filepath.Join(t.TempDir(), "config.yaml"), f.profile.ServerOptions()...)
	for _, tc := range []struct {
		path, body string
		status     int
	}{
		{"/v1beta/models/gemini-native-fixture:generateContent", `null`, 400},
		{"/v1beta/models/gemini-native-fixture:generateContent", `{"model":"codex2/gpt-identity-fixture"}`, 400},
		{"/v1beta/models/gemini-native-fixture:generateContent", `{"model":"gemini-native-fixture","\u006dodel":"gemini-native-fixture"}`, 400},
		{"/v1beta/models/gemini-native-fixture:countTokens", `{"generateContentRequest":{"model":"models/codex2/gpt-identity-fixture"}}`, 400},
		{"/v1beta/models/gemini-native-fixture:countTokens", `{"generateContentRequest":{},"generate_content_request":{}}`, 400},
		{"/v1beta/models/codex2/gpt-identity-fixture:generateContent", `{"contents":[]}`, 400},
		{"/v1beta/models/gpt-unpublished-fixture:generateContent", `{"contents":[]}`, 404},
		{"/v1beta/models/gemini-native-fixture:unknown", `{}`, 404},
		{"/v1beta/models/gemini-native-fixture:generateContent:countTokens", `{}`, 404},
		{"/v1beta/models/google%2Fa:generateContent", `{}`, 404},
		{"/v1beta/interactions", `{}`, 404},
	} {
		w := nativeRequest(t, server, "POST", tc.path, tc.body, http.Header{"Authorization": {"Bearer admin-test-key"}})
		if w.Code != tc.status {
			t.Fatalf("bad route accepted: %s: %d %s", tc.path, w.Code, w.Body.String())
		}
	}
	r := httptest.NewRequest("POST", "/v1beta/models/gemini-native-fixture:generateContent", strings.NewReader(`{}`))
	r.ContentLength = MaxBodyBytes + 1
	r.Header.Set("Authorization", "Bearer admin-test-key")
	w := httptest.NewRecorder()
	server.Handler().ServeHTTP(w, r)
	if w.Code != 413 {
		t.Fatal("native payload limit bypassed")
	}
	if len(f.executor.selected) != 0 || f.executor.results != 0 {
		t.Fatal("invalid native traffic dispatched or poisoned Codex")
	}
}

func TestNativeGeminiExecutionGuardRejectsCodexAndForeignBackends(t *testing.T) {
	f := newProfileFixture(t, false)
	ctx := context.WithValue(context.Background(), metadataKey{}, requestMetadata{Path: "/v1beta/models/x:generateContent"})
	ctx = f.profile.decorateAdmission(ctx, "")
	// Direct manager execution exercises the last credential boundary even when
	// HTTP catalog/admission are skipped or a registration changes concurrently.
	_, err := f.manager.Execute(ctx, []string{"codex"}, coreexecutor.Request{Model: identityModel}, coreexecutor.Options{})
	if err == nil || len(f.executor.selected) != 0 || f.executor.results != 0 {
		t.Fatal("native execution policy did not remain terminal")
	}
}

func TestGeminiBodyIdentityAndKeyPolicy(t *testing.T) {
	for _, raw := range []string{`{"contents":[]}`, `{"model":"models/google-a/gemini-fixture"}`, `{"generateContentRequest":{"model":"models/google-a/gemini-fixture","contents":[]}}`} {
		if err := validateGeminiBody([]byte(raw), "google-a/gemini-fixture", "countTokens"); err != nil {
			t.Fatal(err)
		}
	}
	f := newProfileFixture(t, false)
	auth := &coreauth.Auth{ID: "gemini-disabled", Provider: "gemini", Status: coreauth.StatusActive}
	_, _ = f.manager.Register(context.Background(), auth)
	registry.GetGlobalRegistry().RegisterClient(auth.ID, "gemini", []*registry.ModelInfo{{ID: geminiFixtureModel, Name: geminiFixtureModel}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
	access := sdkaccess.NewManager()
	access.SetProviders(sdkaccess.RegisteredProviders())
	server := api.NewServer(&config.Config{}, f.manager, access, filepath.Join(t.TempDir(), "config.yaml"), f.profile.ServerOptions()...)
	hash := sha256.Sum256([]byte("issued-test-key"))
	raw, _ := json.Marshal(map[string]any{"keys": []map[string]any{{"id": "issued-fixture", "hash": hex.EncodeToString(hash[:]), "modelPatterns": []string{"codex2/*"}}}})
	if err := f.profile.Keys.Reload(raw); err != nil {
		t.Fatal(err)
	}
	header := http.Header{"X-Goog-Api-Key": {"issued-test-key"}}
	w := nativeRequest(t, server, "GET", "/v1beta/models", "", header)
	if w.Code != 200 || strings.Contains(w.Body.String(), geminiFixtureModel) {
		t.Fatal("restricted native catalog leaked model")
	}
	w = nativeRequest(t, server, "POST", "/v1beta/models/"+geminiFixtureModel+":generateContent", `{}`, header)
	if w.Code != 403 {
		t.Fatal("native generation bypassed key model policy")
	}
	auth.Disabled = true
	auth.Status = coreauth.StatusDisabled
	_, _ = f.manager.Update(context.Background(), auth)
	w = nativeRequest(t, server, "GET", "/v1beta/models", "", http.Header{"Authorization": {"Bearer admin-test-key"}})
	if w.Code != 200 || strings.Contains(w.Body.String(), geminiFixtureModel) {
		t.Fatal("disabled native account remained visible")
	}
}
