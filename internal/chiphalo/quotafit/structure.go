package quotafit

import (
	"encoding/json"
	"math"
	"slices"
	"sort"
	"strings"
)

type Column struct {
	Model        string  `json:"model"`
	Kind         string  `json:"kind"`
	CacheWeight  float64 `json:"cache_weight,omitempty"`
	OutputWeight float64 `json:"output_weight,omitempty"`
}
type Dropped struct {
	Model  string `json:"model"`
	Tokens Tokens `json:"tokens"`
}
type Structure struct {
	Totals     map[string]Tokens `json:"totals"`
	Active     map[string]int    `json:"active"`
	Individual []string          `json:"individual"`
	Pooled     []string          `json:"pooled"`
	Dropped    []Dropped         `json:"dropped"`
	Threshold  int               `json:"threshold"`
}
type Ratios struct {
	Cache  float64   `json:"r_cache"`
	Output float64   `json:"r_output"`
	Theta  []float64 `json:"theta"`
	Models []string  `json:"models"`
	Score  float64   `json:"score"`
}
type fitResult struct {
	Spec        []Column    `json:"spec"`
	Names       []string    `json:"names"`
	Rows        [][]float64 `json:"rows"`
	Targets     []float64   `json:"targets"`
	Weights     []float64   `json:"weights"`
	UsedWeights []float64   `json:"used_weights"`
	Theta       []float64   `json:"theta"`
	Ratios      *Ratios     `json:"ratios"`
	Overhead    bool        `json:"request_overhead"`
}
type Quantity struct {
	Input            float64  `json:"input_coefficient"`
	Cache            float64  `json:"cache_coefficient"`
	Output           float64  `json:"output_coefficient"`
	Request          float64  `json:"request_coefficient"`
	CacheMultiplier  *float64 `json:"cache_multiplier"`
	OutputMultiplier *float64 `json:"output_multiplier"`
	Source           string   `json:"source"`
}

