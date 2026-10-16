package webgui

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/billing"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/quota"
)

func TestGeminiPricesPreserveExplicitRatesAndLegacyAliases(t *testing.T) {
	custom := &billing.Rates{Input: 2, Cached: .2, Output: 10}
	prices := map[string]*billing.Rates{"gpt-fixture": custom, "antigravity/gemini-3.8-flash-high": custom}
	result := geminiPrices(prices)
	if len(prices) != 2 || result["gpt-fixture"] != custom || result["antigravity/gemini-3.8-flash-n"] != custom {
		t.Fatal("configured rates mutated or legacy alias not respected")
	}
	defaults := geminiPrices(nil)
	if len(defaults) != 6 || defaults["gemini-3.8-flash-n"] != nil || defaults["merchant/gemini-3.8-flash-n"] != nil {
		t.Fatal("default estimates escaped the reviewed account namespace")
	}
	for name, rate := range defaults {
		if rate.Input != .75 || rate.Cached != .075 || rate.Output != 3.75 || rate.Write != 0 {
			t.Fatalf("wrong supplied rate for %s: %+v", name, rate)
		}
		// Output already includes thinking. Do not add reasoning again.
		if got := billing.Cost(billing.Row{Model: name, Input: 100, Cached: 80, Output: 10}, defaults); got != .0000585 {
			t.Fatalf("wrong Gemini token price: %.8f", got)
		}
	}
}

func geminiBillingAccount(now int64) quota.AntigravityAccount {
	return quota.AntigravityAccount{ID: strings.Repeat("a", 64), Prefix: "antigravity", Status: "ok", Checked: now, Updated: new(now), Models: []quota.AntigravityModel{}, Gemini: &quota.AntigravityGeminiQuota{Status: "ok", Checked: now, Updated: new(now), FiveHour: &quota.Window{Seconds: new(int64(18000)), Reset: new(now + 7200)}, Weekly: &quota.Window{Seconds: new(int64(604800)), Reset: new(now + 304800)}}}
}

func TestGeminiBillingUsesOfficialTimestampsAndNeverFitsOrMergesAccounts(t *testing.T) {
	const now int64 = 1800000000
	account := geminiBillingAccount(now)
	acc := newGeminiBilling([]quota.AntigravityAccount{account}, now)
	row := billing.Row{TS: now - 60, Model: "antigravity/gemini-3.8-flash-n", Input: 100, Cached: 80, Output: 10}
	prices := geminiPrices(nil)
	acc.add(row, prices)
	row.Model = "antigravity/gemini-3.8-flash-high"
	acc.add(row, prices)
	row.TS = now - 40000 // Weekly only, outside the current 5h period.
	acc.add(row, prices)
	row.TS = now - 30
	row.Model = "antigravity/gemini-unpriced"
	acc.add(row, prices)
	row.TS = now + 1
	acc.add(row, prices)
	row.TS = now - 400000 // Outside both official windows.
	acc.add(row, prices)
	row.TS, row.Model = now-10, "other/gemini-3.8-flash-n"
	acc.add(row, prices)
	row.Model, row.OAuthAccountID = "antigravity/gemini-3.8-flash-n", "unknown-account"
	acc.add(row, prices)
	row.OAuthAccountID, row.Model = "", "antigravity/claude-fixture"
	acc.add(row, prices)
	sums := acc.sums[account.ID]
	if sums.FiveHour.Requests != 3 || sums.Weekly.Requests != 4 || sums.FiveHour.Input != 60 || sums.FiveHour.Cached != 240 || sums.FiveHour.Output != 30 || sums.FiveHour.UnpricedRequests != 1 || sums.Weekly.UnpricedRequests != 1 || math.Abs(sums.FiveHour.CostUSD-.000117) > 1e-12 || math.Abs(sums.Weekly.CostUSD-.0001755) > 1e-12 {
		t.Fatalf("wrong period statistics: %+v", sums)
	}
	if *sums.FiveHour.CycleStartTS != now-10800 || *sums.Weekly.CycleStartTS != now-300000 || sums.Basis != "model_prefix" {
		t.Fatal("official period or attribution basis lost")
	}
	account.Gemini.Weekly = nil
	account.Gemini.FiveHour.Reset = new(now - 1)
	acc = newGeminiBilling([]quota.AntigravityAccount{account}, now)
	if acc.sums[account.ID].FiveHour.WindowAvailable || acc.sums[account.ID].Weekly.WindowAvailable {
		t.Fatal("expired/missing official windows guessed or borrowed")
	}
	second := account
	second.ID = strings.Repeat("b", 64)
	acc = newGeminiBilling([]quota.AntigravityAccount{account, second}, now)
	if acc.sums[account.ID].Error != "ambiguous_prefix" || acc.sums[second.ID].Error != "ambiguous_prefix" {
		t.Fatal("multiple accounts sharing a prefix silently merged")
	}
}

func TestGeminiBillingIncludesPeriodStartAndRejectsEnd(t *testing.T) {
	const now int64 = 1800000000
	account := geminiBillingAccount(now)
	acc := newGeminiBilling([]quota.AntigravityAccount{account}, now)
	for _, ts := range []int64{now - 10800, now - 10801, now + 7200} {
		acc.add(billing.Row{TS: ts, Model: "antigravity/gemini-3.8-flash-n", Input: 1}, geminiPrices(nil))
	}
	if acc.sums[account.ID].FiveHour.Requests != 1 || acc.sums[account.ID].Weekly.Requests != 2 {
		t.Fatal("billing interval is not [official start, official reset)")
	}
}

func TestGeminiBillingAccountAPIRepricesHistoryWithoutWritingIt(t *testing.T) {
	s, clock := fixtureServer(t)
	account := geminiBillingAccount(clock.Unix())
	if err := writeJSON(s.options.Quota, map[string]any{"accounts": []any{}, "antigravity_accounts": []any{account}}); err != nil {
		t.Fatal(err)
	}
	data := []byte(fmt.Sprintf(`{"ts":%d,"model":"antigravity/gemini-3.8-flash-high","input":100,"cached":80,"output":10}`+"\n", clock.Unix()-1))
	if err := os.WriteFile(s.options.Usage, data, 0600); err != nil {
		t.Fatal(err)
	}
	configBefore, _ := os.ReadFile(s.auth.config)
	for range 2 {
		w := httptest.NewRecorder()
		s.accountResponse(w, httptest.NewRequest("GET", "/api/oauth-accounts?include_reset=0", nil))
		var result struct {
			Antigravity []struct {
				Billing geminiBillingPeriods `json:"billing"`
			} `json:"antigravity_accounts"`
		}
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || len(result.Antigravity) != 1 || result.Antigravity[0].Billing.Weekly.CostUSD != .0000585 || result.Antigravity[0].Billing.FiveHour.Requests != 1 {
			t.Fatalf("Gemini billing missing from account API: %d %s", w.Code, w.Body)
		}
	}
	configAfter, _ := os.ReadFile(s.auth.config)
	after, _ := os.ReadFile(s.options.Usage)
	if string(configBefore) != string(configAfter) || string(after) != string(data) {
		t.Fatal("read-only statistics rewrote prices, credentials or historical usage")
	}
}
