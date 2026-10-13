package webgui

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/billing"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/quotafit"
)

func (s *Server) fitConfig() (quotafit.Config, error) {
	cfg := quotafit.DefaultConfig()
	cfg["max_points"] = 150
	path := filepath.Join(s.options.Data, "quota-model.json")
	var overrides map[string]any
	if _, err := os.Lstat(path); os.IsNotExist(err) {
		return cfg, nil
	}
	if err := readJSON(path, 1024*1024, false, &overrides); err != nil {
		return nil, errors.New("quota fit configuration invalid")
	}
	for key, value := range overrides {
		if _, ok := cfg[key]; ok && value != nil {
			cfg[key] = value
		}
	}
	// Refuse resource-exhausting overrides instead of silently changing a fit.
	for _, bound := range []struct {
		Key       string
		Low, High float64
	}{{"window_seconds", 600, 604800}, {"max_observations", 1, 8000}, {"max_models", 1, 24}, {"bootstrap", 0, 400}, {"robust_iterations", 0, 16}, {"max_points", 1, 1000}} {
		value := fitNumber(cfg[bound.Key])
		if value < bound.Low || value > bound.High {
			return nil, errors.New("quota fit configuration exceeds resource bounds")
		}
	}
	return cfg, nil
}
func fitNumber(value any) float64 {
	switch v := value.(type) {
	case float64:
		return v
	case int:
		return float64(v)
	case int64:
		return float64(v)
	case *float64:
		if v != nil {
			return *v
		}
	case *int64:
		if v != nil {
			return float64(*v)
		}
	case json.Number:
		f, _ := v.Float64()
		return f
	}
	return 0
}
func fitMap(value any) map[string]any {
	v, _ := value.(map[string]any)
	if v == nil {
		return map[string]any{}
	}
	return v
}
func tailQuotaHistory(path string) ([]any, error) {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return []any{}, nil
	}
	if err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("quota history unavailable")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return nil, errors.New("quota history changed during read")
	}
	size := opened.Size()
	start := max(int64(0), size-8*1024*1024)
	// Fix the captured extent: appends must not grow this read indefinitely.
	reader := bufio.NewReaderSize(io.NewSectionReader(file, start, size-start), 64*1024)
	if start > 0 {
		// Match the Python tail convention, without allocating a partial line.
		for {
			_, err := reader.ReadSlice('\n')
			if err == bufio.ErrBufferFull {
				continue
			}
			if err == io.EOF {
				return []any{}, nil
			}
			if err != nil {
				return nil, errors.New("quota history read failed")
			}
			break
		}
	}
	rows := []any{}
	for {
		var line []byte
		var err error
		for {
			var fragment []byte
			fragment, err = reader.ReadSlice('\n')
			if len(line)+len(fragment) > 1024*1024 {
				return nil, errors.New("quota history line exceeds bound")
			}
			line = append(line, fragment...)
			if err != bufio.ErrBufferFull {
				break
			}
		}
		if len(line) > 0 {
			var row map[string]any
			if json.Unmarshal(line, &row) == nil && row != nil {
				rows = append(rows, row)
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, errors.New("quota history read failed")
		}
	}
	return rows, nil
}
func selectQuotaSnapshots(rows []any, window string, cycles int) ([]any, []map[string]any) {
	groups := map[string][]map[string]any{}
	order := []string{}
	for _, raw := range rows {
		row, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		key := stringValue(row["account_id"])
		if key == "" {
			key = stringValue(row["prefix"])
		}
		if key == "" {
			continue
		}
		if _, ok := groups[key]; !ok {
			order = append(order, key)
		}
		groups[key] = append(groups[key], row)
	}
	selected := []any{}
	summary := []map[string]any{}
	for _, key := range order {
		items := groups[key]
		sort.SliceStable(items, func(i, j int) bool { return fitNumber(items[i]["ts"]) < fitNumber(items[j]["ts"]) })
		buckets := map[int64][]map[string]any{}
		keys := []int64{}
		for _, row := range items {
			cycle := int64(fitNumber(row["cycle_start_ts"]))
			if _, ok := buckets[cycle]; !ok {
				keys = append(keys, cycle)
			}
			buckets[cycle] = append(buckets[cycle], row)
		}
		slices.Sort(keys)
		available := len(keys)
		if window != "all" && len(keys) > max(1, cycles) {
			keys = keys[len(keys)-max(1, cycles):]
		}
		samples := 0
		for _, cycle := range keys {
			for _, row := range buckets[cycle] {
				selected = append(selected, row)
			}
			samples += len(buckets[cycle])
		}
		var start any
		if len(keys) > 0 {
			start = keys[0]
		}
		summary = append(summary, map[string]any{"account": key, "cycles_available": available, "cycles_used": len(keys), "cycle_start_ts": start, "samples": samples})
	}
	return selected, summary
}
func emptyFitReport(window string, cycles int, message string, now int64) map[string]any {
	return map[string]any{"version": quotafit.Version, "generated_at": now, "window": window, "cycles_requested": cycles, "accounts": []map[string]any{}, "pooled": nil, "models": map[string]any{}, "metrics": nil, "regime": map[string]any{"status": "unknown", "cycles_compared": 0, "max_input_drift": nil}, "observations": 0, "cycles_count": 0, "cycles": []any{}, "snapshots_total": 0, "snapshots_used": 0, "billing_model_version": quotafit.BillingModelVersion, "fitting_epoch": now, "computing": false, "status": "insufficient_data", "message": message, "notes": []string{message}, "billing_basis": "independent of API Equivalent Cost; no price calibration is applied"}
}
func compactFitPoints(points []map[string]any) []map[string]any {
	if len(points) > 150 {
		points = points[len(points)-150:]
	}
	result := []map[string]any{}
	for _, point := range points {
		entry := map[string]any{}
		for _, key := range []string{"cycle_id", "start_ts", "end_ts", "coverage", "is_rounded", "actual", "predicted", "residual", "excluded", "exclude_reason"} {
			entry[key] = point[key]
		}
		result = append(result, entry)
	}
	return result
}
func fitStatusRank(status any) int {
	order := map[string]int{"stable": 0, "experimental": 1, "poorly_identified": 2, "insufficient_data": 3}
	if n, ok := order[stringValue(status)]; ok {
		return n
	}
	return 3
}
func modelTable(value any) map[string]map[string]any {
	if rows, ok := value.(map[string]map[string]any); ok {
		return rows
	}
	out := map[string]map[string]any{}
	for key, value := range fitMap(value) {
		out[key] = fitMap(value)
	}
	return out
}
func mergeFitModels(reports []map[string]any) map[string]any {
	merged := map[string]any{}
	for _, entry := range reports {
		prefix := stringValue(entry["prefix"])
		fit := fitMap(entry["fit"])
		used := fitMap(fit["diagnostics"])["observations_used"]
		for model, info := range modelTable(fit["models"]) {
			current, exists := merged[model]
			row := fitMap(current)
			if !exists {
				row = shallowCopy(info)
				row["accounts"] = []string{}
				row["confidence"] = info["status"]
				row["observations"] = used
				merged[model] = row
			}
			accounts := row["accounts"].([]string)
			if prefix != "" && !slices.Contains(accounts, prefix) {
				accounts = append(accounts, prefix)
				row["accounts"] = accounts
			}
			if fitStatusRank(info["status"]) > fitStatusRank(row["confidence"]) {
				row = shallowCopy(info)
				row["accounts"] = accounts
				row["confidence"] = info["status"]
				row["observations"] = used
				merged[model] = row
			}
		}
	}
	return merged
}
func shallowCopy(source map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range source {
		out[k] = v
	}
	return out
}
func mergeFitRegime(reports []map[string]any) map[string]any {
	order := map[string]int{"unknown": 0, "stable": 1, "drifting": 2, "shifted": 3}
	worst := "unknown"
	cycles := 0
	var drift *float64
	for _, entry := range reports {
		regime := fitMap(fitMap(entry["fit"])["regime"])
		status := stringValue(regime["status"])
		if order[status] > order[worst] {
			worst = status
		}
		cycles = max(cycles, int(fitNumber(regime["cycles_compared"])))
		if v := regime["max_input_drift"]; v != nil {
			value := fitNumber(v)
			if drift == nil || value > *drift {
				drift = new(value)
			}
		}
	}
	var value any
	if drift != nil {
		value = *drift
	}
	return map[string]any{"status": worst, "cycles_compared": cycles, "max_input_drift": value}
}
func mergeFitMetrics(reports []map[string]any, pooled map[string]any) any {
	source := "pooled"
	var metrics map[string]any
	if pooled != nil {
		metrics = fitMap(pooled["metrics"])
	} else {
		best := -1
		for _, entry := range reports {
			fit := fitMap(entry["fit"])
			used := int(fitNumber(fitMap(fit["diagnostics"])["observations_used"]))
			if used > best {
				best = used
				source = "account:" + stringValue(entry["prefix"])
				metrics = fitMap(fit["metrics"])
			}
		}
	}
	if metrics == nil {
		return nil
	}
	return map[string]any{"source": source, "in_sample": metrics["in_sample"], "test": metrics["test"], "split": metrics["split"]}
}
func combinedFitStatus(reports []map[string]any) string {
	if len(reports) == 0 {
		return "insufficient_data"
	}
	worst := 0
	for _, entry := range reports {
		worst = max(worst, fitStatusRank(fitMap(entry["fit"])["status"]))
	}
	for _, name := range []string{"stable", "experimental", "poorly_identified", "insufficient_data"} {
		if fitStatusRank(name) == worst {
			return name
		}
	}
	return "experimental"
}
func aggregateFitNotes(report map[string]any, reports []map[string]any) []string {
	notes := []string{}
	used, obs := fitNumber(report["snapshots_used"]), fitNumber(report["observations"])
	if used == 0 {
		notes = append(notes, "no quota snapshots inside the requested window")
	}
	if len(reports) == 0 {
		notes = append(notes, "no OAuth account has quota snapshots yet")
	}
	if obs > 0 && obs < 24 {
		notes = append(notes, "few observations: confidence intervals will be wide")
	}
	cfg := fitMap(report["config"])
	window := fitNumber(cfg["window_seconds"])
	if window != 0 {
		notes = append(notes, fmt.Sprintf("aggregation window %d s", int64(window)))
	}
	if used > 0 && obs == 0 {
		coverage := fitNumber(cfg["min_coverage"])
		if coverage == 0 {
			coverage = .5
		}
		skipped := report["snapshot_skipped"].(map[string]int)
		keys := []string{}
		for key := range skipped {
			keys = append(keys, key)
		}
		slices.Sort(keys)
		parts := []string{}
		for _, key := range keys {
			parts = append(parts, fmt.Sprintf("%s=%d", key, skipped[key]))
		}
		detail := strings.Join(parts, ", ")
		if detail == "" {
			detail = "no skipped bucket"
		}
		notes = append(notes, fmt.Sprintf("snapshots recorded but no delta interval is usable yet (%s); one delta needs about %d s of snapshot span", detail, int64(window*coverage)))
	}
	regime := fitMap(report["regime"])
	if regime["status"] == "drifting" {
		notes = append(notes, "billing regime drifting across cycles")
	} else if regime["status"] == "shifted" {
		notes = append(notes, "billing regime changed across cycles: the long-run average is not stable")
	}
	for _, entry := range reports {
		overhead := fitMap(fitMap(entry["fit"])["overhead"])
		if overhead["recommended"] == true {
			notes = append(notes, "account "+stringValue(entry["prefix"])+": a per-request overhead term measurably improves the held-out fit (kept OFF by default)")
			break
		}
	}
	return notes
}

