package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/traffic"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers/openai"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	exec "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	"github.com/tidwall/gjson"
)

type continuationExecutor struct {
	*profileExecutor
	mu       sync.Mutex
	requests [][]byte
}

func (e *continuationExecutor) Execute(ctx context.Context, a *coreauth.Auth, r exec.Request, o exec.Options) (exec.Response, error) {
	if _, err := e.profileExecutor.Execute(ctx, a, r, o); err != nil {
		return exec.Response{}, err
	}
	e.mu.Lock()
	e.requests = append(e.requests, bytes.Clone(r.Payload))
	n := len(e.requests)
	e.mu.Unlock()
	output := `[{"type":"function_call","id":"fc_tool","call_id":"call_tool","name":"lookup","arguments":"{}"}]`
	if n > 1 {
		output = `[{"type":"message","id":"msg_final","role":"assistant","content":[{"type":"output_text","text":"tool completed"}]}]`
	}
	return exec.Response{Payload: []byte(fmt.Sprintf(`{"id":"resp_traffic_%d","object":"response","status":"completed","model":%q,"output":%s}`, n, identityModel, output))}, nil
}
func (e *continuationExecutor) ExecuteStream(ctx context.Context, a *coreauth.Auth, r exec.Request, o exec.Options) (*exec.StreamResult, error) {
	resp, err := e.Execute(ctx, a, r, o)
	if err != nil {
		return nil, err
	}
	chunks := make(chan exec.StreamChunk, 3)
	chunks <- exec.StreamChunk{Payload: []byte(`data: {"type":"response.output_text.delta","delta":"` + strings.Repeat("streamed-text-", 1000) + `"}` + "\n\n")}
	frame := []byte(`data: {"type":"response.completed","response":` + string(resp.Payload) + "}\n\n")
	// Exercise event parsing across arbitrary transport chunk boundaries.
	chunks <- exec.StreamChunk{Payload: frame[:23]}
	chunks <- exec.StreamChunk{Payload: frame[23:]}
	close(chunks)
	return &exec.StreamResult{Chunks: chunks}, nil
}

func trafficFixture(t *testing.T) (*profileFixture, *continuationExecutor, *httptest.Server) {
	t.Helper()
	f := newProfileFixture(t, false)
	e := &continuationExecutor{profileExecutor: f.executor}
	f.manager.RegisterExecutor(e)
	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, f.manager)
	h := openai.NewOpenAIResponsesAPIHandler(base)
	r := gin.New()
	_ = r.SetTrustedProxies(nil)
	r.Use(PublicSurface(), func(c *gin.Context) {
		key := strings.TrimPrefix(c.Request.Header.Get("Authorization"), "Bearer ")
		id := ""
		switch key {
		case "admin-test-key", "admin-test-key-rotated":
			id = "admin"
		case "issued-test-key":
			id = "issued-fixture"
		}
		if id == "" {
			abort(c, 401, "invalid key")
			return
		}
		c.Set("userApiKey", id)
		c.Next()
	}, traffic.HTTP(), f.profile.Admission())
	r.POST("/v1/responses", h.Responses)
	r.GET("/v1/responses", h.ResponsesWebsocket)
	server := httptest.NewServer(r)
	t.Cleanup(server.Close)
	return f, e, server
}

