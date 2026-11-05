package helps

import (
	"encoding/json"

	"github.com/tidwall/gjson"
)

// Messages SSE usage is split between message_start and cumulative
// message_delta updates. Retain original billing fields, not SDK Detail.
func (r *UsageReporter) observeClaudeRawUsage(root gjson.Result) bool {
	kind := root.Get("type").String()
	if kind != "message" && kind != "message_start" && kind != "message_delta" {
		return false
	}
	value := lastJSONProperty(root, "usage")
	if kind == "message_start" {
		value = lastJSONProperty(lastJSONProperty(root, "message"), "usage")
	}
	if !value.IsObject() {
		return true
	}
	r.rawUsageMu.Lock()
	defer r.rawUsageMu.Unlock()
	fields := map[string]json.RawMessage{}
	if kind == "message_delta" && (r.rawUsageFormat == "claude" || r.rawUsageFormat == "claude-pending") {
		_ = json.Unmarshal(r.rawUsage, &fields)
	}
	for _, name := range []string{"input_tokens", "output_tokens", "cache_read_input_tokens", "cache_creation_input_tokens"} {
		v := lastJSONProperty(value, name)
		if !v.Exists() {
			continue
		}
		if len(v.Raw) > maxRawUsageBytes {
			r.rawUsage = nil
			r.rawUsageFormat = "claude-invalid"
			return true
		}
		fields[name] = json.RawMessage(v.Raw)
	}
	raw, err := json.Marshal(fields)
	if err != nil || len(raw) > maxRawUsageBytes {
		r.rawUsage = nil
		r.rawUsageFormat = "claude-invalid"
		return true
	}
	format := "claude"
	if kind == "message_start" || kind == "message_delta" && r.rawUsageFormat != "claude" && !lastJSONProperty(value, "output_tokens").Exists() {
		format = "claude-pending"
	}
	r.rawUsage = raw
	r.rawUsageFormat = format
	return true
}
