package quota

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestClaudeQuotaProjectionAndOfficialWindowStart(t *testing.T) {
	now := time.Date(2026, 11, 1, 14, 0, 0, 0, time.UTC).Unix()
	raw := []byte(`{"access_token":"must-not-leak","five_hour":{"utilization":6,"resets_at":"2026-11-01T16:19:59.951+00:00"},"seven_day":{"utilization":3,"resets_at":"2026-11-07T09:59:59.951+00:00"},"seven_day_breakdown":{"window_started_at":"2026-11-01T09:59:59.951+00:00"}}`)
	v, err := ProjectClaude(raw, "claude_0123456789abcdef01234567", now)
	if err != nil || v.FiveHour.Used != 6 || v.FiveHour.Remaining != 94 || v.Weekly.Start != time.Date(2026, 11, 1, 9, 59, 59, 0, time.UTC).Unix() {
		t.Fatalf("projection %+v %v", v, err)
	}
	out, _ := json.Marshal(v)
	if strings.Contains(string(out), "must-not-leak") {
		t.Fatal("secret leaked")
	}
	v, err = ProjectClaude(raw, v.ID, now+7*86400)
	if err != nil || v.FiveHour != nil || v.Weekly != nil {
		t.Fatal("expired cycles were advanced or guessed")
	}
}

func TestClaudeQuotaFixedEndpointAndProxy(t *testing.T) {
	for _, code := range []int{200, 302, 401, 429, 500} {
		calls := 0
		f := Fetcher{Transport: func(proxy string) (http.RoundTripper, error) {
			if proxy != "socks5h://127.0.0.1:1080" {
				t.Fatal("proxy lost")
			}
			return transportFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.URL.String() != ClaudeUsageURL || r.Method != "GET" || r.Header.Get("Authorization") != "Bearer fixture" || r.Header.Get("anthropic-beta") != "oauth-2025-04-20" {
					t.Fatal("wrong official request")
				}
				rsp := response(code, `{"five_hour":null,"seven_day":null,"token":"secret"}`)
				rsp.Header.Set("Location", "https://evil.example")
				return rsp, nil
			}), nil
		}}
		v, err := f.FetchClaude(t.Context(), "fixture", "socks5h://127.0.0.1:1080", "claude_0123456789abcdef01234567", 1800000000)
		if calls != 1 || code == 200 && err != nil || code != 200 && err == nil {
			t.Fatalf("status %d: %+v %v", code, v, err)
		}
		if err != nil && strings.Contains(err.Error(), "secret") {
			t.Fatal("upstream error body leaked")
		}
	}
}
