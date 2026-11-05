package helps

import (
	"context"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/billing"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

func TestClaudeRawStreamUsageUsesFinalCumulativeCounts(t *testing.T) {
	r := NewUsageReporter(usage.WithRawUsageCapture(context.Background()), "claude", "fixture", nil)
	r.ObserveResponseModel([]byte(`{"type":"message_start","message":{"model":"fixture","content":[{"text":"PRIVATE"}],"usage":{"input_tokens":50,"output_tokens":0,"cache_read_input_tokens":500,"cache_creation_input_tokens":5}}}`))
	if _, format := r.rawUsageSnapshot(); format != "claude-pending" {
		t.Fatal("provisional start usage was treated as a final bill")
	}
	for _, count := range []string{"10", "30"} {
		r.ObserveResponseModel([]byte(`{"type":"message_delta","usage":{"output_tokens":` + count + `}}`))
	}
	record := r.buildRecord(usage.Detail{InputTokens: 999999, OutputTokens: 888888}, false)
	if record.RawUsageFormat != "claude" || strings.Contains(string(record.RawUsage), "PRIVATE") {
		t.Fatal("missing original usage or content retained")
	}
	raw, err := billing.ClaudeUsage(record.RawUsage)
	if err != nil {
		t.Fatal(err)
	}
	row, err := billing.FromRawUsage(raw, billing.Row{})
	if err != nil || row.Input != 555 || row.Output != 30 || row.Cached != 500 || row.Write != 5 {
		t.Fatalf("cumulative usage incorrectly merged: %+v / %v", row, err)
	}
}

func TestClaudeNonStreamUsageAndMissingStart(t *testing.T) {
	for _, payload := range []string{`{"type":"message","model":"fixture","usage":{"input_tokens":5,"output_tokens":7}}`, `{"type":"message_delta","usage":{"output_tokens":7}}`} {
		r := NewUsageReporter(usage.WithRawUsageCapture(context.Background()), "claude", "fixture", nil)
		r.ObserveResponseModel([]byte(payload))
		raw, format := r.rawUsageSnapshot()
		if format != "claude" {
			t.Fatal("native usage not captured")
		}
		_, err := billing.ClaudeUsage(raw)
		if strings.Contains(payload, "message_delta") != (err != nil) {
			t.Fatal("missing native input was invented")
		}
	}
}
