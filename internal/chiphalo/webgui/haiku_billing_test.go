package webgui

import (
	"encoding/json"
	"math"
	"net/url"
	"os"
	"testing"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/billing"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/quota"
)

func TestHaikuTierMatchesUsageAndClaudePeriodWithoutRewritingHistory(t *testing.T) {
	s, clock := fixtureServer(t)
	now := clock.Unix()
	prices := map[string]*billing.Rates{"claude/claude-haiku-5-5": {Input: 1, Cached: .1, Write: 2, Output: 5, SeparateCacheWrite: true}}
	raw, err := os.ReadFile(s.auth.config)
	if err != nil {
		t.Fatal(err)
	}
	doc := ojson.NewDoc(raw)
	if err = doc.Set("prices", prices); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(s.auth.config, doc.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	rows := []billing.Row{{TS: now - 2, Model: "claude/claude-haiku-5-5", Input: 100000, Cached: 90000, Write: 10000, Output: 1000}, {TS: now - 1, Model: "claude/claude-haiku-5-5", Input: 100001, Cached: 90000, Write: 10000, Output: 1000}}
	var ledger []byte
	for _, r := range rows {
		b, _ := json.Marshal(r)
		ledger = append(append(ledger, b...), '\n')
	}
	if err = os.WriteFile(s.options.Usage, ledger, 0600); err != nil {
		t.Fatal(err)
	}
	got, err := s.usage(url.Values{})
	if err != nil {
		t.Fatal(err)
	}
	const want = .204005
	if total := got["totals"].(map[string]any)["cost"].(float64); math.Abs(total-want) > 1e-12 {
		t.Fatalf("usage cost %v, want %v", total, want)
	}
	recent := got["recent"].([]map[string]any)
	if len(recent) != 2 || recent[0]["cost"] != .170005 || recent[1]["cost"] != .034 {
		t.Fatalf("wrong recent costs: %+v", recent)
	}
	p := claudePeriodFor(&quota.ClaudeWindow{Start: now - 100, Reset: now + 100}, now)
	for _, r := range rows {
		addClaudePeriod(&p, r, prices, now)
	}
	if math.Abs(p.CostUSD-want) > 1e-12 || p.Requests != 2 || p.Input != 1 || p.Cached != 180000 || p.Write != 20000 || p.Output != 2000 {
		t.Fatalf("wrong period: %+v", p)
	}
	after, _ := os.ReadFile(s.options.Usage)
	if string(after) != string(ledger) {
		t.Fatal("history rewritten")
	}
}
