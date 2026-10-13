package gateway

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/appendlog"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

type AuditRow struct {
	TS         int64  `json:"ts"`
	RequestID  string `json:"request_id"`
	ClientIP   string `json:"client_ip"`
	Method     string `json:"method"`
	Path       string `json:"path"`
	Model      string `json:"model,omitempty"`
	KeyID      string `json:"key_id,omitempty"`
	ProviderID string `json:"provider_id,omitempty"`
	Status     int    `json:"status"`
	ErrorClass string `json:"error_class,omitempty"`
	DurationMS int64  `json:"duration_ms"`
	TTFTMS     int64  `json:"ttft_ms,omitempty"`
	RetryCount int    `json:"retry_count"`
	Event      string `json:"event,omitempty"`
}

type AuditSink struct {
	file    *appendlog.Log
	mu      sync.Mutex
	lastErr error
}

func OpenAuditSink(path string) (*AuditSink, error) {
	l, err := appendlog.Open(path)
	if err != nil {
		return nil, err
	}
	return &AuditSink{file: l}, nil
}

func auditClass(status int) string {
	switch status {
	case 400, 404:
		return "invalid_request"
	case 401:
		return "authentication_error"
	case 403:
		return "authorization_error"
	case 413:
		return "payload_too_large"
	case 429:
		return "rate_limit_error"
	case 499:
		return "client_disconnected"
	case 502, 503, 504:
		return "upstream_unavailable"
	}
	if status >= 400 {
		return "request_failed"
	}
	return ""
}

func (s *AuditSink) append(row AuditRow) {
	raw, err := json.Marshal(row)
	if err == nil {
		err = s.file.Append(append(raw, '\n'))
	}
	if err != nil {
		s.mu.Lock()
		s.lastErr = err
		s.mu.Unlock()
	}
}

// Middleware records admission errors, including authentication, unsupported
// bodies and local limits. No request body, query, token or provider error body
// is copied into this shared legacy log.
func (s *AuditSink) Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request.URL.Path == "/health" {
			c.Next()
			return
		}
		start := time.Now()
		id := logging.GetGinRequestID(c)
		if id == "" {
			var err error
			id, err = logging.GenerateRequestID()
			if err != nil {
				abort(c, 503, "CPA audit identity unavailable")
				return
			}
		}
		row := AuditRow{TS: start.Unix(), RequestID: id, ClientIP: c.ClientIP(), Method: c.Request.Method, Path: c.Request.URL.Path, Event: "http_request"}
		c.Next()
		if meta, ok := c.Request.Context().Value(metadataKey{}).(requestMetadata); ok {
			row.Model = meta.Model
			row.KeyID = meta.KeyID
		}
		if p, ok := c.Request.Context().Value(profileContextKey{}).(*LegacyProfile); ok && row.KeyID == "" {
			row.KeyID = p.Keys.KeyID(c.GetString("userApiKey"))
		}
		row.Status = c.Writer.Status()
		row.ErrorClass = auditClass(row.Status)
		row.DurationMS = time.Since(start).Milliseconds()
		s.append(row)
	}
}

// HandleUsage preserves upstream attempt failures even if SSE headers already
// committed 200. The raw provider failure body is intentionally never logged.
func (s *AuditSink) HandleUsage(ctx context.Context, record usage.Record) {
	if !record.Failed {
		return
	}
	meta, ok := ctx.Value(metadataKey{}).(requestMetadata)
	if !ok {
		return
	}
	status := record.Fail.StatusCode
	if status < 400 {
		status = 502
	}
	completed := time.Now()
	if !record.RequestedAt.IsZero() {
		completed = record.RequestedAt.Add(record.Latency)
	}
	s.append(AuditRow{TS: completed.Unix(), RequestID: record.TraceID, ClientIP: meta.IP, Method: "POST", Path: meta.Path,
		Model: meta.Model, KeyID: meta.KeyID, ProviderID: record.Provider, Status: status, ErrorClass: auditClass(status),
		DurationMS: completed.Sub(meta.Started).Milliseconds(), TTFTMS: record.TTFT.Milliseconds(), Event: "upstream_attempt"})
}

func (s *AuditSink) Err() error   { s.mu.Lock(); defer s.mu.Unlock(); return s.lastErr }
func (s *AuditSink) Close() error { return s.file.Close() }
