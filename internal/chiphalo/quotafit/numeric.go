package quotafit

import (
	"math"
	"slices"
)

func pearson(left, right []float64) *float64 {
	n := min(len(left), len(right))
	if n < 3 {
		return nil
	}
	ml, mr := 0.0, 0.0
	for i := 0; i < n; i++ {
		ml += left[i]
		mr += right[i]
	}
	ml /= float64(n)
	mr /= float64(n)
	ll, rr, lr := 0.0, 0.0, 0.0
	for i := 0; i < n; i++ {
		l, r := left[i]-ml, right[i]-mr
		ll += l * l
		rr += r * r
		lr += l * r
	}
	if ll <= 0 || rr <= 0 {
		return nil
	}
	return new(lr / math.Sqrt(ll*rr))
}
func median(values []float64) float64 {
	v := slices.Clone(values)
	slices.Sort(v)
	n := len(v)
	if n == 0 {
		return 0
	}
	if n%2 == 1 {
		return v[n/2]
	}
	return 0.5 * (v[n/2-1] + v[n/2])
}
func madSigma(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	center := median(values)
	deviation := make([]float64, len(values))
	rms := 0.0
	for i, v := range values {
		deviation[i] = math.Abs(v - center)
		rms += v * v
	}
	sigma := 1.4826 * median(deviation)
	if sigma <= 1e-12 {
		rms = math.Sqrt(rms / float64(len(values)))
		if rms > 1e-12 {
			return rms
		}
		return 0
	}
	return sigma
}
func percentile(ordered []float64, quantile float64) *float64 {
	if len(ordered) == 0 {
		return nil
	}
	position := float64(len(ordered)-1) * max(0, min(1, quantile))
	lower, upper := int(math.Floor(position)), int(math.Ceil(position))
	if lower == upper {
		return new(ordered[lower])
	}
	fraction := position - float64(lower)
	return new(ordered[lower]*(1-fraction) + ordered[upper]*fraction)
}
func matrix(n int) [][]float64 {
	out := make([][]float64, n)
	for i := range out {
		out[i] = make([]float64, n)
	}
	return out
}
func weightedGram(rows [][]float64, targets, weights []float64) ([][]float64, []float64) {
	p := 0
	if len(rows) > 0 {
		p = len(rows[0])
	}
	gram := matrix(p)
	linear := make([]float64, p)
	for i := 0; i < min(len(rows), min(len(targets), len(weights))); i++ {
		row, target, weight := rows[i], targets[i], weights[i]
		if weight <= 0 {
			continue
		}
		for a := 0; a < p; a++ {
			if row[a] == 0 {
				continue
			}
			scaled := weight * row[a]
			linear[a] += scaled * target
			for b := a; b < p; b++ {
				gram[a][b] += scaled * row[b]
			}
		}
	}
	for a := 0; a < p; a++ {
		for b := 0; b < a; b++ {
			gram[a][b] = gram[b][a]
		}
	}
	return gram, linear
}
func choleskySolve(input [][]float64, rhs []float64) []float64 {
	n := len(input)
	lower := matrix(n)
	for i := 0; i < n; i++ {
		for j := 0; j <= i; j++ {
			total := input[i][j]
			for k := 0; k < j; k++ {
				total -= lower[i][k] * lower[j][k]
			}
			if i == j {
				if total <= 0 {
					return nil
				}
				lower[i][j] = math.Sqrt(total)
			} else {
				if lower[j][j] == 0 {
					return nil
				}
				lower[i][j] = total / lower[j][j]
			}
		}
	}
	y := make([]float64, n)
	for i := 0; i < n; i++ {
		total := rhs[i]
		for k := 0; k < i; k++ {
			total -= lower[i][k] * y[k]
		}
		y[i] = total / lower[i][i]
	}
	x := make([]float64, n)
	for i := n - 1; i >= 0; i-- {
		total := y[i]
		for k := i + 1; k < n; k++ {
			total -= lower[k][i] * x[k]
		}
		x[i] = total / lower[i][i]
	}
	return x
}
func remove(values []int, item int) []int {
	for i, value := range values {
		if value == item {
			return append(values[:i:i], values[i+1:]...)
		}
	}
	return values
}