func TestHTTPIncrementalToolContinuationAndCompression(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint("stream=", stream), func(t *testing.T) {
			_, e, server := trafficFixture(t)
			first, _ := json.Marshal(map[string]any{"model": identityModel, "stream": stream, "input": []map[string]any{{"role": "user", "content": strings.Repeat("context-data-", 10000)}}, "tools": []map[string]any{{"type": "function", "name": "lookup", "parameters": map[string]any{"type": "object"}}}})
			post := func(body []byte, key string) (int, []byte, int) {
				t.Helper()
				var compressed bytes.Buffer
				z, _ := traffic.Encoder(&compressed, "gzip")
				_, _ = z.Write(body)
				_ = z.Close()
				r, _ := http.NewRequest("POST", server.URL+"/v1/responses", bytes.NewReader(compressed.Bytes()))
				r.Header.Set("Authorization", "Bearer "+key)
				r.Header.Set("Content-Encoding", "gzip")
				r.Header.Set("Accept-Encoding", "gzip")
				client := &http.Client{Transport: &http.Transport{DisableCompression: true}}
				defer client.CloseIdleConnections()
				resp, err := client.Do(r)
				if err != nil {
					t.Fatal(err)
				}
				wire, err := io.ReadAll(resp.Body)
				resp.Body.Close()
				if err != nil {
					t.Fatal(err)
				}
				b := wire
				if resp.Header.Get("Content-Encoding") == "gzip" {
					d, err := traffic.Decoder(bytes.NewReader(wire), "gzip")
					if err != nil {
						t.Fatal(err)
					}
					b, err = io.ReadAll(d)
					_ = d.Close()
					if err != nil {
						t.Fatal(err)
					}
				}
				return resp.StatusCode, b, len(wire)
			}
			status, response, wire := post(first, "admin-test-key")
			if status != 200 || !bytes.Contains(response, []byte("resp_traffic_1")) {
				t.Fatalf("first %d %s", status, response)
			}
			delta := []byte(fmt.Sprintf(`{"model":%q,"stream":%t,"previous_response_id":"resp_traffic_1","input":[{"type":"function_call_output","call_id":"call_tool","output":"tool executed"}]}`, identityModel, stream))
			status, response, wire2 := post(delta, "admin-test-key")
			if status != 200 || !bytes.Contains(response, []byte("tool completed")) {
				t.Fatalf("continuation %d %s", status, response)
			}
			e.mu.Lock()
			upstream := bytes.Clone(e.requests[1])
			e.mu.Unlock()
			items := gjson.GetBytes(upstream, "input").Array()
			if len(items) != 3 || items[1].Get("type").String() != "function_call" || items[2].Get("call_id").String() != "call_tool" || gjson.GetBytes(upstream, "previous_response_id").Exists() {
				t.Fatalf("tool conversation corrupted: %s", upstream)
			}
			if status, _, _ := post(delta, "issued-test-key"); status != 409 {
				t.Fatalf("cross-key continuation admitted: %d", status)
			}
			if status, _, _ := post(delta, "admin-test-key-rotated"); status != 409 {
				t.Fatalf("rotated key inherited old continuation: %d", status)
			}
			wrong := bytes.Replace(delta, []byte(identityModel), []byte("codex2/"+identityModel), 1)
			if status, _, _ := post(wrong, "admin-test-key"); status != 409 {
				t.Fatalf("cross-account continuation admitted: %d", status)
			}
			t.Logf("HTTP stream=%t full_upload=%d incremental_upload=%d provider_replay=%d first_response_wire=%d second_response_wire=%d", stream, len(first), len(delta), len(upstream), wire, wire2)
		})
	}
}

func TestContinuationTTLBoundsAndIncomplete(t *testing.T) {
	s := NewConversationStore()
	now := time.Unix(1000, 0)
	s.now = func() time.Time { return now }
	s.maxEntries = 2
	s.maxBytes = 100
	key := conversationKey{Key: "k", Model: "m", Path: "/v1/responses", ID: "one"}
	s.put(key, []byte("[]"), "a")
	now = now.Add(s.ttl)
	if _, ok := s.get(key); ok {
		t.Fatal("expired conversation retained")
	}
	for _, id := range []string{"one", "two", "three"} {
		key.ID = id
		s.put(key, []byte("[]"), "a")
	}
	if len(s.items) != 2 || s.bytes != 4 {
		t.Fatal("cache bounds exceeded")
	}
	key.ID = "big"
	s.put(key, make([]byte, continuationEntryLimit+1), "a")
	if len(s.items) != 2 {
		t.Fatal("oversized entry retained")
	}
	for _, event := range []string{`data: {"type":"response.incomplete","response":{"id":"partial","status":"incomplete","output":[]}}` + "\n\n", `data: {"type":"response.completed","response":{"id":"partial","status":"completed","output":[]}}` + "\n\n" + `data: {"type":"error"}` + "\n\n", `data: {"type":"response.completed"}`} {
		w := &continuationWriter{ResponseWriter: newGinWriter(t)}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(event))
		if w.completed() != nil {
			t.Fatal("partial or failed stream retained")
		}
	}
	for _, bad := range []string{`{"input":[],"\u0069nput":[],"previous_response_id":"one"}`, `{"previous_response_id":123}`, `{"previous_response_id":"missing"}`} {
		if _, _, err := s.prepare([]byte(bad), key, ""); err == nil {
			t.Fatal("ambiguous or missing continuation admitted")
		}
	}
}

