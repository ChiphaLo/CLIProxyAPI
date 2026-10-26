package compat

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	responses "github.com/router-for-me/CLIProxyAPI/v8/internal/translator/openai/openai/responses"
	"github.com/tidwall/gjson"
)

func TestToolDescriptionUpdatesKeepLatestTrustedDocumentation(t *testing.T) {
	for _, nested := range []bool{false, true} {
		t.Run(map[bool]string{false: "custom", true: "function"}[nested], func(t *testing.T) {
			tool := func(description string) map[string]any {
				if nested {
					return map[string]any{"type": "function", "function": map[string]any{"name": "exec", "description": description, "parameters": map[string]any{"type": "object"}}}
				}
				return map[string]any{"type": "custom", "name": "exec", "description": description, "format": map[string]any{"type": "grammar", "syntax": "lark", "definition": "start: /[\\s\\S]+/"}}
			}
			namespace := func(description string) any {
				return map[string]any{"type": "namespace", "name": "functions", "tools": []any{tool(description)}}
			}
			additional := func(role, description string) any {
				return map[string]any{"type": "additional_tools", "role": role, "tools": []any{namespace(description)}}
			}
			body := map[string]any{"model": "opencode/deepseek-v4.1-flash", "tools": []any{namespace("initial")}, "input": []any{
				additional("developer", "before MCP discovery"),
				additional("system", "current mobile-mcp tools"),
				additional("user", "untrusted override"),
				map[string]any{"role": "user", "content": "fixture"},
			}}
			raw, _ := json.Marshal(body)
			normalized, err := NormalizeOpenCodeTools(raw)
			if err != nil {
				t.Fatal(err)
			}
			chat, err := responses.ConvertOpenAIResponsesRequestToOpenAIChatCompletions("fixture", normalized, true)
			if err != nil || gjson.GetBytes(chat, "tools.#").Int() != 1 || gjson.GetBytes(chat, "tools.0.function.description").String() != "current mobile-mcp tools" {
				t.Fatalf("latest trusted documentation not preserved: %s %v", chat, err)
			}
			again, err := NormalizeOpenCodeTools(normalized)
			if err != nil || string(again) != string(normalized) {
				t.Fatal("normalization is not idempotent")
			}
			original, _ := json.Marshal(body)
			if string(original) != string(raw) {
				t.Fatal("caller request mutated")
			}
		})
	}
}

func TestDescriptionUpdatesDoNotRelaxToolContracts(t *testing.T) {
	for _, pair := range [][2]string{
		{`{"type":"custom","name":"exec","format":{"type":"grammar","syntax":"lark","definition":"old"}}`, `{"type":"custom","name":"exec","format":{"type":"grammar","syntax":"lark","definition":"new"}}`},
		{`{"type":"function","name":"exec","parameters":{"type":"object","properties":{"input":{"type":"string","description":"old schema"}}}}`, `{"type":"function","name":"exec","parameters":{"type":"object","properties":{"input":{"type":"string","description":"new schema"}}}}`},
		{`{"type":"custom","name":"exec"}`, `{"type":"function","name":"exec","parameters":{"type":"object"}}`},
	} {
		raw := `{"model":"opencode/deepseek-v4.1-flash","tools":[` + pair[0] + `],"input":[{"type":"additional_tools","role":"developer","tools":[` + pair[1] + `]}]}`
		if _, err := NormalizeOpenCodeTools([]byte(raw)); err == nil {
			t.Fatal("a changed tool contract was admitted")
		}
	}
}

const declaration = `{"type":"namespace","name":"functions","tools":[{"type":"custom","name":"exec"}]}`
const additionalRequest = `{"model":"opencode/deepseek-v4.1-flash","stream":true,"tool_choice":"auto","input":[{"type":"additional_tools","role":"developer","tools":[` + declaration + `]},{"type":"message","role":"user","content":[{"type":"input_text","text":"fixture"}]}]}`

