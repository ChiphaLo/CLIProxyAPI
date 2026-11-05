package webgui

import (
	"context"
	"encoding/json"
	"math"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/billing"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/quota"
)

type claudeFixtureHelper struct{ account quota.ClaudeSnapshot }

func (h claudeFixtureHelper) Call(_ context.Context, op string, _ map[string]any) (map[string]any, error) {
	return map[string]any{"ok": op == "claude_usage", "account": h.account}, nil
}

func TestClaudeBillingAPIIsScopedAndDoesNotRewriteHistory(t *testing.T) {
	s, clock := fixtureServer(t)
	now := clock.Unix()
	a := quota.ClaudeSnapshot{ID: "claude_0123456789abcdef01234567", Prefix: "claude", Updated: now, Source: quota.ClaudeUsageURL,
		FiveHour: &quota.ClaudeWindow{Start: now - 100, Reset: now + 17900, Seconds: 18000, Used: 6, Remaining: 94},
		Weekly:   &quota.ClaudeWindow{Start: now - 600000, Reset: now + 4800, Seconds: 604800, Used: 3, Remaining: 97}}
	s.options.Helper = claudeFixtureHelper{a}
	var doc map[string]any
	_ = readJSON(filepath.Join(s.options.Data, "gui.json"), 2097152, true, &doc)
	doc["prices"] = map[string]*billing.Rates{"claude/claude-sonnet-5-5": {Input: 2, Cached: .1, Write: 4, Output: 10, SeparateCacheWrite: true}}
	if err := writeJSON(filepath.Join(s.options.Data, "gui.json"), doc); err != nil {
		t.Fatal(err)
	}
	rows := []billing.Row{
		{TS: now - 100, Model: "claude/claude-sonnet-5-5", Input: 100, Cached: 50, Write: 30, Output: 10},
		{TS: now - 101, Model: "claude/claude-sonnet-5-5", Input: 100, Cached: 50, Write: 30, Output: 10},
		{TS: now - 600001, Model: "claude/claude-sonnet-5-5", Input: 999999},
		{TS: now - 10, Model: "devin/claude-sonnet-5-5", Input: 999999},
		{TS: now - 10, Model: "claude/claude-sonnet-5-5", OAuthAccountID: "other-account", Input: 999999},
		{TS: now + 1, Model: "claude/claude-sonnet-5-5", Input: 999999},
	}
	var raw []byte
	for _, r := range rows {
		b, _ := json.Marshal(r)
		raw = append(append(raw, b...), '\n')
	}
	if err := os.WriteFile(s.options.Usage, raw, 0600); err != nil {
		t.Fatal(err)
	}
	rsp := httptest.NewRecorder()
	s.claudeAccountResponse(rsp, httptest.NewRequest("GET", "/gui/api/claude-account", nil))
	if rsp.Code != 200 {
		t.Fatal(rsp.Body.String())
	}
	var got struct {
		Billing struct {
			Five claudePeriod `json:"five_hour"`
			Week claudePeriod `json:"weekly"`
		}
	}
	if json.Unmarshal(rsp.Body.Bytes(), &got) != nil || got.Billing.Five.Requests != 1 || got.Billing.Week.Requests != 2 || got.Billing.Five.CostUSD != .000265 {
		t.Fatal(rsp.Body.String())
	}
	after, _ := os.ReadFile(s.options.Usage)
	if string(after) != string(raw) {
		t.Fatal("history rewritten")
	}
}

func TestClaudeBillingUsesCurrentOfficialTimestamps(t *testing.T) {
	const now int64 = 1800000000
	w := &quota.ClaudeWindow{Start: now - 100, Reset: now + 17900, Seconds: 18000}
	p := claudePeriodFor(w, now)
	prices := map[string]*billing.Rates{"claude-sonnet-5-5": {Input: 2, Cached: .1, Write: 4, Output: 10, SeparateCacheWrite: true}}
	for _, ts := range []int64{now - 101, now - 100, now, now + 1, now + 17900} {
		addClaudePeriod(&p, billing.Row{TS: ts, Model: "claude/claude-sonnet-5-5", Input: 100, Cached: 50, Write: 30, Output: 10}, prices, now)
	}
	if p.Requests != 2 || p.Input != 40 || p.Cached != 100 || p.Write != 60 || p.Output != 20 || math.Abs(p.CostUSD-.00053) > 1e-12 {
		t.Fatalf("wrong period %+v", p)
	}
	if *p.FirstTS != now-100 || *p.LastTS != now {
		t.Fatal("wrong included timestamps")
	}
	if claudePeriodFor(nil, now).WindowAvailable || claudePeriodFor(w, now+18000).WindowAvailable {
		t.Fatal("unavailable period guessed")
	}
}
