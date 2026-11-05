package billing

import (
	"encoding/json"
	"fmt"
	"math"
)

// ClaudeUsage adapts original Messages usage to the existing ledger schema.
// Input includes the disjoint uncached, cache-read and cache-creation buckets.
// Existing prices and the legacy cache-write cost formula remain unchanged.
func ClaudeUsage(raw []byte) ([]byte, error) {
	value, err := decodeObject(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid Claude usage")
	}
	counts := map[string]int64{}
	for _, field := range []string{"input_tokens", "output_tokens", "cache_read_input_tokens", "cache_creation_input_tokens"} {
		v, exists := value[field]
		if !exists {
			if field == "input_tokens" || field == "output_tokens" {
				return nil, fmt.Errorf("incomplete Claude usage")
			}
			continue
		}
		number, ok := v.(json.Number)
		if !ok {
			return nil, fmt.Errorf("invalid Claude token count")
		}
		n, err := number.Int64()
		if err != nil || n < 0 {
			return nil, fmt.Errorf("invalid Claude token count")
		}
		counts[field] = n
	}
	input, cached, write := counts["input_tokens"], counts["cache_read_input_tokens"], counts["cache_creation_input_tokens"]
	if input > math.MaxInt64-cached || input+cached > math.MaxInt64-write {
		return nil, fmt.Errorf("Claude token count overflow")
	}
	return json.Marshal(map[string]any{
		"input_tokens":         input + cached + write,
		"input_tokens_details": map[string]int64{"cached_tokens": cached, "cache_write_tokens": write},
		"output_tokens":        counts["output_tokens"],
	})
}
