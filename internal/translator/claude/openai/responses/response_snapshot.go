package responses

import (
	"github.com/MoeForge/moego/ojson"
	translatorcommon "github.com/router-for-me/CLIProxyAPI/v8/internal/translator/common"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
)

// Own only response-echo fields and tool identities. History text, arguments,
// file bytes and images are not needed to translate response events.
func snapshotResponsesRequestMetadata(request []byte) []byte {
	doc := ojson.NewDoc([]byte(`{}`))
	seen := make(map[string]bool)
	var input ojson.Result
	util.ParseGJSONBytesNoCopy(request).ForEach(func(key, value ojson.Result) bool {
		name := key.String()
		if seen[name] {
			return true
		}
		seen[name] = true
		switch name {
		case "input":
			input = value
		case "instructions", "max_output_tokens", "max_tool_calls", "model", "parallel_tool_calls",
			"previous_response_id", "prompt_cache_key", "reasoning", "safety_identifier", "service_tier",
			"store", "temperature", "text", "tool_choice", "tools", "top_logprobs", "top_p", "truncation", "user", "metadata":
			_ = doc.SetRaw(name, value.Raw)
		}
		return true
	})
	var retained [][]byte
	input.ForEach(func(_, item ojson.Result) bool {
		kind := item.Get("type").String()
		switch kind {
		case "function_call", "custom_tool_call", "additional_tools", "tool_search_output":
		default:
			return true
		}
		entry := ojson.NewDoc([]byte(`{}`))
		_ = entry.Set("type", kind)
		fields := []string{"name", "namespace"}
		if kind == "additional_tools" || kind == "tool_search_output" {
			fields = []string{"tools"}
		}
		for _, field := range fields {
			if value := item.Get(field); value.Exists() {
				_ = entry.SetRaw(field, value.Raw)
			}
		}
		retained = append(retained, entry.Bytes())
		return true
	})
	if len(retained) > 0 {
		_ = doc.SetRawBytes("input", translatorcommon.JoinRawArray(retained))
	}
	return doc.Bytes()
}
