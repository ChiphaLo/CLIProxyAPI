package helps

import (
	"strings"
	"testing"
	"time"

	"github.com/MoeForge/moego/ojson"
)

func TestResponsesStreamFailure(t *testing.T) {
	for _, tc := range []struct {
		name, payload string
		status        int
		retry         time.Duration
	}{
		{"devin_free_model_limit", `{"type":"response.failed","status":429,"error":{"code":"rate_limit_exceeded","message":"resource_exhausted: Reached free model rate limit. Your limit will reset in 9 hours 44 minutes (at 17:01 UTC).","type":"rate_limit_error"},"response":{"output":[{"text":"PRIVATE OUTPUT"}]}}`, 429, 9*time.Hour + 44*time.Minute},
		{"nested_limit", `{"type":"response.failed","response":{"status":"failed","error":{"type":"rate_limit_error","code":"rate_limit_exceeded","message":"reset in 30 seconds"}}}`, 429, 30 * time.Second},
		{"explicit_hint", `{"type":"error","error":{"type":"rate_limit_error","message":"busy","retry_after":75}}`, 429, 75 * time.Second},
		{"codex_hint", `{"type":"error","error":{"code":"rate_limit_exceeded","message":"busy (try again in 15s)"}}`, 429, 15 * time.Second},
		{"auth", `{"type":"error","error":{"type":"authentication_error","message":"login required"}}`, 401, 0},
		{"context", `{"type":"response.failed","response":{"error":{"code":"context_length_exceeded","message":"too large"}}}`, 400, 0},
		{"explicit_status_wins", `{"type":"error","status":502,"error":{"type":"rate_limit_error","message":"bad gateway"}}`, 502, 0},
		{"invalid_hint", `{"type":"error","status":429,"error":{"message":"try again in forever","retry_after":-1}}`, 429, 0},
		{"overflow_hint", `{"type":"error","status":429,"error":{"message":"busy","retry_after":9223372036854775807}}`, 429, 0},
		{"unknown", `{"type":"response.failed","response":{"output":[{"text":"PRIVATE OUTPUT"}]}}`, 502, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, body, delay := ResponsesStreamFailure([]byte(tc.payload))
			if status != tc.status {
				t.Fatalf("status=%d, want %d", status, tc.status)
			}
			if strings.Contains(string(body), "PRIVATE OUTPUT") {
				t.Fatal("retained output in error envelope")
			}
			if !ojson.GetBytes(body, "error.message").Exists() {
				t.Fatal("missing normalized error")
			}
			if tc.retry == 0 {
				if delay != nil {
					t.Fatalf("unexpected retry=%v", *delay)
				}
			} else {
				if delay == nil || *delay != tc.retry {
					t.Fatalf("retry=%v, want %v", delay, tc.retry)
				}
				if !strings.Contains(ojson.GetBytes(body, "error.message").String(), "try again in ") {
					t.Fatal("missing machine-readable retry hint")
				}
			}
		})
	}
}
