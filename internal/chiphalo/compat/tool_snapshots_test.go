package compat

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/tidwall/gjson"
)

func jsSnapshotTool(current bool) map[string]any {
	parameters := map[string]any{"type": "object", "properties": map[string]any{"code": map[string]string{"type": "string"}}, "required": []string{"code"}}
	if current {
		delete(parameters, "required")
		parameters["properties"].(map[string]any)["completion_summary"] = map[string]string{"type": "string"}
		parameters["oneOf"] = []any{map[string]any{"required": []string{"code"}}, map[string]any{"required": []string{"completion_summary"}}}
	}
	return map[string]any{"type": "namespace", "name": "mcp__cua_repl", "tools": []any{map[string]any{"type": "function", "name": "js", "strict": false, "parameters": parameters}}}
}

func TestLatestTrustedToolSnapshotReplacesHistoricalSchema(t *testing.T) {
	old, current := jsSnapshotTool(false), jsSnapshotTool(true)
	body := map[string]any{"model": "claude/claude-sonnet-5-5", "input": []any{
		map[string]any{"type": "additional_tools", "role": "developer", "tools": []any{old}},
		map[string]any{"type": "additional_tools", "role": "developer", "tools": []any{current}},
		map[string]any{"type": "additional_tools", "role": "user", "tools": []any{old}},
		map[string]any{"role": "user", "content": "fixture"},
	}}
	raw, _ := json.Marshal(body)
	out, err := NormalizeResponsesTools(raw)
	if err != nil {
		t.Fatal(err)
	}
	if gjson.GetBytes(out, "tools.#").Int() != 1 || gjson.GetBytes(out, "tools.0.tools.0.parameters.oneOf.#").Int() != 2 || gjson.GetBytes(out, "tools.0.tools.0.parameters.required").Exists() {
		t.Fatalf("historical schema overrode the updated js contract: %s", out)
	}
	if gjson.GetBytes(out, "input.#").Int() != 1 || gjson.GetBytes(out, "input.0.content").String() != "fixture" {
		t.Fatal("trusted tool snapshots leaked into conversation input")
	}
	again, err := NormalizeResponsesTools(out)
	if err != nil || !bytes.Equal(again, out) {
		t.Fatal("tool snapshot resolution is not idempotent")
	}
}

func TestCurrentToolDeclarationOwnsTheContract(t *testing.T) {
	current := jsSnapshotTool(true)
	body := map[string]any{"tools": []any{current}, "input": []any{
		map[string]any{"type": "additional_tools", "role": "developer", "tools": []any{jsSnapshotTool(false)}},
	}}
	raw, _ := json.Marshal(body)
	out, err := NormalizeResponsesTools(raw)
	if err != nil || !gjson.GetBytes(out, "tools.0.tools.0.parameters.oneOf").Exists() {
		t.Fatalf("current runtime contract lost: %s %v", out, err)
	}
}

func TestAmbiguousToolSnapshotStillFailsWhenShadowed(t *testing.T) {
	for _, authoritative := range []bool{false, true} {
		body := map[string]any{"input": []any{map[string]any{"type": "additional_tools", "role": "developer", "tools": []any{jsSnapshotTool(false), jsSnapshotTool(true)}}}}
		if authoritative {
			body["tools"] = []any{jsSnapshotTool(true)}
		}
		raw, _ := json.Marshal(body)
		if _, err := NormalizeResponsesTools(raw); err == nil {
			t.Fatal("conflicting contracts in one snapshot were admitted")
		}
	}
}
