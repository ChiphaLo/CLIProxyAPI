package executor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestGeminiCountTokensNormalizesNamespaceBeforeFinalPayloadRules(t *testing.T) {
	for _, nested := range []string{"generateContentRequest", "generate_content_request"} {
		for _, rule := range []string{"none", "override", "filter"} {
			t.Run(nested+"/"+rule, func(t *testing.T) {
				var received []byte
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					received, _ = io.ReadAll(r.Body)
					if r.URL.Path != "/v1beta/models/gemini-count-fixture:countTokens" {
						t.Errorf("wrong upstream URL %s", r.URL.Path)
					}
					_, _ = w.Write([]byte(`{"totalTokens":5}`))
				}))
				defer server.Close()
				cfg := &config.Config{}
				if rule == "override" {
					cfg.Payload.Override = []config.PayloadRule{{Models: []config.PayloadModelRule{{Name: "gemini-count-fixture", Protocol: "gemini"}}, Params: map[string]any{nested + ".model": "models/payload-override"}}}
				}
				if rule == "filter" {
					cfg.Payload.Filter = []config.PayloadFilterRule{{Models: []config.PayloadModelRule{{Name: "gemini-count-fixture", Protocol: "gemini"}}, Params: []string{nested + ".model"}}}
				}
				e := NewGeminiExecutor(cfg)
				auth := &coreauth.Auth{ID: "namespace-fixture", Provider: "gemini", Attributes: map[string]string{"api_key": "test-upstream", "base_url": server.URL}}
				req := coreexecutor.Request{Model: "gemini-count-fixture", Payload: []byte(`{"` + nested + `":{"model":"models/google-a/gemini-count-fixture","contents":[{"parts":[{"text":"count"}]}]}}`)}
				if _, err := e.CountTokens(context.Background(), auth, req, coreexecutor.Options{SourceFormat: sdktranslator.FormatGemini}); err != nil {
					t.Fatal(err)
				}
				value := gjson.GetBytes(received, nested+".model")
				want := "models/gemini-count-fixture"
				if rule == "override" {
					want = "models/payload-override"
				}
				if rule == "filter" {
					if value.Exists() {
						t.Fatal("normalizer restored filtered model")
					}
				} else if value.String() != want {
					t.Fatalf("wrong final model %s", value.String())
				}
			})
		}
	}
}
