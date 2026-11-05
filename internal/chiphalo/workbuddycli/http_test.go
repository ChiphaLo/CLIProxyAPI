package workbuddycli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	responses "github.com/router-for-me/CLIProxyAPI/v8/internal/translator/openai/openai/responses"
)

const fixtureKey = "a-strong-private-key-only-for-the-test-fixture"

func fixtureServer(t *testing.T) *Server {
	t.Helper()
	s, err := NewServer(Config{}, fixtureKey)
	if err != nil {
		t.Fatal(err)
	}
	s.generate = func(context.Context, Request, func(string) error) (*Result, error) {
		t.Fatal("unexpected CLI invocation")
		return nil, nil
	}
	return s
}

func TestHTTPRequiresAuthenticationAndDoesNotExposeCredential(t *testing.T) {
	s := fixtureServer(t)
	for _, path := range []string{"/v1/models", "/v1/chat/completions", "/credential"} {
		w := httptest.NewRecorder()
		s.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 401 || strings.Contains(w.Body.String(), fixtureKey) {
			t.Fatalf("unauthorized response: %d %s", w.Code, w.Body.String())
		}
	}
}

func TestHTTPRejectsUnsupportedGenerationControls(t *testing.T) {
	s := fixtureServer(t)
	for _, extra := range []string{`,"max_tokens":0`, `,"temperature":0.2`, `,"response_format":{"type":"json_object"}`, `,"store":true`} {
		r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"fixture"}]`+extra+`}`))
		r.Header.Set("Authorization", "Bearer "+fixtureKey)
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != 400 {
			t.Fatalf("unsupported control silently ignored: %d %s", w.Code, w.Body.String())
		}
	}
}

func TestDecodeRequestAcceptsOnlyOrdinaryTextResponseFormat(t *testing.T) {
	for _, value := range []string{`{"type":"text"}`, `null`, `{"type":"te\u0078t"}`} {
		raw := []byte(`{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"fixture"}],"response_format":` + value + `}`)
		if _, _, err := decodeRequest(raw); err != nil {
			t.Fatalf("ordinary text default was rejected: %v", err)
		}
	}
	for _, value := range []string{`{"type":"json_object"}`, `{"type":"json_schema","json_schema":{"name":"fixture","schema":{"type":"object"}}}`, `{"type":"text","json_schema":{}}`, `{}`, `[]`, `"text"`, `{"type":null}`} {
		raw := []byte(`{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"fixture"}],"response_format":` + value + `}`)
		if _, _, err := decodeRequest(raw); err == nil || !strings.Contains(err.Error(), "response_format") {
			t.Fatal("structured or malformed output constraint was silently discarded")
		}
	}
}

func TestCodexResponsesTextDefaultPassesRealTranslatorAndHTTPAdapter(t *testing.T) {
	raw, err := responses.ConvertOpenAIResponsesRequestToOpenAIChatCompletions("deepseek-v4.1-flash", []byte(`{"model":"workbuddy-intl/deepseek-v4.1-flash","instructions":"Inert protocol fixture.","input":[{"role":"user","content":[{"type":"input_text","text":"Reply OK."}]}],"text":{"format":{"type":"text"},"verbosity":"low"},"reasoning":{"effort":"high"},"stream":true,"store":false}`), true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"response_format":{"type":"text"}`) {
		t.Fatal("fixture did not exercise the real Responses format conversion")
	}
	s := fixtureServer(t)
	s.generate = func(_ context.Context, r Request, emit func(string) error) (*Result, error) {
		if r.ReasoningEffort != "high" {
			t.Fatal("reasoning changed")
		}
		if err := emit("OK"); err != nil {
			return nil, err
		}
		return &Result{Text: "OK", Model: r.Model, FinishReason: "stop", Usage: json.RawMessage(`{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}`)}, nil
	}
	r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(string(raw)))
	r.Header.Set("Authorization", "Bearer "+fixtureKey)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"content":"OK"`) || !strings.Contains(w.Body.String(), "[DONE]") {
		t.Fatalf("Codex text format compatibility failed: %d %s", w.Code, w.Body.String())
	}
}

func TestDecodeRequestNamesUnsupportedParameterWithoutEchoingValues(t *testing.T) {
	_, _, err := decodeRequest([]byte(`{"model":"deepseek-v4.1-flash","messages":[],"temperature":"private-value-fixture","max_tokens":128}`))
	if err == nil || !strings.Contains(err.Error(), `"temperature"`) || strings.Contains(err.Error(), "private-value-fixture") {
		t.Fatalf("unsafe or nondeterministic diagnostic: %v", err)
	}
}

func TestHTTPStreamsTextAndAuthoritativeUsageExactlyOnce(t *testing.T) {
	s := fixtureServer(t)
	s.generate = func(_ context.Context, r Request, emit func(string) error) (*Result, error) {
		if err := emit("fixture"); err != nil {
			return nil, err
		}
		return &Result{Text: "fixture", Model: r.Model, FinishReason: "stop", Usage: json.RawMessage(`{"prompt_tokens":100,"completion_tokens":30,"total_tokens":130,"completion_tokens_details":{"reasoning_tokens":20}}`)}, nil
	}
	r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"fixture"}],"stream":true}`))
	r.Header.Set("Authorization", "Bearer "+fixtureKey)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"content":"fixture"`) || strings.Count(w.Body.String(), `"usage":`) != 1 || !strings.Contains(w.Body.String(), `"reasoning_tokens":20`) || strings.Count(w.Body.String(), "[DONE]") != 1 {
		t.Fatalf("invalid SSE: %d %s", w.Code, w.Body.String())
	}
}

func TestHTTPDoesNotInventCompletionOnFailure(t *testing.T) {
	s := fixtureServer(t)
	s.generate = func(context.Context, Request, func(string) error) (*Result, error) {
		return nil, errors.New("fixture upstream failure")
	}
	r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"fixture"}],"stream":true}`))
	r.Header.Set("Authorization", "Bearer "+fixtureKey)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if !strings.Contains(w.Body.String(), `"error":`) || strings.Contains(w.Body.String(), "[DONE]") || strings.Contains(w.Body.String(), `"usage":`) {
		t.Fatalf("failure became fake completion: %s", w.Body.String())
	}
}

