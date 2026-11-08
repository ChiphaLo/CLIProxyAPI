package compat

import (
	"encoding/json"
	"testing"

	"github.com/MoeForge/moego/ojson"
	responses "github.com/router-for-me/CLIProxyAPI/v8/internal/translator/openai/openai/responses"
)

func TestDiscoveredToolsWithSameShortNameKeepTheirNamespaces(t *testing.T) {
	for _, model := range []string{"devin/swe-2", "devin/swe-2-flash"} {
		raw := []byte(`{"model":"` + model + `","tools":[{"type":"namespace","name":"mcp__node_repl","tools":[{"type":"function","name":"js","parameters":{"type":"object","properties":{"code":{"type":"string"}}}}]}],"input":[{"type":"additional_tools","role":"developer","tools":[{"type":"namespace","name":"mcp__cua_repl","tools":[{"type":"function","name":"js","parameters":{"type":"object","properties":{"title":{"type":"string"}}}}]},{"type":"namespace","name":"mcp__node_repl","tools":[{"type":"function","name":"js","description":"updated","parameters":{"type":"object","properties":{"code":{"type":"string"}}}}]}]},{"role":"user","content":"fixture"}]}`)
		normalized, err := NormalizeResponsesTools(raw)
		if err != nil {
			t.Fatal(err)
		}
		chat, err := responses.ConvertOpenAIResponsesRequestToOpenAIChatCompletions(model, normalized, true)
		if err != nil {
			t.Fatal(err)
		}
		if ojson.GetBytes(chat, "tools.#").Int() != 2 || ojson.GetBytes(chat, "tools.0.function.name").String() != "mcp__node_repl__js" || ojson.GetBytes(chat, "tools.1.function.name").String() != "mcp__cua_repl__js" {
			t.Fatalf("namespaced tool identities lost: %s", chat)
		}
		if ojson.GetBytes(chat, "tools.0.function.description").String() != "" {
			t.Fatal("historical documentation overrode the current declaration")
		}
	}
}

func TestNestedNamespacesAndBareToolsHaveSeparateIdentities(t *testing.T) {
	raw := []byte(`{"tools":[{"type":"function","name":"js","parameters":{"type":"object"}},{"type":"namespace","name":"a","tools":[{"type":"namespace","name":"shared","tools":[{"type":"custom","name":"js"}]}]}],"input":[{"type":"additional_tools","role":"system","tools":[{"type":"namespace","namespace":"b","tools":[{"type":"namespace","name":"shared","tools":[{"type":"function","name":"js","parameters":{"type":"object"}}]}]}]}]}`)
	normalized, err := NormalizeResponsesTools(raw)
	if err != nil {
		t.Fatal(err)
	}
	var body struct{ Tools []any }
	if err := json.Unmarshal(normalized, &body); err != nil || len(body.Tools) != 3 {
		t.Fatalf("lost independent tools: %s %v", normalized, err)
	}
}
