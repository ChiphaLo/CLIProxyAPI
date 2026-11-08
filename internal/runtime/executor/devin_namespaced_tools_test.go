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

func TestDevinDiscoveredJSToolsRoundTrip(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				raw, _ := io.ReadAll(r.Body)
				if ojson.GetBytes(raw, "tools.#").Int() != 2 || ojson.GetBytes(raw, "tools.0.name").String() != "mcp__node_repl__js" || ojson.GetBytes(raw, "tools.1.name").String() != "mcp__cua_repl__js" || ojson.GetBytes(raw, "reasoning.effort").String() != "high" {
					t.Errorf("tool identities or reasoning lost: %s", raw)
				}
				result := `{"id":"resp_js","object":"response","model":"swe-2-flash-high","status":"completed","output":[{"type":"function_call","id":"fc_js","call_id":"call_js","name":"mcp__cua_repl__js","arguments":"{\"code\":\"fixture\"}"}]}`
				if stream {
					w.Header().Set("Content-Type", "text/event-stream")
					fmt.Fprintf(w, "data: {\"type\":\"response.completed\",\"response\":%s}\n\n", result)
				} else {
					w.Header().Set("Content-Type", "application/json")
					fmt.Fprint(w, result)
				}
			}))
			defer server.Close()
			e := NewOpenAICompatExecutor("devin", &config.Config{})
			auth := &coreauth.Auth{ID: "devin-fixture", Attributes: map[string]string{"base_url": server.URL, "upstream_protocol": "responses"}}
			raw := []byte(`{"model":"swe-2-flash","reasoning":{"effort":"high"},"tools":[{"type":"namespace","name":"mcp__node_repl","tools":[{"type":"function","name":"js","parameters":{"type":"object"}}]}],"input":[{"type":"additional_tools","role":"developer","tools":[{"type":"namespace","name":"mcp__cua_repl","tools":[{"type":"function","name":"js","parameters":{"type":"object"}}]}]},{"role":"user","content":"fixture"}]}`)
			req := coreexecutor.Request{Model: "swe-2-flash", Payload: raw}
			opts := coreexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse, Stream: stream}
			var output string
			if stream {
				v, err := e.ExecuteStream(context.Background(), auth, req, opts)
				if err != nil {
					t.Fatal(err)
				}
				for chunk := range v.Chunks {
					if chunk.Err != nil {
						t.Fatal(chunk.Err)
					}
					output += string(chunk.Payload)
				}
			} else {
				v, err := e.Execute(context.Background(), auth, req, opts)
				if err != nil {
					t.Fatal(err)
				}
				output = string(v.Payload)
			}
			if !strings.Contains(output, `"namespace":"mcp__cua_repl"`) || !strings.Contains(output, `"name":"js"`) || !strings.Contains(output, `"call_id":"call_js"`) {
				t.Fatalf("wrong client tool identity: %s", output)
			}
		})
	}
}
