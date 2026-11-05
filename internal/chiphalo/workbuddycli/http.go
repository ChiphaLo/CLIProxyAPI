package workbuddycli

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
)

type Server struct {
	Client             Config
	APIKey             string
	slot               chan struct{}
	generate           func(context.Context, Request, func(string) error) (*Result, error)
	fallback           http.Handler
	managementFallback http.Handler
	native             http.Handler
	legacyModels       map[string]bool
	queueMu            sync.Mutex
	queued             int
	queuedBytes        int64
	maxQueued          int
}

var legacyModelName = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,128}$`)

// SetNativeUpstream routes the Intl DeepSeek model to an isolated Go gateway.
// Original plugin management and legacy models keep their existing endpoints.
func (s *Server) SetNativeUpstream(address string) error {
	u, err := url.Parse(address)
	if err != nil || u.Scheme != "http" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" || net.ParseIP(u.Hostname()) == nil || !net.ParseIP(u.Hostname()).IsLoopback() || u.Port() == "" {
		return errors.New("native upstream must be a fixed loopback HTTP origin")
	}
	proxy := httputil.NewSingleHostReverseProxy(u)
	direct := proxy.Director
	proxy.Director = func(r *http.Request) { direct(r); r.Host = u.Host; r.Header.Set("Authorization", "Bearer "+s.APIKey) }
	proxy.Transport = &http.Transport{Proxy: nil}
	proxy.FlushInterval = -1
	proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, _ error) {
		writeError(w, 502, "native_transport_unavailable", "Native WorkBuddy Intl transport unavailable")
	}
	s.native = proxy
	return nil
}

// SetFallback retains explicitly allowlisted pre-existing WorkBuddy models on
// their original loopback plugin host. It cannot be configured through HTTP.
func (s *Server) SetFallback(address string, models []string) error {
	u, err := url.Parse(address)
	if err != nil || u.Scheme != "http" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" && u.Path != "/" || net.ParseIP(u.Hostname()) == nil || !net.ParseIP(u.Hostname()).IsLoopback() || u.Port() == "" {
		return errors.New("legacy fallback must be a fixed loopback HTTP origin")
	}
	allowed := map[string]bool{}
	for _, model := range models {
		if !legacyModelName.MatchString(model) || model == "deepseek-v4.1-flash" || allowed[model] {
			return errors.New("invalid legacy fallback model allowlist")
		}
		allowed[model] = true
	}
	u.Path = ""
	proxy := httputil.NewSingleHostReverseProxy(u)
	direct := proxy.Director
	proxy.Director = func(r *http.Request) { direct(r); r.Host = u.Host; r.Header.Set("Authorization", "Bearer "+s.APIKey) }
	proxy.Transport = &http.Transport{Proxy: nil}
	proxy.FlushInterval = -1
	proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, _ error) {
		writeError(w, 502, "legacy_transport_unavailable", "Legacy WorkBuddy transport unavailable")
	}
	management := httputil.NewSingleHostReverseProxy(u)
	managementDirector := management.Director
	management.Director = func(r *http.Request) {
		managementDirector(r)
		r.Host = u.Host
		r.Header.Del("Forwarded")
		r.Header.Del("X-Forwarded-For")
		r.Header.Del("X-Real-IP")
	}
	management.Transport = &http.Transport{Proxy: nil}
	management.FlushInterval = -1
	management.ErrorHandler = proxy.ErrorHandler
	// Preserve the existing private plugin management surface. Authentication
	// remains entirely at the original host; API keys are never substituted for
	// management keys, and no new management implementation is introduced.
	s.managementFallback = management
	s.fallback, s.legacyModels = proxy, allowed
	return nil
}

func NewServer(cfg Config, key string) (*Server, error) {
	return NewServerWithQueue(cfg, key, 8)
}

// NewServerWithQueue keeps one active official CLI and bounds waiting requests.
// Queue configuration is startup-only, never controlled by downstream JSON.
func NewServerWithQueue(cfg Config, key string, maxQueued int) (*Server, error) {
	if len(key) < 32 || strings.ContainsAny(key, "\x00\r\n") {
		return nil, errors.New("official-client adapter requires a strong private API key")
	}
	if maxQueued < 0 || maxQueued > 32 {
		return nil, errors.New("official-client queue capacity must be between 0 and 32")
	}
	return &Server{Client: cfg, APIKey: key, slot: make(chan struct{}, 1), generate: cfg.Generate, maxQueued: maxQueued}, nil
}

var errQueueFull = errors.New("official-client waiting queue is full")

const maxQueuedPayloadBytes int64 = 32 << 20

func (s *Server) acquire(ctx context.Context, payloadBytes int64, heartbeat func() error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case s.slot <- struct{}{}:
		return nil
	default:
	}
	s.queueMu.Lock()
	if s.queued >= s.maxQueued || payloadBytes > maxQueuedPayloadBytes-s.queuedBytes {
		s.queueMu.Unlock()
		return errQueueFull
	}
	s.queued++
	s.queuedBytes += payloadBytes
	s.queueMu.Unlock()
	defer func() {
		s.queueMu.Lock()
		s.queued--
		s.queuedBytes -= payloadBytes
		s.queueMu.Unlock()
	}()
	var tick <-chan time.Time
	if heartbeat != nil {
		if err := heartbeat(); err != nil {
			return err
		}
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		tick = ticker.C
	}
	for {
		select {
		case s.slot <- struct{}{}:
			if err := ctx.Err(); err != nil {
				<-s.slot
				return err
			}
			return nil
		case <-ctx.Done():
			return ctx.Err()
		case <-tick:
			if err := heartbeat(); err != nil {
				return err
			}
		}
	}
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	kind := "invalid_request_error"
	if status >= 500 {
		kind = "server_error"
	} else if status == 429 {
		kind = "rate_limit_error"
	} else if status == 401 {
		kind = "authentication_error"
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"type": kind, "code": code, "message": message}})
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if s.managementFallback != nil && strings.HasPrefix(r.URL.Path, "/v0/management/") {
		s.managementFallback.ServeHTTP(w, r)
		return
	}
	if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+s.APIKey)) != 1 {
		writeError(w, 401, "unauthorized", "Authentication required")
		return
	}
	if r.Method == "GET" && r.URL.Path == "/v1/models" {
		w.Header().Set("Content-Type", "application/json")
		ids := []string{"deepseek-v4.1-flash"}
		for model := range s.legacyModels {
			ids = append(ids, model)
		}
		slices.Sort(ids)
		data := make([]any, 0, len(ids))
		for _, model := range ids {
			data = append(data, map[string]any{"id": model, "object": "model", "owned_by": "workbuddy"})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
		return
	}
	if r.Method != "POST" || r.URL.Path != "/v1/chat/completions" {
		writeError(w, 404, "not_found", "Route not found")
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<20))
	if err != nil {
		writeError(w, 413, "body_too_large", "Request body exceeds worker resource bound")
		return
	}
	var route struct {
		Model string `json:"model"`
	}
	if json.Unmarshal(raw, &route) == nil && route.Model == "deepseek-v4.1-flash" && s.native != nil {
		r.Body = io.NopCloser(bytes.NewReader(raw))
		r.ContentLength = int64(len(raw))
		s.native.ServeHTTP(w, r)
		return
	}
	if json.Unmarshal(raw, &route) == nil && s.legacyModels[route.Model] && s.fallback != nil {
		r.Body = io.NopCloser(bytes.NewReader(raw))
		r.ContentLength = int64(len(raw))
		s.fallback.ServeHTTP(w, r)
		return
	}
	request, parallel, err := decodeRequest(raw)
	if err != nil {
		writeError(w, 400, "unsupported_request", err.Error())
		return
	}
	if _, err = prepare(request); err != nil {
		writeError(w, 400, "unsupported_request", err.Error())
		return
	}
	var nonce [16]byte
	if _, err = rand.Read(nonce[:]); err != nil {
		writeError(w, 503, "worker_unavailable", "Cannot allocate response identity")
		return
	}
	var heartbeat func() error
	if request.Stream {
		heartbeat = func() error {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("X-Accel-Buffering", "no")
			if _, errWrite := io.WriteString(w, ": waiting for official-client worker\n\n"); errWrite != nil {
				return errWrite
			}
			return http.NewResponseController(w).Flush()
		}
	}
	if err = s.acquire(r.Context(), int64(len(raw)), heartbeat); err != nil {
		if errors.Is(err, errQueueFull) {
			w.Header().Set("Retry-After", "2")
			writeError(w, 429, "worker_queue_full", "Official-client waiting queue has reached its resource bound")
		}
		return
	}
	defer func() { <-s.slot }()
	id, created := "chatcmpl-cpa-"+hex.EncodeToString(nonce[:]), time.Now().Unix()
	chunk := func(choices any, usage json.RawMessage) map[string]any {
		v := map[string]any{"id": id, "created": created, "object": "chat.completion.chunk", "model": request.Model, "choices": choices}
		if len(usage) != 0 {
			v["usage"] = usage
		}
		return v
	}
	emit := func(value any) error {
		data, errJSON := json.Marshal(value)
		if errJSON != nil {
			return errJSON
		}
		if _, errWrite := w.Write(append(append([]byte("data: "), data...), '\n', '\n')); errWrite != nil {
			return errWrite
		}
		return http.NewResponseController(w).Flush()
	}
	var delta func(string) error
	if request.Stream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Accel-Buffering", "no")
		// This frame announces transport readiness, not a generated text token.
		if err = emit(chunk([]any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant"}, "finish_reason": nil}}, nil)); err != nil {
			return
		}
		delta = func(text string) error {
			return emit(chunk([]any{map[string]any{"index": 0, "delta": map[string]any{"content": text}, "finish_reason": nil}}, nil))
		}
	}
	result, err := s.generate(r.Context(), request, delta)
	if err == nil && !parallel && len(result.Calls) > 1 {
		err = errors.New("official client did not honor parallel_tool_calls=false")
	}
	if err != nil {
		if request.Stream {
			_ = emit(map[string]any{"error": map[string]any{"type": "server_error", "code": "official_client_failed", "message": err.Error()}})
		} else {
			writeError(w, 502, "official_client_failed", err.Error())
		}
		return
	}
	if request.Stream {
		for i, call := range result.Calls {
			if err = emit(chunk([]any{map[string]any{"index": 0, "delta": map[string]any{"tool_calls": []any{map[string]any{"index": i, "id": call.ID, "type": "function", "function": call.Function}}}, "finish_reason": nil}}, nil)); err != nil {
				return
			}
		}
		if emit(chunk([]any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": result.FinishReason}}, nil)) != nil {
			return
		}
		if emit(chunk([]any{}, result.Usage)) != nil {
			return
		}
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		_ = http.NewResponseController(w).Flush()
		return
	}
	message := map[string]any{"role": "assistant", "content": result.Text}
	if len(result.Calls) != 0 {
		message["tool_calls"] = result.Calls
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "object": "chat.completion", "created": created, "model": result.Model, "choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": result.FinishReason}}, "usage": result.Usage})
}

func decodeRequest(raw []byte) (Request, bool, error) {
	var r Request
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &r) != nil || json.Unmarshal(raw, &fields) != nil || fields == nil {
		return r, false, errors.New("invalid JSON request")
	}
	parallel := true
	outputLimit := 0
	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, key := range keys {
		value := fields[key]
		switch key {
		case "max_tokens", "max_completion_tokens":
			if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
				continue
			}
			var limit int
			if json.Unmarshal(value, &limit) != nil || limit < 1 || limit > 100000 {
				return r, false, fmt.Errorf("request parameter %q must be an integer between 1 and 100000", key)
			}
			if outputLimit != 0 && outputLimit != limit {
				return r, false, errors.New("max_tokens and max_completion_tokens must agree when both are specified")
			}
			outputLimit, r.MaxTokens = limit, limit
		case "model", "messages", "tools", "tool_choice", "stream", "reasoning_effort", "stream_options", "user", "metadata":
		case "response_format":
			// ACP already produces ordinary text. An explicit text default is
			// equivalent to omission, not a structured-output constraint.
			if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
				continue
			}
			var format map[string]json.RawMessage
			var kind string
			if json.Unmarshal(value, &format) != nil || len(format) != 1 || json.Unmarshal(format["type"], &kind) != nil || kind != "text" {
				return r, false, errors.New("response_format supports only ordinary text in the official-client adapter; structured output is not enabled")
			}
		case "parallel_tool_calls":
			if json.Unmarshal(value, &parallel) != nil {
				return r, false, errors.New("parallel_tool_calls must be a boolean")
			}
		case "store":
			if string(value) != "false" && string(value) != "null" {
				return r, false, errors.New("this worker never persists conversations")
			}
		default:
			// Unsupported generation controls are never silently discarded.
			if toolName.MatchString(key) {
				return r, false, fmt.Errorf("request parameter %q is not yet supported by the official-client adapter", key)
			}
			return r, false, errors.New("request contains an unrecognized parameter name")
		}
	}
	return r, parallel, nil
}
