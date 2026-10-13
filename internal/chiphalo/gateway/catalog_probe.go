package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/catalogsync"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/quota"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	exec "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

const catalogProbePrompt = "Reply with OK."

type guardedProbeTransport struct {
	base                            http.RoundTripper
	token, workspace, model, effort string
	sent                            bool
}

func (g *guardedProbeTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	// Validate the final wire payload after stock translation and user payload
	// rules. Overrides must not become false evidence of a model's capability.
	if g.sent || r.Method != "POST" || r.URL.String() != "https://chatgpt.com/backend-api/codex/responses" || r.Header.Get("Authorization") != "Bearer "+g.token || r.Header.Get("ChatGPT-Account-Id") != g.workspace || r.Header.Get("Content-Encoding") != "" || r.GetBody == nil {
		return nil, errors.New("catalog probe final transport contract rejected")
	}
	body, err := r.GetBody()
	if err != nil {
		return nil, errors.New("catalog probe payload unavailable")
	}
	raw, err := io.ReadAll(io.LimitReader(body, 32769))
	_ = body.Close()
	var payload struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
		Input  []struct {
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"input"`
		Reasoning struct {
			Effort string `json:"effort"`
		} `json:"reasoning"`
		Tools []json.RawMessage `json:"tools"`
	}
	if err != nil || len(raw) > 32768 || json.Unmarshal(raw, &payload) != nil || payload.Model != g.model || !payload.Stream || len(payload.Tools) > 0 || len(payload.Input) != 1 || payload.Input[0].Role != "user" || len(payload.Input[0].Content) != 1 || payload.Input[0].Content[0].Type != "input_text" || payload.Input[0].Content[0].Text != catalogProbePrompt || g.effort != "" && payload.Reasoning.Effort != g.effort {
		return nil, errors.New("catalog probe final payload contract rejected")
	}
	g.sent = true
	resp, err := g.base.RoundTrip(r)
	if err == nil && resp != nil && resp.Body != nil {
		resp.Body = &boundedProbeBody{ReadCloser: resp.Body, left: 1024 * 1024}
	}
	return resp, err
}

type boundedProbeBody struct {
	io.ReadCloser
	left int64
}

func (b *boundedProbeBody) Read(p []byte) (int, error) {
	if b.left == 0 {
		var one [1]byte
		n, err := b.ReadCloser.Read(one[:])
		if n > 0 {
			return 0, errors.New("catalog probe response exceeds limit")
		}
		return 0, err
	}
	if int64(len(p)) > b.left {
		p = p[:b.left]
	}
	n, err := b.ReadCloser.Read(p)
	b.left -= int64(n)
	return n, err
}

// CatalogProbe uses the stock HTTP Codex executor with one exact credential,
// never the scheduler, retries, automatic refresh or another member's auth.
// The factory is injectable for isolated tests; production uses stock uTLS.
func (l *LiveProfile) CatalogProbe(cfg *config.Config, manager *coreauth.Manager, factory quota.TransportFactory) catalogsync.Probe {
	return func(ctx context.Context, b quota.Binding, model catalogsync.Model, effort string) catalogsync.ProbeResult {
		result := catalogsync.ProbeResult{}
		if ctx.Err() != nil || cfg == nil || manager == nil || !catalogsync.Eligible(model.Name) {
			return result
		}
		profile, err := l.Refresh()
		if err != nil {
			return result
		}
		authID := ""
		for _, binding := range profile.Accounts.Bindings() {
			if binding.ID == b.ID && binding.Prefix == b.Prefix && binding.WorkspaceID == b.Workspace && binding.Enabled() {
				authID = binding.AuthID
			}
		}
		auth, found := manager.GetByID(authID)
		if !found || profile.Accounts.ValidateAuth(auth) != nil {
			return result
		}
		token, _ := auth.Metadata["access_token"].(string)
		if token == "" || strings.ContainsAny(token+b.Workspace, "\r\n\x00") {
			return result
		}
		proxy := auth.ProxyURL
		if proxy == "" {
			proxy = cfg.ProxyURL
		}
		var transport http.RoundTripper
		if factory != nil {
			transport, err = factory(proxy)
		} else {
			transport = helps.NewUtlsHTTPClient(ctx, cfg, auth, 0).Transport
		}
		if err != nil || transport == nil {
			return result
		}
		guard := &guardedProbeTransport{base: transport, token: token, workspace: b.Workspace, model: model.Name, effort: effort}
		// Only transport hosting is separated; the real configured payload rules
		// remain the final semantic barrier and are verified at RoundTrip.
		probeCfg := *cfg
		probeCfg.ProxyURL = ""
		probeCfg.DisableImageGeneration = config.DisableImageGenerationAll
		selected := auth.Clone()
		selected.ProxyURL = ""
		payload := map[string]any{"model": model.Name, "input": []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_text", "text": catalogProbePrompt}}}}, "max_output_tokens": 1, "stream": false}
		if effort != "" {
			payload["reasoning"] = map[string]any{"effort": effort}
		}
		raw, _ := json.Marshal(payload)
		ctx = context.WithValue(ctx, "cliproxy.roundtripper", http.RoundTripper(guard))
		ctx = context.WithValue(ctx, metadataKey{}, requestMetadata{Model: b.Prefix + "/" + model.Name, KeyID: "admin", Path: "/internal/catalog-probe", Started: time.Now()})
		ctx = profile.decorateAdmission(usage.WithRawUsageCapture(ctx), authID)
		format := translator.FromString("openai-response")
		response, err := executor.NewCodexExecutor(&probeCfg).Execute(ctx, selected, exec.Request{Model: model.Name, Payload: raw, Format: format}, exec.Options{SourceFormat: format, OriginalRequest: raw})
		if err != nil {
			if code, ok := err.(interface{ StatusCode() int }); ok {
				result.Status = code.StatusCode()
			}
			return result
		}
		var value struct {
			Status string `json:"status"`
			Model  string `json:"model"`
		}
		if !guard.sent || json.Unmarshal(response.Payload, &value) != nil || value.Status != "completed" || value.Model == "" {
			return result
		}
		result.OK, result.Status, result.ReportedModel = true, 200, value.Model
		return result
	}
}
