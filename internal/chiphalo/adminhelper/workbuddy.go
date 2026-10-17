package adminhelper

import (
	"context"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/privatefile"
)

// EnableWorkBuddy selects a deployment-owned control file before Serve. No
// management caller may supply credentials, URLs, ports or plugin operations.
func (s *Service) EnableWorkBuddy(accessFile string) error {
	if !filepath.IsAbs(accessFile) || filepath.Clean(accessFile) != accessFile {
		return errors.New("explicit WorkBuddy control file required")
	}
	s.workBuddyAccess = accessFile
	return nil
}

type workBuddyAccess struct {
	Port int    `json:"port"`
	Key  string `json:"management_key"`
}

type workBuddyQuota struct {
	Interval int `json:"interval_minutes"`
	Known    int `json:"accounts_known"`
	Accounts int `json:"accounts_total"`
	LastRun  []struct {
		Credits *int64    `json:"credits"`
		Total   *int64    `json:"credits_total"`
		Known   bool      `json:"known"`
		Region  string    `json:"region"`
		At      time.Time `json:"fetched_at"`
		Error   string    `json:"error"`
	} `json:"last_run"`
}

type workBuddyCheckin struct {
	Enabled bool   `json:"enabled"`
	Hour    int    `json:"hour"`
	Minute  int    `json:"minute"`
	Next    string `json:"next_run"`
	History []struct {
		Finished  time.Time `json:"finished_at"`
		Succeeded int       `json:"succeeded"`
	} `json:"history"`
}

func workBuddyHTTPClient() *http.Client {
	return &http.Client{
		Transport:     &http.Transport{Proxy: nil, DisableKeepAlives: true},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func readWorkBuddyCredits(parent context.Context, path string, now time.Time, client *http.Client) Result {
	accounts := []map[string]any{}
	var access map[string]workBuddyAccess
	raw, err := privatefile.Read(path, 16384, true)
	configured := err == nil && privatefile.Decode(raw, &access) == nil
	// This bounds local management reads, never established inference streams.
	ctx, cancel := context.WithTimeout(parent, 3*time.Second)
	defer cancel()
	for _, region := range []string{"cn", "intl"} {
		item := map[string]any{"region": region, "prefix": "workbuddy-" + region, "status": "unavailable", "remaining_credits": nil, "total_credits": nil, "updated_at": nil, "stale": true, "checkin_supported": region == "cn"}
		accounts = append(accounts, item)
		if !configured {
			item["status"] = "not_configured"
			continue
		}
		worker, ok := access[region]
		port, upstreamRegion := 8331, "cn"
		if region == "intl" {
			port, upstreamRegion = 8332, "global"
		}
		if !ok || worker.Port != port || worker.Key == "" || len(worker.Key) > 4096 || strings.ContainsAny(worker.Key, "\r\n\x00") {
			continue
		}
		get := func(endpoint string, target any) bool {
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://127.0.0.1:"+strconv.Itoa(port)+"/v0/management/workbuddy/"+endpoint, nil)
			if err != nil {
				return false
			}
			req.Header.Set("Authorization", "Bearer "+worker.Key)
			resp, err := client.Do(req)
			if err != nil {
				return false
			}
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != http.StatusOK {
				return false
			}
			data, err := io.ReadAll(io.LimitReader(resp.Body, 65537))
			return err == nil && len(data) <= 65536 && privatefile.Decode(data, target) == nil
		}
		var quota workBuddyQuota
		if get("quota/status", &quota) && quota.Accounts == 1 && quota.Known == 1 && len(quota.LastRun) == 1 {
			q := quota.LastRun[0]
			if q.Known && q.Error == "" && q.Region == upstreamRegion && q.Credits != nil && *q.Credits >= 0 && !q.At.IsZero() && !q.At.After(now.Add(time.Minute)) {
				interval := max(1, min(quota.Interval, 1440))
				item["status"], item["remaining_credits"], item["updated_at"] = "ok", *q.Credits, q.At.Unix()
				item["stale"], item["interval_minutes"] = now.Sub(q.At) > time.Duration(interval+2)*time.Minute, interval
				if q.Total != nil && *q.Total > 0 && *q.Total >= *q.Credits {
					item["total_credits"] = *q.Total
				}
			}
		}
		if region == "cn" {
			var checkin workBuddyCheckin
			if get("checkin/status", &checkin) && checkin.Hour >= 0 && checkin.Hour < 24 && checkin.Minute >= 0 && checkin.Minute < 60 {
				item["checkin_enabled"], item["checkin_hour"], item["checkin_minute"] = checkin.Enabled, checkin.Hour, checkin.Minute
				if at, err := time.Parse(time.RFC3339, checkin.Next); err == nil {
					item["next_checkin_at"] = at.Unix()
				}
				if len(checkin.History) > 0 && !checkin.History[0].Finished.IsZero() {
					item["last_checkin_at"], item["last_checkin_success"] = checkin.History[0].Finished.Unix(), checkin.History[0].Succeeded > 0
				}
			}
		}
	}
	return Result{"ok": true, "accounts": accounts, "source": "workbuddy_plugin_upstream_quota", "generated_at": now.Unix()}
}
