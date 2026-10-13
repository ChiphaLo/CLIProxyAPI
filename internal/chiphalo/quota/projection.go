// Package quota projects official quota without coupling it to CPA token prices.
package quota

import (
	"encoding/json"
	"errors"
	"math"
)

// Window deliberately accepts nullable fields: absent official usage is unknown,
// not zero consumption. Timestamps remain the official Unix-second anchors.
type Window struct {
	Used      *float64 `json:"used_percent"`
	Remaining *float64 `json:"remaining_percent"`
	Seconds   *int64   `json:"limit_window_seconds"`
	Reset     *int64   `json:"reset_at"`
}
type Rate struct {
	Allowed   *bool   `json:"allowed"`
	Reached   *bool   `json:"limit_reached"`
	Primary   *Window `json:"primary_window"`
	Secondary *Window `json:"secondary_window"`
}
type Credits struct {
	Has       *bool `json:"has_credits"`
	Unlimited *bool `json:"unlimited"`
	Balance   any   `json:"balance"`
}
type Additional struct {
	Name string `json:"limit_name"`
	Rate *Rate  `json:"rate_limit"`
}
type Usage struct {
	Plan       string       `json:"plan_type"`
	Rate       *Rate        `json:"rate_limit"`
	Review     *Rate        `json:"code_review_rate_limit"`
	Credits    *Credits     `json:"credits"`
	Additional []Additional `json:"additional_rate_limits"`
}

func number(value any) (float64, bool) {
	v, ok := value.(float64)
	return v, ok && !math.IsNaN(v) && !math.IsInf(v, 0)
}
func integer(value any) *int64 {
	v, ok := number(value)
	if !ok || v < 0 || v > 1e12 || math.Trunc(v) != v {
		return nil
	}
	return new(int64(v))
}
func boolean(value any) *bool {
	v, ok := value.(bool)
	if !ok {
		return nil
	}
	return new(v)
}
func window(value any) *Window {
	source, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	result := &Window{Seconds: integer(source["limit_window_seconds"]), Reset: integer(source["reset_at"])}
	if used, ok := number(source["used_percent"]); ok && used >= 0 && used <= 100 {
		result.Used = new(used)
		result.Remaining = new(max(0, min(100, 100-used)))
	}
	return result
}
func rate(value any) *Rate {
	source, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	return &Rate{Allowed: boolean(source["allowed"]), Reached: boolean(source["limit_reached"]), Primary: window(source["primary_window"]), Secondary: window(source["secondary_window"])}
}

// Project reads only allowlisted fields. A valid top-level rate_limit object is
// required, and arbitrary upstream fields never survive projection.
func Project(raw []byte) (Usage, error) {
	var result Usage
	if len(raw) > 262144 {
		return result, errors.New("official quota response exceeds limit")
	}
	var source map[string]any
	if json.Unmarshal(raw, &source) != nil || source == nil {
		return result, errors.New("official quota response invalid")
	}
	result.Rate = rate(source["rate_limit"])
	if result.Rate == nil {
		return result, errors.New("official quota schema unavailable")
	}
	result.Review = rate(source["code_review_rate_limit"])
	if plan, ok := source["plan_type"].(string); ok && len(plan) <= 80 {
		result.Plan = plan
	}
	if credits, ok := source["credits"].(map[string]any); ok {
		result.Credits = &Credits{Has: boolean(credits["has_credits"]), Unlimited: boolean(credits["unlimited"])}
		switch balance := credits["balance"].(type) {
		case string:
			if len(balance) <= 256 {
				result.Credits.Balance = balance
			}
		case float64:
			if !math.IsNaN(balance) && !math.IsInf(balance, 0) {
				result.Credits.Balance = balance
			}
		}
	}
	result.Additional = []Additional{}
	rows, _ := source["additional_rate_limits"].([]any)
	for _, raw := range rows {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		name, _ := item["limit_name"].(string)
		if len(name) > 128 {
			continue
		}
		result.Additional = append(result.Additional, Additional{Name: name, Rate: rate(item["rate_limit"])})
		if len(result.Additional) >= 32 {
			break
		}
	}
	return result, nil
}

func (u Usage) Weekly(now int64) (int64, int64, bool) {
	if u.Rate == nil {
		return 0, 0, false
	}
	for _, w := range []*Window{u.Rate.Primary, u.Rate.Secondary} {
		if w == nil || w.Seconds == nil || *w.Seconds != 604800 || w.Reset == nil || *w.Reset <= 0 {
			continue
		}
		end := *w.Reset
		if end <= now {
			end += ((now-end)/604800 + 1) * 604800
		}
		return end - 604800, end, true
	}
	return 0, 0, false
}
