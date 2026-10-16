package modelidentity

import "testing"

func TestAntigravityReportedIdentityIsNarrowAndPreservesRequests(t *testing.T) {
	for _, tc := range []struct {
		requested, reported, displayed string
		mismatch                       bool
	}{
		{"antigravity/gemini-3.8-flash-high", "gemini-3.8-flash-n", "antigravity/gemini-3.8-flash-n", false},
		{"antigravity/gemini-3.8-flash-n", "gemini-3.8-flash-n", "antigravity/gemini-3.8-flash-n", false},
		{"antigravity/gemini-3.8-flash-high", "gemini-other", "antigravity/gemini-other", true},
		{"merchant/gemini-3.8-flash-high", "gemini-3.8-flash-n", "merchant/gemini-3.8-flash-high", true},
		{"antigravity/gemini-3.8-flash-high", "", "antigravity/gemini-3.8-flash-high", false},
		{"antigravity/gemini-3-flash", "gemini-3-flash", "antigravity/gemini-3-flash", false},
		{"gpt-6.1-sol", "prefix/gpt-6.1-sol", "gpt-6.1-sol", false},
		{"gpt-6.1-sol", "gpt-6-luna", "gpt-6.1-sol", true},
	} {
		if Mismatch(tc.requested, tc.reported) != tc.mismatch || Display(tc.requested, tc.reported) != tc.displayed {
			t.Fatalf("wrong identity projection for %+v", tc)
		}
	}
}