func modelKeys(o Observation) []string {
	if len(o.modelOrder) > 0 {
		return o.modelOrder
	}
	keys := []string{}
	for key := range o.Tokens {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}
func modelTotals(observations []Observation) (map[string]Tokens, map[string]int, []string) {
	totals := map[string]Tokens{}
	active := map[string]int{}
	order := []string{}
	for _, o := range observations {
		for _, model := range modelKeys(o) {
			v := o.Tokens[model]
			if _, ok := totals[model]; !ok {
				order = append(order, model)
			}
			t := totals[model]
			t.Input += v.Input
			t.Cache += v.Cache
			t.Output += v.Output
			t.Requests += v.Requests
			totals[model] = t
			if v.Input > 0 || v.Cache > 0 || v.Output > 0 {
				active[model]++
			}
		}
	}
	return totals, active, order
}
func structure(observations []Observation, cfg Config) Structure {
	totals, active, order := modelTotals(observations)
	sort.SliceStable(order, func(i, j int) bool { return totals[order[i]].total() > totals[order[j]].total() })
	cap := int(max(1, integer(cfg["max_models"], 10)))
	minimum := int(max(3, integer(cfg["min_model_observations"], 8)))
	s := Structure{Totals: totals, Active: active, Individual: []string{}, Pooled: []string{}, Dropped: []Dropped{}, Threshold: minimum}
	for i, model := range order {
		if i >= cap {
			s.Dropped = append(s.Dropped, Dropped{Model: model, Tokens: totals[model]})
			continue
		}
		if active[model] >= minimum {
			s.Individual = append(s.Individual, model)
		} else {
			s.Pooled = append(s.Pooled, model)
		}
	}
	return s
}
func specFor(individual, pooled []string, cache, output float64, overhead bool) ([]Column, []string) {
	spec := []Column{}
	names := []string{}
	for _, model := range individual {
		for _, kind := range []string{"input", "cache", "output"} {
			spec = append(spec, Column{Model: model, Kind: kind})
		}
		if overhead {
			spec = append(spec, Column{Model: model, Kind: "requests"})
		}
	}
	for _, model := range pooled {
		spec = append(spec, Column{Model: model, Kind: "pooled", CacheWeight: cache, OutputWeight: output})
	}
	for _, column := range spec {
		names = append(names, column.Model+":"+column.Kind)
	}
	return spec, names
}
func design(observations []Observation, spec []Column) [][]float64 {
	rows := make([][]float64, len(observations))
	for i, o := range observations {
		row := make([]float64, len(spec))
		for j, column := range spec {
			v := o.Tokens[column.Model]
			value := 0.0
			switch column.Kind {
			case "input":
				value = v.Input
			case "cache":
				value = v.Cache
			case "output":
				value = v.Output
			case "requests":
				value = v.Requests
			case "pooled":
				value = v.Input + column.CacheWeight*v.Cache + column.OutputWeight*v.Output
			}
			row[j] = value / 1e6
		}
		rows[i] = row
	}
	return rows
}
func targetsWeights(observations []Observation) ([]float64, []float64) {
	targets, weights := make([]float64, len(observations)), make([]float64, len(observations))
	for i, o := range observations {
		targets[i], weights[i] = o.Delta, o.Weight
	}
	return targets, weights
}

type pooledScore struct {
	score                   float64
	theta, targets, weights []float64
	cache, output           float64
}

func scorePooled(observations []Observation, models []string, cache, output float64, cfg Config) *pooledScore {
	spec, _ := specFor(nil, models, cache, output, false)
	rows := design(observations, spec)
	targets, weights := targetsWeights(observations)
	theta, _ := solve(rows, targets, weights, cfg, new(false), nil)
	if theta == nil {
		return nil
	}
	score := 0.0
	for i, row := range rows {
		r := targets[i] - dot(row, theta)
		score += weights[i] * r * r
	}
	return &pooledScore{score: score, theta: theta, targets: targets, weights: weights}
}
func refinePooled(observations []Observation, models []string, cfg Config, start [2]float64, rounds int) *pooledScore {
	cache, output := start[0], start[1]
	best := scorePooled(observations, models, cache, output, cfg)
	if best == nil {
		return nil
	}
	for iteration := 0; iteration < max(1, rounds); iteration++ {
		rows := make([][]float64, len(observations))
		targets := make([]float64, len(observations))
		for i, o := range observations {
			base, cached, out := 0.0, 0.0, 0.0
			for position, model := range models {
				v := o.Tokens[model]
				scale := 0.0
				if position < len(best.theta) {
					scale = best.theta[position]
				}
				base += scale * v.Input / 1e6
				cached += scale * v.Cache / 1e6
				out += scale * v.Output / 1e6
			}
			rows[i] = []float64{cached, out}
			targets[i] = best.targets[i] - base
		}
		ratio, _ := solve(rows, targets, best.weights, cfg, new(false), nil)
		if len(ratio) < 2 {
			break
		}
		candidate := scorePooled(observations, models, ratio[0], ratio[1], cfg)
		if candidate == nil || candidate.score >= best.score-1e-12 {
			break
		}
		best = candidate
		cache, output = ratio[0], ratio[1]
	}
	best.cache, best.output = cache, output
	return best
}
func fitPooledRatios(observations []Observation, models []string, cfg Config, starts [][2]float64) *Ratios {
	if len(observations) == 0 || len(models) == 0 {
		return nil
	}
	if len(starts) == 0 {
		starts = [][2]float64{{1, 1}, {.1, 4}, {.01, 1}, {1, 8}, {.3, 2}, {.05, 12}}
	}
	var best *pooledScore
	for _, start := range starts {
		candidate := refinePooled(observations, models, cfg, start, 8)
		if candidate != nil && (best == nil || candidate.score < best.score) {
			best = candidate
		}
	}
	if best == nil {
		return nil
	}
	return &Ratios{Cache: best.cache, Output: best.output, Theta: best.theta, Models: slices.Clone(models), Score: best.score}
}
func fitStructure(observations []Observation, s Structure, cfg Config, robust *bool, ratios *Ratios, requestOverhead *bool) *fitResult {
	overhead := truth(cfg["request_overhead"])
	if requestOverhead != nil {
		overhead = *requestOverhead
	}
	if len(s.Pooled) > 0 && ratios == nil {
		ratios = fitPooledRatios(observations, append(slices.Clone(s.Individual), s.Pooled...), cfg, nil)
	}
	if len(s.Pooled) > 0 && ratios == nil {
		return nil
	}
	cache, output := 0.0, 0.0
	if ratios != nil {
		cache, output = ratios.Cache, ratios.Output
	}
	spec, names := specFor(s.Individual, s.Pooled, cache, output, overhead)
	if len(spec) == 0 {
		return nil
	}
	rows := design(observations, spec)
	targets, weights := targetsWeights(observations)
	theta, used := solve(rows, targets, weights, cfg, robust, nil)
	if theta == nil {
		return nil
	}
	return &fitResult{Spec: spec, Names: names, Rows: rows, Targets: targets, Weights: weights, UsedWeights: used, Theta: theta, Ratios: ratios, Overhead: overhead}
}
func quantities(fit *fitResult, s Structure) map[string]Quantity {
	out := map[string]Quantity{}
	for i, column := range fit.Spec {
		q := out[column.Model]
		switch column.Kind {
		case "input", "pooled":
			q.Input = fit.Theta[i]
		case "cache":
			q.Cache = fit.Theta[i]
		case "output":
			q.Output = fit.Theta[i]
		default:
			q.Request = fit.Theta[i]
		}
		out[column.Model] = q
	}
	for model, q := range out {
		if slices.Contains(s.Individual, model) {
			if q.Input > 1e-12 {
				q.CacheMultiplier = new(q.Cache / q.Input)
				q.OutputMultiplier = new(q.Output / q.Input)
			}
			q.Source = "own"
		} else {
			if fit.Ratios != nil {
				q.CacheMultiplier = new(fit.Ratios.Cache)
				q.OutputMultiplier = new(fit.Ratios.Output)
			}
			q.Source = "pooled"
		}
		out[model] = q
	}
	return out
}
func referenceModel(s Structure, values map[string]Quantity, active map[string]int) *string {
	candidates, preferred := []string{}, []string{}
	for _, model := range s.Individual {
		if values[model].Input > 1e-12 {
			candidates = append(candidates, model)
			if strings.Contains(strings.ToLower(model), "luna") {
				preferred = append(preferred, model)
			}
		}
	}
	if len(candidates) == 0 {
		return nil
	}
	pool := candidates
	if len(preferred) > 0 {
		pool = preferred
	}
	sort.Slice(pool, func(i, j int) bool {
		if active[pool[i]] != active[pool[j]] {
			return active[pool[i]] > active[pool[j]]
		}
		return pool[i] < pool[j]
	})
	return new(pool[0])
}
func shareVariation(observations []Observation, model string) map[string]any {
	cache, output := []float64{}, []float64{}
	for _, o := range observations {
		v, ok := o.Tokens[model]
		if !ok || v.total() <= 0 {
			continue
		}
		cache = append(cache, v.Cache/v.total())
		output = append(output, v.Output/v.total())
	}
	return map[string]any{"cache_share": coefficientVariation(cache), "output_share": coefficientVariation(output), "samples": len(cache)}
}
func ciWidths(info map[string]any) map[string]float64 {
	widths := map[string]float64{}
	for _, kind := range []string{"cache", "output"} {
		ci, ok := info[kind+"_ci"].([]any)
		value := floating(info[kind+"_multiplier"], 0)
		if ok && len(ci) == 2 && value != 0 {
			widths[kind] = math.Abs(floating(ci[1], 0)-floating(ci[0], 0)) / math.Abs(value)
		}
	}
	return widths
}
func statusFor(entry map[string]any, widths map[string]float64, cfg Config) (string, []string) {
	active := integer(entry["active_observations"], 0)
	flags := []string{}
	if entry["source"] == "pooled" {
		flags = append(flags, "estimated_from_pooled_data")
	}
	if active < 3 {
		return "insufficient_data", []string{"no_active_observations"}
	}
	if truth(entry["insufficient_variation"]) {
		flags = append(flags, "insufficient_variation")
	}
	for _, key := range []string{"collinear_with", "correlated_with"} {
		if pairs, ok := entry[key].([]any); ok {
			for _, p := range pairs {
				if pair, ok := p.(map[string]any); ok {
					flags = append(flags, key+":"+text(pair["model"]))
				}
			}
		}
		if key == "collinear_with" && truth(entry["collinear_token_types"]) {
			flags = append(flags, "collinear_token_types")
		}
	}
	if v, ok := entry["bootstrap_stability"]; ok && v != nil && floating(v, 0) < floating(cfg["bootstrap_stability_min"], .7) {
		flags = append(flags, "unstable_bootstrap")
	}
	worst := 0.0
	for _, v := range widths {
		worst = max(worst, v)
	}
	if worst > floating(cfg["ci_experimental"], 1.2) {
		flags = append(flags, "wide_confidence_interval")
	}
	blocking := map[string]bool{"insufficient_variation": true, "unstable_bootstrap": true, "wide_confidence_interval": true, "collinear_with": true, "collinear_token_types": true}
	for _, flag := range flags {
		base, _, _ := strings.Cut(flag, ":")
		if blocking[base] {
			return "poorly_identified", flags
		}
	}
	if entry["source"] == "pooled" {
		return "experimental", flags
	}
	for _, flag := range flags {
		if strings.HasPrefix(flag, "correlated_with:") {
			return "experimental", flags
		}
	}
	stability, exists := entry["bootstrap_stability"]
	if worst > 0 && worst <= floating(cfg["ci_stable"], .35) && (!exists || stability == nil || floating(stability, 0) >= .9) {
		return "stable", flags
	}
	return "experimental", flags
}

// Preserve Python dictionary insertion order for tied model totals. The order is
// internal metadata and does not add a field to observations or GUI reports.
func (o *Observation) UnmarshalJSON(raw []byte) error {
	type alias Observation
	var value alias
	if err := json.Unmarshal(raw, &value); err != nil {
		return err
	}
	*o = Observation(value)
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	decoder := json.NewDecoder(strings.NewReader(string(fields["tokens"])))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return nil
	}
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			return err
		}
		var ignored json.RawMessage
		if err := decoder.Decode(&ignored); err != nil {
			return err
		}
		o.modelOrder = append(o.modelOrder, key.(string))
	}
	return nil
}
