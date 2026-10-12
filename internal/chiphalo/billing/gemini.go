package billing

import (
	"encoding/json"
	"fmt"
	"math"
)

// GeminiUsage adapts original Gemini usageMetadata into the deployed accounting
// schema. It never uses SDK usage.Detail or changes existing rates/formulas.
// Gemini reports thoughts separately from candidates, unlike OpenAI output.
func GeminiUsage(raw []byte) ([]byte, error) {
	value, err := decodeObject(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid Gemini usage metadata")
	}
	counts := map[string]int64{}
	known := false
	for _, field := range []string{"promptTokenCount", "candidatesTokenCount", "thoughtsTokenCount", "cachedContentTokenCount", "toolUsePromptTokenCount", "totalTokenCount"} {
		v, exists := value[field]
		if !exists {
			continue
		}
		if field == "promptTokenCount" || field == "candidatesTokenCount" || field == "thoughtsTokenCount" {
			known = true
		}
		number, ok := v.(json.Number)
		if !ok {
			return nil, fmt.Errorf("invalid Gemini token count")
		}
		count, err := number.Int64()
		if err != nil || count < 0 {
			return nil, fmt.Errorf("invalid Gemini token count")
		}
		counts[field] = count
	}
	if !known || counts["cachedContentTokenCount"] > counts["promptTokenCount"] {
		return nil, fmt.Errorf("incomplete or inconsistent Gemini usage metadata")
	}
	input, tool := counts["promptTokenCount"], counts["toolUsePromptTokenCount"]
	output, thoughts := counts["candidatesTokenCount"], counts["thoughtsTokenCount"]
	if input > math.MaxInt64-tool || output > math.MaxInt64-thoughts {
		return nil, fmt.Errorf("Gemini token count overflow")
	}
	return json.Marshal(map[string]any{
		"input_tokens":          input + tool,
		"input_tokens_details":  map[string]int64{"cached_tokens": counts["cachedContentTokenCount"]},
		"output_tokens":         output + thoughts,
		"output_tokens_details": map[string]int64{"reasoning_tokens": thoughts},
	})
}
