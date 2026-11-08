package executor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

const nativeResponsesTool = `{"type":"function","name":"ping","strict":true,"parameters":{"type":"object","properties":{"value":{"type":"string"}},"required":["value"],"additionalProperties":false}}`
const nativeResponsesCall = `{"type":"function_call","id":"fc_fixture","call_id":"call_fixture","name":"ping","arguments":"{\"value\":\"hello\"}"}`
const nativeResponsesResult = `{"id":"resp_fixture","object":"response","model":"gpt-fixture","status":"completed","output":[` + nativeResponsesCall + `],"usage":{"input_tokens":20,"input_tokens_details":{"cached_tokens":10},"output_tokens":5,"output_tokens_details":{"reasoning_tokens":2},"total_tokens":25}}`

func TestNativeResponsesToolRoundTripAndEarlyFlush(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			first := make(chan struct{})
			release := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				raw, _ := io.ReadAll(r.Body)
				if r.URL.Path != "/responses" || r.Header.Get("Authorization") != "Bearer fixture-key" || r.Header.Get("x-opencode-session") != "" {
					t.Error("wrong native transport")
				}
				if ojson.GetBytes(raw, "reasoning.effort").String() != "low" || ojson.GetBytes(raw, "store").Bool() || !ojson.GetBytes(raw, "store").Exists() || ojson.GetBytes(raw, "tools.0.name").String() != "ping" || ojson.GetBytes(raw, "tool_choice").String() != "required" {
					t.Errorf("payload semantics lost: %s", raw)
				}
				if len(ojson.GetBytes(raw, "tools").Array()) != 1 || ojson.GetBytes(raw, "input.0.name").String() != "retired__exec" || ojson.GetBytes(raw, "input.0.call_id").String() != "call_prior" || ojson.GetBytes(raw, "input.1.call_id").String() != "call_prior" {
					t.Errorf("retired history was lost or reauthorized: %s", raw)
				}
				if !stream {
					w.Header().Set("Content-Type", "application/json")
					fmt.Fprint(w, nativeResponsesResult)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_fixture\",\"model\":\"gpt-fixture\"}}\n\n")
				w.(http.Flusher).Flush()
				close(first)
				<-release
				fmt.Fprintf(w, "data: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":%s}\n\n", nativeResponsesCall)
				fmt.Fprint(w, "data: {\"type\":\"response.function_call_arguments.delta\",\"item_id\":\"fc_fixture\",\"output_index\":0,\"delta\":\"{\\\"value\\\":\\\"hello\\\"}\"}\n\n")
				fmt.Fprintf(w, "data: {\"type\":\"response.completed\",\"response\":%s}\n\n", nativeResponsesResult)
			}))
			defer func() {
				select {
				case <-release:
				default:
					close(release)
				}
				server.Close()
			}()
			e := NewOpenAICompatExecutor("fixture", &config.Config{})
			auth := &coreauth.Auth{ID: "native-fixture", Prefix: "not-a-model-name", Attributes: map[string]string{"base_url": server.URL, "api_key": "fixture-key", "upstream_protocol": "responses"}}
			req := coreexecutor.Request{Model: "gpt-fixture", Payload: []byte(`{"model":"gpt-fixture","input":[{"type":"function_call","namespace":"retired","call_id":"call_prior","name":"exec","arguments":"{}"},{"type":"function_call_output","call_id":"call_prior","output":"hello"}],"reasoning":{"effort":"low"},"tool_choice":"required","tools":[` + nativeResponsesTool + `]}`)}
			opts := coreexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse, Stream: stream}
			if !stream {
				v, err := e.Execute(context.Background(), auth, req, opts)
				if err != nil || !strings.Contains(string(v.Payload), "call_fixture") {
					t.Fatalf("native result: %v", err)
				}
				return
			}
			v, err := e.ExecuteStream(context.Background(), auth, req, opts)
			if err != nil {
				t.Fatal(err)
			}
			<-first
			chunk := <-v.Chunks
			if chunk.Err != nil || !strings.Contains(string(chunk.Payload), "response.created") {
				t.Fatal("no early event before provider completion")
			}
			close(release)
			var output strings.Builder
			for c := range v.Chunks {
				if c.Err != nil {
					t.Fatal(c.Err)
				}
				output.Write(c.Payload)
			}
			if !strings.Contains(output.String(), "response.function_call_arguments.delta") || !strings.Contains(output.String(), "response.completed") {
				t.Fatal("tool stream incomplete")
			}
		})
	}
}

