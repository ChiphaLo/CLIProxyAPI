package quotafit

import (
	"fmt"
	"math"
	"slices"
	"sort"
)

func observationSpan(observations []Observation) *Span {
	if len(observations) == 0 {
		return nil
	}
	start, end := observations[0].Start, observations[0].End
	for _, o := range observations {
		start = min(start, o.Start)
		end = max(end, o.End)
	}
	return &Span{Start: start, End: end}
}
func testMetrics(fit *fitResult, observations []Observation) *Metrics {
	if fit == nil || len(observations) == 0 {
		return nil
	}
	rows := design(observations, fit.Spec)
	actual, predicted := make([]float64, len(rows)), make([]float64, len(rows))
	for i, row := range rows {
		actual[i] = observations[i].Delta
		predicted[i] = dot(row, fit.Theta)
	}
	return new(metrics(actual, predicted))
}
func regimeStability(observations []Observation, s Structure, fit *fitResult, cfg Config) map[string]any {
	models := append(slices.Clone(s.Individual), s.Pooled...)
	threshold, blocking := floating(cfg["regime_drift_max"], .35), floating(cfg["regime_drift_blocking"], 1)
	report := map[string]any{"status": "unknown", "reason": "", "cycles_total": 0, "cycles_compared": 0, "max_input_drift": nil, "max_ratio_drift": nil, "threshold": threshold, "blocking": blocking, "per_cycle": []map[string]any{}}
	if len(models) == 0 || fit == nil {
		report["reason"] = "no fitted model structure"
		return report
	}
	minimum := int(max(4, floating(cfg["regime_min_observations"], 8)))
	buckets := map[string][]Observation{}
	for _, o := range observations {
		label := o.Cycle
		if label == "" {
			label = "unknown"
		}
		buckets[label] = append(buckets[label], o)
	}
	report["cycles_total"] = len(buckets)
	global := quantities(fit, s)
	keys := []string{}
	for key := range buckets {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	inputDrifts, ratioDrifts := []float64{}, []float64{}
	entries := []map[string]any{}
	compared := 0
	for _, id := range keys {
		subset := buckets[id]
		entry := map[string]any{"cycle_id": id, "observations": len(subset), "comparable": false, "coefficients": map[string]float64{}, "input_drift": nil, "ratio_drift": nil, "reason": ""}
		if len(subset) < minimum {
			entry["reason"] = "too_few_observations"
			entries = append(entries, entry)
			continue
		}
		local := fitStructure(subset, s, cfg, new(false), fit.Ratios, nil)
		if local == nil {
			entry["reason"] = "design_not_solvable"
			entries = append(entries, entry)
			continue
		}
		values := quantities(local, s)
		cycleDrifts := []float64{}
		coefficients := map[string]float64{}
		for _, model := range models {
			base, value := global[model].Input, values[model].Input
			coefficients[model] = rounded(value, 6)
			if base > 1e-12 {
				cycleDrifts = append(cycleDrifts, math.Abs(value-base)/base)
			}
		}
		entry["coefficients"] = coefficients
		comparable := false
		if len(cycleDrifts) > 0 {
			comparable = true
			drift := rounded(slices.Max(cycleDrifts), 6)
			entry["input_drift"] = drift
			inputDrifts = append(inputDrifts, drift)
		}
		ratioPairs := []float64{}
		if fit.Ratios != nil {
			localCache, localOutput := 0.0, 0.0
			if local.Ratios != nil {
				localCache, localOutput = local.Ratios.Cache, local.Ratios.Output
			}
			if fit.Ratios.Cache > 1e-12 {
				ratioPairs = append(ratioPairs, math.Abs(localCache-fit.Ratios.Cache)/fit.Ratios.Cache)
			}
			if fit.Ratios.Output > 1e-12 {
				ratioPairs = append(ratioPairs, math.Abs(localOutput-fit.Ratios.Output)/fit.Ratios.Output)
			}
		}
		if len(ratioPairs) > 0 {
			drift := rounded(slices.Max(ratioPairs), 6)
			entry["ratio_drift"] = drift
			ratioDrifts = append(ratioDrifts, drift)
			if len(cycleDrifts) == 0 {
				comparable = true
			}
		}
		entry["comparable"] = comparable
		if comparable {
			compared++
		}
		entries = append(entries, entry)
	}
	report["per_cycle"] = entries
	report["cycles_compared"] = compared
	if len(inputDrifts) == 0 && len(ratioDrifts) == 0 {
		report["reason"] = "no cycle has enough comparable observations"
		return report
	}
	if compared < 2 {
		report["reason"] = "only one comparable cycle: a regime change cannot be tested yet"
		return report
	}
	worstInput, worstRatio := 0.0, 0.0
	if len(inputDrifts) > 0 {
		worstInput = slices.Max(inputDrifts)
	}
	if len(ratioDrifts) > 0 {
		worstRatio = slices.Max(ratioDrifts)
		report["max_ratio_drift"] = rounded(worstRatio, 6)
	}
	report["max_input_drift"] = rounded(worstInput, 6)
	combined := max(worstInput, worstRatio)
	status := "stable"
	if combined > blocking {
		status = "shifted"
	} else if combined > threshold {
		status = "drifting"
	}
	report["status"] = status
	report["reason"] = "per-cycle coefficients compared with the global fit; relative drift of input coefficients and of the pooled ratios"
	return report
}
func requestOverheadEffect(observations []Observation, s Structure, fit *fitResult, cfg Config) map[string]any {
	minimum := int(max(8, floating(cfg["request_overhead_min_observations"], 40)))
	gainMinimum := floating(cfg["request_overhead_min_gain"], .02)
	report := map[string]any{"tested": false, "enabled": truth(cfg["request_overhead"]), "recommended": false, "min_observations": minimum, "min_gain": gainMinimum, "reason": "", "coefficients": nil, "gain": nil, "baseline_rmse": nil, "with_requests_rmse": nil}
	ordered := slices.Clone(observations)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].Start < ordered[j].Start })
	if len(ordered) < minimum {
		report["reason"] = fmt.Sprintf("not enough observations to test a per-request term (%d < %d)", len(ordered), minimum)
		return report
	}
	cut := int(max(6, float64(len(ordered))*floating(cfg["trend_split"], .8)))
	cut = min(cut, len(ordered))
	train, test := ordered[:cut], ordered[cut:]
	if len(test) < 4 {
		report["reason"] = "not enough held-out observations"
		return report
	}
	var ratios *Ratios
	if fit != nil {
		ratios = fit.Ratios
	}
	baseline := fitStructure(train, s, cfg, nil, ratios, new(false))
	extended := fitStructure(train, s, cfg, nil, ratios, new(true))
	if baseline == nil || extended == nil {
		report["reason"] = "request-overhead design could not be solved"
		return report
	}
	baseMetrics, extendedMetrics := testMetrics(baseline, test), testMetrics(extended, test)
	if baseMetrics == nil || extendedMetrics == nil {
		report["reason"] = "held-out metrics unavailable"
		return report
	}
	baseRMSE, extendedRMSE := 0.0, 0.0
	if baseMetrics.RMSE != nil {
		baseRMSE = *baseMetrics.RMSE
	}
	if extendedMetrics.RMSE != nil {
		extendedRMSE = *extendedMetrics.RMSE
	}
	gain := 0.0
	if baseRMSE > 1e-12 {
		gain = (baseRMSE - extendedRMSE) / baseRMSE
	}
	coefficients := map[string]float64{}
	nonnegative := true
	for i, column := range extended.Spec {
		if column.Kind == "requests" {
			value := rounded(extended.Theta[i], 6)
			coefficients[column.Model] = value
			nonnegative = nonnegative && value >= 0
		}
	}
	report["tested"] = true
	report["train_observations"], report["test_observations"] = len(train), len(test)
	report["baseline_rmse"], report["with_requests_rmse"] = rounded(baseRMSE, 6), rounded(extendedRMSE, 6)
	report["gain"], report["coefficients"] = rounded(gain, 6), coefficients
	if len(coefficients) > 0 && nonnegative && gain >= gainMinimum {
		report["recommended"] = true
		report["reason"] = fmt.Sprintf("per-request term improves held-out RMSE by %.1f%%", 100*gain)
	} else if len(coefficients) == 0 {
		report["reason"] = "no per-request coefficient was produced"
	} else {
		report["reason"] = "per-request term does not improve the held-out fit by the required margin"
	}
	return report
}
