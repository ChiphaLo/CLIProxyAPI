package billing

import (
	"encoding/json"
	"os"
	"testing"
)

func TestPythonCostParity(t *testing.T) {
	data, err := os.ReadFile("testdata/python-parity.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Row    json.RawMessage   `json:"row"`
		Prices map[string]*Rates `json:"prices"`
		Want   float64           `json:"cost"`
	}
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	for i, fixture := range cases {
		row, err := DecodeRow(fixture.Row)
		if err != nil {
			t.Fatalf("case %d: %v", i, err)
		}
		if got := Cost(row, fixture.Prices); got != fixture.Want {
			t.Fatalf("case %d: Go %.12f != Python %.12f", i, got, fixture.Want)
		}
	}
}

func TestPythonChatUsageParity(t *testing.T) {
	data, err := os.ReadFile("testdata/usage-parity.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Raw  json.RawMessage `json:"raw"`
		Want any             `json:"mapped"`
	}
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	for i, fixture := range cases {
		got, err := ChatUsage(fixture.Raw)
		if err != nil {
			t.Fatalf("case %d: %v", i, err)
		}
		var object any
		if got != nil {
			if err := json.Unmarshal(got, &object); err != nil {
				t.Fatal(err)
			}
		}
		encoded, _ := json.Marshal(object)
		want, _ := json.Marshal(fixture.Want)
		if string(encoded) != string(want) {
			t.Fatalf("case %d: %s != %s", i, encoded, want)
		}
	}
}

func TestRawOutputDoesNotAddReasoning(t *testing.T) {
	row, err := FromRawUsage([]byte(`{"input_tokens":10000,"output_tokens":8000,"input_tokens_details":{"cached_tokens":7000,"cache_write_tokens":300},"output_tokens_details":{"reasoning_tokens":6500}}`), Row{TS: 100, DurationMS: 2000})
	if err != nil {
		t.Fatal(err)
	}
	if row.Input != 10000 || row.Output != 8000 || row.Cached != 7000 || row.Write != 300 || row.OutputTPS != 4000 {
		t.Fatalf("raw billing contract changed: %+v", row)
	}
}

func TestCostPreservesCacheWriteFormula(t *testing.T) {
	prices := map[string]*Rates{"fixture": {Input: 2, Cached: 0.2, Write: 2.5, Output: 10}}
	// This intentionally preserves input-cached, even when cache write exists.
	if got := Cost(Row{Model: "fixture", Input: 1000, Cached: 500, Write: 100, Output: 200}, prices); got != 0.00335 {
		t.Fatalf("existing formula changed: %.8f", got)
	}
}

func TestLoadPricesRejectsInvalidEntries(t *testing.T) {
	rates, err := LoadPrices([]byte(`{"prices":{"ok":{"input":"2","cached":0.2,"write":2.5,"output":10},"invalid":{"input":true},"negative":{"output":-1},"infinite":{"input":"Inf"},"too_high":{"input":1000001}}}`))
	if err != nil || len(rates) != 1 || rates["ok"].Input != 2 {
		t.Fatalf("price validation changed: %+v %v", rates, err)
	}
}

func TestMalformedCountDoesNotCreateReceipt(t *testing.T) {
	for _, raw := range []string{`null`, `{} {}`, `{} garbage`, `{"input_tokens":{}}`, `{"input_tokens":[]}`, `{"input_tokens":"garbage"}`, `{"input_tokens":1e30}`} {
		if _, err := FromRawUsage([]byte(raw), Row{}); err == nil {
			t.Fatalf("accepted invalid usage: %s", raw)
		}
	}
}
