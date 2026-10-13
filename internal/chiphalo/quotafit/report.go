package quotafit

import (
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"
)

func roundPointer(value *float64) any {
	if value == nil {
		return nil
	}
	return rounded(*value, 6)
}
func pointRows(observations []Observation, predicted map[int]float64, cfg Config) []map[string]any {
	rows := []map[string]any{}
	for i, o := range observations {
		entry := map[string]any{"cycle_id": o.Cycle, "start_ts": o.Start, "end_ts": o.End, "coverage": o.Coverage, "is_rounded": o.Rounded, "actual": o.Delta, "excluded": o.Excluded, "exclude_reason": o.ExcludeReason, "tokens": o.Tokens}
		if v, ok := predicted[i]; ok {
			entry["predicted"] = rounded(v, 6)
			entry["residual"] = rounded(o.Delta-v, 6)
		}
		rows = append(rows, entry)
	}
	limit := int(max(1, integer(cfg["max_points"], 400)))
	if len(rows) > limit {
		rows = rows[len(rows)-limit:]
	}
	return rows
}
func overallStatus(rows map[string]map[string]any, used int) string {
	order := map[string]int{"insufficient_data": 3, "poorly_identified": 2, "experimental": 1, "stable": 0}
	if len(rows) == 0 {
		return "insufficient_data"
	}
	worst := 0
	for _, entry := range rows {
		rank, ok := order[text(entry["status"])]
		if !ok {
			rank = 3
		}
		worst = max(worst, rank)
	}
	if used < 12 && worst < 3 {
		return "experimental"
	}
	for _, name := range []string{"stable", "experimental", "poorly_identified", "insufficient_data"} {
		if order[name] == worst {
			return name
		}
	}
	return "experimental"
}
func toSlice(values []float64) any {
	if values == nil {
		return nil
	}
	out := make([]any, len(values))
	for i, v := range values {
		out[i] = v
	}
	return out
}
func reportNotes(report map[string]any, s Structure) []string {
	notes := []string{}
	if len(s.Pooled) > 0 {
		notes = append(notes, "pooled estimate (shared cache/output ratios) for: "+strings.Join(s.Pooled, ", "))
	}
	if len(s.Dropped) > 0 {
		notes = append(notes, fmt.Sprintf("%d low-volume models dropped from the fit", len(s.Dropped)))
	}
	if report["status"] == "insufficient_data" {
		notes = append(notes, "not enough independent variation to identify coefficients")
	}
	regime, _ := report["regime"].(map[string]any)
	switch regime["status"] {
	case "drifting":
		notes = append(notes, fmt.Sprintf("billing regime drifting across cycles (max input drift %.2f): coefficients are not treated as stable", floating(regime["max_input_drift"], 0)))
	case "shifted":
		notes = append(notes, fmt.Sprintf("billing regime changed across cycles (max input drift %.2f): the long-run average hides a structural change", floating(regime["max_input_drift"], 0)))
	case "unknown":
		if truth(regime["cycles_compared"]) {
			notes = append(notes, "billing regime stability not testable yet: "+text(regime["reason"]))
		}
	}
	overhead, _ := report["overhead"].(map[string]any)
	if truth(overhead["recommended"]) {
		notes = append(notes, fmt.Sprintf("per-request overhead term improves the held-out fit by %.1f%%; it stays OFF by default", 100*floating(overhead["gain"], 0)))
	}
	return notes
}