func newGinWriter(t *testing.T) gin.ResponseWriter {
	t.Helper()
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	return c.Writer
}

func TestCPAWebsocketCompressedToolContinuationAndAdmission(t *testing.T) {
	f, e, server := trafficFixture(t)
	dialer := websocket.Dialer{EnableCompression: true}
	headers := http.Header{"Authorization": []string{"Bearer admin-test-key"}}
	conn, resp, err := dialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", headers)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if !strings.Contains(resp.Header.Get("Sec-WebSocket-Extensions"), "permessage-deflate") {
		t.Fatal("WS compression was not negotiated")
	}
	write := func(raw string) {
		t.Helper()
		if err := conn.WriteMessage(websocket.TextMessage, []byte(raw)); err != nil {
			t.Fatal(err)
		}
	}
	read := func() []byte {
		t.Helper()
		for {
			_, b, err := conn.ReadMessage()
			if err != nil {
				t.Fatal(err)
			}
			kind := gjson.GetBytes(b, "type").String()
			if kind == "response.completed" || kind == "error" {
				return b
			}
		}
	}
	write(fmt.Sprintf(`{"type":"response.create","model":%q,"input":[{"role":"user","content":"run lookup"}],"tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}]}`, identityModel))
	if b := read(); gjson.GetBytes(b, "type").String() != "response.completed" {
		t.Fatalf("first WS turn: %s", b)
	}
	write(`{"type":"response.create","previous_response_id":"resp_traffic_1","input":[{"type":"function_call_output","call_id":"call_tool","output":"executed"}]}`)
	if b := read(); gjson.GetBytes(b, "type").String() != "response.completed" {
		t.Fatalf("WS continuation: %s", b)
	}
	e.mu.Lock()
	second := bytes.Clone(e.requests[1])
	count := len(e.requests)
	e.mu.Unlock()
	if len(gjson.GetBytes(second, "input").Array()) != 3 || gjson.GetBytes(second, "previous_response_id").Exists() {
		t.Fatalf("HTTP-only provider did not receive full replay: %s", second)
	}
	write(fmt.Sprintf(`{"type":"response.create","model":%q,"previous_response_id":"resp_traffic_2","input":[]}`, "codex2/"+identityModel))
	if b := read(); gjson.GetBytes(b, "type").String() != "error" {
		t.Fatalf("account switch admitted: %s", b)
	}
	write(`{"type":"response.create","model":"x","\u006dodel":"y","input":[]}`)
	if b := read(); gjson.GetBytes(b, "type").String() != "error" {
		t.Fatalf("duplicate model admitted: %s", b)
	}
	write(`{"type":"response.steer","input":"must not bypass admission"}`)
	if b := read(); gjson.GetBytes(b, "type").String() != "error" {
		t.Fatalf("unadmitted steer executed: %s", b)
	}
	write(`{"type":"response.create","stream_id":"other-lane","input":[]}`)
	if b := read(); gjson.GetBytes(b, "type").String() != "error" {
		t.Fatalf("unsupported multiplexed lane admitted: %s", b)
	}
	e.mu.Lock()
	if len(e.requests) != count {
		t.Error("denied frame reached executor")
	}
	e.mu.Unlock()
	// Existing member identity guards remain active on every admitted execution.
	changed := f.auths["codex"].Clone()
	changed.Metadata["account_id"] = "different-team"
	_, _ = f.manager.Update(context.Background(), changed)
	write(fmt.Sprintf(`{"type":"response.create","model":%q,"input":[]}`, identityModel))
	if _, b, err := conn.ReadMessage(); err == nil && gjson.GetBytes(b, "type").String() != "error" {
		t.Fatalf("changed identity admitted: %s", b)
	}
	e.mu.Lock()
	if len(e.requests) != count {
		t.Error("changed member reached executor")
	}
	e.mu.Unlock()
	for _, header := range []http.Header{{}, {"Authorization": []string{"Bearer admin-test-key"}, "Origin": []string{"https://attacker.invalid"}}} {
		c, resp, err := dialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", header)
		if c != nil {
			c.Close()
		}
		if err == nil || resp == nil || (resp.StatusCode != 401 && resp.StatusCode != 403) {
			t.Fatal("unauthorized/origin WS handshake admitted")
		}
	}
}

type wireCounterConn struct {
	net.Conn
	written *atomic.Int64
}

