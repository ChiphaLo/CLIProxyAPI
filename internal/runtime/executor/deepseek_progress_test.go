package executor

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestDeepSeekMilestoneOutgoingRequest(t *testing.T) {
	for _, model := range []string{"deepseek-v4.1-flash", "gpt-6.1-sol"} {
		for _, stream := range []bool{false, true} {
			for _, format := range []sdktranslator.Format{sdktranslator.FormatOpenAI, sdktranslator.FormatOpenAIResponse} {
				t.Run(model+"/"+format.String()+map[bool]string{true: "/stream", false: "/execute"}[stream], func(t *testing.T) {
					captured := make(chan []byte, 1)
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						body, errRead := io.ReadAll(r.Body)
						if errRead != nil {
							t.Error(errRead)
						}
						captured <- body
						if stream {
							w.Header().Set("Content-Type", "text/event-stream")
							_, _ = io.WriteString(w, "data: {\"id\":\"fixture\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"ok\"},\"finish_reason\":null}]}\n\ndata: {\"id\":\"fixture\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":1,\"total_tokens\":11}}\n\ndata: [DONE]\n\n")
						} else {
							w.Header().Set("Content-Type", "application/json")
							_, _ = io.WriteString(w, `{"id":"fixture","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":1,"total_tokens":11}}`)
						}
					}))
					defer server.Close()
					payload := []byte(`{"messages":[{"role":"system","content":"original instructions"},{"role":"user","content":"hello"}],"tools":[{"type":"function","function":{"name":"inspect_project","parameters":{"type":"object","properties":{}}}}]}`)
					if format == sdktranslator.FormatOpenAIResponse {
						payload = []byte(`{"instructions":"original instructions","input":[{"role":"user","content":"hello"}],"tools":[{"type":"function","name":"inspect_project","parameters":{"type":"object","properties":{}}}]}`)
					}
					executor := NewOpenAICompatExecutor("fixture", &config.Config{})
					auth := &cliproxyauth.Auth{ID: "fixture", Provider: "fixture", Attributes: map[string]string{"base_url": server.URL, "api_key": "test"}}
					req := cliproxyexecutor.Request{Model: model, Payload: payload}
					opts := cliproxyexecutor.Options{SourceFormat: format}
					if stream {
						result, errStream := executor.ExecuteStream(context.Background(), auth, req, opts)
						if errStream != nil {
							t.Fatal(errStream)
						}
						for chunk := range result.Chunks {
							if chunk.Err != nil {
								t.Fatal(chunk.Err)
							}
						}
					} else if _, errExecute := executor.Execute(context.Background(), auth, req, opts); errExecute != nil {
						t.Fatal(errExecute)
					}
					body := <-captured
					content := gjson.GetBytes(body, "messages.0.content").String()
					if !strings.Contains(content, "original instructions") || bytes.Contains(body, []byte("cpa-progress-policy")) != strings.HasPrefix(model, "deepseek-") {
						t.Fatal("outgoing guidance missing or non-DeepSeek request changed")
					}
					if gjson.GetBytes(body, "tools.0.function.name").String() != "inspect_project" {
						t.Fatal("tool declaration changed")
					}
				})
			}
		}
	}
}