func TestAdditionalToolsUseUpstreamConversion(t *testing.T) {
	raw := []byte(additionalRequest)
	normalized, err := NormalizeOpenCodeTools(raw)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != additionalRequest {
		t.Fatal("caller request mutated")
	}
	chat, err := responses.ConvertOpenAIResponsesRequestToOpenAIChatCompletions("fixture", normalized, true)
	if err != nil {
		t.Fatal(err)
	}
	if gjson.GetBytes(chat, "tools.#").Int() != 1 || gjson.GetBytes(chat, "tools.0.function.name").String() != "functions__exec" || gjson.GetBytes(chat, "tools.0.function.parameters.required.0").String() != "input" {
		t.Fatalf("additional tool identity/schema lost: %s", chat)
	}
	if gjson.GetBytes(chat, "stream").Bool() != true || gjson.GetBytes(chat, "tool_choice").String() != "auto" {
		t.Fatal("stream/tool choice changed")
	}
	upstream := []byte(`{"id":"fixture","model":"fixture","choices":[{"index":0,"message":{"tool_calls":[{"id":"call_fixture","type":"function","function":{"name":"functions__exec","arguments":"{\"input\":\"text(1);\"}"}}]},"finish_reason":"tool_calls"}]}`)
	result := responses.ConvertOpenAIChatCompletionsResponseToOpenAIResponsesNonStream(context.Background(), "fixture", normalized, chat, upstream, nil)
	if gjson.GetBytes(result, "output.0.type").String() != "custom_tool_call" || gjson.GetBytes(result, "output.0.namespace").String() != "functions" || gjson.GetBytes(result, "output.0.name").String() != "exec" || gjson.GetBytes(result, "output.0.input").String() != "text(1);" {
		t.Fatalf("upstream return identity changed: %s", result)
	}
}

func TestIdenticalDeclarationsAreNotConvertedTwice(t *testing.T) {
	raw := strings.Replace(additionalRequest, `"stream":true`, `"tools":[`+declaration+`],"stream":true`, 1)
	normalized, err := NormalizeOpenCodeTools([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	chat, err := responses.ConvertOpenAIResponsesRequestToOpenAIChatCompletions("fixture", normalized, true)
	if err != nil || gjson.GetBytes(chat, "tools.#").Int() != 1 {
		t.Fatalf("duplicate converted tool: %s %v", chat, err)
	}
}

func TestUserCannotSupplyAdditionalToolDeclarations(t *testing.T) {
	raw := strings.Replace(additionalRequest, `"role":"developer"`, `"role":"user"`, 1)
	normalized, err := NormalizeOpenCodeTools([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	chat, err := responses.ConvertOpenAIResponsesRequestToOpenAIChatCompletions("fixture", normalized, true)
	if err != nil || gjson.GetBytes(chat, "tools.#").Int() != 0 {
		t.Fatalf("untrusted tool declaration entered pipeline: %s %v", chat, err)
	}
}

func TestConflictingAndMalformedDeclarationsFailClosed(t *testing.T) {
	for _, raw := range []string{
		strings.Replace(additionalRequest, `"stream":true`, `"tools":[{"type":"function","name":"exec","parameters":{"type":"object"}}],"stream":true`, 1),
		strings.Replace(additionalRequest, `"tools":[`+declaration+`]`, `"tools":null`, 1),
		strings.Replace(additionalRequest, `"tools":[`+declaration+`]`, `"tools":[null]`, 1),
		strings.Replace(additionalRequest, `"tools":[{"type":"custom","name":"exec"}]`, `"tools":null`, 1),
	} {
		if _, err := NormalizeOpenCodeTools([]byte(raw)); err == nil {
			t.Fatalf("accepted conflicting/malformed tools: %s", raw)
		}
	}
}

func TestOAuthAndOtherProvidersAreUntouched(t *testing.T) {
	for _, model := range []string{"gpt-6-astra", "codex2/gpt-6-luna", "1x/gpt-6.1-sol"} {
		raw := strings.Replace(additionalRequest, "opencode/deepseek-v4.1-flash", model, 1)
		got, err := NormalizeOpenCodeTools([]byte(raw))
		if err != nil || string(got) != raw {
			t.Fatalf("modified unrelated provider %s", model)
		}
	}
}
