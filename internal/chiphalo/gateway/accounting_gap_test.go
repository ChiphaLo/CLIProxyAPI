package gateway

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

func TestAuditedUsageGapsDoNotInventReceiptsOrPoisonStorageHealth(t *testing.T) {
	dir := t.TempDir()
	ledger, auditPath := filepath.Join(dir, "usage.jsonl"), filepath.Join(dir, "audit.jsonl")
	history := "{\"historical\":true}\n"
	if err := os.WriteFile(auditPath, []byte(history), 0640); err != nil {
		t.Fatal(err)
	}
	audit, err := OpenAuditSink(auditPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = audit.Close() }()
	sink, err := OpenReceiptSink(ledger, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sink.Close() }()
	sink.EnableGapAudit(audit)
	stamp := time.Unix(1800000000, 0)
	ctx := context.WithValue(context.Background(), metadataKey{}, requestMetadata{Model: "codex2/fixture", KeyID: "admin", IP: "192.0.2.1", Path: "/v1/responses", Started: stamp})
	record := usage.Record{TraceID: "fixture-trace", Model: "fixture", Provider: "codex", RequestedAt: stamp, Latency: time.Second,
		Detail: usage.Detail{InputTokens: 999999, OutputTokens: 888888}, APIKey: "SECRET_KEY"}
	sink.HandleUsage(ctx, record)
	record.RawUsageFormat, record.RawUsage = "responses", []byte(`{"input_tokens":"invalid SECRET_RAW"}`)
	sink.HandleUsage(ctx, record)
	if sink.Err() != nil || audit.Err() != nil {
		t.Fatal("durably audited per-request gaps poisoned storage health")
	}
	status := sink.AccountingStatus()
	if !status.StorageHealthy || status.Complete || status.UnpricedRequests != 2 {
		t.Fatalf("accounting completeness was hidden: %+v", status)
	}
	data, _ := os.ReadFile(ledger)
	if len(data) != 0 {
		t.Fatal("unknown usage became a fabricated zero/SDK-count receipt")
	}
	record.RawUsage = []byte(`{"input_tokens":11,"output_tokens":7}`)
	sink.HandleUsage(ctx, record)
	if sink.Err() != nil {
		t.Fatal(sink.Err())
	}
	data, _ = os.ReadFile(ledger)
	if strings.Count(string(data), "\n") != 1 || strings.Contains(string(data), "999999") {
		t.Fatal("subsequent valid receipt lost or normalized SDK counts used")
	}
	raw, _ := os.ReadFile(auditPath)
	if !strings.HasPrefix(string(raw), history) || strings.Contains(string(raw), "SECRET_") {
		t.Fatal("audit history was modified or private payload was retained")
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 3 {
		t.Fatal("accounting gaps were not durably recorded")
	}
	for i, kind := range []string{"usage_unavailable", "usage_invalid"} {
		var row AuditRow
		if json.Unmarshal([]byte(lines[i+1]), &row) != nil || row.Event != "accounting_gap" || row.ErrorClass != kind || row.Model != "codex2/fixture" || row.Status != 200 || row.RequestID != "fixture-trace" || row.TS != stamp.Unix()+1 {
			t.Fatalf("wrong per-request audit metadata: %+v", row)
		}
	}
}

func TestAccountingGapAuditFailureStillFailsClosed(t *testing.T) {
	dir := t.TempDir()
	audit, err := OpenAuditSink(filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	sink, err := OpenReceiptSink(filepath.Join(dir, "usage.jsonl"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sink.Close() }()
	sink.EnableGapAudit(audit)
	if err := audit.Close(); err != nil {
		t.Fatal(err)
	}
	sink.HandleUsage(context.WithValue(context.Background(), metadataKey{}, requestMetadata{}), usage.Record{})
	if sink.Err() == nil || sink.AccountingStatus().StorageHealthy {
		t.Fatal("undurable accounting warning must not pass readiness")
	}
}
