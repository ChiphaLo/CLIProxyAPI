// Package quotafit ports CPA's empirical quota model. It does not read token
// prices, charge requests, refresh credentials or consume reset credits.
package quotafit

import (
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"sort"
	"strconv"
	"strings"
)

const Version = "1.0.0"
const BillingModelVersion = "codex-quota-empirical/1.0.0"

var roundingSigma = math.Sqrt(1.0 / 6.0)

const rawSigma = 0.02
const collinearityBlocking = 0.999

type Config map[string]any

func DefaultConfig() Config {
	return Config{
		"window_seconds": 21600, "min_coverage": 0.5, "lag_seconds": 0, "max_observations": 4000, "min_model_observations": 8, "max_models": 10,
		"robust": true, "robust_iterations": 4, "huber_delta": 2.5, "outlier_z": 3.5, "outlier_sigma_floor": 0.1, "ridge": 1e-8, "bootstrap": 120, "seed": 20260928, "request_overhead": false,
		"ci_stable": 0.35, "ci_experimental": 1.20, "collinearity": 0.995, "ratio_variation_min": 0.05, "bootstrap_stability_min": 0.70, "trend_split": 0.8, "min_split_observations": 30, "max_points": 400,
		"regime_min_observations": 8, "regime_drift_max": 0.35, "regime_drift_blocking": 1.0, "request_overhead_diagnostic": true, "request_overhead_min_observations": 40, "request_overhead_min_gain": 0.02,
	}
}
func configuration(overrides Config) Config {
	cfg := DefaultConfig()
	for k, v := range overrides {
		if _, ok := cfg[k]; ok && v != nil {
			cfg[k] = v
		}
	}
	return cfg
}
func floating(value any, fallback float64) float64 {
	var result float64
	switch v := value.(type) {
	case float64:
		result = v
	case float32:
		result = float64(v)
	case int:
		result = float64(v)
	case int64:
		result = float64(v)
	case json.Number:
		parsed, err := v.Float64()
		if err != nil {
			return fallback
		}
		result = parsed
	case string:
		parsed, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		if err != nil {
			return fallback
		}
		result = parsed
	case bool:
		if v {
			result = 1
		}
	default:
		return fallback
	}
	if math.IsNaN(result) || math.IsInf(result, 0) {
		return fallback
	}
	return result
}
func integer(value any, fallback int64) int64 {
	switch v := value.(type) {
	case string:
		parsed, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		if err != nil {
			return fallback
		}
		return parsed
	default:
		n := floating(value, math.NaN())
		if math.IsNaN(n) || n >= math.Exp2(63) || n < -math.Exp2(63) {
			return fallback
		}
		return int64(n)
	}
}
func truth(value any) bool {
	switch v := value.(type) {
	case nil:
		return false
	case bool:
		return v
	case string:
		return v != ""
	case []any:
		return len(v) > 0
	case map[string]any:
		return len(v) > 0
	default:
		return floating(value, 0) != 0
	}
}
func text(value any) string {
	if !truth(value) {
		return ""
	}
	switch v := value.(type) {
	case string:
		return v
	case bool:
		if v {
			return "True"
		}
		return "False"
	case json.Number:
		return v.String()
	default:
		return fmt.Sprint(v)
	}
}
func rounded(value float64, digits int) float64 {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return value
	}
	v, _ := strconv.ParseFloat(strconv.FormatFloat(value, 'f', digits, 64), 64)
	return v
}
func NormalizeModel(model string, aliases map[string]string, known map[string]bool) string {
	raw := strings.TrimSpace(model)
	name := raw
	if _, base, found := strings.Cut(raw, "/"); found {
		base = strings.TrimSpace(base)
		if base != "" && (strings.HasPrefix(base, "gpt-") || known[base]) {
			name = base
		}
	}
	if aliases == nil {
		aliases = map[string]string{"gpt-5.6": "gpt-5.6-sol"}
	}
	if alias, ok := aliases[name]; ok {
		return alias
	}
	return name
}
func NormalizePercent(value any) (*float64, bool) {
	number := floating(value, -1)
	if number < 0 {
		return nil, true
	}
	whole := math.Abs(number-math.RoundToEven(number)) < 1e-9
	if !whole && number >= 0 && number <= 1 {
		number *= 100
	}
	number = max(0, min(100, number))
	return &number, whole
}

type Tokens struct {
	Input    float64 `json:"input"`
	Cache    float64 `json:"cache"`
	Output   float64 `json:"output"`
	Requests float64 `json:"requests"`
}

func (t Tokens) total() float64 { return t.Input + t.Cache + t.Output }

