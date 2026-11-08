package helps

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/MoeForge/moego/ojson"
)

func TestCodexHistoryCallIDsStayPaired(t *testing.T) {
	for _, kind := range []string{"function_call", "custom_tool_call", "computer_call", "shell_call", "apply_patch_call", "tool_search_call"} {
		for _, id := range []string{strings.Repeat("a", 63), strings.Repeat("a", 64), strings.Repeat("a", 65), "call_" + strings.Repeat("d", 73), "foreign:call#1"} {
			t.Run(fmt.Sprintf("%s/%d", kind, len(id)), func(t *testing.T) {
				body, _ := json.Marshal(map[string]any{"input": []any{
					map[string]any{"type": kind, "call_id": id, "arguments": "{\"call_id\":\"keep:#unchanged\"}"},
					map[string]any{"type": kind + "_output", "call_id": id, "output": "keep:#unchanged"},
					map[string]any{"role": "user", "content": id},
				}})
				out := SanitizeCodexInputItemIDs(body)
				callID := ojson.GetBytes(out, "input.0.call_id").String()
				if !isPortableToolCallID(callID) || len(callID) > 64 || callID != ojson.GetBytes(out, "input.1.call_id").String() {
					t.Fatalf("tool call/result IDs are invalid or unmatched: %s", out)
				}
				if len(id) <= 64 && isPortableToolCallID(id) && callID != id {
					t.Fatal("existing legal call ID changed")
				}
				for _, path := range []string{"input.0.arguments", "input.1.output", "input.2"} {
					if ojson.GetBytes(body, path).Raw != ojson.GetBytes(out, path).Raw {
						t.Fatalf("content changed at %s", path)
					}
				}
				if !bytes.Equal(out, SanitizeCodexInputItemIDs(out)) || !bytes.Equal(out, SanitizeCodexInputItemIDs(body)) {
					t.Fatal("call ID mapping changed on replay")
				}
			})
		}
	}
}

func TestCodexHistoryCallIDsAvoidNativeCollisions(t *testing.T) {
	long := "call_" + strings.Repeat("d", 73)
	native := shortenCodexInputItemID(long)
	other := "call_" + strings.Repeat("d", 72) + "e"
	body := []byte(fmt.Sprintf(`{"input":[{"type":"function_call_output","call_id":%q,"output":"first"},{"type":"function_call","call_id":%q,"name":"probe","arguments":"{}"},{"type":"function_call","call_id":%q,"name":"probe","arguments":"{}"},{"type":"function_call","call_id":%q,"name":"probe","arguments":"{}"}]}`, long, native, long, other))
	out := SanitizeCodexInputItemIDs(body)
	first := ojson.GetBytes(out, "input.0.call_id").String()
	if first != ojson.GetBytes(out, "input.2.call_id").String() || first == native || first == ojson.GetBytes(out, "input.3.call_id").String() {
		t.Fatal("call ID mapping collided or depended on call/result ordering")
	}
	if ojson.GetBytes(out, "input.1.call_id").String() != native {
		t.Fatal("existing native call ID was changed")
	}
}
