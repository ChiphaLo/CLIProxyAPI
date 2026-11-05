package webgui

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/billing"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/quota"
)

type claudePeriod struct {
	billing.Summary
	Write int64 `json:"write"`
}

func claudePeriodFor(w *quota.ClaudeWindow, now int64) claudePeriod {
	if w == nil || w.Start > now || w.Reset <= now {
		return claudePeriod{}
	}
	start, end := w.Start, w.Reset
	return claudePeriod{Summary: billing.Summary{WindowAvailable: true, CycleStartTS: &start, CycleEndTS: &end}}
}

func addClaudePeriod(sum *claudePeriod, row billing.Row, prices map[string]*billing.Rates, now int64) {
	if !sum.WindowAvailable || row.TS < *sum.CycleStartTS || row.TS >= *sum.CycleEndTS || row.TS > now {
		return
	}
	total := max(row.Input, 0)
	cached := min(total, max(row.Cached, 0))
	write := min(total-cached, max(row.Write, 0))
	sum.Requests++
	sum.Input += total - cached - write
	sum.Cached += cached
	sum.Write += write
	sum.Output += max(row.Output, 0)
	if prices[row.Model] == nil && prices[row.Model[strings.LastIndexByte(row.Model, '/')+1:]] == nil {
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

func (s *Server) claudeAccountResponse(w http.ResponseWriter, r *http.Request) {
	value, err := s.call(r.Context(), "claude_usage", nil)
	if err != nil || value["ok"] != true {
		reject(w, 503, "Claude 官方额度暂不可用")
		return
	}
	raw, _ := json.Marshal(value["account"])
	var account quota.ClaudeSnapshot
	if json.Unmarshal(raw, &account) != nil || account.Validate() != nil {
		reject(w, 503, "Claude 官方额度数据无效")
		return
	}
	prices, err := s.prices()
	if err != nil {
		reject(w, 503, "Claude 计费价格不可用")
		return
	}
	now := s.options.Now().Unix()
	five, week := claudePeriodFor(account.FiveHour, now), claudePeriodFor(account.Weekly, now)
	if s.visitUsage(func(entry usageRecord) error {
		row := entry.Row
		// The isolated core refuses ambiguous accounts. Historical records lack
		// OAuth IDs; use only this dedicated prefix and reject conflicting IDs.
		if !strings.HasPrefix(row.Model, "claude/claude-") || row.OAuthAccountID != "" && row.OAuthAccountID != account.ID {
			return nil
		}
		addClaudePeriod(&five, row, prices, now)
		addClaudePeriod(&week, row, prices, now)
		return nil
	}, nil) != nil {
		reject(w, 503, "Claude 调用记录不可用")
		return
	}
	five.CostUSD = billing.RoundDecimal(five.CostUSD, 8)
	week.CostUSD = billing.RoundDecimal(week.CostUSD, 8)
	respond(w, 200, map[string]any{"account": account, "billing": map[string]any{"five_hour": five, "weekly": week, "as_of_ts": now, "attribution_basis": "dedicated_single_account_prefix"}})
}
