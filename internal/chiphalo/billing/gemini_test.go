package billing

import (
	"math"
	"testing"
)

func TestGeminiRawUsageAccounting(t *testing.T) {
	raw, err := GeminiUsage([]byte(`{"promptTokenCount":1000,"cachedContentTokenCount":700,"toolUsePromptTokenCount":20,"candidatesTokenCount":60,"thoughtsTokenCount":140,"totalTokenCount":1200}`))
	if err != nil {
		t.Fatal(err)
	}
	row, err := FromRawUsage(raw, Row{Model: "google-a/gemini-fixture"})
	if err != nil {
		t.Fatal(err)
	}
	if row.Input != 1020 || row.Output != 200 || row.Cached != 700 || row.Write != 0 {
		t.Fatalf("bad native adaptation: %+v", row)
	}
	cost := Cost(row, map[string]*Rates{"gemini-fixture": {Input: 2, Cached: .2, Output: 10}})
	if math.Abs(cost-.00278) > 1e-12 {
		t.Fatalf("legacy pricing changed: %.12f", cost)
	}
}

func TestGeminiUsageRejectsInvalidRatherThanGuessing(t *testing.T) {
	for _, raw := range []string{`{}`, `{"totalTokenCount":10}`, `null`, `{"promptTokenCount":-1}`, `{"promptTokenCount":true}`, `{"promptTokenCount":"1"}`, `{"promptTokenCount":1.5}`, `{"promptTokenCount":1,"cachedContentTokenCount":2}`, `{"promptTokenCount":9223372036854775807,"toolUsePromptTokenCount":1}`, `{"candidatesTokenCount":9223372036854775807,"thoughtsTokenCount":1}`} {
		if _, err := GeminiUsage([]byte(raw)); err == nil {
			t.Fatalf("accepted invalid usage %s", raw)
		}
	}
	if _, err := GeminiUsage([]byte(`{"promptTokenCount":0,"candidatesTokenCount":0}`)); err != nil {
		t.Fatal("explicit zero usage is valid")
	}
}
