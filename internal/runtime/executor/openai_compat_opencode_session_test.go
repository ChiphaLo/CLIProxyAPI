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
	coresession "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/session"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func TestOpenCodeGoDynamicSessionHTTPAndSSE(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			sessions := make(chan string, 8)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				session := r.Header.Get("x-opencode-session")
				sessions <- session
				if session == "" || strings.Contains(session, "$CPA") {
					w.WriteHeader(400)
					_, _ = io.WriteString(w, `{"error":{"type":"MissingSessionID"}}`)
					return
				}
				if stream {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, "data: {\"id\":\"fixture\",\"model\":\"longcat-2.5-preview-free\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"OK\"},\"finish_reason\":null}]}\n\n")
					_, _ = io.WriteString(w, "data: {\"id\":\"fixture\",\"model\":\"longcat-2.5-preview-free\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":1,\"total_tokens\":4}}\n\ndata: [DONE]\n\n")
				} else {
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, `{"id":"fixture","model":"longcat-2.5-preview-free","choices":[{"index":0,"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}`)
				}
			}))
			defer server.Close()
			e := NewOpenAICompatExecutor("fixture", &config.Config{})
			auth := &coreauth.Auth{ID: "opencode-fixture", Prefix: "opencode", Provider: "fixture", Attributes: map[string]string{"base_url": server.URL, "api_key": "upstream-secret", "header:x-opencode-session": "$CPA-SESSION-ID"}}
			call := func(payload string, headers http.Header) string {
				t.Helper()
				req := coreexecutor.Request{Model: "longcat-2.5-preview-free", Payload: []byte(payload)}
				opts := coreexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse, Stream: stream, Headers: headers}
				// Production's conductor enriches identities before executor dispatch.
				opts.Metadata = map[string]any{coreexecutor.CallerScopeMetadataKey: coresession.CallerScope("downstream-fixture")}
				req, opts = coresession.Enrich(req, opts)
				if stream {
					result, err := e.ExecuteStream(context.Background(), auth, req, opts)
					if err != nil {
						t.Fatal(err)
					}
					for chunk := range result.Chunks {
						if chunk.Err != nil {
							t.Fatal(chunk.Err)
						}
					}
				} else if _, err := e.Execute(context.Background(), auth, req, opts); err != nil {
					t.Fatal(err)
				}
				value := <-sessions
				if value == "" || strings.Contains(value, "upstream-secret") || strings.Contains(value, "initial-turn") {
					t.Fatalf("invalid session projection: %q", value)
				}
				return value
			}
			first := `{"model":"longcat-2.5-preview-free","input":[{"role":"user","content":"initial-turn"}]}`
			next := `{"model":"longcat-2.5-preview-free","input":[{"role":"user","content":"initial-turn"},{"role":"assistant","content":"OK"},{"role":"user","content":"next-turn"}]}`
			root := call(first, nil)
			if got := call(next, nil); got != root {
				t.Fatalf("history append changed session: %q != %q", got, root)
			}
			if got := call(strings.Replace(first, "initial-turn", "new-conversation", 1), nil); got == root {
				t.Fatal("different conversations share the same session")
			}
			explicit := call(first, http.Header{"Session_id": []string{"codex-thread-fixture"}})
			if got := call(next, http.Header{"Session_id": []string{"codex-thread-fixture"}}); got != explicit {
				t.Fatal("explicit downstream session changed across turns")
			}
		})
	}
}