type Observation struct {
	Excluded      bool   `json:"excluded,omitempty"`
	ExcludeReason string `json:"exclude_reason,omitempty"`
	modelOrder    []string
	Cycle         string            `json:"cycle_id"`
	Start         int64             `json:"start_ts"`
	End           int64             `json:"end_ts"`
	Duration      int64             `json:"duration_seconds"`
	Coverage      float64           `json:"coverage"`
	Delta         float64           `json:"delta_quota"`
	Rounded       bool              `json:"is_rounded"`
	Sigma         float64           `json:"sigma"`
	Weight        float64           `json:"weight"`
	Tokens        map[string]Tokens `json:"tokens"`
	Samples       int               `json:"samples"`
	Quality       []string          `json:"quality"`
}
type Cycle struct {
	ID       string `json:"cycle_id"`
	Start    int64  `json:"start_ts"`
	End      int64  `json:"end_ts"`
	CycleEnd int64  `json:"cycle_end_ts"`
	Samples  int    `json:"samples"`
}
type Span struct {
	Start int64 `json:"start_ts"`
	End   int64 `json:"end_ts"`
}
type Diagnostics struct {
	Prefix       string         `json:"prefix"`
	Window       int64          `json:"window_seconds"`
	Lag          int64          `json:"lag_seconds"`
	Snapshots    int            `json:"snapshots"`
	Cycles       []Cycle        `json:"cycles"`
	Segments     int            `json:"segments"`
	Observations int            `json:"observations"`
	Skipped      map[string]int `json:"skipped"`
	Span         *Span          `json:"span,omitempty"`
}
type point struct {
	ts       int64
	percent  float64
	rounded  bool
	cycle    string
	cycleEnd int64
}
type tokenRow struct {
	ts     int64
	model  string
	tokens Tokens
}

