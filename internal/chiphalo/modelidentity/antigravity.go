// Package modelidentity contains reviewed differences between callable routes
// and upstream-reported identities. It never infers model identity from text.
package modelidentity

import "strings"

// Verified against official Antigravity SSE response.modelVersion on 2026-10-16.
// The reported version is a public alias, not assumed to be callable upstream.
const (
	AntigravityFlashRoute    = "gemini-3.8-flash-high"
	AntigravityFlashReported = "gemini-3.8-flash-n"
)

// Expected returns the expected reported identity for a known Antigravity route.
// Preserve account namespaces; other providers and arbitrary suffixes are never
// treated as equivalent. Unknown responses must still trigger mismatch checks.
func Expected(model string) string {
	if model == "antigravity/"+AntigravityFlashRoute {
		return "antigravity/" + AntigravityFlashReported
	}
	return model
}

func Mismatch(requested, reported string) bool {
	name := func(value string) string {
		value = strings.TrimSpace(value)
		return value[strings.LastIndexByte(value, '/')+1:]
	}
	return name(reported) != "" && name(requested) != "" && name(Expected(requested)) != name(reported)
}

// Display uses the captured response identity for Antigravity Gemini calls,
// without replacing the original request used for routing, grouping or billing.
func Display(requested, reported string) string {
	if strings.HasPrefix(requested, "antigravity/gemini-") && strings.HasPrefix(strings.TrimSpace(reported), "gemini-") {
		return "antigravity/" + strings.TrimSpace(reported)
	}
	return requested
}
