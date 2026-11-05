package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	agresponses "github.com/router-for-me/CLIProxyAPI/v8/internal/translator/antigravity/openai/responses"
	clauderesponses "github.com/router-for-me/CLIProxyAPI/v8/internal/translator/claude/openai/responses"
	chatresponses "github.com/router-for-me/CLIProxyAPI/v8/internal/translator/openai/openai/responses"
	"github.com/tidwall/gjson"
)

func TestProviderPromptProjectionPreservesProgressInstructions(t *testing.T) {
	request := []byte(`{"model":"fixture","instructions":"CPA_AUDIT_SYSTEM: Report verified milestones, not each command.","input":[{"role":"developer","content":[{"type":"input_text","text":"CPA_AUDIT_DEVELOPER: Keep progress updates concise."}]},{"role":"user","content":[{"type":"input_text","text":"Inspect a fixture."}]}]}`)
	for _, tc := range []struct {
		name  string
		model string
		build func(string, []byte, bool) ([]byte, error)
	}{
		{"claude", "claude-sonnet-5-5", clauderesponses.ConvertOpenAIResponsesRequestToClaude},
		{"gemini", "gemini-3.5-flash-lite", agresponses.ConvertOpenAIResponsesRequestToAntigravity},
		{"chat-completions", "deepseek-v4.1-flash", chatresponses.ConvertOpenAIResponsesRequestToOpenAIChatCompletions},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, err := tc.build(tc.model, request, true)
			if err != nil {
				t.Fatal(err)
			}
			for _, marker := range [][]byte{[]byte("CPA_AUDIT_SYSTEM"), []byte("CPA_AUDIT_DEVELOPER")} {
				if !bytes.Contains(body, marker) {
					t.Fatalf("progress instruction missing: %s", marker)
				}
			}
		})
	}
}

func TestProviderResponsesPhaseProjectionAudit(t *testing.T) {
	request := []byte(`{"model":"fixture","tools":[{"type":"function","name":"probe","parameters":{"type":"object"}}]}`)
	fixtures := []struct {
		name string
		raw  string
		call func(context.Context, string, []byte, []byte, []byte, *any) []byte
	}{
		{"claude", `{"id":"msg_fixture","model":"claude-sonnet-5-5","role":"assistant","type":"message","stop_reason":"tool_use","content":[{"type":"text","text":"A verified milestone."},{"type":"tool_use","id":"call_probe","name":"probe","input":{}}],"usage":{"input_tokens":10,"output_tokens":5}}`, clauderesponses.ConvertClaudeResponseToOpenAIResponsesNonStream},
		{"gemini", `{"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"A verified milestone."},{"functionCall":{"name":"probe","args":{}}}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":5}}}`, agresponses.ConvertAntigravityResponseToOpenAIResponsesNonStream},
		{"chat-completions", `{"id":"chat_fixture","model":"deepseek-v4.1-flash","choices":[{"index":0,"message":{"role":"assistant","content":"A verified milestone.","tool_calls":[{"id":"call_probe","type":"function","function":{"name":"probe","arguments":"{}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`, chatresponses.ConvertOpenAIChatCompletionsResponseToOpenAIResponsesNonStream},
	}
	for _, tc := range fixtures {
		t.Run(tc.name, func(t *testing.T) {
			var state any
			body := tc.call(context.Background(), "fixture", request, request, []byte(tc.raw), &state)
			if !gjson.ValidBytes(body) {
				t.Fatal("invalid Responses projection")
			}
			messages := 0
			phases := []string{}
			for _, item := range gjson.GetBytes(body, "output").Array() {
				if item.Get("type").String() == "message" {
					messages++
					phases = append(phases, item.Get("phase").String())
					if item.Get("phase").String() != "commentary" {
						t.Fatal("tool-call narrative is not commentary")
					}
				}
			}
			if messages == 0 {
				t.Fatal("fixture message missing")
			}
			report, _ := json.Marshal(map[string]any{"provider_protocol": tc.name, "messages": messages, "projected_phases": phases})
			t.Log(string(report))
		})
	}
}