// BuildObservations follows the established (start,end] token interval and
// account-local reset segmentation. It does not guess model equivalences.
func BuildObservations(prefix string, snapshots, tokenRows []any, overrides Config) ([]Observation, Diagnostics) {
	cfg := configuration(overrides)
	window := max(600, integer(cfg["window_seconds"], 21600))
	coverageMin := max(0, min(1, floating(cfg["min_coverage"], 0.5)))
	lag := integer(cfg["lag_seconds"], 0)
	diag := Diagnostics{Prefix: prefix, Window: window, Lag: lag, Cycles: []Cycle{}, Skipped: map[string]int{}}
	note := func(reason string) { diag.Skipped[reason]++ }
	points := []point{}
	for _, raw := range snapshots {
		snapshot, ok := raw.(map[string]any)
		if !ok {
			note("snapshot_invalid")
			continue
		}
		ts := integer(snapshot["ts"], 0)
		used := snapshot["raw_used_percent"]
		if used == nil || used == "" {
			used = snapshot["used_percent"]
		}
		percent, whole := NormalizePercent(used)
		if ts == 0 || percent == nil {
			note("snapshot_invalid")
			continue
		}
		if flag, exists := snapshot["is_rounded"]; exists && (flag == false || floating(flag, math.NaN()) == 0) && used != nil && used != "" {
			whole = false
		}
		cycle := "unknown"
		if start := integer(snapshot["cycle_start_ts"], 0); start > 0 {
			p := prefix
			if p == "" {
				p = "?"
			}
			cycle = fmt.Sprintf("%s:%d", p, start)
		}
		points = append(points, point{ts: ts, percent: *percent, rounded: whole, cycle: cycle, cycleEnd: integer(snapshot["cycle_end_ts"], 0)})
	}
	sort.SliceStable(points, func(i, j int) bool { return points[i].ts < points[j].ts })
	diag.Snapshots = len(points)
	observations := []Observation{}
	if len(points) == 0 {
		return observations, diag
	}
	rows := []tokenRow{}
	for _, raw := range tokenRows {
		row, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		ts := integer(row["ts"], 0)
		input, cache, output := max(0, integer(row["input"], 0)), max(0, integer(row["cached"], 0)), max(0, integer(row["output"], 0))
		if ts <= 0 || input+cache+output <= 0 {
			continue
		}
		rows = append(rows, tokenRow{ts: ts, model: text(row["model"]), tokens: Tokens{Input: float64(input), Cache: float64(cache), Output: float64(output), Requests: float64(max(0, integer(row["requests"], 1)))}})
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].ts < rows[j].ts })
	between := func(start, end int64) (map[string]Tokens, []string) {
		lo := sort.Search(len(rows), func(i int) bool { return rows[i].ts > start-lag })
		hi := sort.Search(len(rows), func(i int) bool { return rows[i].ts > end-lag })
		totals := map[string]Tokens{}
		order := []string{}
		for _, row := range rows[lo:hi] {
			if row.model == "" {
				continue
			}
			if _, ok := totals[row.model]; !ok {
				order = append(order, row.model)
			}
			t := totals[row.model]
			t.Input += row.tokens.Input
			t.Cache += row.tokens.Cache
			t.Output += row.tokens.Output
			t.Requests += row.tokens.Requests
			totals[row.model] = t
		}
		return totals, order
	}
	groups := map[string][]point{}
	order := []string{}
	for _, p := range points {
		if _, ok := groups[p.cycle]; !ok {
			order = append(order, p.cycle)
		}
		groups[p.cycle] = append(groups[p.cycle], p)
	}
	for _, id := range order {
		group := groups[id]
		end := int64(0)
		for _, p := range group {
			end = max(end, p.cycleEnd)
		}
		diag.Cycles = append(diag.Cycles, Cycle{ID: id, Start: group[0].ts, End: group[len(group)-1].ts, CycleEnd: end, Samples: len(group)})
		segments := [][]point{}
		current := []point{}
		previous := math.NaN()
		for _, p := range group {
			if !math.IsNaN(previous) && p.percent < previous-1e-9 {
				note("series_cut_on_quota_decrease")
				segments = append(segments, current)
				current = []point{}
			}
			previous = p.percent
			current = append(current, p)
		}
		if len(current) > 0 {
			segments = append(segments, current)
		}
		diag.Segments += len(segments)
		for _, samples := range segments {
			anchor := samples[0].ts
			buckets := map[int64][]point{}
			indices := []int64{}
			for _, p := range samples {
				index := (p.ts - anchor) / window
				if _, ok := buckets[index]; !ok {
					indices = append(indices, index)
				}
				buckets[index] = append(buckets[index], p)
			}
			slices.Sort(indices)
			for _, index := range indices {
				block := buckets[index]
				if len(block) < 2 {
					note("single_sample")
					continue
				}
				first, last := block[0], block[len(block)-1]
				duration := last.ts - first.ts
				if duration <= 0 {
					note("zero_duration")
					continue
				}
				coverage := min(1, float64(duration)/float64(window))
				if coverage < coverageMin {
					note("short_window")
					continue
				}
				delta := last.percent - first.percent
				if delta < -1e-9 {
					note("negative_delta")
					continue
				}
				totals, modelOrder := between(first.ts, last.ts)
				total := 0.0
				for _, t := range totals {
					total += t.total()
				}
				if total <= 0 {
					note("no_tokens")
					continue
				}
				whole := first.rounded || last.rounded
				sigma := rawSigma
				quality := []string{}
				if whole {
					sigma = roundingSigma
					quality = append(quality, "rounded_percent")
				}
				sigma = max(sigma, 1e-6)
				observations = append(observations, Observation{modelOrder: modelOrder, Cycle: id, Start: first.ts, End: last.ts, Duration: duration, Coverage: rounded(coverage, 6), Delta: delta, Rounded: whole, Sigma: sigma, Weight: coverage / (sigma * sigma), Tokens: totals, Samples: len(block), Quality: quality})
			}
		}
	}
	sort.SliceStable(observations, func(i, j int) bool { return observations[i].Start < observations[j].Start })
	cap := int(max(1, integer(cfg["max_observations"], 4000)))
	if len(observations) > cap {
		observations = observations[len(observations)-cap:]
	}
	diag.Observations = len(observations)
	if len(observations) > 0 {
		diag.Span = &Span{Start: observations[0].Start, End: observations[len(observations)-1].End}
	}
	return observations, diag
}

type LagSample struct {
	Lag          int64    `json:"lag_seconds"`
	Observations int      `json:"observations"`
	Correlation  *float64 `json:"correlation"`
}
type LagReport struct {
	Bucket  int64       `json:"bucket_seconds"`
	Samples []LagSample `json:"samples"`
	Best    *LagSample  `json:"best"`
}

func LagProfile(prefix string, snapshots, rows []any, overrides Config, lags []int64, bucket int64) LagReport {
	cfg := configuration(overrides)
	cfg["window_seconds"] = max(600, bucket)
	report := LagReport{Bucket: bucket, Samples: []LagSample{}}
	if lags == nil {
		lags = []int64{0, 300, -300, 900, -900, 1800, -1800}
	}
	for _, lag := range lags {
		cfg["lag_seconds"] = lag
		observations, _ := BuildObservations(prefix, snapshots, rows, cfg)
		q, t := []float64{}, []float64{}
		for _, o := range observations {
			q = append(q, o.Delta)
			total := 0.0
			for _, v := range o.Tokens {
				total += v.total()
			}
			t = append(t, total)
		}
		report.Samples = append(report.Samples, LagSample{Lag: lag, Observations: len(observations), Correlation: pearson(q, t)})
	}
	for i := range report.Samples {
		sample := &report.Samples[i]
		if sample.Correlation != nil && (report.Best == nil || *sample.Correlation > *report.Best.Correlation) {
			report.Best = sample
		}
	}
	return report
}
