package gateway

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/billing"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

func TestReceiptTTFTIncludesAdmissionAndClampsToDuration(t *testing.T) {
	stamp := time.Unix(1800000000, 0)
	for _, tt := range []struct {
		name          string
		requested     time.Time
		latency, ttft time.Duration
		want          int64
	}{
		{"preparation", stamp.Add(time.Second), 2 * time.Second, 500 * time.Millisecond, 1500},
		{"unknown-first-token", stamp.Add(time.Second), 2 * time.Second, 0, 0},
		{"clamped", stamp.Add(time.Second), 2 * time.Second, 5 * time.Second, 3000},
		{"no-request-clock", time.Time{}, 0, 500 * time.Millisecond, 500},
		{"overlapping-clock", stamp.Add(-time.Second), 4 * time.Second, time.Second, 1000},
	} {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "usage.jsonl")
			sink, err := OpenReceiptSink(path, nil)
			if err != nil {
				t.Fatal(err)
			}
			sink.now = func() time.Time { return stamp.Add(3 * time.Second) }
			ctx := context.WithValue(context.Background(), metadataKey{}, requestMetadata{Started: stamp})
			sink.HandleUsage(ctx, usage.Record{RequestedAt: tt.requested, Latency: tt.latency, TTFT: tt.ttft, RawUsageFormat: "responses", RawUsage: []byte(`{"input_tokens":10,"output_tokens":2}`)})
			if err = sink.Close(); err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			row, err := billing.DecodeRow(raw)
			if err != nil {
				t.Fatal(err)
			}
			if row.TTFTMS != tt.want || row.UpstreamTTFTMS != tt.ttft.Milliseconds() || row.DurationMS != 3000 {
				t.Fatalf("unexpected clocks: %+v", row)
			}
		})
	}
}
