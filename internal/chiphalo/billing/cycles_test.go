package billing

import "testing"

func fixtureAccount() Account {
	account := Account{ID: "fixture-account", Prefix: "fixture", LegacySince: 1000}
	account.Quota.RateLimit.Secondary = QuotaWindow{Duration: weekSeconds, ResetAt: 1_000_000}
	return account
}

func TestWeeklyCycleAtResetAndManualMark(t *testing.T) {
	account := fixtureAccount()
	for _, fixture := range []struct{ now, mark, start, end int64 }{
		{999999, 0, 395200, 1000000}, {1000000, 0, 1000000, 1604800},
		{1604800, 0, 1604800, 2209600}, {999999, 500000, 500000, 1000000},
		{999999, 1000000, 395200, 1000000},
	} {
		cycle, ok := WeeklyCycle(account, fixture.now, fixture.mark)
		if !ok || cycle.Start != fixture.start || cycle.End != fixture.end {
			t.Fatalf("cycle changed: %+v", cycle)
		}
	}
}

func TestExplicitAccountDoesNotFallBack(t *testing.T) {
	account := fixtureAccount()
	row := Row{TS: 2000, OAuthAccountID: "unknown", RoutedModel: "fixture/model"}
	if _, ok := AccountForRow(row, []Account{account}, map[string]bool{"model": true}); ok {
		t.Fatal("unknown explicit account reassigned")
	}
	row.OAuthAccountID = ""
	row.TS = 999
	if _, ok := AccountForRow(row, []Account{account}, map[string]bool{"model": true}); ok {
		t.Fatal("legacy record predating credential attributed")
	}
	row.TS = 1000
	if got, ok := AccountForRow(row, []Account{account}, map[string]bool{"model": true}); !ok || got.ID != account.ID {
		t.Fatal("verified legacy record lost")
	}
}

func TestCycleSummaryUsesTimestampsNotLastNRequests(t *testing.T) {
	account := fixtureAccount()
	acc := NewAccumulator([]Account{account}, map[string]bool{"model": true}, map[string]*Rates{"model": {Input: 2, Output: 10}}, 999998, nil)
	for _, stamp := range []int64{1, 395199, 395200, 999998, 999999, 1000000} {
		acc.Add(Row{TS: stamp, Model: "model", OAuthAccountID: account.ID, Input: 1000, Cached: 2000, Output: 10})
	}
	summary := acc.Summaries()[account.ID]
	if summary.Requests != 2 || summary.Cached != 2000 || summary.Input != 0 || summary.Output != 20 || summary.CostUSD != 0.0002 {
		t.Fatalf("cycle summary changed: %+v", summary)
	}
}
