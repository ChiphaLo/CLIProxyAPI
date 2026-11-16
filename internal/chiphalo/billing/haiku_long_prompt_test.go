package billing

import "testing"

func TestHaikuLongPromptCost(t *testing.T) {
	prices := map[string]*Rates{"claude-haiku-5-5": {Input: 1, Cached: .1, Write: 2, Output: 5, SeparateCacheWrite: true}}
	for _, tc := range []struct {
		name string
		row  Row
		want float64
	}{
		{"below", Row{Model: "claude/claude-haiku-5-5", Input: 99999, Cached: 90000, Write: 9999, Output: 1000}, .033998},
		{"exact threshold", Row{Model: "claude/claude-haiku-5-5", Input: 100000, Cached: 90000, Write: 10000, Output: 1000}, .034},
		{"cached prompt crosses threshold", Row{Model: "claude/claude-haiku-5-5", Input: 100001, Cached: 90000, Write: 10000, Output: 1000}, .170005},
		{"cache read alone crosses threshold", Row{Model: "claude-haiku-5-5", Input: 100001, Cached: 100001}, .0500005},
		{"cache creation alone crosses threshold", Row{Model: "claude-haiku-5-5", Input: 100001, Write: 100001}, 1.00001},
		{"output is not prompt", Row{Model: "claude-haiku-5-5", Input: 100000, Output: 1000000}, 5.1},
		{"output uses fivefold rate too", Row{Model: "claude-haiku-5-5", Input: 100001, Output: 1000000}, 25.500005},
		{"negative input", Row{Model: "claude-haiku-5-5", Input: -1, Output: 1000000}, 5},
		{"unpriced", Row{Model: "other/claude-haiku-unknown", Input: 200000}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := tc.row
			got := Cost(tc.row, prices)
			if got != tc.want {
				t.Fatalf("cost %.8f, want %.8f", got, tc.want)
			}
			if tc.row != before {
				t.Fatal("token record mutated")
			}
		})
	}
	if prices["claude-haiku-5-5"].Input != 1 || prices["claude-haiku-5-5"].Output != 5 {
		t.Fatal("base prices mutated")
	}
}

func TestHaikuTariffNamesAndOtherModels(t *testing.T) {
	for _, tc := range []struct {
		model string
		want  float64
	}{
		{"claude-haiku-5-5", 1}, {"claude/claude-haiku-5-5", 1}, {"other/claude-haiku-5.5", 1}, {"CLAUDE/CLAUDE-HAIKU-5-5", 1}, {"claude-3-5-haiku-20241022", 1},
		{"claude/claude-opus-5-5", .2}, {"claude/claude-sonnet-5-5", .2}, {"gpt-6-luna", .2}, {"deepseek-v4.1-flash", .2}, {"not-haiku", .2}, {"claude-haikux-5-5", .2},
	} {
		t.Run(tc.model, func(t *testing.T) {
			if got := Cost(Row{Model: tc.model, Input: 200000}, map[string]*Rates{tc.model: {Input: 1}}); got != tc.want {
				t.Fatalf("cost %v, want %v", got, tc.want)
			}
		})
	}
}

func TestHaikuLongPromptRoundsOnceAndKeepsLegacyWriteMode(t *testing.T) {
	row := Row{Model: "claude-haiku-5-5", Input: 100001, Output: 1}
	if got := Cost(row, map[string]*Rates{row.Model: {Output: .0057}}); got != .00000003 {
		t.Fatalf("multiplier must precede rounding: %.8f", got)
	}
	row.Cached = 90000
	row.Write = 10000
	row.Output = 1000
	if got := Cost(row, map[string]*Rates{row.Model: {Input: 1, Cached: .1, Write: 2, Output: 5}}); got != .220005 {
		t.Fatalf("legacy write accounting changed: %.8f", got)
	}
}
