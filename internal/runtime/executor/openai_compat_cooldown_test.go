package executor

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

const bridgeCooldownFixture = `{"error":{"code":"model_cooldown","message":"All credentials are cooling down","model":"fixture","reset_seconds":7200,"reset_time":"2h0m0s"}}`

func TestBridgeCooldownStatusError(t *testing.T) {
	for _, tt := range []struct {
		name, body, header string
		status             int
		wantScoped         bool
		wantDelay          time.Duration
	}{
		{"body deadline", bridgeCooldownFixture, "", 429, true, 2 * time.Hour},
		{"short header cannot shorten inner deadline", bridgeCooldownFixture, "7", 429, true, 2 * time.Hour},
		{"long header wins", bridgeCooldownFixture, "8000", 429, true, 8000 * time.Second},
		{"ordinary quota", `{"error":{"code":"rate_limit_error"}}`, "9", 429, false, 9 * time.Second},
		{"wrong status", bridgeCooldownFixture, "", 503, false, 0},
		{"missing model", `{"error":{"code":"model_cooldown","reset_seconds":7}}`, "", 429, false, 0},
		{"negative", strings.Replace(bridgeCooldownFixture, "7200", "-1", 1), "", 429, false, 0},
		{"fraction", strings.Replace(bridgeCooldownFixture, "7200", "1.5", 1), "", 429, false, 0},
		{"overflow", strings.Replace(bridgeCooldownFixture, "7200", "1e100", 1), "", 429, false, 0},
		{"string delay", strings.Replace(bridgeCooldownFixture, "7200", `"7200"`, 1), "", 429, false, 0},
		{"invalid JSON", bridgeCooldownFixture + "x", "", 429, false, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := newOpenAICompatStatusError(tt.status, http.Header{"Retry-After": {tt.header}}, []byte(tt.body))
			if err.IsRequestScoped() != tt.wantScoped || err.Error() != tt.body || err.StatusCode() != tt.status {
				t.Fatalf("classification changed: %+v", err)
			}
			if tt.wantDelay == 0 {
				if err.RetryAfter() != nil {
					t.Fatalf("unexpected hint: %v", err.RetryAfter())
				}
				return
			}
			if err.RetryAfter() == nil || *err.RetryAfter() != tt.wantDelay {
				t.Fatalf("hint: %v, want %v", err.RetryAfter(), tt.wantDelay)
			}
		})
	}
}

func TestBridgeCooldownStopsManagerReplay(t *testing.T) {
	for _, native := range []bool{false, true} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("responses=%v/stream=%v", native, stream), func(t *testing.T) {
				var attempts atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					attempts.Add(1)
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(429)
					fmt.Fprint(w, bridgeCooldownFixture)
				}))
				defer server.Close()
				provider := "bridge-cooldown-fixture"
				manager := coreauth.NewManager(nil, nil, nil)
				manager.SetRetryConfig(3, 0, 0)
				manager.RegisterExecutor(NewOpenAICompatExecutor(provider, &config.Config{}))
				auth := &coreauth.Auth{ID: t.Name(), Provider: provider, Attributes: map[string]string{"base_url": server.URL, "api_key": "fixture-key"}}
				if native {
					auth.Attributes["upstream_protocol"] = "responses"
				}
				reg := registry.GetGlobalRegistry()
				reg.RegisterClient(auth.ID, provider, []*registry.ModelInfo{{ID: "fixture"}})
				defer reg.UnregisterClient(auth.ID)
				if _, err := manager.Register(context.Background(), auth); err != nil {
					t.Fatal(err)
				}
				req := coreexecutor.Request{Model: "fixture", Payload: []byte(`{"model":"fixture","input":"hi"}`)}
				opts := coreexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse, Stream: stream}
				var err error
				if stream {
					var result *coreexecutor.StreamResult
					result, err = manager.ExecuteStream(context.Background(), []string{provider}, req, opts)
					if err == nil && result != nil {
						for chunk := range result.Chunks {
							if chunk.Err != nil {
								err = chunk.Err
							}
						}
					}
				} else {
					_, err = manager.Execute(context.Background(), []string{provider}, req, opts)
				}
				if err == nil || !strings.Contains(err.Error(), "model_cooldown") {
					t.Fatalf("missing inner cooldown: %v", err)
				}
				if n := attempts.Load(); n != 1 {
					t.Fatalf("upstream attempts=%d, want exactly 1", n)
				}
				if hint := coreauth.SafeResponseHeaders(err).Get("Retry-After"); hint != "7200" {
					t.Fatalf("retry header=%q", hint)
				}
				updated, _ := manager.GetByID(auth.ID)
				if updated.Quota.Exceeded || updated.Unavailable {
					t.Fatalf("bridge credential penalized: %+v", updated.Quota)
				}
				for _, state := range updated.ModelStates {
					if state.Quota.Exceeded || state.NextRetryAfter.After(time.Now()) {
						t.Fatal("duplicate outer cooldown created")
					}
				}
			})
		}
	}
}
