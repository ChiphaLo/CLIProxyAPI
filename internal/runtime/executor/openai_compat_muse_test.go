package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
)

const museNativeFixture = `{"model":"opencode/muse-spark-1.3-contributor","tool_choice":"auto","input":[{"type":"additional_tools","role":"developer","tools":[{"type":"namespace","name":"functions","tools":[{"type":"custom","name":"lookup","format":{"type":"text"}}]}]},{"type":"reasoning","id":"rs_old_other_provider","summary":[{"type":"summary_text","text":"prior portable summary"}]},{"type":"message","role":"user","content":[{"type":"input_text","text":"fixture"}]}],"max_output_tokens":128}`
const museNativeItem = `{"type":"function_call","name":"functions__lookup","id":"fc_fixture","call_id":"call_fixture","arguments":"{\"input\":\"7\"}"}`

func TestOpenCodeMuseNativeHTTPAndSSE(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			var final []byte
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				final, _ = io.ReadAll(r.Body)
				if r.URL.Path != "/responses" || !strings.HasPrefix(r.Header.Get("x-opencode-session"), "cpa-go-") {
					t.Error("native endpoint/session not used")
				}
				if gjson.GetBytes(final, "input.0.type").String() != "reasoning" || gjson.GetBytes(final, "input.0.id").Exists() || gjson.GetBytes(final, "tools.0.name").String() != "functions__lookup" || gjson.GetBytes(final, "max_output_tokens").Int() != 128 {
					t.Errorf("invalid native payload: %s", final)
				}
				response := `{"id":"resp_fixture","object":"response","status":"completed","model":"muse-spark-1.3-contributor","output":[` + museNativeItem + `],"usage":{"input_tokens":10,"output_tokens":9,"total_tokens":19,"output_tokens_details":{"reasoning_tokens":8}}}`
				if stream {
					w.Header().Set("Content-Type", "text/event-stream")
					added := strings.Replace(museNativeItem, `{\"input\":\"7\"}`, "", 1)
					fmt.Fprintf(w, "data: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":%s}\n\n", added)
					fmt.Fprint(w, `data: {"type":"response.function_call_arguments.delta","item_id":"fc_fixture","delta":"{\"input\":\"7\"}"}`+"\n\n")
					fmt.Fprint(w, `data: {"type":"response.function_call_arguments.done","item_id":"fc_fixture","arguments":"{\"input\":\"7\"}"}`+"\n\n")
					fmt.Fprintf(w, "data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":%s}\n\n", museNativeItem)
					fmt.Fprintf(w, "data: {\"type\":\"response.completed\",\"response\":%s}\n\n", response)
				} else {
					w.Header().Set("Content-Type", "application/json")
					fmt.Fprint(w, response)
				}
			}))
			defer server.Close()
			e := NewOpenAICompatExecutor("fixture", &config.Config{})
			auth := &coreauth.Auth{ID: "native-fixture", Prefix: "opencode", Provider: "fixture", Attributes: map[string]string{"api_key": "synthetic", "base_url": server.URL}}
			req := coreexecutor.Request{Model: "muse-spark-1.3-contributor", Payload: []byte(museNativeFixture)}
			opts := coreexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse, Stream: stream}
			var result strings.Builder
			if stream {
				response, err := e.ExecuteStream(context.Background(), auth, req, opts)
				if err != nil {
					t.Fatal(err)
				}
				for chunk := range response.Chunks {
					if chunk.Err != nil {
						t.Fatal(chunk.Err)
					}
					result.Write(chunk.Payload)
				}
			} else {
				response, err := e.Execute(context.Background(), auth, req, opts)
				if err != nil {
					t.Fatal(err)
				}
				result.Write(response.Payload)
			}
			if !strings.Contains(result.String(), `"custom_tool_call"`) || !strings.Contains(result.String(), `"namespace":"functions"`) || !strings.Contains(result.String(), `"input":"7"`) || strings.Contains(result.String(), `"name":"functions__lookup"`) {
				t.Fatalf("bad native result: %s", result.String())
			}
		})
	}
}

