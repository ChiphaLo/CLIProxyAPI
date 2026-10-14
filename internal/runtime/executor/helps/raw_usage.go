package helps

import (
	"bytes"

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
	if len(value.Raw) > maxRawUsageBytes {
		return
	}
	r.rawUsageMu.Lock()
	r.rawUsage = append(r.rawUsage[:0], value.Raw...)
	r.rawUsageFormat = format
	r.rawUsageMu.Unlock()
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
