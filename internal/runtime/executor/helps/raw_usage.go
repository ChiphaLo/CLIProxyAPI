package helps

import (
	"bytes"
	"encoding/json"

	"github.com/tidwall/gjson"
)

const maxRawUsageBytes = 64 * 1024

// observeRawUsage retains only the original usage object, never model output,
// request content, credentials, or the SDK's canonical token breakdown.
func (r *UsageReporter) observeRawUsage(payload []byte) {
	if r == nil || !r.captureRawUsage {
		return
	}
	payload = bytes.TrimSpace(payload)
	if bytes.HasPrefix(payload, []byte("data:")) {
		payload = bytes.TrimSpace(bytes.TrimPrefix(payload, []byte("data:")))
	}
	if !gjson.ValidBytes(payload) {
		return
	}
	root := gjson.ParseBytes(payload)
	value := lastJSONProperty(lastJSONProperty(root, "response"), "usage")
	format := "responses"
	gemini := lastJSONProperty(lastJSONProperty(root, "response"), "usageMetadata")
	if !gemini.IsObject() {
		gemini = lastJSONProperty(root, "usageMetadata")
	}
	if !gemini.IsObject() {
		gemini = lastJSONProperty(lastJSONProperty(root, "response"), "usage_metadata")
	}
	if !gemini.IsObject() {
		gemini = lastJSONProperty(root, "usage_metadata")
	}
	if gemini.IsObject() {
		value, format = gemini, "gemini"
	}
	if !value.IsObject() {
		value = lastJSONProperty(root, "usage")
		if !value.IsObject() {
			return
		}
		switch {
		case value.Get("prompt_tokens").Exists() || value.Get("completion_tokens").Exists() || root.Get("object").String() == "chat.completion" || root.Get("object").String() == "chat.completion.chunk":
			format = "chat"
		case root.Get("object").String() == "response" || root.Get("object").String() == "response.compaction":
			format = "responses"
		case root.Get("type").String() == "response.completed" || root.Get("type").String() == "response.done" || root.Get("type").String() == "response.incomplete":
			format = "responses"
		default:
			// Do not silently interpret unknown native usage as Responses usage.
			return
		}
	}
	// Empty/metadata-only usage is not an authoritative zero-token bill.
	if (format == "responses" || format == "chat") && !hasOpenAIStyleUsageTokenFields(value) {
		return
	}
	raw := boundedBillingUsage(value, format)
	if len(raw) == 0 {
		return
	}
	r.rawUsageMu.Lock()
	r.rawUsage = append(r.rawUsage[:0], raw...)
	r.rawUsageFormat = format
	r.rawUsageMu.Unlock()
}

// OpenAI usage.attribution can exceed the entire capture bound on long Codex
// histories. Preserve raw billing values, not SDK-normalized counts, while
// omitting the per-message attribution that CPA's billing never consumes.
func boundedBillingUsage(value gjson.Result, format string) []byte {
	if len(value.Raw) <= maxRawUsageBytes {
		return []byte(value.Raw)
	}
	if format != "responses" && format != "chat" {
		return nil
	}
	out := map[string]json.RawMessage{}
	for _, key := range []string{"input_tokens", "output_tokens", "prompt_tokens", "completion_tokens", "total_tokens", "prompt_cache_hit_tokens", "prompt_cache_miss_tokens"} {
		field := lastJSONProperty(value, key)
		if !field.Exists() {
			continue
		}
		if len(field.Raw) > maxRawUsageBytes {
			return nil
		}
		out[key] = json.RawMessage(field.Raw)
	}
	for _, key := range []string{"input_tokens_details", "output_tokens_details", "prompt_tokens_details", "completion_tokens_details"} {
		field := lastJSONProperty(value, key)
		if !field.Exists() {
			continue
		}
		if !field.IsObject() {
			if len(field.Raw) > maxRawUsageBytes {
				return nil
			}
			out[key] = json.RawMessage(field.Raw)
			continue
		}
		details := map[string]json.RawMessage{}
		for _, name := range []string{"cached_tokens", "cache_write_tokens", "cache_creation_tokens", "reasoning_tokens", "accepted_prediction_tokens", "rejected_prediction_tokens"} {
			count := lastJSONProperty(field, name)
			if count.Exists() {
				if len(count.Raw) > maxRawUsageBytes {
					return nil
				}
				details[name] = json.RawMessage(count.Raw)
			}
		}
		encoded, err := json.Marshal(details)
		if err != nil || len(encoded) > maxRawUsageBytes {
			return nil
		}
		out[key] = encoded
	}
	raw, err := json.Marshal(out)
	if err != nil || len(raw) > maxRawUsageBytes {
		return nil
	}
	return raw
}

// The deployed json.loads decoder used last-key-wins. Avoid changing receipt
// values merely because the upstream GJSON path lookup picks the first key.
func lastJSONProperty(object gjson.Result, name string) gjson.Result {
	var result gjson.Result
	if object.IsObject() {
		object.ForEach(func(key, value gjson.Result) bool {
			if key.String() == name {
				result = value
			}
			return true
		})
	}
	return result
}

func (r *UsageReporter) rawUsageSnapshot() ([]byte, string) {
	if r == nil || !r.captureRawUsage {
		return nil, ""
	}
	r.rawUsageMu.RLock()
	defer r.rawUsageMu.RUnlock()
	return append([]byte(nil), r.rawUsage...), r.rawUsageFormat
}