// NNLS preserves the Lawson-Hanson active/passive ordering and singular-system
// jitter of the deployed fitter, rather than substituting unconstrained OLS.
func NNLS(gram [][]float64, linear []float64, ridge float64, maxIterations int) []float64 {
	p := len(linear)
	if p == 0 {
		return []float64{}
	}
	x := make([]float64, p)
	passive := []int{}
	active := make([]int, p)
	scale := 1.0
	for i, v := range linear {
		active[i] = i
		scale = max(scale, math.Abs(v))
	}
	tol := 1e-11 * scale
	limit := maxIterations
	if limit == 0 {
		limit = 3*p + 30
	}
	for step := 0; step < limit; step++ {
		gradient := make([]float64, p)
		for i := 0; i < p; i++ {
			sum := 0.0
			for j := 0; j < p; j++ {
				sum += gram[i][j] * x[j]
			}
			gradient[i] = linear[i] - sum
		}
		best := math.Inf(-1)
		chosen := -1
		for _, index := range active {
			if gradient[index] > best {
				best = gradient[index]
				chosen = index
			}
		}
		if chosen < 0 || best <= tol {
			break
		}
		active = remove(active, chosen)
		passive = append(passive, chosen)
		inner := 0
		for {
			inner++
			if inner > 4*p+40 {
				return nil
			}
			k := len(passive)
			sub := matrix(k)
			rhs := make([]float64, k)
			for a := 0; a < k; a++ {
				for b := 0; b < k; b++ {
					sub[a][b] = gram[passive[a]][passive[b]]
				}
				sub[a][a] += ridge
				rhs[a] = linear[passive[a]]
			}
			solution := choleskySolve(sub, rhs)
			if solution == nil {
				largest := 0.0
				for a := 0; a < k; a++ {
					largest = max(largest, math.Abs(sub[a][a]))
				}
				jitter := max(1e-12, 1e-6*largest)
				for a := 0; a < k; a++ {
					sub[a][a] += jitter
				}
				solution = choleskySolve(sub, rhs)
				if solution == nil {
					return nil
				}
			}
			candidate := make([]float64, p)
			positive := true
			for a, index := range passive {
				candidate[index] = solution[a]
				if candidate[index] <= 1e-12 {
					positive = false
				}
			}
			if positive {
				x = candidate
				break
			}
			alpha := 1.0
			for _, index := range passive {
				if candidate[index] <= 1e-12 {
					denominator := x[index] - candidate[index]
					if denominator > 0 {
						alpha = min(alpha, x[index]/denominator)
					}
				}
			}
			for i := 0; i < p; i++ {
				x[i] += alpha * (candidate[i] - x[i])
			}
			for _, index := range slices.Clone(passive) {
				if x[index] <= 1e-12 {
					x[index] = 0
					passive = remove(passive, index)
					active = append(active, index)
				}
			}
			if len(passive) == 0 {
				x = make([]float64, p)
				break
			}
		}
	}
	return x
}
func dot(row, theta []float64) float64 {
	total := 0.0
	for i, v := range row {
		if v != 0 {
			total += v * theta[i]
		}
	}
	return total
}
func huberWeight(value, delta float64) float64 {
	magnitude := math.Abs(value)
	if delta <= 0 || magnitude <= delta {
		return 1
	}
	return delta / magnitude
}
func solve(rows [][]float64, targets, weights []float64, cfg Config, robust *bool, ridgeRelative *float64) ([]float64, []float64) {
	if len(rows) == 0 {
		return nil, nil
	}
	p := len(rows[0])
	base := make([]float64, len(weights))
	for i, w := range weights {
		base[i] = max(0, floating(w, 1))
	}
	gram, linear := weightedGram(rows, targets, base)
	trace := 0.0
	for i := 0; i < p; i++ {
		trace += gram[i][i]
	}
	trace /= float64(max(1, p))
	r := floating(cfg["ridge"], 0)
	if ridgeRelative != nil {
		r = *ridgeRelative
	}
	ridge := max(0, r) * max(1e-12, trace)
	theta := NNLS(gram, linear, ridge, 0)
	if theta == nil {
		return nil, nil
	}
	current := base
	useRobust := truth(cfg["robust"])
	if robust != nil {
		useRobust = *robust
	}
	if useRobust {
		for iteration := 0; iteration < int(max(0, integer(cfg["robust_iterations"], 0))); iteration++ {
			residuals := make([]float64, len(rows))
			for i, row := range rows {
				residuals[i] = targets[i] - dot(row, theta)
			}
			sigma := madSigma(residuals)
			if sigma <= 0 {
				break
			}
			delta := max(0.1, floating(cfg["huber_delta"], 2.5))
			current = make([]float64, len(rows))
			for i, r := range residuals {
				current[i] = base[i] * huberWeight(r/sigma, delta)
			}
			gram, linear = weightedGram(rows, targets, current)
			updated := NNLS(gram, linear, ridge, 0)
			if updated == nil {
				break
			}
			theta = updated
		}
	}
	return theta, current
}
func eigenvalues(input [][]float64, sweeps int) []float64 {
	n := len(input)
	if n == 0 {
		return []float64{}
	}
	work := matrix(n)
	for i, row := range input {
		copy(work[i], row)
	}
	for iteration := 0; iteration < sweeps; iteration++ {
		off := 0.0
		for i := 0; i < n; i++ {
			for j := i + 1; j < n; j++ {
				off += work[i][j] * work[i][j]
			}
		}
		if off <= 1e-22 {
			break
		}
		for p := 0; p < n-1; p++ {
			for q := p + 1; q < n; q++ {
				if math.Abs(work[p][q]) < 1e-18 {
					continue
				}
				theta := (work[q][q] - work[p][p]) / (2 * work[p][q])
				sign := 1.0
				if theta < 0 {
					sign = -1
				}
				t := sign / (math.Abs(theta) + math.Sqrt(theta*theta+1))
				c := 1 / math.Sqrt(t*t+1)
				s := t * c
				for k := 0; k < n; k++ {
					kp, kq := work[k][p], work[k][q]
					work[k][p] = c*kp - s*kq
					work[k][q] = s*kp + c*kq
				}
				for k := 0; k < n; k++ {
					pk, qk := work[p][k], work[q][k]
					work[p][k] = c*pk - s*qk
					work[q][k] = s*pk + c*qk
				}
			}
		}
	}
	out := make([]float64, n)
	for i := range out {
		out[i] = work[i][i]
	}
	slices.SortFunc(out, func(a, b float64) int {
		if a > b {
			return -1
		}
		if a < b {
			return 1
		}
		return 0
	})
	return out
}

