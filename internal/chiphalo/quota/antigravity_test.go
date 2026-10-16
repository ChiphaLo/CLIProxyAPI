package quota

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestAntigravityQuotaProjection(t *testing.T) {
	raw := []byte(`{"access_token":"never-export","models":{"gemini-fixture":{"quotaInfo":{"remainingFraction":0.375,"resetTime":"2026-10-17T00:00:00Z"},"modelExperiments":{"prompt":"never-export"}},"missing":{"quotaInfo":{}},"invalid":{"quotaInfo":{"remainingFraction":2,"resetTime":"bad"}},"zero":{"quotaInfo":{"remainingFraction":0}},"hidden":{"quotaInfo":{"remainingFraction":1}}}}`)
	rows, err := ProjectAntigravity(raw, map[string]bool{"gemini-fixture": true, "missing": true, "invalid": true, "zero": true})
	if err != nil || len(rows) != 4 {
		t.Fatal("projection failed", err)
	}
	if rows[0].Model != "gemini-fixture" || rows[0].Remaining == nil || *rows[0].Remaining != 37.5 || rows[0].Reset == nil {
		t.Fatal("official fraction/reset lost")
	}
	if rows[1].Remaining != nil || rows[1].Reset != nil || rows[2].Remaining != nil || rows[3].Remaining == nil || *rows[3].Remaining != 0 {
		t.Fatal("unknown and real zero confused")
	}
	data, _ := json.Marshal(rows)
	if strings.Contains(string(data), "never-export") {
		t.Fatal("unexpected official fields escaped projection")
	}
	for _, raw := range []string{`{}`, `{"models":null}`, `{"models":[]}`, strings.Repeat("x", (8<<20)+1)} {
		if _, err := ProjectAntigravity([]byte(raw), nil); err == nil {
			t.Fatal("invalid schema accepted")
		}
	}
}

func TestAntigravityQuotaFetchIsFixedMetadataRead(t *testing.T) {
	calls := 0
	f := Fetcher{Transport: func(proxy string) (http.RoundTripper, error) {
		if proxy != "socks5h://fixture:10808" {
			t.Fatal("proxy lost")
		}
		return transportFunc(func(r *http.Request) (*http.Response, error) {
			calls++
			body, _ := io.ReadAll(r.Body)
			if r.Method != "POST" || r.URL.String() != AntigravityModelsURL || r.Header.Get("Authorization") != "Bearer private" || string(body) != `{"project":"project-fixture"}` {
				t.Fatal("metadata endpoint/identity changed")
			}
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"models":{"gemini-fixture":{"quotaInfo":{"remainingFraction":1}}}}`)), Header: http.Header{}}, nil
		}), nil
	}}
	rows, err := f.FetchAntigravity(context.Background(), Credential{Token: "private", Workspace: "project-fixture", Proxy: "socks5h://fixture:10808"}, map[string]bool{"gemini-fixture": true})
	if err != nil || calls != 1 || len(rows) != 1 || *rows[0].Remaining != 100 {
		t.Fatal("metadata fetch failed", err)
	}
}

func TestAntigravityStaleSnapshotIsIdentityBound(t *testing.T) {
	previous := Snapshot{Antigravity: []AntigravityAccount{{ID: strings.Repeat("a", 64), Prefix: "antigravity", Models: []AntigravityModel{{Model: "gemini-fixture", Remaining: new(50.0)}}, Updated: new(int64(100))}}}
	current := Snapshot{Antigravity: []AntigravityAccount{{ID: strings.Repeat("a", 64), Prefix: "antigravity", Status: "unavailable", Checked: 200}}}
	got := CarryPrevious(current, previous)
	if len(got.Antigravity[0].Models) != 1 || got.Antigravity[0].Status != "unavailable" {
		t.Fatal("stale state lost")
	}
	current.Antigravity[0].ID = strings.Repeat("b", 64)
	current.Antigravity[0].Models = nil
	if len(CarryPrevious(current, previous).Antigravity[0].Models) != 0 {
		t.Fatal("quota crossed account identity")
	}
	raw := []byte(`{"accounts":[],"checked_at":200,"antigravity_accounts":[{"id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","prefix":"antigravity","status":"ok","checked_at":200,"updated_at":200,"models":[{"model":"gemini-fixture","remaining_percent":50}],"access_token":"never-export"}]}`)
	snapshot, err := DecodeSnapshot(raw)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := snapshotBytes(snapshot)
	if err != nil || strings.Contains(string(encoded), "never-export") {
		t.Fatal("snapshot projection failed", err)
	}
}
