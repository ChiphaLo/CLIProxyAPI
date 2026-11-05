package executor

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func TestNativeResponsesPreservesRateLimitFailure(t *testing.T) {
	for _, partial := range []bool{false, true} {
		t.Run(fmt.Sprint(partial), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_test\",\"model\":\"swe-2-max\"}}\n\n")
				if partial {
					fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"item_id\":\"msg_test\",\"output_index\":0,\"content_index\":0,\"delta\":\"partial\"}\n\n")
				}
				fmt.Fprint(w, `data: {"type":"response.failed","status":429,"error":{"type":"rate_limit_error","code":"rate_limit_exceeded","message":"Reached free model rate limit. Your limit will reset in 9 hours 44 minutes (at 17:01 UTC)."},"response":{"output":[{"text":"PRIVATE OUTPUT"}]}}`+"\n\n")
			}))
			defer server.Close()
			e := NewOpenAICompatExecutor("devin1", &config.Config{})
			a := &coreauth.Auth{ID: "failure-fixture", Attributes: map[string]string{"base_url": server.URL, "upstream_protocol": "responses"}}
			result, err := e.ExecuteStream(context.Background(), a, coreexecutor.Request{Model: "swe-2", Payload: []byte(`{"input":"hello"}`)}, coreexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse, Stream: true})
			if err != nil {
				t.Fatal(err)
			}
			var failure statusErr
			count := 0
			for chunk := range result.Chunks {
				if chunk.Err != nil {
					count++
					if !errors.As(chunk.Err, &failure) {
						t.Fatal(chunk.Err)
					}
				}
				if strings.Contains(string(chunk.Payload), "response.completed") {
					t.Fatal("failed stream completed")
				}
			}
			if count != 1 || failure.StatusCode() != 429 {
				t.Fatalf("failures=%d, status=%d", count, failure.StatusCode())
			}
			if failure.RetryAfter() == nil || *failure.RetryAfter() != 9*time.Hour+44*time.Minute {
				t.Fatal("retry hint lost")
			}
			if !strings.Contains(failure.Error(), "rate_limit_exceeded") || !strings.Contains(failure.Error(), "free model rate limit") || !strings.Contains(failure.Error(), "try again in 35040s") {
				t.Fatalf("upstream reason masked: %s", failure.Error())
			}
			if strings.Contains(failure.Error(), "PRIVATE OUTPUT") {
				t.Fatal("response output leaked into error")
			}
		})
	}
}
