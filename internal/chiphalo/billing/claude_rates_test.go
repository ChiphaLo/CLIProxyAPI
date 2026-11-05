package billing

import "testing"

func TestClaudeOneHourRatesDoNotDoubleChargeWrites(t *testing.T) {
	for _, tt := range []struct {
		name                               string
		input, write, cached, output, want float64
	}{
		{"claude-fable-5-1", 10, 20, .25, 50, 80.25},
		{"claude-opus-5-5", 4, 8, .20, 20, 32.20},
		{"claude-sonnet-5-5", 2, 4, .10, 10, 16.10},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rates := map[string]*Rates{tt.name: {Input: tt.input, Write: tt.write, Cached: tt.cached, Output: tt.output, SeparateCacheWrite: true}}
			row := Row{Model: "claude/" + tt.name, Input: 3000000, Cached: 1000000, Write: 1000000, Output: 1000000}
			if got := Cost(row, rates); got != tt.want {
				t.Fatalf("cost %.8f, want %.8f", got, tt.want)
			}
			rates[tt.name].SeparateCacheWrite = false
			if got := Cost(row, rates); got != RoundDecimal(tt.want+tt.input, 8) {
				t.Fatal("legacy formula changed")
			}
		})
	}
}

func TestClaudePriceModeRoundTrip(t *testing.T) {
	p, err := LoadPrices([]byte(`{"prices":{"claude/a":{"input":2,"write":4,"cached":0.1,"output":10,"separate_cache_write":true}}}`))
	if err != nil || !p["claude/a"].SeparateCacheWrite {
		t.Fatal("new price mode discarded")
	}
}
