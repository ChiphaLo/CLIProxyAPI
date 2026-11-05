package executor

import (
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestCodexWebsocketRequestAcceptsDevinSearchHistory(t *testing.T) {
	body := []byte(`{"model":"gpt-6.1-sol","input":[{"type":"web_search_call","id":"ws_web_search:1#8db94bd3f9624d7f99283c23186af7cf","status":"completed","action":{"type":"search","query":"fixture"}},{"type":"web_search_call","id":"ws_native","status":"completed","action":{"type":"search","query":"unchanged"}}]}`)
	first := buildCodexWebsocketRequestBody(body)
	second := buildCodexWebsocketRequestBody(body)
	id := gjson.GetBytes(first, "input.0.id").String()
	if id == "" || strings.ContainsAny(id, ":#") || len(id) > 64 || string(first) != string(second) {
		t.Fatalf("invalid or unstable websocket history ID: %q", id)
	}
	if gjson.GetBytes(first, "input.0.action").Raw != gjson.GetBytes(body, "input.0.action").Raw || gjson.GetBytes(first, "input.1").Raw != gjson.GetBytes(body, "input.1").Raw {
		t.Fatal("websocket history content or native ID changed")
	}
}
