package helps

import (
	"bytes"
	"net/http"
	"strings"
	"testing"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestCodexProgressPolicyIsStableAndPrecedesPayloadRules(t *testing.T) {
	headers := http.Header{"User-Agent": {"codex_cli_rs/1.0"}}
	for _, tc := range []struct{ model, protocol, path, body string }{
		{"claude-opus-5-5", "claude", "system", `{"system":"original","messages":[]}`},
		{"claude-sonnet-5-5", "openai-response", "instructions", `{"instructions":"original","input":"hi"}`},
		{"beam:free", "openai", "messages.0.content", `{"messages":[{"role":"system","content":"original"}]}`},
		{"claude-opus-5-5", "devin", "system_prompt", `{"system_prompt":"original","prompts":[]}`},
	} {
		body := []byte(tc.body)
		out := ApplyPayloadConfigWithRequest(nil, tc.model, tc.protocol, "openai-response", "", body, body, tc.model, "/v1/responses", headers)
		if strings.Count(ojson.GetBytes(out, tc.path).String(), deepSeekMilestoneGuidance) != 1 {
			t.Fatalf("missing progress policy: %s", out)
		}
		if again := ApplyPayloadConfigWithRequest(nil, tc.model, tc.protocol, "openai-response", "", out, body, tc.model, "/v1/responses", headers); !bytes.Equal(again, out) {
			t.Fatal("unstable prompt prefix on retry")
		}
		if ordinary := ApplyPayloadConfigWithRequest(nil, tc.model, tc.protocol, "openai-response", "", body, body, tc.model, "/v1/responses", nil); !bytes.Equal(ordinary, body) {
			t.Fatal("non-Codex request changed")
		}
	}
	body := []byte(`{"instructions":"original","input":"hi"}`)
	if out := ApplyPayloadConfigWithRequest(nil, "gpt-6.1-sol", "openai-response", "openai-response", "", body, body, "gpt-6.1-sol", "/v1/responses", headers); !bytes.Equal(out, body) {
		t.Fatal("native GPT instructions changed")
	}
	cfg := &config.Config{Payload: config.PayloadConfig{Filter: []config.PayloadFilterRule{{Models: []config.PayloadModelRule{{Name: "claude-*"}}, Params: []string{"instructions"}}}}}
	out := ApplyPayloadConfigWithRequest(cfg, "claude-opus-5-5", "openai-response", "openai-response", "", body, body, "claude-opus-5-5", "/v1/responses", headers)
	if ojson.GetBytes(out, "instructions").Exists() {
		t.Fatal("built-in prompt restored a user-filtered field")
	}
}
