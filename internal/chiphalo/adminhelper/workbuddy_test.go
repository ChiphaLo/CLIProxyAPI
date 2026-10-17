package adminhelper

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type workBuddyRoundTrip func(*http.Request) (*http.Response, error)

func (f workBuddyRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestWorkBuddyProjectionKeepsRegionsAndZeroBalancesAndSecretsPrivate(t *testing.T) {
	now := time.Unix(1800000000, 0)
	path := filepath.Join(t.TempDir(), "access.json")
	if err := os.WriteFile(path, []byte(`{"cn":{"port":8331,"management_key":"private-cn"},"intl":{"port":8332,"management_key":"private-intl"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	reads := 0
	client := &http.Client{Transport: workBuddyRoundTrip(func(r *http.Request) (*http.Response, error) {
		reads++
		if r.Method != "GET" || r.URL.Hostname() != "127.0.0.1" || r.URL.RawQuery != "" {
			t.Fatal("unbounded or mutating plugin request")
		}
		region, secret, remaining, total := "cn", "private-cn", 2195, 2200
		if r.URL.Port() == "8332" {
			region, secret, remaining, total = "global", "private-intl", 0, 350
		} else if r.URL.Port() != "8331" {
			t.Fatal("unexpected destination")
		}
		if r.Header.Get("Authorization") != "Bearer "+secret {
			t.Fatal("worker credentials crossed regions")
		}
		body := ""
		switch r.URL.Path {
		case "/v0/management/workbuddy/quota/status":
			body = fmt.Sprintf(`{"interval_minutes":30,"accounts_known":1,"accounts_total":1,"uid":"never-public","accessToken":"never-public","last_run":[{"credits":%d,"credits_total":%d,"known":true,"region":%q,"fetched_at":%q,"uid":"never-public"}]}`, remaining, total, region, now.Add(-time.Minute).Format(time.RFC3339))
		case "/v0/management/workbuddy/checkin/status":
			if region != "cn" {
				t.Fatal("unsupported international check-in requested")
			}
			body = `{"enabled":true,"hour":8,"minute":0,"next_run":"2027-01-16T08:00:00+08:00","history":[{"finished_at":"2027-01-15T08:00:00+08:00","succeeded":1}]}`
		default:
			t.Fatal("unapproved plugin path")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
	})}
	v := readWorkBuddyCredits(t.Context(), path, now, client)
	rows := v["accounts"].([]map[string]any)
	if v["ok"] != true || reads != 3 || len(rows) != 2 || rows[0]["remaining_credits"] != int64(2195) || rows[1]["remaining_credits"] != int64(0) || rows[1]["status"] != "ok" || rows[0]["stale"] != false || rows[1]["checkin_supported"] != false {
		t.Fatalf("incorrect credit projection: %+v", rows)
	}
	raw, _ := json.Marshal(v)
	for _, secret := range []string{"private-cn", "private-intl", "never-public", "accessToken", "management_key", "uid"} {
		if strings.Contains(string(raw), secret) {
			t.Fatal("private plugin fields leaked")
		}
	}
}

func TestWorkBuddyUnavailableNeverBecomesZeroAndNeverChangesRouting(t *testing.T) {
	now := time.Unix(1800000000, 0)
	path := filepath.Join(t.TempDir(), "access.json")
	for _, access := range []string{`{}`, `{"cn":{"port":8320,"management_key":"private"}}`, `{"cn":{"port":8331,"management_key":"private"}}`} {
		if err := os.WriteFile(path, []byte(access), 0600); err != nil {
			t.Fatal(err)
		}
		client := &http.Client{Transport: workBuddyRoundTrip(func(r *http.Request) (*http.Response, error) {
			if r.URL.Host != "127.0.0.1:8331" {
				t.Fatal("configuration redirected local management read")
			}
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"accounts_known":0,"accounts_total":1,"total_credits":0,"last_run":[{"credits":0,"known":false,"error":"private-error-token"}]}`)), Header: http.Header{}}, nil
		})}
		v := readWorkBuddyCredits(t.Context(), path, now, client)
		for _, r := range v["accounts"].([]map[string]any) {
			if r["status"] == "ok" || r["remaining_credits"] != nil {
				t.Fatal("unknown credits became a real zero")
			}
		}
	}
	client := workBuddyHTTPClient()
	if err := client.CheckRedirect(nil, nil); err != http.ErrUseLastResponse {
		t.Fatal("plugin redirect permitted")
	}
	if client.Transport.(*http.Transport).Proxy != nil {
		t.Fatal("local plugin inherited an external proxy")
	}
	service := &Service{now: func() time.Time { return now }}
	v := service.dispatchContext(context.Background(), "workbuddy_credits", map[string]json.RawMessage{"url": json.RawMessage(`"http://attacker.example"`)})
	if v["ok"] != false {
		t.Fatal("management caller supplied a lookup target")
	}
}

func TestWorkBuddyOldReadingsAreMarkedStale(t *testing.T) {
	now := time.Unix(1800000000, 0)
	path := filepath.Join(t.TempDir(), "access.json")
	_ = os.WriteFile(path, []byte(`{"cn":{"port":8331,"management_key":"private"}}`), 0600)
	client := &http.Client{Transport: workBuddyRoundTrip(func(*http.Request) (*http.Response, error) {
		body := fmt.Sprintf(`{"interval_minutes":30,"accounts_known":1,"accounts_total":1,"last_run":[{"known":true,"region":"cn","credits":12,"fetched_at":%q}]}`, now.Add(-time.Hour).Format(time.RFC3339))
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	v := readWorkBuddyCredits(t.Context(), path, now, client)
	r := v["accounts"].([]map[string]any)[0]
	if r["remaining_credits"] != int64(12) || r["stale"] != true {
		t.Fatal("old quota was hidden or represented as fresh")
	}
}
