package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/appendlog"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/billing"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

// AccountBinding pins usage attribution to the selected auth ID. It must be
// supplied by the account registry; a client model prefix is not authoritative.
type AccountBinding struct {
	ID, Prefix string
}

type ReceiptSink struct {
	mu       sync.Mutex
	file     *appendlog.Log
	accounts map[string]AccountBinding
	lastErr  error
	now      func() time.Time
	gapAudit *AuditSink
	unpriced uint64
}

// AccountingStatus separates storage health from durable per-request gaps.
// The counter is process-local; individual gaps persist in the audit log.
type AccountingStatus struct {
	StorageHealthy   bool   `json:"storage_healthy"`
	Complete         bool   `json:"complete"`
	UnpricedRequests uint64 `json:"unpriced_requests_since_start"`
}

// EnableGapAudit must run before registering the sink with the usage manager.
// Without a durable audit destination, unavailable usage still fails closed.
func (s *ReceiptSink) EnableGapAudit(audit *AuditSink) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gapAudit = audit
}

func (s *ReceiptSink) accountingGap(ctx context.Context, record usage.Record, kind string) {
	if s.gapAudit == nil {
		s.lastErr = fmt.Errorf("CPA receipt %s: %s", record.RequestID, kind)
		return
	}
	if err := s.gapAudit.RecordAccountingGap(ctx, record, kind); err != nil {
		s.lastErr = fmt.Errorf("CPA accounting gap audit persistence failed: %w", err)
		return
	}
	s.unpriced++
}

func (s *ReceiptSink) AccountingStatus() AccountingStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	return AccountingStatus{StorageHealthy: s.lastErr == nil, Complete: s.lastErr == nil && s.unpriced == 0, UnpricedRequests: s.unpriced}
}

func OpenReceiptSink(path string, accounts map[string]AccountBinding) (*ReceiptSink, error) {
	file, err := appendlog.Open(path)
	if err != nil {
		return nil, err
	}
	bindings := make(map[string]AccountBinding, len(accounts))
	for id, binding := range accounts {
		bindings[id] = binding
	}
	return &ReceiptSink{file: file, accounts: bindings, now: time.Now}, nil
}

// HandleUsage deliberately ignores record.Detail. Only the original usage
// object goes through the deployed ChatUsage and FromRawUsage implementations.
func (s *ReceiptSink) HandleUsage(ctx context.Context, record usage.Record) {
	meta, ok := ctx.Value(metadataKey{}).(requestMetadata)
	if !ok || record.Failed || !usage.GenerateEnabled(record.Generate) {
		return
	}
	raw := record.RawUsage
	var err error
	switch record.RawUsageFormat {
	case "chat":
		raw, err = billing.ChatUsage(raw)
	case "gemini":
		raw, err = billing.GeminiUsage(raw)
	case "responses":
	default:
		err = fmt.Errorf("raw upstream usage unavailable")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil || len(raw) == 0 {
		s.accountingGap(ctx, record, "usage_unavailable")
		return
	}
	routed := record.Model
	completed := s.now()
	if !record.RequestedAt.IsZero() {
		// Queue lag must not move a receipt across an official quota boundary.
		completed = record.RequestedAt.Add(record.Latency)
	}
	row, err := billing.FromRawUsage(raw, billing.Row{
		TS: completed.Unix(), Model: meta.Model, KeyID: meta.KeyID, ClientIP: meta.IP, Path: meta.Path,
		RequestID: record.TraceID, RoutedModel: routed, UpstreamReportedModel: record.ResponseModel,
		DurationMS: completed.Sub(meta.Started).Milliseconds(), TTFTMS: record.TTFT.Milliseconds(),
	})
	if err != nil {
		s.accountingGap(ctx, record, "usage_invalid")
		return
	}
	account, exists := s.accounts[record.AuthID]
	if meta.AccountAuthID != "" {
		if meta.AccountAuthID != record.AuthID {
			s.lastErr = fmt.Errorf("CPA receipt credential does not match admitted account")
			return
		}
		account, exists = meta.Account, true
	}
	if exists {
		row.OAuthAccountID, row.OAuthPrefix = account.ID, account.Prefix
		row.RoutedModel = account.Prefix + "/" + terminalName(routed)
	}
	row.ModelMismatch = row.UpstreamReportedModel != "" && terminalName(row.Model) != terminalName(row.UpstreamReportedModel)
	data, err := json.Marshal(row)
	if err == nil {
		err = s.file.Append(append(data, '\n'))
	}
	if err != nil {
		s.lastErr = err
	}
}

func terminalName(model string) string {
	model = strings.TrimSpace(model)
	return model[strings.LastIndexByte(model, '/')+1:]
}

// Err reports storage/identity failures. Audited gaps remain visible in
// AccountingStatus without blocking unrelated inference or configuration.
func (s *ReceiptSink) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastErr
}

func (s *ReceiptSink) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.file.Close()
}
