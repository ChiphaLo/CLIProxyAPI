package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/opencode"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestOpenAICompatDSMLHonorsFinalPayloadBarrier(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, policy := range []string{"none", "filtered-tools", "overridden-schema"} {
			t.Run(fmt.Sprintf("stream=%t/%s", stream, policy), func(t *testing.T) {
				models := []config.PayloadModelRule{{Name: "wire-fixture", Protocol: "openai"}}
				cfg := &config.Config{}
				switch policy {
				case "none":
					cfg.Payload.Override = []config.PayloadRule{{Models: models, Params: map[string]any{"tool_choice": "none"}}}
				case "filtered-tools":
					cfg.Payload.Filter = []config.PayloadFilterRule{{Models: models, Params: []string{"tools"}}}
				case "overridden-schema":
					cfg.Payload.Override = []config.PayloadRule{{Models: models, Params: map[string]any{"tools.0.function.parameters.properties.input.type": "integer"}}}
				}
				var finalPayload []byte
				text := `<｜｜DSML｜｜ calls><｜｜DSML｜｜ invoke name="exec"><｜｜DSML｜｜ parameter name="input" string="true">text(1);</｜｜DSML｜｜ parameter></｜｜DSML｜｜ invoke></｜｜DSML｜｜ calls>`
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					finalPayload, _ = io.ReadAll(r.Body)
					choice := map[string]any{"index": 0, "finish_reason": "stop"}
					if stream {
						choice["delta"] = map[string]any{"content": text}
					} else {
						choice["message"] = map[string]any{"role": "assistant", "content": text}
					}
					value := map[string]any{"id": "fixture", "object": "chat.completion", "model": "wire-fixture", "choices": []any{choice}, "usage": map[string]any{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2}}
					raw, _ := json.Marshal(value)
					if stream {
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", raw)
					} else {
						w.Header().Set("Content-Type", "application/json")
						_, _ = w.Write(raw)
					}
				}))
				defer server.Close()
				executor := NewOpenAICompatExecutor("fixture", cfg)
				auth := &cliproxyauth.Auth{Provider: "fixture", Attributes: map[string]string{"api_key": "synthetic", "base_url": server.URL}}
				req := cliproxyexecutor.Request{Model: "wire-fixture", Payload: []byte(`{"model":"wire-fixture","messages":[{"role":"user","content":"fixture"}],"tool_choice":"auto","tools":[{"type":"function","function":{"name":"exec","parameters":{"type":"object","properties":{"input":{"type":"string"}},"required":["input"],"additionalProperties":false}}}]}`)}
				opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI, Stream: stream}
				ctx := opencode.WithRecovery(context.Background())
				var result strings.Builder
				if stream {
					response, err := executor.ExecuteStream(ctx, auth, req, opts)
					if err != nil {
						t.Fatal(err)
					}
					for chunk := range response.Chunks {
						if chunk.Err != nil {
							t.Fatal(chunk.Err)
						}
						_, _ = result.Write(chunk.Payload)
					}
				} else {
					response, err := executor.Execute(ctx, auth, req, opts)
					if err != nil {
						t.Fatal(err)
					}
					_, _ = result.Write(response.Payload)
				}
				if !strings.Contains(result.String(), opencode.FormatError) || strings.Contains(result.String(), `"tool_calls"`) {
					t.Fatalf("final user rules were bypassed: %s", result.String())
				}
				if policy == "none" && gjson.GetBytes(finalPayload, "tool_choice").String() != "none" {
					t.Fatal("tool_choice override lost")
				}
				if policy == "filtered-tools" && gjson.GetBytes(finalPayload, "tools").Exists() {
					t.Fatal("filtered tools restored")
				}
				if policy == "overridden-schema" && gjson.GetBytes(finalPayload, "tools.0.function.parameters.properties.input.type").String() != "integer" {
					t.Fatal("schema override lost")
				}
			})
		}
	}
}
