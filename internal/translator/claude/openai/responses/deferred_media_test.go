package responses

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/MoeForge/moego/ojson"
)

func TestDeferredMediaPreservesLargeAndEscapedSources(t *testing.T) {
	data := strings.Repeat("ABC+/=&<>\"\\\n", 10000)
	part := map[string]any{"type": "input_image", "image_url": "data:image/png;base64," + data}
	file := map[string]any{"type": "input_file", "file_data": "data:application/pdf;base64," + data}
	body, _ := json.Marshal(map[string]any{"input": []any{
		map[string]any{"role": "user", "content": []any{part, file}},
		map[string]any{"type": "function_call", "call_id": "call_1", "name": "read", "arguments": "{}"},
		map[string]any{"type": "function_call_output", "call_id": "call_1", "output": []any{part, file}},
	}, "tools": []any{map[string]any{"type": "function", "name": "read", "parameters": map[string]any{"type": "object"}}}})
	out, err := ConvertOpenAIResponsesRequestToClaude("claude-opus-5-5", body, true)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "cpa-media:") {
		t.Fatal("internal handle leaked")
	}
	root := ojson.ParseBytes(out)
	var count int
	var walk func(ojson.Result)
	walk = func(value ojson.Result) {
		if value.IsObject() {
			if source := value.Get("source.data"); source.Exists() {
				if source.String() != data {
					t.Fatal("media changed")
				}
				count++
			}
		}
		value.ForEach(func(_, v ojson.Result) bool {
			if v.IsArray() || v.IsObject() {
				walk(v)
			}
			return true
		})
	}
	walk(root)
	if count != 4 {
		t.Fatalf("media blocks = %d, want 4", count)
	}
}
