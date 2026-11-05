package executor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
)

// Exercise the actual production route: OpenAI-compatible native Responses ->
// Devin sidecar. Prompt steering must not invent messages or alter tool batches.
func TestSWENativeResponsesMilestonesPreserveToolBatch(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			tool1 := `{"type":"function_call","id":"fc_one","call_id":"call_one","name":"ping","arguments":"{\"value\":\"one\"}"}`
			tool2 := `{"type":"function_call","id":"fc_two","call_id":"call_two","name":"ping","arguments":"{\"value\":\"two\"}"}`
			response := `{"id":"resp_swe","object":"response","model":"swe-2","status":"completed","output":[` + tool1 + `,` + tool2 + `]}`
			body := []byte(`{"model":"swe-2","instructions":"original instructions","input":[{"role":"user","content":"Do two checks."}],"tools":[` + nativeResponsesTool + `],"reasoning":{"effort":"medium"}}`)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				raw, _ := io.ReadAll(r.Body)
				if !strings.HasPrefix(gjson.GetBytes(raw, "instructions").String(), "original instructions\n\n<cpa-progress-policy") {
					t.Error("SWE policy missing from upstream request")
				}
				for _, path := range []string{"input", "tools", "reasoning"} {
					if gjson.GetBytes(raw, path).Raw != gjson.GetBytes(body, path).Raw {
						t.Errorf("policy changed %s", path)
					}
				}
				if !stream {
					w.Header().Set("Content-Type", "application/json")
					fmt.Fprint(w, response)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_swe\",\"model\":\"swe-2\"}}\n\n")
				w.(http.Flusher).Flush()
				for index, tool := range []string{tool1, tool2} {
					fmt.Fprintf(w, "data: {\"type\":\"response.output_item.added\",\"output_index\":%d,\"item\":%s}\n\n", index, tool)
					fmt.Fprintf(w, "data: {\"type\":\"response.output_item.done\",\"output_index\":%d,\"item\":%s}\n\n", index, tool)
				}
				fmt.Fprintf(w, "data: {\"type\":\"response.completed\",\"response\":%s}\n\n", response)
			}))
			defer server.Close()
			e := NewOpenAICompatExecutor("devin1", &config.Config{})
			auth := &coreauth.Auth{ID: "swe-fixture", Attributes: map[string]string{"base_url": server.URL, "upstream_protocol": "responses"}}
			req := coreexecutor.Request{Model: "swe-2", Payload: body}
			opts := coreexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse, Stream: stream}
			if !stream {
				result, err := e.Execute(context.Background(), auth, req, opts)
				if err != nil {
					t.Fatal(err)
				}
				if gjson.GetBytes(result.Payload, "output").Raw != gjson.Get(response, "output").Raw {
					t.Fatal("tool batch modified")
				}
				return
			}
			result, err := e.ExecuteStream(context.Background(), auth, req, opts)
			if err != nil {
				t.Fatal(err)
			}
			var output strings.Builder
			for chunk := range result.Chunks {
				if chunk.Err != nil {
					t.Fatal(chunk.Err)
				}
				output.Write(chunk.Payload)
			}
			if strings.Contains(output.String(), `"type":"message"`) || !strings.Contains(output.String(), "call_one") || !strings.Contains(output.String(), "call_two") || !strings.Contains(output.String(), "response.completed") {
				t.Fatal("stream tool batch lost or spurious narration inserted")
			}
		})
	}
}
