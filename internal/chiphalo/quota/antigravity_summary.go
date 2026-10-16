package quota

import (
	"encoding/json"
	"errors"
	"math"
	"time"
)

func ProjectAntigravitySummary(raw []byte) (*AntigravityGeminiQuota, error) {
	type bucket struct {
		ID        string   `json:"bucketId"`
		Window    string   `json:"window"`
		Remaining *float64 `json:"remainingFraction"`
		Reset     string   `json:"resetTime"`
		Disabled  bool     `json:"disabled"`
	}
	type group struct {
		Buckets []bucket `json:"buckets"`
	}
	var response struct {
		Groups []group `json:"groups"`
		Nested *struct {
			Groups []group `json:"groups"`
		} `json:"quotaSummary"`
	}
	if len(raw) > 8<<20 || json.Unmarshal(raw, &response) != nil {
		return nil, errors.New("official Gemini quota summary invalid")
	}
	groups := response.Groups
	if groups == nil && response.Nested != nil {
		groups = response.Nested.Groups
	}
	result := &AntigravityGeminiQuota{Status: "ok", Checked: time.Now().Unix(), Updated: new(time.Now().Unix())}
	for _, g := range groups {
		for _, b := range g.Buckets {
			var target **Window
			seconds := int64(0)
			switch b.ID {
			case "gemini-5h":
				if b.Window != "" && b.Window != "5h" {
					continue
				}
				target = &result.FiveHour
				seconds = 18000
			case "gemini-weekly":
				if b.Window != "" && b.Window != "weekly" {
					continue
				}
				target = &result.Weekly
				seconds = 604800
			default:
				continue
			}
			if b.Disabled {
				continue
			}
			if *target != nil {
				return nil, errors.New("official Gemini quota bucket ambiguous")
			}
			w := &Window{Seconds: new(seconds)}
			if f := b.Remaining; f != nil && !math.IsNaN(*f) && !math.IsInf(*f, 0) && *f >= 0 && *f <= 1 {
				w.Remaining = new(*f * 100)
				w.Used = new(100 - *f*100)
			}
			if reset, err := time.Parse(time.RFC3339, b.Reset); err == nil && reset.Unix() > 0 {
				w.Reset = new(reset.Unix())
			}
			*target = w
		}
	}
	if result.FiveHour == nil && result.Weekly == nil {
		return nil, errors.New("official Gemini quota summary unavailable")
	}
	return result, nil
}

func validateGeminiQuota(q *AntigravityGeminiQuota) error {
	if q == nil {
		return nil
	}
	if q.Checked <= 0 || q.Status != "ok" && q.Status != "unavailable" || q.Status == "ok" && (q.Updated == nil || *q.Updated <= 0) {
		return errors.New("Gemini quota identity invalid")
	}
	for _, item := range []struct {
		w       *Window
		seconds int64
	}{{q.FiveHour, 18000}, {q.Weekly, 604800}} {
		if item.w == nil {
			continue
		}
		w := item.w
		if w.Seconds == nil || *w.Seconds != item.seconds || w.Reset != nil && *w.Reset <= 0 {
			return errors.New("Gemini quota window invalid")
		}
		for _, value := range []*float64{w.Used, w.Remaining} {
			if value != nil && (math.IsNaN(*value) || math.IsInf(*value, 0) || *value < 0 || *value > 100) {
				return errors.New("Gemini quota percentage invalid")
			}
		}
	}
	return nil
}
