package helps

import (
	"context"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

func TestUsageReporterPreservesUpstreamStartAfterFirstToken(t *testing.T) {
	r := NewUsageReporter(context.Background(), "fixture", "fixture", nil)
	r.StartResponseTTFT()
	start := r.upstreamRequestTime()
	if start.IsZero() || start.Before(r.requestedAt) {
		t.Fatal("outbound start missing")
	}
	r.ObserveTokenEvent(true)
	r.StartResponseTTFT()
	record := r.buildRecord(usage.Detail{OutputTokens: 1}, false)
	if !record.UpstreamRequestedAt.Equal(start) || !r.ttftStart.IsZero() {
		t.Fatal("token observation lost or restarted the outbound clock")
	}
	if (*UsageReporter)(nil).upstreamRequestTime() != (time.Time{}) {
		t.Fatal("nil reporter has an outbound clock")
	}
}
