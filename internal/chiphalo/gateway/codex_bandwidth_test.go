package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/traffic"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers/openai"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexec "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	"github.com/tidwall/gjson"
)

type bandwidthExecutor struct {
	*profileExecutor
	requests [][]byte
	mu       sync.Mutex
}

func (e *bandwidthExecutor) ExecuteStream(ctx context.Context, auth *coreauth.Auth, req coreexec.Request, opts coreexec.Options) (*coreexec.StreamResult, error) {
	if _, err := e.profileExecutor.Execute(ctx, auth, req, opts); err != nil {
		return nil, err
	}
	e.mu.Lock()
	e.requests = append(e.requests, bytes.Clone(req.Payload))
	n := len(e.requests)
	e.mu.Unlock()
	item := map[string]any{"type": "message", "id": "msg_bandwidth", "role": "assistant", "status": "completed", "content": []map[string]any{{"type": "output_text", "text": "OK", "annotations": []any{}}}}
	if n == 1 {
		name, namespace := "", ""
		var available []string
		var find func(gjson.Result, string)
		find = func(tools gjson.Result, ns string) {
			for _, tool := range tools.Array() {
				if tool.Get("type").String() == "namespace" {
					find(tool.Get("tools"), tool.Get("name").String())
				} else {
					available = append(available, ns+"."+tool.Get("name").String())
					if candidate := tool.Get("name").String(); candidate == "exec_command" || candidate == "shell_command" || candidate == "update_plan" {
						name, namespace = candidate, ns
					}
				}
			}
		}
		find(gjson.GetBytes(req.Payload, "tools"), "")
		if name == "" {
			return nil, fmt.Errorf("installed Codex does not declare a fixture tool: %v", available)
		}
		arguments := `{"cmd":"echo CPA-WS-OK","max_output_tokens":128}`
		if name == "shell_command" {
			arguments = `{"command":"echo CPA-WS-OK"}`
		} else if name == "update_plan" {
			arguments = `{"plan":[{"step":"Verify compressed incremental transport","status":"completed"}]}`
		}
		item = map[string]any{"type": "function_call", "id": "fc_bandwidth", "call_id": "call_bandwidth", "name": name, "arguments": arguments}
		if namespace != "" {
			item["namespace"] = namespace
		}
	}
	response := map[string]any{"id": fmt.Sprintf("resp_bandwidth_%d", n), "object": "response", "status": "completed", "model": identityModel, "output": []any{item}, "usage": map[string]int{"input_tokens": 10, "output_tokens": 1, "total_tokens": 11}}
	created := map[string]any{"id": response["id"], "model": identityModel, "status": "in_progress", "output": []any{}}
	events := []map[string]any{{"type": "response.created", "response": created}, {"type": "response.output_item.added", "output_index": 0, "item": item}, {"type": "response.output_item.done", "output_index": 0, "item": item}, {"type": "response.completed", "response": response}}
	chunks := make(chan coreexec.StreamChunk, len(events))
	for _, event := range events {
		raw, _ := json.Marshal(event)
		chunks <- coreexec.StreamChunk{Payload: append(append([]byte("data: "), raw...), '\n', '\n')}
	}
	close(chunks)
	return &coreexec.StreamResult{Chunks: chunks}, nil
}

type bandwidthListener struct {
	net.Listener
	read *atomic.Int64
}

type bandwidthConn struct {
	net.Conn
	read *atomic.Int64
}

func (l bandwidthListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return bandwidthConn{c, l.read}, nil
}

func (c bandwidthConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	c.read.Add(int64(n))
	return n, err
}

