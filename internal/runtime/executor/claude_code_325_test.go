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
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexec "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func TestClaude325MeasuredAdaptiveCapabilities(t *testing.T) {
	// Independently recorded from official 2.1.325 against an isolated loopback
	// endpoint, with fake OAuth and no default route (CLI + SDK, four models).
	const want = "claude-code-20250219,oauth-2025-04-20,interleaved-thinking-2025-05-14,context-management-2025-06-27,prompt-caching-scope-2026-01-05,per-turn-control-2026-07-01,mid-conversation-tool-changes-2026-07-01,tool-provenance-2026-10-26,thinking-token-count-2026-05-13,mid-conversation-system-clear-at-2026-08-21,effort-2025-11-24,thinking-binding-controls-2026-08-01,thinking-display-updates-2026-08-18,extended-cache-ttl-2025-04-11,cache-diagnosis-2026-04-07"
	for _, model := range []string{"claude-opus-5-5", "claude-sonnet-5-5", "claude-haiku-5-5", "claude-fable-5-2", "claude/claude-opus-5.5", "claude/claude-sonnet-5.5", "claude-fable-5.2"} {
		t.Run(model, func(t *testing.T) {
			body := []byte(fmt.Sprintf(`{"model":%q,"thinking":{"type":"adaptive","display":"updates"},"output_config":{"effort":"xhigh"},"diagnostics":{"previous_message_id":null}}`, model))
			expected := want
			if isClaudeHaiku55Model(model) {
				expected = strings.Replace(strings.TrimPrefix(want, "claude-code-20250219,"), "prompt-caching-scope-2026-01-05,", "prompt-caching-scope-2026-01-05,claude-code-20250219,", 1)
			}
			if got := claudeCodeCLIBetas(body, nil, true); got != expected {
				t.Fatalf("betas = %s, want %s", got, expected)
			}
			if !isClaudePreservedThinkingModel(model) || !claudeModelHasPerTurnEffort(model) || !claudeModelUsesProgressDisplay(model) {
				t.Fatal("missing measured adaptive capability")
			}
			if got := computeFingerprint("Reply OK.", helps.DefaultClaudeCodeVersion); got != "735" {
				t.Fatalf("billing fingerprint = %s, measured 735", got)
			}
		})
	}
	for _, model := range []string{"claude-opus-5-50", "claude-sonnet-5-50", "claude-fable-5-20", "claude-haiku-4-5-20251001"} {
		if isClaudeCurrentAdaptiveModel(model) {
			t.Fatalf("incorrect modern family match: %s", model)
		}
	}
	if claudeModelHasPerTurnTiming("claude-haiku-5-5") || !claudeModelHasPerTurnTiming("claude-sonnet-5-5") {
		t.Fatal("per-turn timing disagrees with the measured 2.1.325 catalog")
	}
	legacy := claudeCodeCLIBetas([]byte(`{"model":"claude-haiku-4-5-20251001"}`), nil, true)
	if strings.Contains(legacy, claudeEffortBeta) || strings.Contains(legacy, claudeToolProvenanceBeta) {
		t.Fatal("new capability leaked into legacy helper")
	}
}

func TestClaude325NativeFeaturesSurviveAllRequestPaths(t *testing.T) {
	for _, mode := range []string{"stream", "nonstream", "count"} {
		t.Run(mode, func(t *testing.T) {
			var captured []byte
			var headers http.Header
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				captured, _ = io.ReadAll(r.Body)
				headers = r.Header.Clone()
				w.WriteHeader(400)
				_, _ = io.WriteString(w, `{"error":{"type":"offline_capture","message":"captured"}}`)
			}))
			defer server.Close()
			const sid = "11111111-2222-4333-8444-555555555555"
			body := ojson.NewDocString(`{"model":"claude-haiku-5-5","max_tokens":128000,"thinking":{"type":"adaptive","display":"omitted"},"output_config":{"effort":"xhigh"},"system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.325.735; cc_entrypoint=cli; cc_prompt_index=1; cc_turn_index=2;"}],"tools":[{"name":"read_file","description":"Read a file","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":"Read file"},{"role":"assistant","content":[{"type":"tool_use","id":"toolu_325","name":"read_file","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_325","content":"fixture output"}]}]}`)
			_ = body.Set("metadata.user_id", `{"device_id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","account_uuid":"11111111-2222-4333-8444-555555555555","session_id":"`+sid+`"}`)
			raw := body.Bytes()
			in := http.Header{"User-Agent": {"claude-cli/2.1.325 (external, cli)"}, "X-App": {"cli"}, "Anthropic-Beta": {"claude-code-20250219,tool-provenance-2026-10-26,per-turn-control-2026-07-01,effort-2025-11-24"}, "X-Claude-Code-Prompt-Id": {sid}, "X-Claude-1p-Request-Class": {"main"}, "X-Claude-Code-Sandy-Plover": {"[]"}, "X-Claude-Code-Session-Id": {sid}}
			e := NewClaudeExecutor(&config.Config{})
			auth := &coreauth.Auth{ID: "native-325", Metadata: claudeOAuthTestMetadata(), Attributes: map[string]string{"api_key": "sk-ant-oat-fake-isolated", "base_url": server.URL}}
			req := coreexec.Request{Model: "claude-haiku-5-5", Payload: raw}
			opts := coreexec.Options{SourceFormat: sdktranslator.FormatClaude, OriginalRequest: raw, Headers: in}
			switch mode {
			case "stream":
				_, _ = e.ExecuteStream(context.Background(), auth, req, opts)
			case "nonstream":
				_, _ = e.Execute(context.Background(), auth, req, opts)
			case "count":
				_, _ = e.countTokensUpstream(context.Background(), auth, req, opts)
			}
			if len(captured) == 0 {
				t.Fatal("no loopback request")
			}
			for _, path := range []string{"messages", "tools", "thinking", "output_config"} {
				if got := ojson.GetBytes(captured, path).Raw; got != body.Get(path).Raw {
					t.Fatalf("native %s changed: %s", path, got)
				}
			}
			if got := headers.Get("X-Claude-Code-Prompt-Id"); got != sid {
				t.Fatalf("prompt ID dropped: %s", got)
			}
			if got := headers.Get("X-Claude-Code-Sandy-Plover"); got != "[]" {
				t.Fatalf("provenance envelope dropped: %s", got)
			}
			if mode != "count" && !strings.Contains(string(captured), "cc_prompt_index=1; cc_turn_index=2;") {
				t.Fatal("native turn tracking lost")
			}
			if mode != "count" && !strings.Contains(headers.Get("Anthropic-Beta"), claudeToolProvenanceBeta) {
				t.Fatal("tool provenance beta dropped")
			}
		})
	}
}
