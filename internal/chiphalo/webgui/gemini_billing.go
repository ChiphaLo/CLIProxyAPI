package webgui

import (
	"encoding/json"
	"maps"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/billing"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/quota"
)

// These are the user's 2026-10-16 USD/1M estimates, not a claim about Google's
// subscription charges. Explicit configured prices always take precedence.
func geminiPrices(prices map[string]*billing.Rates) map[string]*billing.Rates {
	result := maps.Clone(prices)
	if result == nil {
		result = map[string]*billing.Rates{}
	}
	for _, name := range []string{"gemini-3-flash", "gemini-3.1-flash-image", "gemini-3.1-flash-lite", "gemini-3.5-flash-lite", "gemini-3.8-flash-n", "gemini-3.8-flash-high"} {
		key := "antigravity/" + name
		if _, exists := prices[key]; exists {
			continue
		}
		rate, exists := prices[name]
		if !exists && (name == "gemini-3.8-flash-n" || name == "gemini-3.8-flash-high") {
			other := "gemini-3.8-flash-high"
			if name == other {
				other = "gemini-3.8-flash-n"
			}
			rate, exists = prices["antigravity/"+other]
			if !exists {
				rate, exists = prices[other]
			}
		}
		if !exists {
			rate = &billing.Rates{Input: .75, Cached: .075, Output: 3.75}
		}
		result[key] = rate
	}
	return result
}

type geminiBillingPeriods struct {
	FiveHour billing.Summary `json:"five_hour"`
	Weekly   billing.Summary `json:"weekly"`
	Basis    string          `json:"attribution_basis"`
	AsOf     int64           `json:"as_of_ts"`
	Error    string          `json:"error,omitempty"`
}

type geminiBillingAccumulator struct {
	accounts     []quota.AntigravityAccount
	prefixCounts map[string]int
	sums         map[string]*geminiBillingPeriods
	now          int64
}

// A past reset is not advanced synthetically: only a current official period
// supplies the billing boundary. Missing weekly data never borrows the 5h reset.
func geminiPeriod(window *quota.Window, duration, now int64) billing.Summary {
	if window == nil || window.Seconds == nil || *window.Seconds != duration || window.Reset == nil || *window.Reset <= now || *window.Reset-now > duration {
		return billing.Summary{}
	}
	start, end := *window.Reset-duration, *window.Reset
	return billing.Summary{WindowAvailable: true, CycleStartTS: &start, CycleEndTS: &end}
}

func newGeminiBilling(accounts []quota.AntigravityAccount, now int64) *geminiBillingAccumulator {
	acc := &geminiBillingAccumulator{accounts: accounts, prefixCounts: map[string]int{}, sums: map[string]*geminiBillingPeriods{}, now: now}
	for _, account := range accounts {
		acc.prefixCounts[account.Prefix]++
	}
	for _, account := range accounts {
		row := &geminiBillingPeriods{Basis: "model_prefix", AsOf: now}
		if account.Gemini != nil {
			row.FiveHour = geminiPeriod(account.Gemini.FiveHour, 18000, now)
			row.Weekly = geminiPeriod(account.Gemini.Weekly, 604800, now)
		}
		if acc.prefixCounts[account.Prefix] != 1 {
			row.Error = "ambiguous_prefix"
			row.FiveHour, row.Weekly = billing.Summary{}, billing.Summary{}
		}
		acc.sums[account.ID] = row
	}
	return acc
}

func (acc *geminiBillingAccumulator) add(row billing.Row, prices map[string]*billing.Rates) {
	prefix, name, found := strings.Cut(row.Model, "/")
	if !found || !strings.HasPrefix(name, "gemini-") || acc.prefixCounts[prefix] != 1 || row.TS > acc.now {
		return
	}
	for _, account := range acc.accounts {
		if account.Prefix != prefix || row.OAuthAccountID != "" && row.OAuthAccountID != account.ID {
			continue
		}
		periods := acc.sums[account.ID]
		total := max(row.Input, 0)
		row.Input, row.Cached, row.Output = total, min(total, max(row.Cached, 0)), max(row.Output, 0)
		for _, sum := range []*billing.Summary{&periods.FiveHour, &periods.Weekly} {
			if !sum.WindowAvailable || row.TS < *sum.CycleStartTS || row.TS >= *sum.CycleEndTS {
				continue
			}
			sum.Requests++
			sum.Input += max(row.Input-row.Cached, 0)
			sum.Cached += max(row.Cached, 0)
			sum.Output += max(row.Output, 0)
			if prices[row.Model] == nil && prices[name] == nil {
				sum.UnpricedRequests++
			} else {
				sum.CostUSD += billing.Cost(row, prices)
			}
			if sum.FirstTS == nil || row.TS < *sum.FirstTS {
				sum.FirstTS = new(row.TS)
			}
			if sum.LastTS == nil || row.TS > *sum.LastTS {
				sum.LastTS = new(row.TS)
			}
		}
	}
}

func (acc *geminiBillingAccumulator) public() []map[string]any {
	result := make([]map[string]any, 0, len(acc.accounts))
	for _, account := range acc.accounts {
		// Account was allowlisted by PublicAntigravity; attach only local billing.
		raw, _ := json.Marshal(account)
		var row map[string]any
		_ = json.Unmarshal(raw, &row)
		periods := acc.sums[account.ID]
		periods.FiveHour.CostUSD = billing.RoundDecimal(periods.FiveHour.CostUSD, 8)
		periods.Weekly.CostUSD = billing.RoundDecimal(periods.Weekly.CostUSD, 8)
		row["billing"] = periods
		result = append(result, row)
	}
	return result
}
