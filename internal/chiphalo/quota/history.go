package quota

import (
	"encoding/json"
	"errors"
	"math"
)

type Interval struct {
	AccountID string `json:"account_id"`
	Start     int64  `json:"start_ts"`
	End       int64  `json:"end_ts"`
}
type Exclusions struct {
	Intervals []Interval `json:"intervals"`
}
type HistoryRow struct {
	TS        int64    `json:"ts"`
	ID        string   `json:"account_id"`
	Prefix    string   `json:"prefix"`
	Start     int64    `json:"cycle_start_ts"`
	End       int64    `json:"cycle_end_ts"`
	Used      float64  `json:"used_percent"`
	Remaining *float64 `json:"remaining_percent"`
	RawUsed   float64  `json:"raw_used_percent"`
	Rounded   bool     `json:"is_rounded"`
	Seconds   int64    `json:"window_seconds"`
	Source    string   `json:"source"`
}

func History(snapshot Snapshot, exclusions Exclusions) ([]HistoryRow, error) {
	if exclusions.Intervals == nil || len(exclusions.Intervals) > 4096 {
		return nil, errors.New("history exclusions invalid")
	}
	for _, rule := range exclusions.Intervals {
		if rule.AccountID == "" || rule.Start <= 0 || rule.End <= rule.Start {
			return nil, errors.New("history exclusion invalid")
		}
	}
	rows := []HistoryRow{}
	for _, account := range snapshot.Accounts {
		excluded := false
		for _, rule := range exclusions.Intervals {
			if rule.AccountID == account.ID && rule.Start <= snapshot.Checked && snapshot.Checked < rule.End {
				excluded = true
				break
			}
		}
		if excluded || account.Status != "ok" || account.Quota == nil || account.Quota.Rate == nil {
			continue
		}
		var weekly *Window
		for _, w := range []*Window{account.Quota.Rate.Primary, account.Quota.Rate.Secondary} {
			if w != nil && w.Seconds != nil && *w.Seconds == 604800 {
				weekly = w
				break
			}
		}
		start, end, available := account.Quota.Weekly(snapshot.Checked)
		if weekly == nil || weekly.Used == nil || !available {
			continue
		}
		rows = append(rows, HistoryRow{TS: snapshot.Checked, ID: account.ID, Prefix: account.Prefix, Start: start, End: end, Used: *weekly.Used, RawUsed: *weekly.Used, Remaining: weekly.Remaining, Rounded: *weekly.Used == math.RoundToEven(*weekly.Used), Seconds: 604800, Source: "wham/usage"})
	}
	return rows, nil
}
func snapshotBytes(snapshot Snapshot) ([]byte, error) {
	if _, err := PublicAntigravity(snapshot.Antigravity); err != nil {
		return nil, err
	}
	if snapshot.Accounts == nil || len(snapshot.Accounts) > 128 || snapshot.Checked <= 0 {
		return nil, errors.New("public quota snapshot invalid")
	}
	ids, prefixes := map[string]bool{}, map[string]bool{}
	for _, row := range snapshot.Accounts {
		if row.ID == "" || row.Prefix == "" || row.Workspace == "" || ids[row.ID] || prefixes[row.Prefix] || row.Source != OfficialUsageURL || row.Status != "ok" && row.Status != "unavailable" || row.Status == "ok" && (row.Quota == nil || row.Updated == nil) {
			return nil, errors.New("public quota identity/status invalid")
		}
		ids[row.ID], prefixes[row.Prefix] = true, true
	}
	raw, err := json.Marshal(snapshot)
	if err != nil || len(raw) > 4*1024*1024 {
		return nil, errors.New("public quota snapshot exceeds limit")
	}
	return append(raw, '\n'), nil
}
