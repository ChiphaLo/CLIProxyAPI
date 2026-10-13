package webgui

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/billing"
)

type usageStat struct {
	Requests, Input, Output, Cached, Write int64
	Cost, TPS                              float64
	TPSN                                   int64
	TTFT, TTFTN                            int64
	TS                                     int64
}

func (s *usageStat) add(row billing.Row, cost float64) {
	s.Requests++
	s.Input += row.Input
	s.Output += row.Output
	s.Cached += row.Cached
	s.Write += row.Write
	s.Cost += cost
	if row.OutputTPS > 0 {
		s.TPS += row.OutputTPS
		s.TPSN++
	}
	if row.TTFTMS > 0 {
		s.TTFT += row.TTFTMS
		s.TTFTN++
	}
}
func (s usageStat) value() map[string]any {
	input := max(s.Input-s.Cached, 0)
	tps := 0.0
	ttft := int64(0)
	if s.TPSN > 0 {
		tps = billing.RoundDecimal(s.TPS/float64(s.TPSN), 2)
	}
	if s.TTFTN > 0 {
		ttft = int64(billing.RoundDecimal(float64(s.TTFT)/float64(s.TTFTN), 0))
	}
	return map[string]any{"ts": s.TS, "requests": s.Requests, "input": input, "output": s.Output, "cached": s.Cached, "write": s.Write, "cost": billing.RoundDecimal(s.Cost, 8), "output_tps": tps, "ttft_ms": ttft, "cache_pct": billing.RoundDecimal(float64(s.Cached)*100/float64(max(input+s.Cached, 1)), 1)}
}
func scanUsage(path string, visit func(billing.Row, map[string]any) error) error {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return errors.New("usage unavailable")
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return errors.New("usage file changed")
	}
	reader := bufio.NewReaderSize(file, 64*1024)
	for {
		line, err := reader.ReadSlice('\n')
		if err == bufio.ErrBufferFull {
			for err == bufio.ErrBufferFull {
				_, err = reader.ReadSlice('\n')
			}
			if err != nil {
				if err == io.EOF {
					break
				}
				return errors.New("usage read failed")
			}
			continue
		}
		if err != nil {
			if err == io.EOF {
				break
			}
			return errors.New("usage read failed")
		}
		row, err := billing.DecodeRow(line)
		if err != nil {
			continue
		}
		var value map[string]any
		if json.Unmarshal(line, &value) != nil {
			continue
		}
		if err := visit(row, value); err != nil {
			return err
		}
	}
	return nil
}
func integerQuery(values url.Values, key string, defaultValue int64) int64 {
	value, err := strconv.ParseInt(values.Get(key), 10, 64)
	if err != nil {
		return defaultValue
	}
	return value
}
func (s *Server) usage(query url.Values) (map[string]any, error) {
	now := s.options.Now().Unix()
	start := integerQuery(query, "start", now-86400)
	end := integerQuery(query, "end", now+1)
	bucket := max(300, min(integerQuery(query, "bucket", 1800), 3600))
	limit := int(max(1, min(integerQuery(query, "limit", 100), 500)))
	model := strings.TrimSpace(query.Get("model"))
	if start < 0 || end <= start || end-start > 366*86400 {
		return nil, errors.New("invalid usage time range")
	}
	prices, err := s.prices()
	if err != nil {
		return nil, err
	}
	total := &usageStat{}
	byModel := map[string]*usageStat{}
	byDay := map[string]*usageStat{}
	byBucket := map[int64]*usageStat{}
	byHour := map[int64]*usageStat{}
	byHalf := map[int64]*usageStat{}
	models := map[string]bool{}
	recent := []map[string]any{}
	todayKey := s.options.Now().UTC().Format("2006-01-02")
	err = scanUsage(s.options.Usage, func(row billing.Row, raw map[string]any) error {
		if row.TS < start || row.TS >= end {
			return nil
		}
		models[row.Model] = true
		if model != "" && row.Model != model {
			return nil
		}
		cost := billing.Cost(row, prices)
		total.add(row, cost)
		if byModel[row.Model] == nil {
			byModel[row.Model] = &usageStat{}
		}
		byModel[row.Model].add(row, cost)
		day := time.Unix(row.TS, 0).UTC().Format("2006-01-02")
		if byDay[day] == nil {
			byDay[day] = &usageStat{}
		}
		byDay[day].add(row, cost)
		for _, group := range []struct {
			Slots map[int64]*usageStat
			Step  int64
		}{{byBucket, bucket}, {byHour, 3600}, {byHalf, 1800}} {
			slot := row.TS - row.TS%group.Step
			if group.Slots[slot] == nil {
				group.Slots[slot] = &usageStat{TS: slot}
			}
			group.Slots[slot].add(row, cost)
		}
		raw["cost"] = cost
		raw["input"] = max(row.Input-row.Cached, 0)
		raw["total"] = max(row.Input-row.Cached, 0) + row.Cached + row.Output
		if row.DurationMS > 7*86400000 {
			raw["duration_ms"] = 0
		}
		if row.DurationMS <= 0 || row.DurationMS > 7*86400000 {
			raw["output_tps"] = 0
		}
		recent = append(recent, raw)
		if len(recent) > limit {
			slices.SortFunc(recent, func(a, b map[string]any) int { return int(number(b["ts"]) - number(a["ts"])) })
			recent = recent[:limit]
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	// Explicit wall-clock buckets include no-request intervals; zero token counts
	// never compress time or move the nearest nonzero point into a missing slot.
	first := start - start%bucket
	last := (end - 1) - (end-1)%bucket
	for slot := first; slot <= last; slot += bucket {
		if byBucket[slot] == nil {
			byBucket[slot] = &usageStat{TS: slot}
		}
	}
	mapStats := func(source map[string]*usageStat) map[string]any {
		out := map[string]any{}
		for key, value := range source {
			out[key] = value.value()
		}
		return out
	}
	slots := func(source map[int64]*usageStat) []map[string]any {
		keys := []int64{}
		for key := range source {
			keys = append(keys, key)
		}
		slices.Sort(keys)
		out := []map[string]any{}
		for _, key := range keys {
			out = append(out, source[key].value())
		}
		return out
	}
	ids := []string{}
	for id := range models {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	slices.SortFunc(recent, func(a, b map[string]any) int { return int(number(b["ts"]) - number(a["ts"])) })
	today := map[string]any{}
	if byDay[todayKey] != nil {
		today = byDay[todayKey].value()
	}
	return map[string]any{"totals": total.value(), "byModel": mapStats(byModel), "models": ids, "byDay": mapStats(byDay), "byHour": slots(byHour), "byHourAll": slots(byHour), "byHalfHour": slots(byHalf), "byBucket": slots(byBucket), "recent": recent, "today": today, "filters": map[string]any{"model": model, "start": start, "end": end, "bucket": bucket}, "prices": prices, "updatedAt": now}, nil
}
func (s *Server) usageResponse(w http.ResponseWriter, r *http.Request) {
	value, err := s.usage(r.URL.Query())
	if err != nil {
		reject(w, 503, err.Error())
		return
	}
	respond(w, 200, value)
}
func (s *Server) accountResponse(w http.ResponseWriter, r *http.Request) {
	var snapshot struct {
		Accounts []json.RawMessage `json:"accounts"`
		Updated  int64             `json:"updated_at"`
	}
	if readJSON(s.options.Quota, 4*1024*1024, false, &snapshot) != nil || snapshot.Accounts == nil {
		reject(w, 503, "official quota snapshot unavailable")
		return
	}
	accounts := []billing.Account{}
	public := []map[string]any{}
	for _, raw := range snapshot.Accounts {
		var account billing.Account
		var value map[string]any
		if json.Unmarshal(raw, &account) != nil || json.Unmarshal(raw, &value) != nil || account.ID == "" {
			reject(w, 503, "quota account data invalid")
			return
		}
		accounts = append(accounts, account)
		public = append(public, publicAccount(value))
	}
	prices, err := s.prices()
	if err != nil {
		reject(w, 503, err.Error())
		return
	}
	var exposed struct {
		Models []string `json:"models"`
	}
	if readJSON(s.options.Exposed, 1024*1024, false, &exposed) != nil {
		reject(w, 503, "OAuth catalog unavailable")
		return
	}
	models := map[string]bool{}
	prefixes := map[string]bool{}
	for _, account := range accounts {
		prefixes[account.Prefix] = true
	}
	for _, id := range exposed.Models {
		models[id] = true
	}
	marks := map[string]int64{}
	markPath := s.options.Data + "/oauth-billing-resets.json"
	if _, err := os.Lstat(markPath); err == nil {
		if readJSON(markPath, 1024*1024, false, &marks) != nil {
			reject(w, 503, "billing reset markers invalid")
			return
		}
	} else if !os.IsNotExist(err) {
		reject(w, 503, "billing reset markers unavailable")
		return
	}
	now := s.options.Now().Unix()
	acc := billing.NewAccumulator(accounts, models, prices, now, marks)
	unknown := int64(0)
	if scanUsage(s.options.Usage, func(row billing.Row, _ map[string]any) error {
		acc.Add(row)
		if _, ok := billing.AccountForRow(row, accounts, models); !ok {
			prefix, _, _ := strings.Cut(row.Model, "/")
			if row.OAuthAccountID != "" || models[row.Model] || prefixes[prefix] {
				unknown++
			}
		}
		return nil
	}) != nil {
		reject(w, 503, "usage unavailable")
		return
	}
	sums := acc.Summaries()
	var resets map[string]map[string]any
	if r.URL.Query().Get("include_reset") != "0" && len(accounts) > 0 {
		resets = s.resetReports(accounts, public, r.URL.Query().Get("fresh") == "1")
	}
	for i, account := range accounts {
		public[i]["billing"] = sums[account.ID]
		updated := int64(number(public[i]["updated_at"]))
		public[i]["stale"] = public[i]["status"] != "ok" || now-updated > 300
		public[i]["banked_reset"] = resetUnavailable("loading")
		if reset, ok := resets[account.Prefix]; ok {
			public[i]["banked_reset"] = reset
		}
	}
	respond(w, 200, map[string]any{"accounts": public, "unattributed_oauth_requests": unknown, "billing_available": true, "billing_basis": "CPA token usage × currently configured USD / 1M token rates; not official charges", "billing_as_of_ts": now, "updated_at": now})
}

// Public quota snapshots are allowlisted again at the GUI boundary. An extra
// upstream field or an accidentally copied token can never become an API field.
func publicAccount(value map[string]any) map[string]any {
	out := map[string]any{}
	for _, key := range []string{"id", "prefix", "label", "workspace_id", "legacy_since", "checked_at", "updated_at", "source", "status"} {
		if v, ok := value[key]; ok {
			out[key] = v
		}
	}
	quota, _ := value["quota"].(map[string]any)
	q := map[string]any{}
	if plan, ok := quota["plan_type"].(string); ok && len(plan) <= 80 {
		q["plan_type"] = plan
	}
	for _, key := range []string{"rate_limit", "code_review_rate_limit"} {
		q[key] = publicWindows(quota[key])
	}
	credits, _ := quota["credits"].(map[string]any)
	c := map[string]any{}
	for _, key := range []string{"has_credits", "unlimited", "balance"} {
		if v, ok := credits[key]; ok {
			switch v := v.(type) {
			case bool, float64:
				c[key] = v
			case string:
				if len(v) <= 256 {
					c[key] = v
				}
			}
		}
	}
	q["credits"] = c
	additional := []any{}
	raw, _ := quota["additional_rate_limits"].([]any)
	for _, v := range raw {
		item, ok := v.(map[string]any)
		if !ok {
			continue
		}
		row := map[string]any{"rate_limit": publicWindows(item["rate_limit"])}
		if name, ok := item["limit_name"].(string); ok && len(name) <= 128 {
			row["limit_name"] = name
		}
		additional = append(additional, row)
		if len(additional) >= 32 {
			break
		}
	}
	q["additional_rate_limits"] = additional
	out["quota"] = q
	if value["status"] != "ok" {
		out["error"] = "official quota unavailable"
	}
	return out
}
func publicWindows(value any) map[string]any {
	source, _ := value.(map[string]any)
	out := map[string]any{}
	for _, key := range []string{"allowed", "limit_reached"} {
		if v, ok := source[key].(bool); ok {
			out[key] = v
		}
	}
	for _, key := range []string{"primary_window", "secondary_window"} {
		raw, _ := source[key].(map[string]any)
		if raw == nil {
			out[key] = nil
			continue
		}
		window := map[string]any{}
		for _, field := range []string{"used_percent", "remaining_percent", "limit_window_seconds", "reset_at"} {
			if v, ok := raw[field].(float64); ok {
				window[field] = v
			}
		}
		out[key] = window
	}
	return out
}