func TestOpenCodeMusePayloadRulesRemainFinal(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, rule := range []string{"tools-filtered", "choice-required", "schema-overridden", "model-overridden"} {
			t.Run(fmt.Sprintf("%t/%s", stream, rule), func(t *testing.T) {
				cfg := &config.Config{}
				models := []config.PayloadModelRule{{Name: "muse-spark-1.3-contributor", Protocol: "openai-response"}}
				switch rule {
				case "tools-filtered":
					cfg.Payload.Filter = []config.PayloadFilterRule{{Models: models, Params: []string{"tools"}}}
				case "choice-required":
					cfg.Payload.Override = []config.PayloadRule{{Models: models, Params: map[string]any{"tool_choice": "required"}}}
				case "schema-overridden":
					cfg.Payload.Override = []config.PayloadRule{{Models: models, Params: map[string]any{"tools.0.parameters.properties.input.type": "integer"}}}
				case "model-overridden":
					cfg.Payload.Override = []config.PayloadRule{{Models: models, Params: map[string]any{"model": "muse-spark-fixture-override"}}}
				}
				var seen atomic.Int64
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					seen.Add(1)
					raw, _ := io.ReadAll(r.Body)
					if rule == "model-overridden" && gjson.GetBytes(raw, "model").String() != "muse-spark-fixture-override" {
						t.Error("configured model overwritten")
					}
					if rule == "tools-filtered" && gjson.GetBytes(raw, "tools").Exists() {
						t.Error("filtered tools restored")
					}
					value := map[string]any{"object": "response", "status": "completed", "output": []any{map[string]any{"type": "function_call", "name": "functions__lookup", "id": "fc_fixture", "call_id": "call_fixture", "arguments": `{"input":"7"}`}}}
					if stream {
						w.Header().Set("Content-Type", "text/event-stream")
						data, _ := json.Marshal(map[string]any{"type": "response.completed", "response": value})
						fmt.Fprintf(w, "data: %s\n\n", data)
					} else {
						w.Header().Set("Content-Type", "application/json")
						json.NewEncoder(w).Encode(value)
					}
				}))
				defer server.Close()
				e := NewOpenAICompatExecutor("fixture", cfg)
				a := &coreauth.Auth{ID: "rules", Prefix: "opencode", Attributes: map[string]string{"base_url": server.URL}}
				req := coreexecutor.Request{Model: "muse-spark-1.3-contributor", Payload: []byte(museNativeFixture)}
				opts := coreexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse, Stream: stream}
				if stream {
					r, err := e.ExecuteStream(context.Background(), a, req, opts)
					failed := err != nil
					if err == nil && r != nil {
						for chunk := range r.Chunks {
							if chunk.Err != nil {
								failed = true
							}
						}
					}
					if rule != "model-overridden" && !failed {
						t.Fatal("final rule did not prevent invalid streamed tool call")
					}
					if rule == "choice-required" && err == nil {
						t.Fatal("invalid final choice accepted")
					}
				} else {
					_, err := e.Execute(context.Background(), a, req, opts)
					if rule != "model-overridden" && err == nil {
						t.Fatal("prohibited tool call accepted")
					}
				}
				if rule == "choice-required" && seen.Load() != 0 {
					t.Fatal("invalid final rule reached provider")
				}
			})
		}
	}
}

func TestOpenCodeMuseStreamingTruncationAndEarlyFlush(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"early\"}\n\n")
		w.(http.Flusher).Flush()
		close(started)
		<-release
	}))
	defer server.Close()
	e := NewOpenAICompatExecutor("fixture", &config.Config{})
	a := &coreauth.Auth{ID: "flush", Prefix: "opencode", Attributes: map[string]string{"base_url": server.URL}}
	req := coreexecutor.Request{Model: "muse-spark-1.3-contributor", Payload: []byte(`{"model":"muse-spark-1.3-contributor","input":"fixture"}`)}
	r, err := e.ExecuteStream(context.Background(), a, req, coreexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse, Stream: true})
	if err != nil {
		close(release)
		t.Fatal(err)
	}
	<-started
	first := <-r.Chunks
	close(release)
	if !strings.Contains(string(first.Payload), "early") {
		t.Fatal("SSE buffered until completion")
	}
	failure := false
	for chunk := range r.Chunks {
		if chunk.Err != nil {
			failure = true
		}
	}
	if !failure {
		t.Fatal("truncated stream reported success")
	}
}

func TestOpenCodeMuseSelectionUsesCredentialNotPublicPrefix(t *testing.T) {
	r := coreexecutor.Request{Model: "muse-spark-1.3-contributor", Payload: []byte(museNativeFixture)}
	if isOpenCodeMuse(&coreauth.Auth{Prefix: "other"}, r) || isOpenCodeMuse(nil, r) {
		t.Fatal("untrusted model activated native route")
	}
	if !isOpenCodeMuse(&coreauth.Auth{Prefix: "opencode"}, r) {
		t.Fatal("configured Muse route not selected")
	}
	r.Model = "deepseek-v4.1-flash"
	if isOpenCodeMuse(&coreauth.Auth{Prefix: "opencode"}, r) {
		t.Fatal("other model route changed")
	}
}
