package helps

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestCodexAcceptsDevinWebSearchHistoryIDs(t *testing.T) {
	const invalid = "ws_web_search:1#8db94bd3f9624d7f99283c23186af7cf"
	body := []byte(fmt.Sprintf(`{"model":"gpt-6.1-sol","input":[{"type":"item_reference","id":%q},{"type":"web_search_call","id":%q,"status":"completed","action":{"type":"search","query":"fixture"},"results":[{"url":"https://example.com","title":"fixture"}]},{"type":"function_call","id":"fc_native","call_id":"call_native","name":"probe","arguments":"{}"},{"type":"function_call_output","call_id":"call_native","output":"keep : # verbatim"},{"role":"user","content":"keep %s verbatim"}]}`, invalid, invalid, invalid))
	out := SanitizeCodexInputItemIDs(body)
	id := gjson.GetBytes(out, "input.1.id").String()
	if !isPortableToolCallID(id) || len(id) > 64 || id == invalid {
		t.Fatalf("invalid search ID after normalization: %s", id)
	}
	if gjson.GetBytes(out, "input.0.id").String() != id {
		t.Fatal("item reference no longer points to search history")
	}
	if gjson.GetBytes(out, "input.1.action.query").String() != "fixture" || gjson.GetBytes(out, "input.1.action.sources.0.url").String() != "https://example.com" || gjson.GetBytes(out, "input.1.results").Exists() {
		t.Fatal("search action was not projected into its native schema")
	}
	if !strings.Contains(gjson.GetBytes(out, "input.2.content.0.text").String(), gjson.GetBytes(body, "input.1.results").Raw) {
		t.Fatal("search result content was lost")
	}
	for i := 2; i <= 4; i++ {
		if gjson.GetBytes(out, fmt.Sprintf("input.%d", i+1)).Raw != gjson.GetBytes(body, fmt.Sprintf("input.%d", i)).Raw {
			t.Fatalf("history content or tool relationship changed at input %d", i)
		}
	}
	if !bytes.Equal(SanitizeCodexInputItemIDs(out), out) {
		t.Fatal("history ID normalization is not idempotent")
	}
	if !bytes.Equal(SanitizeCodexInputItemIDs(body), out) {
		t.Fatal("history IDs changed between retries")
	}
}

func TestCodexHistoryIDsHavePortableCharactersAcrossItemTypes(t *testing.T) {
	for _, itemType := range []string{"message", "reasoning", "function_call", "function_call_output", "custom_tool_call", "custom_tool_call_output", "web_search_call"} {
		body := []byte(fmt.Sprintf(`{"input":[{"type":%q,"id":"foreign:one#two/three"}]}`, itemType))
		out := SanitizeCodexInputItemIDs(body)
		if id := gjson.GetBytes(out, "input.0.id").String(); !isPortableToolCallID(id) || len(id) > 64 {
			t.Fatalf("%s has invalid ID %q", itemType, id)
		}
	}
}

func TestCodexNativeSearchHistoryRemainsUnchanged(t *testing.T) {
	body := []byte(`{"input":[{"type":"web_search_call","id":"ws_native","status":"completed","action":{"type":"search","query":"fixture","sources":[{"type":"url","url":"https://example.com"}]},"results":[{"type":"text_result","title":"fixture","url":"https://example.com"}]}]}`)
	if out := SanitizeCodexInputItemIDs(body); !bytes.Equal(body, out) {
		t.Fatal("native search history changed")
	}
}
