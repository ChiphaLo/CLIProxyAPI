//go:build linux

package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime/metrics"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"

	"github.com/MoeForge/moego/ojson"
	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers/openai"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

type conversionBenchmarkHarness struct{ router http.Handler }

func newConversionBenchmarkHarness(b *testing.B) conversionBenchmarkHarness {
	b.Helper()
	gin.SetMode(gin.TestMode)
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		_ = r.Body.Close()
		w.Header().Set("Content-Type", "text/event-stream")
		switch r.URL.Path {
		case "/v1/messages":
			_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_fixture\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-opus-5-5\",\"content\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\nevent: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"OK\"}}\n\nevent: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
		case "/chat/completions":
			_, _ = io.WriteString(w, "data: {\"id\":\"chat_fixture\",\"object\":\"chat.completion.chunk\",\"model\":\"deepseek-v4.1-flash\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"OK\"},\"finish_reason\":null}]}\n\ndata: {\"id\":\"chat_fixture\",\"object\":\"chat.completion.chunk\",\"model\":\"deepseek-v4.1-flash\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"total_tokens\":2}}\n\ndata: [DONE]\n\n")
		default:
			response := map[string]any{"id": "resp_fixture", "object": "response", "model": "gpt-6.1-sol", "status": "completed", "output": []any{map[string]any{"id": "msg_fixture", "type": "message", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": "OK", "annotations": []any{}}}}}, "usage": map[string]any{"input_tokens": 1, "output_tokens": 1, "total_tokens": 2}}
			for _, event := range []map[string]any{{"type": "response.created", "response": map[string]any{"id": "resp_fixture", "object": "response", "status": "in_progress", "output": []any{}}}, {"type": "response.output_text.delta", "item_id": "msg_fixture", "output_index": 0, "content_index": 0, "delta": "OK"}, {"type": "response.completed", "response": response}} {
				raw, _ := json.Marshal(event)
				fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event["type"], raw)
			}
		}
	}))
	b.Cleanup(mock.Close)
	cfg, err := config.ParseConfigBytes([]byte("claude-api-key:\n- api-key: sk-ant-api03-fixture-unused\n  base-url: " + mock.URL + "\n  fingerprint-profile: claude-code-cli\n  cloak:\n    mode: always\n    strict-mode: false\n    cache-user-id: true\n"))
	if err != nil {
		b.Fatal(err)
	}
	nativeManager := auth.NewManager(nil, nil, nil)
	nativeManager.SetRetryConfig(0, 0, 0)
	nativeManager.RegisterExecutor(executor.NewClaudeExecutor(cfg))
	nativeAuth := &auth.Auth{ID: "bench-native-claude", Provider: "claude", Status: auth.StatusActive, Attributes: map[string]string{"base_url": mock.URL, "api_key": "sk-ant-api03-fixture-unused"}}
	if _, err = nativeManager.Register(context.Background(), nativeAuth); err != nil {
		b.Fatal(err)
	}
	registry.GetGlobalRegistry().RegisterClient(nativeAuth.ID, "claude", []*registry.ModelInfo{{ID: "claude-opus-5-5"}})
	b.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(nativeAuth.ID) })
	nativeRouter := gin.New()
	nativeRouter.Use(func(c *gin.Context) {
		if c.Request.Header.Get("Authorization") != "Bearer fixture" {
			c.AbortWithStatus(401)
			return
		}
		c.Set("userApiKey", "fixture")
		c.Next()
	})
	nativeHandler := openai.NewOpenAIResponsesAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, nativeManager))
	nativeRouter.POST("/v1/responses", nativeHandler.Responses)
	native := httptest.NewServer(nativeRouter)
	b.Cleanup(native.Close)
	manager := auth.NewManager(nil, nil, nil)
	manager.SetRetryConfig(0, 0, 0)
	for _, c := range []struct{ provider, model, base, key string }{{"codex", "gpt-6.1-sol", mock.URL, "fixture-unused"}, {"bench-claude", "claude-opus-5-5", native.URL + "/v1", "fixture"}, {"opencode2", "deepseek-v4.1-flash", mock.URL, "fixture-unused"}} {
		attrs := map[string]string{"base_url": c.base, "api_key": c.key}
		if c.provider == "bench-claude" {
			attrs["upstream_protocol"] = "responses"
		}
		if c.provider == "codex" {
			manager.RegisterExecutor(executor.NewCodexExecutor(&config.Config{}))
		} else {
			manager.RegisterExecutor(executor.NewOpenAICompatExecutor(c.provider, &config.Config{}))
		}
		a := &auth.Auth{ID: "bench-parent-" + c.provider, Provider: c.provider, Status: auth.StatusActive, Attributes: attrs}
		if _, err = manager.Register(context.Background(), a); err != nil {
			b.Fatal(err)
		}
		registry.GetGlobalRegistry().RegisterClient(a.ID, c.provider, []*registry.ModelInfo{{ID: c.model}})
		id := a.ID
		b.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
	}
	router := gin.New()
	router.Use(Admission(func(string) string { return "benchmark" }))
	h := openai.NewOpenAIResponsesAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager))
	router.POST("/v1/responses", h.Responses)
	return conversionBenchmarkHarness{router: router}
}