// Fit preserves the full existing report contract, including outlier exclusions,
// held-out validation, pooled estimates, bootstrap confidence and regime checks.
// Epoch is supplied by the host so numerical regression fixtures are reproducible.
func Fit(observations []Observation, overrides Config, prefix, label string, epoch int64) map[string]any {
	cfg := configuration(overrides)
	usable := []Observation{}
	indices := []int{}
	cyclesMap := map[string]bool{}
	for i, o := range observations {
		if !o.Excluded {
			usable = append(usable, o)
			indices = append(indices, i)
			id := o.Cycle
			if id == "" {
				id = "unknown"
			}
			cyclesMap[id] = true
		}
	}
	cycles := []string{}
	for id := range cyclesMap {
		cycles = append(cycles, id)
	}
	slices.Sort(cycles)
	report := map[string]any{"prefix": prefix, "label": label, "algorithm": "NNLS (Lawson-Hanson); non-negativity enforced inside the fit", "billing_model_version": BillingModelVersion, "fitting_epoch": epoch, "span": observationSpan(usable), "units": map[string]string{"tokens": "million tokens", "quota": "percentage points of the weekly quota"}, "observations_total": len(observations), "cycles": cycles, "status": "insufficient_data", "models": map[string]any{}, "metrics": map[string]any{"in_sample": metrics(nil, nil), "test": nil, "split": "insufficient"}, "diagnostics": map[string]any{"observations_used": len(usable)}, "points": []map[string]any{}, "notes": []string{}}
	early := func(message string) map[string]any {
		report["notes"] = []string{message}
		report["points"] = pointRows(observations, nil, cfg)
		return report
	}
	if len(usable) < 4 {
		return early("fewer than 4 usable observations")
	}
	s := structure(usable, cfg)
	if len(s.Individual) == 0 && len(s.Pooled) == 0 {
		return early("no model with usable tokens")
	}
	fit := fitStructure(usable, s, cfg, nil, nil, nil)
	if fit == nil {
		return early("design matrix could not be solved")
	}
	residuals := make([]float64, len(usable))
	for i, row := range fit.Rows {
		residuals[i] = fit.Targets[i] - dot(row, fit.Theta)
	}
	sigma := max(madSigma(residuals), max(0, floating(cfg["outlier_sigma_floor"], .1)))
	excluded := 0
	if sigma > 0 {
		center := median(residuals)
		for i := range usable {
			z := (residuals[i] - center) / sigma
			if math.Abs(z) > floating(cfg["outlier_z"], 3.5) {
				index := indices[i]
				observations[index].Excluded = true
				observations[index].ExcludeReason = fmt.Sprintf("robust_residual_z=%.2f", z)
				excluded++
			}
		}
	}
	if excluded > 0 {
		usable = []Observation{}
		indices = []int{}
		for i, o := range observations {
			if !o.Excluded {
				usable = append(usable, o)
				indices = append(indices, i)
			}
		}
		if len(usable) >= 4 {
			s = structure(usable, cfg)
			if refit := fitStructure(usable, s, cfg, nil, nil, nil); refit != nil {
				fit = refit
			}
		}
	}
	ordered := slices.Clone(usable)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].Start < ordered[j].Start })
	var splitMetrics *Metrics
	extras := map[string]any{}
	if len(ordered) >= int(integer(cfg["min_split_observations"], 30)) {
		cut := int(max(3, float64(len(ordered))*floating(cfg["trend_split"], .8)))
		cut = min(cut, len(ordered))
		train, test := ordered[:cut], ordered[cut:]
		if len(test) >= 3 {
			trainStructure := structure(train, cfg)
			trainFit := fitStructure(train, trainStructure, cfg, nil, nil, nil)
			if trainFit != nil {
				splitMetrics = testMetrics(trainFit, test)
				extras["train_count"], extras["test_count"] = len(train), len(test)
				extras["test_span"] = &Span{Start: test[0].Start, End: test[len(test)-1].End}
			}
		}
	}
	boot := bootstrap(fit, s, usable, cfg)
	values := quantities(fit, s)
	reference := referenceModel(s, values, s.Active)
	predicted := map[int]float64{}
	for i, index := range indices {
		predicted[index] = dot(fit.Rows[i], fit.Theta)
	}
	diagnosis := designDiagnostics(fit.Rows, fit.UsedWeights, fit.Names)
	collinear, internal, correlated := map[string][]any{}, map[string][]any{}, map[string][]any{}
	for _, pair := range diagnosis.Pairs {
		left, _, _ := strings.Cut(pair.Left, ":")
		right, _, _ := strings.Cut(pair.Right, ":")
		if left == right {
			if math.Abs(pair.Correlation) >= collinearityBlocking {
				internal[left] = append(internal[left], pair)
			}
			continue
		}
		target := correlated
		if math.Abs(pair.Correlation) >= collinearityBlocking {
			target = collinear
		}
		target[left] = append(target[left], map[string]any{"model": right, "correlation": pair.Correlation})
		target[right] = append(target[right], map[string]any{"model": left, "correlation": pair.Correlation})
	}
	empty := func(values []any) []any {
		if values == nil {
			return []any{}
		}
		return values
	}
	aReference := 0.0
	if reference != nil {
		aReference = values[*reference].Input
	}
	modelRows := map[string]map[string]any{}
	for _, model := range append(slices.Clone(s.Individual), s.Pooled...) {
		q := values[model]
		stats := boot[model]
		variation := shareVariation(usable, model)
		total := s.Totals[model]
		totals := map[string]float64{"input": math.RoundToEven(total.Input), "cache": math.RoundToEven(total.Cache), "output": math.RoundToEven(total.Output), "requests": math.RoundToEven(total.Requests)}
		info := map[string]any{"source": q.Source, "active_observations": s.Active[model], "tokens": totals, "input_coefficient": rounded(q.Input, 6), "cache_coefficient": rounded(q.Cache, 6), "output_coefficient": rounded(q.Output, 6), "input_multiplier": nil, "cache_multiplier": roundPointer(q.CacheMultiplier), "output_multiplier": roundPointer(q.OutputMultiplier), "cache_ci": toSlice(stats.CacheCI), "output_ci": toSlice(stats.OutputCI), "relative_input_weight": nil, "relative_input_weight_ci": toSlice(stats.RelativeCI), "relative_input_weight_status": nil, "bootstrap_samples": stats.Samples}
		if q.Input != 0 {
			info["input_multiplier"] = 1.0
		}
		individual := slices.Contains(s.Individual, model)
		var relative *float64
		if reference != nil && model == *reference {
			relative = new(1.0)
		} else if individual && aReference > 1e-12 {
			relative = new(rounded(q.Input/aReference, 6))
		}
		if relative != nil {
			info["relative_input_weight"] = *relative
			if len(stats.RelativeCI) == 2 && math.Abs(*relative) > 1e-9 {
				width := math.Abs(stats.RelativeCI[1]-stats.RelativeCI[0]) / math.Abs(*relative)
				status := "poorly_identified"
				if width <= floating(cfg["ci_stable"], .35) {
					status = "stable"
				} else if width <= floating(cfg["ci_experimental"], 1.2) {
					status = "experimental"
				}
				info["relative_input_weight_status"] = status
			} else if *relative == 1 {
				info["relative_input_weight_status"] = "stable"
			} else {
				info["relative_input_weight_status"] = "poorly_identified"
			}
		} else if individual {
			info["relative_input_weight_status"] = "insufficient_data"
		}
		cacheVar, _ := variation["cache_share"].(*float64)
		outputVar, _ := variation["output_share"].(*float64)
		insufficient := variation["samples"].(int) >= 5 && cacheVar != nil && outputVar != nil && max(*cacheVar, *outputVar) < floating(cfg["ratio_variation_min"], .05)
		var stability any
		if stats.Stability != nil {
			stability = *stats.Stability
		}
		diag := map[string]any{"active_observations": s.Active[model], "token_totals": totals, "share_variation": variation, "bootstrap_stability": stability, "bootstrap_samples": stats.Samples, "source": q.Source, "collinear_with": empty(collinear[model]), "collinear_token_types": empty(internal[model]), "correlated_with": empty(correlated[model]), "insufficient_variation": insufficient}
		status, flags := statusFor(diag, ciWidths(info), cfg)
		if info["relative_input_weight_status"] == "poorly_identified" {
			flags = append(flags, "relative_weight_poorly_identified")
		}
		info["status"], info["status_flags"], info["diagnostics"] = status, flags, diag
		modelRows[model] = info
	}
	report["models"], report["reference_model"] = modelRows, reference
	pooled := map[string]any{"cache_multiplier": nil, "output_multiplier": nil, "models": s.Pooled}
	if fit.Ratios != nil {
		pooled["cache_multiplier"], pooled["output_multiplier"] = rounded(fit.Ratios.Cache, 6), rounded(fit.Ratios.Output, 6)
	}
	report["pooled"] = pooled
	predictions := make([]float64, len(fit.Rows))
	for i, row := range fit.Rows {
		predictions[i] = dot(row, fit.Theta)
	}
	split := "insufficient"
	if splitMetrics != nil {
		split = "time_ordered_80_20"
	}
	report["metrics"] = map[string]any{"in_sample": metrics(fit.Targets, predictions), "test": splitMetrics, "split": split}
	report["status"] = overallStatus(modelRows, len(usable))
	regime := regimeStability(usable, s, fit, cfg)
	if regime["status"] == "drifting" || regime["status"] == "shifted" {
		for _, info := range modelRows {
			flags := info["status_flags"].([]string)
			flags = append(flags, "billing_regime_"+regime["status"].(string))
			info["status_flags"] = flags
			if regime["status"] == "shifted" && (info["status"] == "stable" || info["status"] == "experimental") {
				info["status"] = "poorly_identified"
			} else if regime["status"] == "drifting" && info["status"] == "stable" {
				info["status"] = "experimental"
			}
		}
		report["status"] = overallStatus(modelRows, len(usable))
	}
	report["regime"] = regime
	overhead := map[string]any{"tested": false, "enabled": truth(cfg["request_overhead"]), "recommended": false, "reason": "diagnostic disabled"}
	if truth(cfg["request_overhead_diagnostic"]) {
		overhead = requestOverheadEffect(usable, s, fit, cfg)
	}
	report["overhead"] = overhead
	diag := map[string]any{"observations_used": len(usable), "observations_excluded": excluded, "individual_models": s.Individual, "pooled_models": s.Pooled, "models_dropped": s.Dropped, "structure_threshold": s.Threshold, "request_overhead": overhead, "billing_regime": map[string]any{"status": regime["status"], "cycles_compared": regime["cycles_compared"], "max_input_drift": regime["max_input_drift"], "threshold": regime["threshold"], "reason": regime["reason"]}, "cycles": cycles, "design": diagnosis}
	for k, v := range extras {
		diag[k] = v
	}
	report["diagnostics"] = diag
	report["points"] = pointRows(observations, predicted, cfg)
	report["notes"] = reportNotes(report, s)
	return report
}
func ReportSummary(report map[string]any) map[string]any {
	out := map[string]any{}
	for _, key := range []string{"status", "models", "metrics", "diagnostics", "cycles", "reference_model", "pooled", "notes", "billing_model_version", "fitting_epoch", "span", "regime", "overhead", "observations_total", "units", "algorithm"} {
		out[key] = report[key]
	}
	return out
}
