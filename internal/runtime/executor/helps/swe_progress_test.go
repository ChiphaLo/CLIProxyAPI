package helps

import (
	"bytes"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/tidwall/gjson"
)

func TestSWEMilestoneGuidance(t *testing.T) {
	for _, model := range []string{"swe-2", "devin/swe-2", "devin1/SWE-2", "swe-2-medium", "swe-2(max)", "swe-2-lightning", "swe-1-6"} {
		for _, tc := range []struct{ protocol, body, path, preserve string }{
			{"openai-response", `{"instructions":"Keep original instructions.","input":[{"type":"function_call","call_id":"c1","name":"probe","arguments":"{}"},{"type":"function_call_output","call_id":"c1","output":[{"type":"input_text","text":"ok"},{"type":"input_image","image_url":"data:image/png;base64,aGVsbG8="}]}],"tools":[{"type":"function","name":"probe","parameters":{"type":"object"}}],"reasoning":{"effort":"max"},"stream":true}`, "instructions", "input"},
			{"openai", `{"messages":[{"role":"system","content":[{"type":"text","text":"original","cache_control":{"type":"ephemeral"}}]},{"role":"assistant","content":null,"reasoning_content":"private","tool_calls":[{"id":"c1","type":"function","function":{"name":"probe","arguments":"{}"}}]},{"role":"tool","tool_call_id":"c1","content":"ok"}],"tools":[],"reasoning_effort":"medium"}`, "messages.0.content.1.text", "messages.1"},
			{"devin", `{"system_prompt":"Keep original instructions.","prompts":[{"source":2,"content":"ok","tool_call_id":"c1"}],"tools":[],"model":"swe-2-medium"}`, "system_prompt", "prompts"},
		} {
			t.Run(model+"/"+tc.protocol, func(t *testing.T) {
				body := []byte(tc.body)
				out := ApplyPayloadConfigWithRequest(nil, model, tc.protocol, "openai-response", "", body, body, model, "/v1/responses", nil)
				if !strings.Contains(gjson.GetBytes(out, tc.path).String(), deepSeekMilestoneGuidance) {
					t.Fatal("milestone policy absent")
				}
				for _, path := range []string{tc.preserve, "tools", "reasoning", "reasoning_effort", "stream", "model"} {
					if gjson.GetBytes(out, path).Raw != gjson.GetBytes(body, path).Raw {
						t.Fatalf("policy changed %s", path)
					}
				}
				if again := ApplyPayloadConfigWithRequest(nil, model, tc.protocol, "openai-response", "", out, body, model, "/v1/responses", nil); !bytes.Equal(again, out) {
					t.Fatal("policy duplicated on retry")
				}
			})
		}
	}
}

func TestSWEMilestoneScopeAndPayloadBarrier(t *testing.T) {
	body := []byte(`{"instructions":"original","input":"hello"}`)
	for _, model := range []string{"gpt-6.1-sol", "claude-opus-5-5", "gemini-3.8-flash", "not-swe-2", "sweeper", "deepseek-v4.1-flash"} {
		if out := applySWEMilestoneGuidance(body, model, "openai-response", ""); !bytes.Equal(out, body) {
			t.Fatalf("non-SWE model %q changed", model)
		}
	}
	for _, protocol := range []string{"openai-response", "devin"} {
		path := "instructions"
		if protocol == "devin" {
			path = "system_prompt"
		}
		cfg := &config.Config{Payload: config.PayloadConfig{Override: []config.PayloadRule{{Models: []config.PayloadModelRule{{Name: "swe-*"}}, Params: map[string]any{path: "administrator override"}}}}}
		out := ApplyPayloadConfigWithRequest(cfg, "swe-2", protocol, "openai-response", "", body, body, "devin/swe-2", "/v1/responses", nil)
		if gjson.GetBytes(out, path).String() != "administrator override" || bytes.Contains(out, []byte("cpa-progress-policy")) {
			t.Fatal("built-in policy overwrote final user rules")
		}
		cfg.Payload = config.PayloadConfig{Filter: []config.PayloadFilterRule{{Models: []config.PayloadModelRule{{Name: "swe-*"}}, Params: []string{path}}}}
		out = ApplyPayloadConfigWithRequest(cfg, "swe-2", protocol, "openai-response", "", body, body, "devin/swe-2", "/v1/responses", nil)
		if gjson.GetBytes(out, path).Exists() {
			t.Fatal("filtered instructions restored")
		}
	}
}
