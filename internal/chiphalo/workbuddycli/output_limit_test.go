package workbuddycli

import (
	"encoding/json"
	"strings"
	"testing"

	responses "github.com/router-for-me/CLIProxyAPI/v8/internal/translator/openai/openai/responses"
)

func TestOutputLimitMapsToOfficialCLIEnvironment(t *testing.T) {
	for _, tc := range []struct {
		fields string
		want   int
	}{
		{``, 0},
		{`,"max_tokens":null`, 0},
		{`,"max_completion_tokens":null`, 0},
		{`,"max_tokens":1`, 1},
		{`,"max_completion_tokens":4096`, 4096},
		{`,"max_tokens":100000,"max_completion_tokens":100000`, 100000},
		{`,"max_tokens":null,"max_completion_tokens":2048`, 2048},
	} {
		r, _, err := decodeRequest([]byte(`{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"fixture"}]` + tc.fields + `}`))
		if err != nil || r.MaxTokens != tc.want {
			t.Fatalf("%s: got %d, %v", tc.fields, r.MaxTokens, err)
		}
		env := outputLimitEnvironment(r)
		if tc.want == 0 {
			if len(env) != 0 {
				t.Fatal("omitted limit must preserve CLI default")
			}
		} else {
			var value int
			if len(env) != 1 || !strings.HasPrefix(env[0], "CODEBUDDY_CODE_MAX_OUTPUT_TOKENS=") || json.Unmarshal([]byte(strings.TrimPrefix(env[0], "CODEBUDDY_CODE_MAX_OUTPUT_TOKENS=")), &value) != nil || value != tc.want {
				t.Fatalf("output bound not forwarded to CLI: %v", env)
			}
		}
	}
}

func TestOutputLimitRejectsInvalidOrConflictingBounds(t *testing.T) {
	for _, fields := range []string{
		`"max_tokens":0`, `"max_tokens":-1`, `"max_tokens":100001`,
		`"max_tokens":1.5`, `"max_tokens":"4096"`, `"max_tokens":true`,
		`"max_completion_tokens":{}`, `"max_tokens":2048,"max_completion_tokens":4096`,
	} {
		if _, _, err := decodeRequest([]byte(`{"messages":[],` + fields + `}`)); err == nil {
			t.Fatalf("invalid bound accepted: %s", fields)
		}
	}
}

func TestResponsesOutputLimitSurvivesTranslatorAndAdapter(t *testing.T) {
	raw, err := responses.ConvertOpenAIResponsesRequestToOpenAIChatCompletions("deepseek-v4.1-flash", []byte(`{"model":"workbuddy-intl/deepseek-v4.1-flash","input":"Reply OK","max_output_tokens":4096}`), false)
	if err != nil {
		t.Fatal(err)
	}
	r, _, err := decodeRequest(raw)
	if err != nil || r.MaxTokens != 4096 {
		t.Fatalf("Responses output limit lost: %s, %v", raw, err)
	}
}

func TestOutputLimitCompletionDoesNotInventSuccess(t *testing.T) {
	for _, tc := range []struct {
		reason, want string
		required     bool
	}{
		{"end_turn", "stop", false}, {"max_tokens", "length", false},
		{"cancelled", "", false}, {"refusal", "", false},
		{"end_turn", "", true}, {"max_tokens", "", true},
	} {
		got, err := textFinishReason(tc.reason, tc.required)
		if got != tc.want || (err != nil) != (tc.want == "") {
			t.Fatalf("%s, required=%v: %q, %v", tc.reason, tc.required, got, err)
		}
	}
}