type CollinearPair struct {
	Left        string  `json:"left"`
	Right       string  `json:"right"`
	Correlation float64 `json:"correlation"`
}
type DesignDiagnostics struct {
	Columns   []string        `json:"columns"`
	Rank      int             `json:"rank"`
	Condition *float64        `json:"condition_number"`
	Deficient *bool           `json:"rank_deficient"`
	Pairs     []CollinearPair `json:"collinear_pairs"`
}

func designDiagnostics(rows [][]float64, weights []float64, names []string) DesignDiagnostics {
	info := DesignDiagnostics{Columns: slices.Clone(names), Pairs: []CollinearPair{}}
	p := 0
	if len(rows) > 0 {
		p = len(rows[0])
	}
	if p == 0 || len(rows) < 2 {
		return info
	}
	gram, _ := weightedGram(rows, make([]float64, len(rows)), weights)
	norms := make([]float64, p)
	for j := 0; j < p; j++ {
		norms[j] = math.Sqrt(max(0, gram[j][j]))
	}
	scaled := make([][]float64, len(rows))
	for i, row := range rows {
		scaled[i] = make([]float64, p)
		for j := 0; j < p; j++ {
			if norms[j] > 0 {
				scaled[i][j] = row[j] / norms[j]
			}
		}
	}
	correlation, _ := weightedGram(scaled, make([]float64, len(rows)), weights)
	ev := eigenvalues(correlation, 80)
	top := 0.0
	if len(ev) > 0 {
		top = ev[0]
	}
	threshold := max(1e-10, top*1e-9)
	positive := []float64{}
	for _, v := range ev {
		if v > threshold {
			positive = append(positive, v)
		}
	}
	info.Rank = len(positive)
	info.Deficient = new(len(positive) < p)
	if len(positive) > 0 {
		smallest := slices.Min(positive)
		if smallest > 0 {
			info.Condition = new(math.Sqrt(top / smallest))
		}
	}
	for a := 0; a < p; a++ {
		for b := a + 1; b < p; b++ {
			if norms[a] <= 0 || norms[b] <= 0 {
				continue
			}
			value := correlation[a][b]
			if math.Abs(value) >= 0.995 {
				info.Pairs = append(info.Pairs, CollinearPair{Left: names[a], Right: names[b], Correlation: rounded(value, 6)})
			}
		}
	}
	return info
}

type Metrics struct {
	Count int      `json:"count"`
	MAE   *float64 `json:"mae"`
	RMSE  *float64 `json:"rmse"`
	R2    *float64 `json:"r2"`
}

func metrics(actual, predicted []float64) Metrics {
	n := min(len(actual), len(predicted))
	m := Metrics{Count: n}
	if n == 0 {
		return m
	}
	mae, sse, mean := 0.0, 0.0, 0.0
	for i := 0; i < n; i++ {
		e := actual[i] - predicted[i]
		mae += math.Abs(e)
		sse += e * e
		mean += actual[i]
	}
	mean /= float64(n)
	sst := 0.0
	for _, v := range actual[:n] {
		sst += (v - mean) * (v - mean)
	}
	m.MAE = new(mae / float64(n))
	m.RMSE = new(math.Sqrt(sse / float64(n)))
	if sst > 1e-12 {
		m.R2 = new(1 - sse/sst)
	}
	return m
}
func coefficientVariation(values []float64) *float64 {
	if len(values) < 3 {
		return nil
	}
	mean := 0.0
	for _, v := range values {
		mean += v
	}
	mean /= float64(len(values))
	if math.Abs(mean) < 1e-12 {
		return nil
	}
	variance := 0.0
	for _, v := range values {
		variance += (v - mean) * (v - mean)
	}
	variance /= float64(len(values) - 1)
	return new(math.Sqrt(variance) / math.Abs(mean))
}
