package quota

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestAntigravityGeminiSummarySeparatesWeeklyAndFiveHour(t *testing.T) {
	raw := `{"groups":[{"displayName":"Gemini Models","buckets":[{"bucketId":"gemini-weekly","window":"weekly","remainingFraction":0.6,"resetTime":"2026-10-23T04:52:33Z"},{"bucketId":"gemini-5h","window":"5h","remainingFraction":0.9,"resetTime":"2026-10-16T09:52:33Z"}]},{"displayName":"Claude and GPT models","buckets":[{"bucketId":"3p-weekly","window":"weekly","remainingFraction":1}]}],"access_token":"never-export"}`
	for _, input := range []string{raw, `{"quotaSummary":` + raw + `}`} {
		q, err := ProjectAntigravitySummary([]byte(input))
		if err != nil || q.Weekly == nil || q.FiveHour == nil || *q.Weekly.Remaining != 60 || *q.FiveHour.Remaining != 90 || *q.Weekly.Seconds != 604800 || *q.FiveHour.Seconds != 18000 || *q.Weekly.Reset == *q.FiveHour.Reset {
			t.Fatal("weekly and short windows mixed", err)
		}
		encoded, _ := json.Marshal(q)
		if strings.Contains(string(encoded), "3p-") || strings.Contains(string(encoded), "never-export") {
			t.Fatal("unselected group or secret leaked")
		}
	}
}

func TestAntigravityGeminiSummaryDoesNotInventQuota(t *testing.T) {
	q, err := ProjectAntigravitySummary([]byte(`{"groups":[{"buckets":[{"bucketId":"gemini-weekly","window":"weekly","remainingFraction":0}]}]}`))
	if err != nil || q.FiveHour != nil || q.Weekly.Remaining == nil || *q.Weekly.Remaining != 0 || q.Weekly.Reset != nil {
		t.Fatal("weekly-only real zero became unlimited or fabricated 5h", err)
	}
	q, err = ProjectAntigravitySummary([]byte(`{"groups":[{"buckets":[{"bucketId":"gemini-5h","window":"5h","remainingFraction":2}]}]}`))
	if err != nil || q.Weekly != nil || q.FiveHour.Remaining != nil {
		t.Fatal("invalid fraction or missing weekly fabricated")
	}
	for _, raw := range []string{
		`{}`,
		`{"groups":[{"buckets":[{"bucketId":"3p-weekly","remainingFraction":1}]}]}`,
		`{"groups":[{"buckets":[{"bucketId":"gemini-weekly","window":"5h","remainingFraction":1}]}]}`,
		`{"groups":[{"buckets":[{"bucketId":"gemini-weekly","disabled":true,"remainingFraction":1}]}]}`,
		`{"groups":[{"buckets":[{"bucketId":"gemini-weekly"},{"bucketId":"gemini-weekly"}]}]}`,
	} {
		if _, err := ProjectAntigravitySummary([]byte(raw)); err == nil {
			t.Fatal("wrong/disabled/ambiguous group accepted")
		}
	}
}

func TestAntigravitySummaryFetchUsesExactProjectAndOrigin(t *testing.T) {
	calls := 0
	f := Fetcher{Transport: func(string) (http.RoundTripper, error) {
		return transportFunc(func(r *http.Request) (*http.Response, error) {
			calls++
			raw, _ := io.ReadAll(r.Body)
			if r.URL.String() != AntigravitySummaryURL || r.Method != "POST" || string(raw) != `{"project":"exact-project"}` || r.Header.Get("Authorization") != "Bearer private" {
				t.Fatal("wrong read origin/identity")
			}
			return response(200, `{"groups":[{"buckets":[{"bucketId":"gemini-weekly","window":"weekly","remainingFraction":0.8}]}]}`), nil
		}), nil
	}}
	q, err := f.FetchAntigravitySummary(context.Background(), Credential{Token: "private", Workspace: "exact-project"})
	if err != nil || calls != 1 || *q.Weekly.Remaining != 80 {
		t.Fatal("summary fetch failed", err)
	}
}

func TestAntigravitySummaryStaleDataNeverCrossesIdentity(t *testing.T) {
	old := AntigravityAccount{ID: strings.Repeat("a", 64), Prefix: "antigravity", Status: "ok", Updated: new(int64(100)), Gemini: &AntigravityGeminiQuota{Status: "ok", Checked: 100, Updated: new(int64(100)), Weekly: &Window{Seconds: new(int64(604800)), Remaining: new(80.0)}}}
	current := AntigravityAccount{ID: old.ID, Prefix: old.Prefix, Status: "ok", Updated: new(int64(200)), Gemini: &AntigravityGeminiQuota{Status: "unavailable", Checked: 200}}
	got := CarryPrevious(Snapshot{Antigravity: []AntigravityAccount{current}}, Snapshot{Antigravity: []AntigravityAccount{old}})
	if got.Antigravity[0].Gemini.Status != "unavailable" || *got.Antigravity[0].Gemini.Updated != 100 || *got.Antigravity[0].Gemini.Weekly.Remaining != 80 {
		t.Fatal("summary failure erased prior weekly or fabricated fresh success")
	}
	current.ID = strings.Repeat("b", 64)
	current.Gemini = &AntigravityGeminiQuota{Status: "unavailable", Checked: 200}
	got = CarryPrevious(Snapshot{Antigravity: []AntigravityAccount{current}}, Snapshot{Antigravity: []AntigravityAccount{old}})
	if got.Antigravity[0].Gemini.Weekly != nil {
		t.Fatal("summary crossed account identity")
	}
}
