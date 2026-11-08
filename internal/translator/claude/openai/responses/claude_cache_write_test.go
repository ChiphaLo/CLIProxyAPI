package responses

import (
	"context"
	"strings"
	"testing"

	"github.com/MoeForge/moego/ojson"
)

func TestClaudeResponsesRetainCacheWrites(t *testing.T) {
	lines := []string{
		`data: {"type":"message_start","message":{"id":"msg_cache","model":"claude-sonnet-5-5","usage":{"input_tokens":13,"output_tokens":1,"cache_read_input_tokens":200,"cache_creation_input_tokens":31}}}`,
		`data: {"type":"message_delta","usage":{"output_tokens":4}}`,
		`data: {"type":"message_stop"}`,
	}
	var param any
	var completed ojson.Result
	for _, line := range lines {
		for _, out := range ConvertClaudeResponseToOpenAIResponses(context.Background(), "claude-sonnet-5-5", nil, nil, []byte(line), &param) {
			for _, part := range strings.Split(string(out), "\n") {
				if strings.HasPrefix(part, "data:") {
					r := ojson.Parse(strings.TrimSpace(strings.TrimPrefix(part, "data:")))
					if r.Get("type").String() == "response.completed" {
						completed = r.Get("response")
					}
				}
			}
		}
	}
	if completed.Get("usage.input_tokens_details.cache_write_tokens").Int() != 31 || completed.Get("usage.input_tokens").Int() != 244 {
		t.Fatalf("stream cache writes lost: %s", completed.Raw)
	}
	out := ConvertClaudeResponseToOpenAIResponsesNonStream(context.Background(), "claude-sonnet-5-5", nil, nil, []byte(strings.Join(lines, "\n")), nil)
	if ojson.GetBytes(out, "usage.input_tokens_details.cache_write_tokens").Int() != 31 {
		t.Fatal("nonstream cache writes lost")
	}
}
