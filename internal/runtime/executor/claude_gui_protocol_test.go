package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/configbundle"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/watcher/synthesizer"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

// Exercise the GUI compiler, parsed credential, auth synthesizer and existing
// Claude translator together without using a restricted live provider.
func TestGUIClaudeNativeStreamingToolCycle(t *testing.T) {
	for _, custom := range []bool{false, true} {
		t.Run(fmt.Sprint(custom), func(t *testing.T) {
			var rounds atomic.Int32
			release := make(chan struct{})
			defer func() {
				select {
				case <-release:
				default:
					close(release)
				}
			}()
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				raw, _ := io.ReadAll(r.Body)
				if r.URL.Path != "/v1/messages" || r.Header.Get("Authorization") != "Bearer fixture-key" {
					t.Error("wrong Messages transport or credential")
				}
				if r.Header.Get("User-Agent") != "cpa-interoperability-fixture/1" || strings.Contains(string(raw), "Claude Code") || ojson.GetBytes(raw, "metadata.user_id").String() != "fixture-user" {
					t.Errorf("native API path identity: ua=%q cli_marker=%v user_id=%v", r.Header.Get("User-Agent"), strings.Contains(string(raw), "Claude Code"), ojson.GetBytes(raw, "metadata.user_id").String() != "fixture-user")
				}
				if !strings.Contains(ojson.GetBytes(raw, "system").Raw, "Preserve fixture system.") {
					t.Error("caller system instruction lost")
				}
				w.Header().Set("Content-Type", "text/event-stream")
				emit := func(value string) { fmt.Fprintf(w, "data: %s\n\n", value); w.(http.Flusher).Flush() }
				emit(`{"type":"message_start","message":{"id":"msg_fixture","type":"message","role":"assistant","model":"claude-fixture","content":[],"usage":{"input_tokens":12,"output_tokens":0}}}`)
				if rounds.Add(1) == 1 {
					<-release
					name := ojson.GetBytes(raw, "tools.0.name").String()
					if name == "" {
						t.Error("compiled tool missing")
					}
					emit(fmt.Sprintf(`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_fixture","name":%q,"input":{}}}`, name))
					args := `{"value":"hello"}`
					if custom {
						args = `{"input":"hello"}`
					}
					emit(fmt.Sprintf(`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":%q}}`, args))
					emit(`{"type":"content_block_stop","index":0}`)
					emit(`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":7}}`)
				} else {
					foundCall, foundResult := false, false
					for _, message := range ojson.GetBytes(raw, "messages").Array() {
						for _, block := range message.Get("content").Array() {
							if block.Get("type").String() == "tool_use" && block.Get("id").String() == "toolu_fixture" {
								foundCall = true
							}
							if block.Get("type").String() == "tool_result" && block.Get("tool_use_id").String() == "toolu_fixture" && strings.Contains(block.Get("content").String(), "hello") {
								foundResult = true
							}
						}
					}
					if !foundCall || !foundResult {
						t.Error("Responses tool result did not reach the matching Messages tool_use")
					}
					emit(`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)
					emit(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"VERIFIED_HELLO"}}`)
					emit(`{"type":"content_block_stop","index":0}`)
					emit(`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":4}}`)
				}
				emit(`{"type":"message_stop"}`)
			}))
			defer func() {
				select {
				case <-release:
				default:
					close(release)
				}
				upstream.Close()
			}()
			out, err := configbundle.Build(context.Background(), configbundle.Input{
				Config: []byte("api-keys: [fixture]\n"), Generated: []byte("chat_models: []\nresponses_models: []\n"), Context: []byte(`{"models":{}}`), Accounts: []byte(`{"accounts":[]}`), Vault: []byte(`{"fixture":"fixture-key"}`),
				State: []byte(`{"managedPrefixes":[],"providers":[{"id":"fixture","type":"anthropic","upstreamProtocol":"messages","prefix":"fixture","baseUrl":"https://fixture.example/v1","models":[{"clientName":"claude-fixture","upstreamName":"claude-fixture","thinking":[]}]}]}`),
				Resolve: func(context.Context, string) ([]netip.Addr, error) {
					return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			cfg, err := config.ParseConfigBytes(out.Config)
			if err != nil {
				t.Fatal(err)
			}
			auths, err := synthesizer.NewConfigSynthesizer().Synthesize(&synthesizer.SynthesisContext{Config: cfg, Now: time.Unix(1, 0), IDGenerator: synthesizer.NewStableIDGenerator()})
			if err != nil || len(auths) != 1 || auths[0].Provider != "claude" {
				t.Fatalf("native credential synthesis: %v", err)
			}
			// Origin validation above uses a public fixture; only this test's
			// credential is redirected to the local mock after compilation.
			auths[0].Attributes["base_url"] = upstream.URL
			cfg.ClaudeKey[0].BaseURL = upstream.URL
			e := NewClaudeExecutor(cfg)
			tool := json.RawMessage(nativeResponsesTool)
			if custom {
				tool = json.RawMessage(`{"type":"namespace","name":"functions","tools":[{"type":"custom","name":"ping","format":{"type":"text"}}]}`)
			}
			input := []any{map[string]any{"role": "user", "content": "Call ping with hello."}}
			body := map[string]any{"model": "claude-fixture", "user": "fixture-user", "input": input, "instructions": "Preserve fixture system.", "tools": []json.RawMessage{tool}, "stream": true}
			opts := coreexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse, Stream: true, Headers: http.Header{"User-Agent": []string{"cpa-interoperability-fixture/1"}}}
			for round := 0; round < 2; round++ {
				raw, _ := json.Marshal(body)
				v, err := e.ExecuteStream(context.Background(), auths[0], coreexecutor.Request{Model: "claude-fixture", Payload: raw}, opts)
				if err != nil {
					t.Fatal(err)
				}
				var all []byte
				if round == 0 {
					first := <-v.Chunks
					if first.Err != nil || !bytes.Contains(first.Payload, []byte("response.created")) {
						t.Fatal("no early response before tool completion")
					}
					all = append(all, first.Payload...)
					close(release)
				}
				for c := range v.Chunks {
					if c.Err != nil {
						t.Fatal(c.Err)
					}
					all = append(all, c.Payload...)
				}
				var final ojson.Result
				for _, line := range bytes.Split(all, []byte("\n")) {
					line = bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
					if ojson.GetBytes(line, "type").String() == "response.completed" {
						final = ojson.GetBytes(line, "response")
					}
				}
				if !final.Exists() || !final.Get("usage").Exists() {
					t.Fatalf("no complete Responses usage: %s", all)
				}
				if round == 0 {
					call := final.Get("output.0")
					typ := "function_call"
					if custom {
						typ = "custom_tool_call"
					}
					if call.Get("type").String() != typ || call.Get("name").String() != "ping" || custom && call.Get("namespace").String() != "functions" {
						t.Fatalf("tool identity lost: %s", call.Raw)
					}
					input = append(input, json.RawMessage(call.Raw), map[string]any{"type": typ + "_output", "call_id": call.Get("call_id").String(), "output": "hello"})
					body["input"] = input
				} else if !strings.Contains(final.Raw, "VERIFIED_HELLO") {
					t.Fatal("final tool result lost")
				}
			}
		})
	}
}
