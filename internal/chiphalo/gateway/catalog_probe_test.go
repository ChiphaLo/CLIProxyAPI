package gateway

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/catalogsync"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/quota"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestCatalogProbeUsesStockFinalPayloadAndOneExactMember(t *testing.T) {
	live, f, _ := liveFixture(t)
	for prefix, auth := range f.auths {
		a := auth.Clone()
		a.Metadata["access_token"] = "probe-token-" + prefix
		if _, err := f.manager.Update(context.Background(), a); err != nil {
			t.Fatal(err)
		}
	}
	cfg := &config.Config{}
	cfg.ProxyURL = "socks5h://172.17.0.1:10808"
	calls := []string{}
	factory := func(proxy string) (http.RoundTripper, error) {
		if proxy != cfg.ProxyURL {
			t.Error("exact member proxy was lost")
		}
		return quotaRoundTrip(func(r *http.Request) (*http.Response, error) {
			calls = append(calls, r.Header.Get("Authorization"))
			body := `data: {"type":"response.completed","response":{"id":"resp_probe","model":"gpt-6-astra","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"OK"}]}],"usage":{"input_tokens":7,"output_tokens":1,"total_tokens":8}}}` + "\n\n"
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
		}), nil
	}
	probe := live.CatalogProbe(cfg, f.manager, factory)
	for _, b := range []quota.Binding{{ID: "codex-member", Prefix: "codex", Workspace: "same-team"}, {ID: "codex2-member", Prefix: "codex2", Workspace: "same-team"}} {
		result := probe(t.Context(), b, catalogsync.Model{Name: "gpt-6-astra"}, "medium")
		if !result.OK || result.ReportedModel != "gpt-6-astra" {
			t.Fatal("stock probe failed", result)
		}
	}
	if len(calls) != 2 || calls[0] != "Bearer probe-token-codex" || calls[1] != "Bearer probe-token-codex2" || f.executor.results != 0 {
		t.Fatal("probe entered scheduling, fallback or another member")
	}
	if cfg.ProxyURL != "socks5h://172.17.0.1:10808" || f.auths["codex"].ProxyURL != "" {
		t.Fatal("probe mutated shared config or credential")
	}
	bad := quota.Binding{ID: "codex-member", Prefix: "codex2", Workspace: "same-team"}
	if result := probe(t.Context(), bad, catalogsync.Model{Name: "gpt-6-astra"}, "medium"); result.OK || len(calls) != 2 {
		t.Fatal("caller changed fixed binding")
	}
}

func TestCatalogProbePayloadOverridesAreNotFalseCapabilityEvidence(t *testing.T) {
	for _, field := range []string{"model", "reasoning.effort", "input", "tools"} {
		t.Run(field, func(t *testing.T) {
			live, f, _ := liveFixture(t)
			auth := f.auths["codex"].Clone()
			auth.Metadata["access_token"] = "probe-fixture"
			if _, err := f.manager.Update(t.Context(), auth); err != nil {
				t.Fatal(err)
			}
			value := any("gpt-6-luna")
			switch field {
			case "reasoning.effort":
				value = "low"
			case "input":
				value = []any{}
			case "tools":
				value = []any{map[string]any{"type": "web_search"}}
			}
			cfg := &config.Config{Payload: config.PayloadConfig{Override: []config.PayloadRule{{Models: []config.PayloadModelRule{{Name: "gpt-6-astra"}}, Params: map[string]any{field: value}}}}}
			calls := 0
			factory := func(string) (http.RoundTripper, error) {
				return quotaRoundTrip(func(*http.Request) (*http.Response, error) {
					calls++
					t.Error("overridden probe reached upstream")
					return nil, nil
				}), nil
			}
			result := live.CatalogProbe(cfg, f.manager, factory)(t.Context(), quota.Binding{ID: "codex-member", Prefix: "codex", Workspace: "same-team"}, catalogsync.Model{Name: "gpt-6-astra"}, "medium")
			if result.OK || calls != 0 {
				t.Fatal("payload override falsely verified requested capability")
			}
		})
	}
}
