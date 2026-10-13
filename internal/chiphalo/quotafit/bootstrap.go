package quotafit

import (
	"math"
	"math/bits"
	"slices"
	"sort"
	"strings"
)

// pythonRandom reproduces CPython's integer-seeded MT19937/getrandbits sampler,
// making bootstrap resamples comparable across the language-only migration.
type pythonRandom struct {
	state [624]uint32
	index int
}

func newPythonRandom(seed int64) *pythonRandom {
	value := uint64(seed)
	if seed < 0 {
		value = uint64(-(seed + 1)) + 1
	}
	key := []uint32{uint32(value)}
	if value>>32 != 0 {
		key = append(key, uint32(value>>32))
	}
	r := &pythonRandom{index: 624}
	r.state[0] = 19650218
	for i := 1; i < 624; i++ {
		r.state[i] = 1812433253*(r.state[i-1]^(r.state[i-1]>>30)) + uint32(i)
	}
	i, j := 1, 0
	for k := max(624, len(key)); k > 0; k-- {
		r.state[i] = (r.state[i] ^ ((r.state[i-1] ^ (r.state[i-1] >> 30)) * 1664525)) + key[j] + uint32(j)
		i++
		j++
		if i >= 624 {
			r.state[0] = r.state[623]
			i = 1
		}
		if j >= len(key) {
			j = 0
		}
	}
	for k := 623; k > 0; k-- {
		r.state[i] = (r.state[i] ^ ((r.state[i-1] ^ (r.state[i-1] >> 30)) * 1566083941)) - uint32(i)
		i++
		if i >= 624 {
			r.state[0] = r.state[623]
			i = 1
		}
	}
	r.state[0] = 0x80000000
	return r
}
func (r *pythonRandom) next() uint32 {
	if r.index >= 624 {
		for i := 0; i < 624; i++ {
			y := (r.state[i] & 0x80000000) | (r.state[(i+1)%624] & 0x7fffffff)
			r.state[i] = r.state[(i+397)%624] ^ (y >> 1)
			if y&1 != 0 {
				r.state[i] ^= 0x9908b0df
			}
		}
		r.index = 0
	}
	y := r.state[r.index]
	r.index++
	y ^= y >> 11
	y ^= (y << 7) & 0x9d2c5680
	y ^= (y << 15) & 0xefc60000
	y ^= y >> 18
	return y
}
func (r *pythonRandom) below(n int) int {
	k := bits.Len(uint(n))
	for {
		value := int(r.next() >> uint(32-k))
		if value < n {
			return value
		}
	}
}

type BootstrapEntry struct {
	Samples         int       `json:"samples"`
	CacheSamples    int       `json:"cache_samples"`
	OutputSamples   int       `json:"output_samples"`
	CacheCI         []float64 `json:"cache_ci"`
	OutputCI        []float64 `json:"output_ci"`
	RelativeCI      []float64 `json:"relative_ci"`
	RelativeSamples int       `json:"relative_samples"`
	Stability       *float64  `json:"stability"`
	RelativeWeight  *float64  `json:"relative_weight,omitempty"`
}

func interval(values []float64) []float64 {
	if len(values) < 5 {
		return nil
	}
	return []float64{rounded(*percentile(values, .025), 6), rounded(*percentile(values, .975), 6)}
}
func bootstrap(fit *fitResult, s Structure, observations []Observation, cfg Config) map[string]BootstrapEntry {
	runs := int(max(0, integer(cfg["bootstrap"], 120)))
	n := len(observations)
	out := map[string]BootstrapEntry{}
	if runs <= 0 || n < 4 {
		return out
	}
	if n > 400 {
		runs = min(runs, 80)
	}
	rng := newPythonRandom(integer(cfg["seed"], 20260928))
	models := append(slices.Clone(s.Individual), s.Pooled...)
	type samples struct{ cache, output, input, relative []float64 }
	buckets := map[string]*samples{}
	for _, model := range models {
		buckets[model] = &samples{cache: []float64{}, output: []float64{}, input: []float64{}, relative: []float64{}}
	}
	reference := []string{}
	for _, model := range s.Individual {
		if strings.Contains(strings.ToLower(model), "luna") {
			reference = append(reference, model)
		}
	}
	if len(reference) == 0 {
		reference = slices.Clone(s.Individual)
	}
	sort.Slice(reference, func(i, j int) bool {
		if s.Active[reference[i]] != s.Active[reference[j]] {
			return s.Active[reference[i]] > s.Active[reference[j]]
		}
		return reference[i] < reference[j]
	})
	ref := ""
	if len(reference) > 0 {
		ref = reference[0]
	}
	completed := 0
	for iteration := 0; iteration < runs; iteration++ {
		resampled := make([]Observation, n)
		for i := range resampled {
			resampled[i] = observations[rng.below(n)]
		}
		var ratios *Ratios
		if len(s.Pooled) > 0 {
			start := [2]float64{1, 1}
			if fit.Ratios != nil {
				start = [2]float64{fit.Ratios.Cache, fit.Ratios.Output}
			}
			ratios = fitPooledRatios(resampled, models, cfg, [][2]float64{start, {1, 1}})
			if ratios == nil {
				continue
			}
		}
		result := fitStructure(resampled, s, cfg, new(false), ratios, nil)
		if result == nil {
			continue
		}
		completed++
		values := quantities(result, s)
		for _, model := range models {
			q := values[model]
			bucket := buckets[model]
			if q.CacheMultiplier != nil {
				bucket.cache = append(bucket.cache, *q.CacheMultiplier)
			}
			if q.OutputMultiplier != nil {
				bucket.output = append(bucket.output, *q.OutputMultiplier)
			}
			bucket.input = append(bucket.input, q.Input)
		}
		if ref != "" {
			if base, ok := values[ref]; ok && base.Input > 1e-12 {
				for _, model := range models {
					if model == ref {
						buckets[model].relative = append(buckets[model].relative, 1)
					} else if slices.Contains(s.Individual, model) {
						buckets[model].relative = append(buckets[model].relative, values[model].Input/base.Input)
					}
				}
			}
		}
	}
	for _, model := range models {
		bucket := buckets[model]
		slices.Sort(bucket.cache)
		slices.Sort(bucket.output)
		slices.Sort(bucket.relative)
		entry := BootstrapEntry{Samples: completed, CacheSamples: len(bucket.cache), OutputSamples: len(bucket.output), CacheCI: interval(bucket.cache), OutputCI: interval(bucket.output), RelativeCI: interval(bucket.relative), RelativeSamples: len(bucket.relative)}
		if len(bucket.cache) > 0 {
			point := median(bucket.cache)
			stable := 0
			for _, v := range bucket.cache {
				if math.Abs(point) > 1e-9 && math.Abs(v-point)/math.Abs(point) <= .2 {
					stable++
				}
			}
			entry.Stability = new(float64(stable) / float64(len(bucket.cache)))
		}
		if len(bucket.relative) > 0 {
			entry.RelativeWeight = percentile(bucket.relative, .5)
		}
		out[model] = entry
	}
	return out
}
