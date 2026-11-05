package gateway

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/billing"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

func TestClaudeReceiptKeepsRequestIdentityAndOriginalTokens(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.jsonl")
	sink, err := OpenReceiptSink(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sink.Close() }()
	stamp := time.Unix(1800000000, 0)
	ctx := context.WithValue(context.Background(), metadataKey{}, requestMetadata{Model: "steal2/fable-5-1", KeyID: "admin", Started: stamp, Path: "/v1/responses"})
	sink.HandleUsage(ctx, usage.Record{Provider: "claude", Model: "claude-fable-5-1", ResponseModel: "claude-fable-5-1", RequestedAt: stamp, Latency: time.Second, RawUsageFormat: "claude", RawUsage: []byte(`{"input_tokens":50,"output_tokens":30,"cache_read_input_tokens":500,"cache_creation_input_tokens":5}`), Detail: usage.Detail{InputTokens: 999999}})
	if sink.Err() != nil || !sink.AccountingStatus().Complete {
		t.Fatal("valid Messages usage did not reach the ledger")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var row billing.Row
	if json.Unmarshal(raw, &row) != nil || row.Model != "steal2/fable-5-1" || row.Input != 555 || row.Cached != 500 || row.Write != 5 || row.Output != 30 {
		t.Fatalf("invalid Claude receipt: %s", raw)
	}
}