func fullPipelineFixture(model string, mib int) []byte {
	d := ojson.NewDoc([]byte(`{"model":"","stream":true,"instructions":"Local protocol conversion benchmark. Files are inert.","input":[{"role":"user","content":[{"type":"input_image","image_url":""}]}],"tools":[{"type":"function","name":"read","parameters":{"type":"object"}}],"reasoning":{"effort":"medium","summary":"auto"}}`))
	_ = d.Set("model", model)
	_ = d.Set("input.0.content.0.image_url", "data:image/png;base64,"+strings.Repeat("ABCD", (mib<<20)/4))
	return d.Bytes()
}
func (h conversionBenchmarkHarness) request(payload []byte) (int, bool) {
	r := httptest.NewRequest("POST", "/v1/responses", bytes.NewReader(payload))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.router.ServeHTTP(w, r)
	return w.Body.Len(), w.Code == 200 && bytes.Contains(w.Body.Bytes(), []byte("response.completed"))
}
func pipelineCPUSeconds() float64 {
	var r syscall.Rusage
	_ = syscall.Getrusage(syscall.RUSAGE_SELF, &r)
	return float64(r.Utime.Sec+r.Stime.Sec) + float64(r.Utime.Usec+r.Stime.Usec)/1e6
}
func pipelineGCSeconds() float64 {
	s := []metrics.Sample{{Name: "/cpu/classes/gc/total:cpu-seconds"}}
	metrics.Read(s)
	return s[0].Value.Float64()
}

func BenchmarkFullPipeline(b *testing.B) {
	for _, model := range []string{"gpt-6.1-sol", "claude-opus-5-5", "deepseek-v4.1-flash"} {
		for _, mib := range []int{1, 8, 32} {
			b.Run(fmt.Sprintf("%s/%dMiB", model, mib), func(b *testing.B) {
				h := newConversionBenchmarkHarness(b)
				payload := fullPipelineFixture(model, mib)
				if _, ok := h.request(payload); !ok {
					b.Fatal("mock protocol did not complete")
				}
				b.ReportAllocs()
				b.SetBytes(int64(len(payload)))
				b.ResetTimer()
				cpu, gc := pipelineCPUSeconds(), pipelineGCSeconds()
				output := 0
				for i := 0; i < b.N; i++ {
					var ok bool
					output, ok = h.request(payload)
					if !ok {
						b.Fatal("mock protocol did not complete")
					}
				}
				b.StopTimer()
				b.ReportMetric((pipelineCPUSeconds()-cpu)*1e9/float64(b.N), "cpu-ns/op")
				b.ReportMetric((pipelineGCSeconds()-gc)*1e9/float64(b.N), "gc-cpu-ns/op")
				b.ReportMetric(float64(len(payload)), "input-B/op")
				b.ReportMetric(float64(output), "output-B/op")
			})
		}
	}
}

func BenchmarkFullPipelineConcurrency(b *testing.B) {
	for _, workers := range []int{1, 4, 16} {
		b.Run(fmt.Sprintf("Claude/1MiB/%dWorkers", workers), func(b *testing.B) {
			h := newConversionBenchmarkHarness(b)
			payload := fullPipelineFixture("claude-opus-5-5", 1)
			if _, ok := h.request(payload); !ok {
				b.Fatal("mock protocol did not complete")
			}
			var next atomic.Int64
			var failed atomic.Bool
			var wg sync.WaitGroup
			b.ReportAllocs()
			b.SetBytes(int64(len(payload)))
			b.ResetTimer()
			for i := 0; i < workers; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for int(next.Add(1)) <= b.N {
						if _, ok := h.request(payload); !ok {
							failed.Store(true)
						}
					}
				}()
			}
			wg.Wait()
			b.StopTimer()
			if failed.Load() {
				b.Fatal("concurrent mock protocol did not complete")
			}
		})
	}
}
