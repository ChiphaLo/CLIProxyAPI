package helps

import (
	"bytes"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/tidwall/gjson"
)

func TestDeepSeekMilestonePreservesHistoryAndThinking(t *testing.T) {
	body := []byte(`{"model":"deepseek-v4.1-flash","messages":[{"role":"system","content":"Keep original instructions."},{"role":"user","content":"Continue the task."},{"role":"assistant","content":null,"reasoning_content":"must be replayed","tool_calls":[{"id":"call_1","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"x\"}"}}]},{"role":"tool","tool_call_id":"call_1","content":"result"}],"tools":[{"type":"function","function":{"name":"read_file","parameters":{"type":"object"}}}],"stream":true,"reasoning_effort":"high","temperature":0.7}`)
	for _, model := range []string{"deepseek-v4.1-flash", "opencode2/deepseek-v4.1-flash", "workbuddy-intl/deepseek-v4.1-flash(high)", "deepseek-ai/DeepSeek-V3.2", "ds/deepseek-flash"} {
		t.Run(model, func(t *testing.T) {
			out := ApplyPayloadConfigWithRequest(nil, model, "openai", "openai-response", "", body, body, model, "/v1/responses", nil)
			if !gjson.ValidBytes(out) || !strings.HasPrefix(gjson.GetBytes(out, "messages.0.content").String(), "Keep original instructions.\n\n") || !bytes.Contains(out, []byte("cpa-progress-policy")) {
				t.Fatal("original instructions missing or policy absent")
			}
			for _, path := range []string{"model", "messages.1", "messages.2", "messages.3", "tools", "stream", "reasoning_effort", "temperature"} {
				if gjson.GetBytes(body, path).Raw != gjson.GetBytes(out, path).Raw {
					t.Errorf("policy changed %s", path)
				}
			}
			if again := applyDeepSeekMilestoneGuidance(out, model, "openai", ""); !bytes.Equal(out, again) {
				t.Fatal("policy duplicated on retry")
			}
		})
	}
}

func TestDeepSeekMilestoneDoesNotTouchOtherModelsOrAliases(t *testing.T) {
	body := []byte(`{"messages":[{"role":"system","content":"original"}],"stream":true}`)
	for _, model := range []string{"gpt-6.1-sol", "codex2/gpt-6-astra", "gemini-3.8-flash-high", "claude-sonnet", "my-deepseek-model", "ds/not-deepseek"} {
		out := ApplyPayloadConfigWithRequest(nil, model, "openai", "openai-response", "", body, body, "ds/deepseek-v4.1-flash", "/v1/responses", nil)
		if !bytes.Equal(out, body) {
			t.Errorf("routed non-DeepSeek model %q changed", model)
		}
	}
}

func TestDeepSeekMilestoneShapesAndNoOriginalSystem(t *testing.T) {
	for _, tc := range []struct {
		name, protocol, root, body, path string
	}{
		{"chat", "openai", "", `{"messages":[{"role":"user","content":"hello"}]}`, "messages.0.content"},
		{"chat_blocks", "openai", "", `{"messages":[{"role":"system","content":[{"type":"text","text":"original","cache_control":{"type":"ephemeral"}}]}]}`, "messages.0.content.1.text"},
		{"responses", "openai-response", "", `{"instructions":"original","input":"hello"}`, "instructions"},
		{"responses_empty", "openai-response", "", `{"input":"hello"}`, "instructions"},
		{"claude", "claude", "", `{"system":[{"type":"text","text":"original"}],"messages":[{"role":"user","content":"hello"}]}`, "system.1.text"},
		{"gemini", "gemini", "", `{"systemInstruction":{"parts":[{"text":"original"}]},"contents":[]}`, "systemInstruction.parts.1.text"},
		{"gemini_new", "gemini", "", `{"contents":[]}`, "systemInstruction.parts.0.text"},
		{"gemini_root", "gemini", "request", `{"project":"keep","request":{"system_instruction":{"parts":[{"text":"original"}]},"contents":[]}}`, "request.system_instruction.parts.1.text"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := applyDeepSeekMilestoneGuidance([]byte(tc.body), "deepseek-v4.1-flash", tc.protocol, tc.root)
			if !gjson.ValidBytes(out) || !strings.Contains(gjson.GetBytes(out, tc.path).String(), deepSeekMilestoneGuidance) {
				t.Fatalf("missing policy at %s", tc.path)
			}
			if again := applyDeepSeekMilestoneGuidance(out, "deepseek-v4.1-flash", tc.protocol, tc.root); !bytes.Equal(out, again) {
				t.Fatal("policy not idempotent")
			}
			if tc.name == "chat" && gjson.GetBytes(out, "messages.1").Raw != gjson.Get(tc.body, "messages.0").Raw {
				t.Fatal("prepend changed original user message")
			}
			if tc.name == "chat_blocks" && gjson.GetBytes(out, "messages.0.content.0").Raw != gjson.Get(tc.body, "messages.0.content.0").Raw {
				t.Fatal("original system block/cache control changed")
			}
		})
	}
}

func TestDeepSeekMilestoneUserPayloadRulesRemainFinal(t *testing.T) {
	body := []byte(`{"messages":[{"role":"system","content":"original"},{"role":"user","content":"hello"}]}`)
	cfg := &config.Config{Payload: config.PayloadConfig{Override: []config.PayloadRule{{Models: []config.PayloadModelRule{{Name: "deepseek-*"}}, Params: map[string]any{"messages.0.content": "administrator override"}}}}}
	out := ApplyPayloadConfigWithRequest(cfg, "deepseek-v4.1-flash", "openai", "openai-response", "", body, body, "opencode/deepseek-v4.1-flash", "/v1/responses", nil)
	if gjson.GetBytes(out, "messages.0.content").String() != "administrator override" || bytes.Contains(out, []byte("cpa-progress-policy")) {
		t.Fatal("built-in policy overwrote final user rules")
	}
	cfg.Payload = config.PayloadConfig{Filter: []config.PayloadFilterRule{{Models: []config.PayloadModelRule{{Name: "deepseek-*"}}, Params: []string{"messages.0.content"}}}}
	out = ApplyPayloadConfigWithRequest(cfg, "deepseek-v4.1-flash", "openai", "openai-response", "", body, body, "opencode/deepseek-v4.1-flash", "/v1/responses", nil)
	if gjson.GetBytes(out, "messages.0.content").Exists() {
		t.Fatal("filtered instructions were restored")
	}
}