func TestNativeResponsesTruncationIsNotSuccess(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"response.created\"}\n\n")
	}))
	defer s.Close()
	e := NewOpenAICompatExecutor("fixture", &config.Config{})
	a := &coreauth.Auth{ID: "fixture", Attributes: map[string]string{"base_url": s.URL, "upstream_protocol": "responses"}}
	v, err := e.ExecuteStream(context.Background(), a, coreexecutor.Request{Model: "fixture", Payload: []byte(`{"input":"test"}`)}, coreexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse})
	if err != nil {
		t.Fatal(err)
	}
	failure := false
	for c := range v.Chunks {
		if c.Err != nil {
			failure = true
		}
	}
	if !failure {
		t.Fatal("truncated stream accepted")
	}
}

func TestNativeResponsesPayloadRulesAreFinal(t *testing.T) {
	cfg := &config.Config{Payload: config.PayloadConfig{Override: []config.PayloadRule{{Models: []config.PayloadModelRule{{Name: "fixture", Protocol: "openai-response"}}, Params: map[string]any{"store": true, "model": "overridden", "reasoning.effort": "high"}}}, Filter: []config.PayloadFilterRule{{Models: []config.PayloadModelRule{{Name: "fixture", Protocol: "openai-response"}}, Params: []string{"tools"}}}}}
	e := NewOpenAICompatExecutor("fixture", cfg)
	p, err := e.prepareNativeResponses(context.Background(), coreexecutor.Request{Model: "fixture", Payload: []byte(`{"input":"test","reasoning":{"effort":"low"},"tools":[` + nativeResponsesTool + `]}`)}, coreexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse}, true)
	if err != nil {
		t.Fatal(err)
	}
	if !ojson.GetBytes(p.body, "store").Bool() || ojson.GetBytes(p.body, "model").String() != "overridden" || ojson.GetBytes(p.body, "reasoning.effort").String() != "high" || ojson.GetBytes(p.body, "tools").Exists() {
		t.Fatalf("payload rules overwritten: %s", p.body)
	}
	if usesNativeResponses(&coreauth.Auth{Prefix: "steal1"}) || usesNativeResponses(nil) {
		t.Fatal("inferred protocol from caller model prefix")
	}
}

func TestNativeResponsesCodeModePayloadRulesAreFinal(t *testing.T) {
	cfg := &config.Config{Payload: config.PayloadConfig{Override: []config.PayloadRule{{Models: []config.PayloadModelRule{{Name: "fixture", Protocol: "openai-response"}}, Params: map[string]any{"tools.0.description": "Operator tool documentation.", "instructions": "Operator instructions."}}}}}
	e := NewOpenAICompatExecutor("fixture", cfg)
	request := []byte(`{"model":"fixture","input":"hello","tools":[{"type":"namespace","name":"functions","tools":[{"type":"custom","name":"exec","description":"Run tools.exec_command inside JavaScript and text(value) for output."}]}]}`)
	for _, stream := range []bool{false, true} {
		prepared, err := e.prepareNativeResponses(context.Background(), coreexecutor.Request{Model: "fixture", Payload: request}, coreexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse}, stream)
		if err != nil {
			t.Fatal(err)
		}
		if ojson.GetBytes(prepared.body, "tools.0.description").String() != "Operator tool documentation." || ojson.GetBytes(prepared.body, "instructions").String() != "Operator instructions." {
			t.Fatal("code-mode guidance overwrote final user payload rules")
		}
	}
}

func TestNativeResponsesUndeclaredToolReportsReason(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"function_call\",\"name\":\"view_image\",\"id\":\"fc_bad\",\"call_id\":\"call_bad\",\"arguments\":\"private-argument\"}}\n\n")
	}))
	defer server.Close()
	executor := NewOpenAICompatExecutor("fixture", &config.Config{})
	auth := &coreauth.Auth{ID: "fixture", Attributes: map[string]string{"base_url": server.URL, "upstream_protocol": "responses"}}
	result, err := executor.ExecuteStream(context.Background(), auth, coreexecutor.Request{Model: "fixture", Payload: []byte(`{"input":"test","tools":[` + nativeResponsesTool + `]}`)}, coreexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse})
	if err != nil {
		t.Fatal(err)
	}
	failed := false
	for chunk := range result.Chunks {
		if chunk.Err == nil {
			continue
		}
		failed = true
		message := chunk.Err.Error()
		if !strings.Contains(message, "undeclared_tool; event=response.output_item.added; tool=view_image") || strings.Contains(message, "private-argument") {
			t.Fatalf("unsafe or missing diagnostic: %s", message)
		}
	}
	if !failed {
		t.Fatal("undeclared tool was accepted")
	}
}