func (c wireCounterConn) Write(b []byte) (int, error) {
	n, err := c.Conn.Write(b)
	c.written.Add(int64(n))
	return n, err
}

func TestCPANativeWebsocketSendsOnlyDeltaToUpstream(t *testing.T) {
	f, _, server := trafficFixture(t)
	upgrader := websocket.Upgrader{EnableCompression: true}
	requests := make(chan []byte, 3)
	done := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		defer close(done)
		for n := 1; n <= 2; n++ {
			_, b, err := conn.ReadMessage()
			if err != nil {
				t.Error(err)
				return
			}
			requests <- bytes.Clone(b)
			output := `[{"type":"function_call","id":"fc_native","call_id":"call_native","name":"lookup","arguments":"{}"}]`
			if n == 2 {
				output = `[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"verified"}]}]`
			}
			created := fmt.Sprintf(`{"type":"response.created","response":{"id":"resp_native_%d","model":%q,"status":"in_progress","output":[]}}`, n, identityModel)
			completed := fmt.Sprintf(`{"type":"response.completed","response":{"id":"resp_native_%d","model":%q,"status":"completed","output":%s,"usage":{"input_tokens":100,"output_tokens":50,"output_tokens_details":{"reasoning_tokens":40},"total_tokens":150}}}`, n, identityModel, output)
			if err := conn.WriteMessage(websocket.TextMessage, []byte(created)); err != nil {
				t.Error(err)
				return
			}
			if err := conn.WriteMessage(websocket.TextMessage, []byte(completed)); err != nil {
				t.Error(err)
				return
			}
		}
		_, _, _ = conn.ReadMessage()
	}))
	defer upstream.Close()
	a := f.auths["codex"].Clone()
	a.Metadata["websockets"] = true
	a.Attributes = map[string]string{"base_url": upstream.URL, "api_key": "native-test-only", "websockets": "true"}
	if _, err := f.manager.Update(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	f.manager.RegisterExecutor(executor.NewCodexAutoExecutor(&config.Config{SDKConfig: sdkconfig.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll}}))
	var written atomic.Int64
	dialer := websocket.Dialer{EnableCompression: true, NetDialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		c, err := (&net.Dialer{}).DialContext(ctx, network, address)
		if err != nil {
			return nil, err
		}
		return wireCounterConn{c, &written}, nil
	}}
	conn, _, err := dialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", http.Header{"Authorization": []string{"Bearer admin-test-key"}})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	read := func() {
		t.Helper()
		for {
			_, b, err := conn.ReadMessage()
			if err != nil {
				t.Fatal(err)
			}
			switch gjson.GetBytes(b, "type").String() {
			case "error":
				t.Fatalf("WS error %s", b)
			case "response.completed":
				return
			}
		}
	}
	first := []byte(fmt.Sprintf(`{"type":"response.create","model":%q,"input":[{"role":"user","content":%q}],"tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}]}`, identityModel, strings.Repeat("native-long-context-", 6000)))
	before := written.Load()
	if err := conn.WriteMessage(websocket.TextMessage, first); err != nil {
		t.Fatal(err)
	}
	read()
	firstWire := written.Load() - before
	second := []byte(`{"type":"response.create","previous_response_id":"resp_native_1","input":[{"type":"function_call_output","call_id":"call_native","output":"executed by client"}]}`)
	before = written.Load()
	if err := conn.WriteMessage(websocket.TextMessage, second); err != nil {
		t.Fatal(err)
	}
	read()
	secondWire := written.Load() - before
	upFirst, upSecond := <-requests, <-requests
	if gjson.GetBytes(upSecond, "previous_response_id").String() != "resp_native_1" || len(gjson.GetBytes(upSecond, "input").Array()) != 1 || gjson.GetBytes(upSecond, "input.0.call_id").String() != "call_native" {
		t.Fatalf("native WS uploaded history instead of delta: %s", upSecond)
	}
	if len(upSecond) >= len(upFirst) || firstWire >= int64(len(first)) {
		t.Fatal("no native incremental/compression savings")
	}
	t.Logf("native WS full_input=%d delta_input=%d full_upstream=%d delta_upstream=%d full_downstream_wire=%d delta_downstream_wire=%d", len(first), len(second), len(upFirst), len(upSecond), firstWire, secondWire)
	_ = conn.Close()
	<-done
}
