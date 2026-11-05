package quota

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"regexp"
	"strings"
	"time"
)

const ClaudeUsageURL = "https://api.anthropic.com/api/oauth/usage"

type ClaudeWindow struct {
	Used      float64 `json:"used_percent"`
	Remaining float64 `json:"remaining_percent"`
	Seconds   int64   `json:"limit_window_seconds"`
	Start     int64   `json:"start_at"`
	Reset     int64   `json:"reset_at"`
}

type ClaudeSnapshot struct {
	ID       string        `json:"id"`
	Prefix   string        `json:"prefix"`
	Updated  int64         `json:"updated_at"`
	Source   string        `json:"source"`
	FiveHour *ClaudeWindow `json:"five_hour"`
	Weekly   *ClaudeWindow `json:"weekly"`
}

func (s ClaudeSnapshot) Validate() error {
	if !regexp.MustCompile(`^claude_[a-f0-9]{24}$`).MatchString(s.ID) || s.Prefix != "claude" || s.Source != ClaudeUsageURL || s.Updated <= 0 {
		return errors.New("invalid Claude quota identity")
	}
	for i, w := range []*ClaudeWindow{s.FiveHour, s.Weekly} {
		if w == nil {
			continue
		}
		duration := int64(18000)
		if i == 1 {
			duration = 604800
		}
		if w.Seconds != duration || w.Start <= 0 || w.Reset <= w.Start || w.Reset-w.Start > duration || math.IsNaN(w.Used) || math.IsInf(w.Used, 0) || w.Used < 0 || w.Used > 100 || w.Remaining != 100-w.Used {
			return errors.New("invalid Claude quota window")
		}
	}
	return nil
}

// ProjectClaude deliberately drops dollar amounts, account details and every
// unknown upstream field. Official percentages never become CPA dollar costs.
func ProjectClaude(raw []byte, id string, now int64) (ClaudeSnapshot, error) {
	var value struct {
		Five *struct {
			Used  *float64 `json:"utilization"`
			Reset string   `json:"resets_at"`
		} `json:"five_hour"`
		Week *struct {
			Used  *float64 `json:"utilization"`
			Reset string   `json:"resets_at"`
		} `json:"seven_day"`
		Breakdown struct {
			Start string `json:"window_started_at"`
		} `json:"seven_day_breakdown"`
	}
	if len(raw) > 262144 || json.Unmarshal(raw, &value) != nil {
		return ClaudeSnapshot{}, errors.New("invalid Claude quota response")
	}
	window := func(used *float64, reset string, seconds int64) *ClaudeWindow {
		end, err := time.Parse(time.RFC3339Nano, reset)
		if err != nil || used == nil || math.IsNaN(*used) || math.IsInf(*used, 0) || *used < 0 || *used > 100 || end.Unix() <= now || end.Unix()-now > seconds {
			return nil
		}
		return &ClaudeWindow{Used: *used, Remaining: 100 - *used, Seconds: seconds, Start: end.Unix() - seconds, Reset: end.Unix()}
	}
	s := ClaudeSnapshot{ID: id, Prefix: "claude", Updated: now, Source: ClaudeUsageURL}
	if value.Five != nil {
		s.FiveHour = window(value.Five.Used, value.Five.Reset, 18000)
	}
	if value.Week != nil {
		s.Weekly = window(value.Week.Used, value.Week.Reset, 604800)
	}
	if s.Weekly != nil && value.Breakdown.Start != "" {
		start, err := time.Parse(time.RFC3339Nano, value.Breakdown.Start)
		if err != nil || start.Unix() < s.Weekly.Start || start.Unix() > now || start.Unix() >= s.Weekly.Reset {
			return ClaudeSnapshot{}, errors.New("invalid Claude weekly start")
		}
		s.Weekly.Start = start.Unix()
	}
	return s, s.Validate()
}

func (f Fetcher) FetchClaude(ctx context.Context, token, proxy, id string, now int64) (ClaudeSnapshot, error) {
	if token == "" || len(token) > 65536 || strings.ContainsAny(token, "\r\n\x00") {
		return ClaudeSnapshot{}, errors.New("Claude credential unavailable")
	}
	factory := f.Transport
	if factory == nil {
		factory = defaultTransport
	}
	t, err := factory(proxy)
	if err != nil {
		return ClaudeSnapshot{}, errors.New("Claude quota transport unavailable")
	}
	if idle, ok := t.(interface{ CloseIdleConnections() }); ok {
		defer idle.CloseIdleConnections()
	}
	client := &http.Client{Transport: t, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	r, err := http.NewRequestWithContext(ctx, http.MethodGet, ClaudeUsageURL, nil)
	if err != nil {
		return ClaudeSnapshot{}, errors.New("Claude quota request unavailable")
	}
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("anthropic-beta", "oauth-2025-04-20")
	r.Header.Set("Accept", "application/json")
	response, err := client.Do(r)
	if err != nil {
		return ClaudeSnapshot{}, errors.New("Claude official quota unavailable")
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return ClaudeSnapshot{}, errors.New("Claude official quota unavailable")
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, 262145))
	if err != nil {
		return ClaudeSnapshot{}, errors.New("Claude official quota unavailable")
	}
	return ProjectClaude(raw, id, now)
}
