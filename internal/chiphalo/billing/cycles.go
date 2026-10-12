package billing

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

const weekSeconds int64 = 604800

type QuotaWindow struct {
	Duration int64 `json:"limit_window_seconds"`
	ResetAt  int64 `json:"reset_at"`
}

type Account struct {
	ID          string `json:"id"`
	Prefix      string `json:"prefix"`
	LegacySince int64  `json:"legacy_since"`
	Quota       struct {
		RateLimit struct {
			Primary   QuotaWindow `json:"primary_window"`
			Secondary QuotaWindow `json:"secondary_window"`
		} `json:"rate_limit"`
	} `json:"quota"`
}

type Cycle struct {
	Start int64
	End   int64
}

// WeeklyCycle preserves oauth_weekly_window and its manual-reset mark. It
// does not call an upstream quota endpoint or consume any reset credit.
func WeeklyCycle(account Account, now, manualReset int64) (Cycle, bool) {
	for _, window := range []QuotaWindow{account.Quota.RateLimit.Primary, account.Quota.RateLimit.Secondary} {
		if window.Duration != weekSeconds || window.ResetAt <= 0 {
			continue
		}
		next := window.ResetAt
		if next <= now {
			next += ((now-next)/window.Duration + 1) * window.Duration
		}
		cycle := Cycle{Start: next - window.Duration, End: next}
		if manualReset >= cycle.Start && manualReset < cycle.End {
			cycle.Start = max(cycle.Start, manualReset)
		}
		return cycle, true
	}
	return Cycle{}, false
}

// AccountForRow is the deployed explicit-account-first attribution rule.
// An unknown explicit ID cannot be silently reassigned through its prefix.
func AccountForRow(row Row, accounts []Account, models map[string]bool) (Account, bool) {
	if row.OAuthAccountID != "" {
		for _, account := range accounts {
			if account.ID == row.OAuthAccountID {
				return account, true
			}
		}
		return Account{}, false
	}
	prefix, base, found := strings.Cut(row.RoutedModel, "/")
	if found && models[base] {
		for _, account := range accounts {
			if account.Prefix == prefix && account.LegacySince != 0 && row.TS >= account.LegacySince {
				return account, true
			}
		}
	}
	return Account{}, false
}

type Summary struct {
	Requests         int64   `json:"requests"`
	Input            int64   `json:"input"`
	Cached           int64   `json:"cached"`
	Output           int64   `json:"output"`
	CostUSD          float64 `json:"cost_usd"`
	UnpricedRequests int64   `json:"unpriced_requests"`
	FirstTS          *int64  `json:"first_ts"`
	LastTS           *int64  `json:"last_ts"`
	WindowAvailable  bool    `json:"window_available"`
	CycleStartTS     *int64  `json:"cycle_start_ts"`
	CycleEndTS       *int64  `json:"cycle_end_ts"`
}

type Accumulator struct {
	accounts []Account
	models   map[string]bool
	prices   map[string]*Rates
	now      int64
	cycles   map[string]Cycle
	sums     map[string]Summary
}

func NewAccumulator(accounts []Account, models map[string]bool, prices map[string]*Rates, now int64, resetMarks map[string]int64) *Accumulator {
	acc := &Accumulator{accounts: accounts, models: models, prices: prices, now: now,
		cycles: make(map[string]Cycle), sums: make(map[string]Summary)}
	for _, account := range accounts {
		summary := Summary{}
		if cycle, ok := WeeklyCycle(account, now, resetMarks[account.Prefix]); ok {
			summary.WindowAvailable = true
			summary.CycleStartTS, summary.CycleEndTS = &cycle.Start, &cycle.End
			acc.cycles[account.ID] = cycle
		}
		acc.sums[account.ID] = summary
	}
	return acc
}

func (acc *Accumulator) Add(row Row) {
	account, ok := AccountForRow(row, acc.accounts, acc.models)
	if !ok {
		return
	}
	cycle, available := acc.cycles[account.ID]
	if !available || row.TS < cycle.Start || row.TS >= cycle.End || row.TS > acc.now {
		return
	}
	summary := acc.sums[account.ID]
	total := max(row.Input, 0)
	cached := min(total, max(row.Cached, 0))
	output := max(row.Output, 0)
	summary.Requests++
	summary.Input += total - cached
	summary.Cached += cached
	summary.Output += output
	rate := acc.prices[row.Model]
	if rate == nil {
		rate = acc.prices[row.Model[strings.LastIndexByte(row.Model, '/')+1:]]
	}
	if rate == nil {
		summary.UnpricedRequests++
	} else {
		row.Input, row.Cached, row.Output = total, cached, output
		summary.CostUSD = float64(summary.CostUSD + Cost(row, acc.prices))
	}
	if summary.FirstTS == nil || row.TS < *summary.FirstTS {
		stamp := row.TS
		summary.FirstTS = &stamp
	}
	if summary.LastTS == nil || row.TS > *summary.LastTS {
		stamp := row.TS
		summary.LastTS = &stamp
	}
	acc.sums[account.ID] = summary
}

func (acc *Accumulator) Summaries() map[string]Summary {
	result := make(map[string]Summary, len(acc.sums))
	for id, summary := range acc.sums {
		summary.CostUSD = RoundDecimal(summary.CostUSD, 8)
		result[id] = summary
	}
	return result
}

// SummaryDigest compares periods without exporting account IDs or histories.
func SummaryDigest(summaries map[string]Summary) string {
	ids := make([]string, 0, len(summaries))
	for id := range summaries {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	hash := sha256.New()
	stamp := func(value *int64) string {
		if value == nil {
			return "-"
		}
		return strconv.FormatInt(*value, 10)
	}
	for _, id := range ids {
		s := summaries[id]
		_, _ = fmt.Fprintf(hash, "%s:%d:%d:%d:%d:%.8f:%d:%s:%s:%t:%s:%s\n", id,
			s.Requests, s.Input, s.Cached, s.Output, s.CostUSD, s.UnpricedRequests,
			stamp(s.FirstTS), stamp(s.LastTS), s.WindowAvailable, stamp(s.CycleStartTS), stamp(s.CycleEndTS))
	}
	return hex.EncodeToString(hash.Sum(nil))
}