func (s *Server) computeQuotaFit(window string, cycles int) (map[string]any, error) {
	now := s.options.Now().Unix()
	cfg, err := s.fitConfig()
	if err != nil {
		return nil, err
	}
	history := s.options.History
	if history == "" {
		history = filepath.Join(filepath.Dir(s.options.Quota), "oauth-quota-history.jsonl")
	}
	rows, err := tailQuotaHistory(history)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return emptyFitReport(window, cycles, "no quota snapshots recorded yet", now), nil
	}
	selected, cycleSummary := selectQuotaSnapshots(rows, window, cycles)
	if len(selected) == 0 {
		return emptyFitReport(window, cycles, "no quota snapshots inside the requested window", now), nil
	}
	var snapshot struct {
		Accounts []json.RawMessage `json:"accounts"`
	}
	if readJSON(s.options.Quota, 4*1024*1024, false, &snapshot) != nil || snapshot.Accounts == nil {
		return nil, errors.New("quota account snapshot unavailable")
	}
	accounts := []billing.Account{}
	metadata := []map[string]any{}
	for _, raw := range snapshot.Accounts {
		var a billing.Account
		var m map[string]any
		if json.Unmarshal(raw, &a) != nil || json.Unmarshal(raw, &m) != nil || a.ID == "" {
			return nil, errors.New("invalid quota account identity")
		}
		accounts = append(accounts, a)
		metadata = append(metadata, m)
	}
	var exposed struct {
		Models []string `json:"models"`
	}
	if readJSON(s.options.Exposed, 1024*1024, false, &exposed) != nil {
		return nil, errors.New("OAuth model list unavailable")
	}
	models := map[string]bool{}
	for _, id := range exposed.Models {
		models[id] = true
	}
	start, end := int64(fitNumber(fitMap(selected[0])["ts"])), int64(fitNumber(fitMap(selected[0])["ts"]))+1
	for _, raw := range selected {
		stamp := int64(fitNumber(fitMap(raw)["ts"]))
		start = min(start, stamp)
		end = max(end, stamp+1)
	}
	tokenRows := map[string][]any{}
	// API price cost is collected separately for side-by-side display, never
	// passed as a feature, target, weight or calibration of the quota engine.
	var priceSums map[string]billing.Summary
	prices, priceErr := s.prices()
	marks := map[string]int64{}
	markPath := filepath.Join(s.options.Data, "oauth-billing-resets.json")
	if _, e := os.Lstat(markPath); e == nil {
		if readJSON(markPath, 1024*1024, false, &marks) != nil {
			priceErr = errors.New("billing reset marks invalid")
		}
	}
	acc := billing.NewAccumulator(accounts, models, prices, now, marks)
	if err := scanUsage(s.options.Usage, func(row billing.Row, _ map[string]any) error {
		if priceErr == nil {
			acc.Add(row)
		}
		if row.TS < start || row.TS >= end {
			return nil
		}
		account, ok := billing.AccountForRow(row, accounts, models)
		if !ok {
			return nil
		}
		total := max(row.Input, 0)
		cache := min(total, max(row.Cached, 0))
		output := max(row.Output, 0)
		if total+output <= 0 {
			return nil
		}
		model := quotafit.NormalizeModel(row.Model, nil, models)
		if model == "" {
			return nil
		}
		tokenRows[account.ID] = append(tokenRows[account.ID], map[string]any{"ts": row.TS, "model": model, "input": total - cache, "cached": cache, "output": output, "requests": 1})
		return nil
	}); err != nil {
		return nil, err
	}
	for _, bucket := range tokenRows {
		sort.SliceStable(bucket, func(i, j int) bool { return fitNumber(fitMap(bucket[i])["ts"]) < fitNumber(fitMap(bucket[j])["ts"]) })
	}
	if priceErr == nil {
		priceSums = acc.Summaries()
	}
	reports := []map[string]any{}
	pooledObservations := []quotafit.Observation{}
	observationsTotal := 0
	cycleIDs := map[string]bool{}
	costTotal := 0.0
	skipped := map[string]int{}
	for index, account := range accounts {
		snaps := []any{}
		for _, raw := range selected {
			item := fitMap(raw)
			key := stringValue(item["account_id"])
			if key == "" {
				key = stringValue(item["prefix"])
			}
			if key == account.ID || key == account.Prefix {
				snaps = append(snaps, item)
			}
		}
		if len(snaps) == 0 {
			continue
		}
		tokens := tokenRows[account.ID]
		observations, diag := quotafit.BuildObservations(account.Prefix, snaps, tokens, cfg)
		label := stringValue(metadata[index]["label"])
		fit := quotafit.Fit(observations, cfg, account.Prefix, label, now)
		quota := fitMap(metadata[index]["quota"])
		primary := fitMap(fitMap(quota["rate_limit"])["primary_window"])
		cycle, available := billing.WeeklyCycle(account, now, 0)
		var cycleStart, cycleEnd any
		if available {
			cycleStart, cycleEnd = cycle.Start, cycle.End
		}
		predicted, actual := 0.0, 0.0
		points := fit["points"].([]map[string]any)
		for _, point := range points {
			if point["excluded"] == true || point["predicted"] == nil {
				continue
			}
			predicted += fitNumber(point["predicted"])
			actual += fitNumber(point["actual"])
		}
		var price any
		billingAvailable := priceSums != nil
		if billingAvailable {
			price = priceSums[account.ID].CostUSD
			costTotal += price.(float64)
		}
		pooledObservations = append(pooledObservations, observations...)
		observationsTotal += len(observations)
		for _, id := range fit["cycles"].([]string) {
			cycleIDs[id] = true
		}
		for key, value := range diag.Skipped {
			skipped[key] += value
		}
		reports = append(reports, map[string]any{"account_id": account.ID, "prefix": account.Prefix, "label": metadata[index]["label"], "plan_type": quota["plan_type"], "observed_used_percent": primary["used_percent"], "cycle_start_ts": cycleStart, "cycle_end_ts": cycleEnd, "snapshots": len(snaps), "observations": len(observations), "span": diag.Span, "window_seconds": cfg["window_seconds"], "observed_change_pp": billing.RoundDecimal(actual, 6), "predicted_change_pp": billing.RoundDecimal(predicted, 6), "residual_pp": billing.RoundDecimal(actual-predicted, 6), "api_cost_usd": price, "api_billing_available": billingAvailable, "lag": quotafit.LagProfile(account.Prefix, snaps, tokens, cfg, nil, 3600), "snapshot_diagnostics": diag, "fit": quotafit.ReportSummary(fit), "points": compactFitPoints(points)})
	}
	var pooled map[string]any
	if len(pooledObservations) >= 4 {
		pooled = quotafit.Fit(pooledObservations, cfg, "pooled", "all OAuth accounts", now)
	}
	configView := map[string]any{}
	for _, key := range []string{"window_seconds", "min_coverage", "lag_seconds", "min_model_observations", "max_models", "bootstrap", "robust", "request_overhead", "outlier_z", "ci_stable", "ci_experimental", "collinearity"} {
		configView[key] = cfg[key]
	}
	var pooledSummary any
	if pooled != nil {
		pooledSummary = quotafit.ReportSummary(pooled)
	}
	report := map[string]any{"version": quotafit.Version, "generated_at": now, "window": window, "cycles_requested": cycles, "config": configView, "algorithm": "NNLS (Lawson-Hanson); non-negativity enforced in the fit", "snapshot_source": "CPA quota collector (chatgpt.com/backend-api/wham/usage)", "billing_basis": "independent of API Equivalent Cost; no price calibration is applied", "snapshots_total": len(rows), "snapshots_used": len(selected), "cycles": cycleSummary, "accounts": reports, "pooled": pooledSummary, "computing": false, "status": combinedFitStatus(reports), "observations": observationsTotal, "cycles_count": len(cycleIDs), "metrics": mergeFitMetrics(reports, pooled), "models": mergeFitModels(reports), "regime": mergeFitRegime(reports), "billing_model_version": quotafit.BillingModelVersion, "fitting_epoch": now, "units": map[string]string{"tokens": "million tokens", "quota": "percentage points of the weekly quota"}, "side_by_side": map[string]any{"api_equivalent_cost_usd": billing.RoundDecimal(costTotal, 6), "note": "API Equivalent Cost is computed from configured API prices and is NOT used to calibrate the empirical quota coefficients"}, "snapshot_skipped": skipped}
	report["notes"] = aggregateFitNotes(report, reports)
	return report, nil
}