func TestHTTPResourceLimitDoesNotStartSecondClient(t *testing.T) {
	s := fixtureServer(t)
	s.maxQueued = 0
	s.slot <- struct{}{}
	r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"fixture"}]}`))
	r.Header.Set("Authorization", "Bearer "+fixtureKey)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 429 || w.Header().Get("Retry-After") == "" {
		t.Fatalf("worker was not bounded: %d", w.Code)
	}
}

func TestHTTPLegacyFallbackPreservesExistingModelRequest(t *testing.T) {
	want := `{"model":"glm-5.2","messages":[{"role":"user","content":"fixture"}],"max_tokens":23,"stream":true}`
	legacy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if string(raw) != want || r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer "+fixtureKey {
			t.Errorf("legacy request changed: %s %s", r.URL.Path, raw)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: legacy-fixture\n\n")
	}))
	defer legacy.Close()
	s := fixtureServer(t)
	if err := s.SetFallback(legacy.URL, []string{"glm-5.2"}); err != nil {
		t.Fatal(err)
	}
	s.slot <- struct{}{}
	r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(want))
	r.Header.Set("Authorization", "Bearer "+fixtureKey)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 200 || w.Body.String() != "data: legacy-fixture\n\n" {
		t.Fatalf("legacy route failed: %d %s", w.Code, w.Body.String())
	}
}

func TestHTTPRejectsNonLoopbackFallbackOrModelCollision(t *testing.T) {
	s := fixtureServer(t)
	for _, url := range []string{"https://example.com", "http://169.254.169.254:80", "http://user:password@127.0.0.1:8332", "http://127.0.0.1:8332/private", "http://127.0.0.1:8332?url=elsewhere"} {
		if err := s.SetFallback(url, []string{"fast-model"}); err == nil {
			t.Fatalf("unsafe fallback accepted: %s", url)
		}
	}
	if err := s.SetFallback("http://127.0.0.1:8332", []string{"deepseek-v4.1-flash"}); err == nil {
		t.Fatal("official model routed through legacy fallback")
	}
}

func TestManagementCompatibilityNeverSubstitutesWorkerKey(t *testing.T) {
	legacy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer distinct-management-fixture" {
			w.WriteHeader(401)
			return
		}
		_, _ = io.WriteString(w, `{"authenticated_by_original_host":true}`)
	}))
	defer legacy.Close()
	s := fixtureServer(t)
	if err := s.SetFallback(legacy.URL, []string{"fast-model"}); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"", fixtureKey, "distinct-management-fixture"} {
		r := httptest.NewRequest("GET", "/v0/management/workbuddy/quota/status", nil)
		r.Header.Set("Authorization", "Bearer "+key)
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		want := 401
		if key == "distinct-management-fixture" {
			want = 200
		}
		if w.Code != want {
			t.Fatalf("management authentication changed: got %d want %d", w.Code, want)
		}
	}
}