func TestCodexCLICompressedIncrementalToolRoundTrip(t *testing.T) {
	cli, err := exec.LookPath("codex")
	if err != nil {
		t.Skip("Codex CLI is not installed")
	}
	f := newProfileFixture(t, false)
	e := &bandwidthExecutor{profileExecutor: f.executor}
	f.manager.RegisterExecutor(e)
	h := openai.NewOpenAIResponsesAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, f.manager))
	r := gin.New()
	var frames [][]byte
	var framesMu sync.Mutex
	var requestsHTTP atomic.Int64
	var compressionOffered atomic.Bool
	r.Use(PublicSurface(), func(c *gin.Context) {
		if c.GetHeader("Authorization") != "Bearer admin-test-key" {
			c.AbortWithStatus(http.StatusUnauthorized)
			return
		}
		c.Set("userApiKey", "admin")
		c.Next()
	}, traffic.HTTP(), f.profile.Admission())
	r.POST("/v1/responses", func(c *gin.Context) { requestsHTTP.Add(1); h.Responses(c) })
	r.GET("/v1/responses", func(c *gin.Context) {
		compressionOffered.Store(strings.Contains(c.GetHeader("Sec-WebSocket-Extensions"), "permessage-deflate"))
		hook := handlers.WebsocketAdmissionFromContext(c.Request.Context())
		original := hook.AdmitTurn
		hook.AdmitTurn = func(ctx context.Context, body []byte) (context.Context, []byte, string, error) {
			framesMu.Lock()
			frames = append(frames, bytes.Clone(body))
			framesMu.Unlock()
			return original(ctx, body)
		}
		h.ResponsesWebsocket(c)
	})
	var wire atomic.Int64
	server := httptest.NewUnstartedServer(r)
	server.Listener = bandwidthListener{server.Listener, &wire}
	server.Start()
	defer server.Close()
	home := t.TempDir()
	cwd := t.TempDir()
	configuration := fmt.Sprintf("model=%q\nmodel_provider=\"custom\"\n[analytics]\nenabled=false\n[model_providers.custom]\nname=\"cpa\"\nbase_url=%q\nwire_api=\"responses\"\nsupports_websockets=true\nrequires_openai_auth=false\nexperimental_bearer_token=\"admin-test-key\"\n", identityModel, server.URL+"/v1")
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(configuration), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	// The loopback fixture emits only the fixed echo command above.
	command := exec.CommandContext(ctx, cli, "exec", "--json", "--ephemeral", "--skip-git-repo-check", "-s", "danger-full-access", "-C", cwd, "Echo CPA-WS-OK using the provided tool once, then reply exactly OK.")
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(strings.ToUpper(entry), "CODEX_HOME=") && !strings.HasPrefix(strings.ToUpper(entry), "NO_PROXY=") {
			command.Env = append(command.Env, entry)
		}
	}
	command.Env = append(command.Env, "CODEX_HOME="+home, "NO_PROXY=127.0.0.1,localhost")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("Codex fixture failed: %v\n%s", err, output)
	}
	finalMessage := false
	commandItem := false
	for _, line := range bytes.Split(output, []byte{'\n'}) {
		if !gjson.ValidBytes(line) || gjson.GetBytes(line, "type").String() != "item.completed" {
			continue
		}
		if gjson.GetBytes(line, "item.type").String() == "agent_message" && gjson.GetBytes(line, "item.text").String() == "OK" {
			finalMessage = true
		}
		if gjson.GetBytes(line, "item.type").String() == "command_execution" {
			commandItem = true
		}
	}
	if !finalMessage || !commandItem {
		t.Fatalf("CLI final message or command event missing: final=%t command=%t\n%s", finalMessage, commandItem, output)
	}
	if requestsHTTP.Load() != 0 || !compressionOffered.Load() {
		t.Fatalf("native compressed WebSocket was not used: HTTP=%d compression=%t", requestsHTTP.Load(), compressionOffered.Load())
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.requests) != 2 || !bytes.Contains(e.requests[1], []byte("call_bandwidth")) {
		t.Fatalf("tool continuation did not replay complete input: calls=%d", len(e.requests))
	}
	framesMu.Lock()
	defer framesMu.Unlock()
	var totalPlain, delta int
	for _, frame := range frames {
		totalPlain += len(frame)
		if gjson.GetBytes(frame, "previous_response_id").String() == "resp_bandwidth_1" {
			delta = len(frame)
			if len(gjson.GetBytes(frame, "input").Array()) != 1 || gjson.GetBytes(frame, "input.0.call_id").String() != "call_bandwidth" {
				t.Fatalf("client sent full history on the followup: %s", frame)
			}
		}
	}
	if delta == 0 || wire.Load() >= int64(totalPlain) {
		t.Fatalf("missing incremental/compression savings: delta=%d plain=%d wire=%d", delta, totalPlain, wire.Load())
	}
	t.Logf("installed Codex: HTTP=0 websocket_frames=%d plaintext_bytes=%d upload_wire_bytes=%d followup_delta_bytes=%d provider_full_followup_bytes=%d", len(frames), totalPlain, wire.Load(), delta, len(e.requests[1]))
}
