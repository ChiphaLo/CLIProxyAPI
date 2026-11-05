package billing

import "testing"

func TestClaudeOriginalUsageAdaptation(t *testing.T) {
	raw, err := ClaudeUsage([]byte(`{"input_tokens":50,"output_tokens":30,"cache_read_input_tokens":500,"cache_creation_input_tokens":5}`))
	if err != nil {
		t.Fatal(err)
	}
	row, err := FromRawUsage(raw, Row{Model: "steal2/fixture"})
	if err != nil || row.Input != 555 || row.Cached != 500 || row.Write != 5 || row.Output != 30 {
		t.Fatalf("native usage adaptation: %+v / %v", row, err)
	}
	prices := map[string]*Rates{"fixture": {Input: 2, Cached: .2, Write: 2.5, Output: 10}}
	if Cost(row, prices) != Cost(Row{Model: "steal2/fixture", Input: 555, Cached: 500, Write: 5, Output: 30}, prices) {
		t.Fatal("legacy price formula changed")
	}
}

func TestClaudeUsageRejectsMissingOrInvalidCounts(t *testing.T) {
	for _, raw := range []string{`{}`, `null`, `{"input_tokens":1}`, `{"output_tokens":1}`, `{"input_tokens":-1,"output_tokens":2}`, `{"input_tokens":1.5,"output_tokens":2}`, `{"input_tokens":"1","output_tokens":2}`, `{"input_tokens":true,"output_tokens":2}`, `{"input_tokens":1,"output_tokens":2,"cache_read_input_tokens":null}`, `{"input_tokens":9223372036854775807,"output_tokens":1,"cache_creation_input_tokens":1}`} {
		if _, err := ClaudeUsage([]byte(raw)); err == nil {
			t.Fatalf("accepted invalid usage %s", raw)
		}
	}
	if _, err := ClaudeUsage([]byte(`{"input_tokens":0,"output_tokens":0}`)); err != nil {
		t.Fatal(err)
	}
}
