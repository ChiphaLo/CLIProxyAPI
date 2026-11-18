package catalogsync

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/quota"
)

func TestScopedCatalogPlanProbesOnlySelectedAccountModels(t *testing.T) {
	bindings, discovery, old, metadata := fixturePlan()
	discovery.Accounts[0].Models = append(discovery.Accounts[0].Models, Model{Name: "gpt-6.2-sol", Visibility: "list"}, Model{Name: "gpt-reserve", Visibility: "hide"})
	discovery.Accounts[1] = Account{ID: "two", Prefix: "codex2", Status: "ok", Models: []Model{{Name: "gpt-6-luna"}, {Name: "gpt-6.2-sol"}}}
	calls := map[string]int{}
	probe := func(_ context.Context, b quota.Binding, m Model, _ string) ProbeResult {
		calls[b.ID+":"+m.Name]++
		return ProbeResult{OK: true, Status: 200, ReportedModel: m.Name}
	}
	scope := &Scope{AccountID: "one", Models: []string{"gpt-6.2-sol", "gpt-reserve", "gpt-missing"}}
	plan, err := BuildScopedPlan(t.Context(), bindings, discovery, old, metadata, probe, time.Unix(100, 0), scope)
	if err != nil || !reflect.DeepEqual(plan.Added, []string{"gpt-6.2-sol"}) {
		t.Fatalf("scoped plan did not add only the selected model: %v %v", err, plan.Added)
	}
	if len(calls) != 1 || calls["one:gpt-6.2-sol"] == 0 {
		t.Fatalf("scoped review probed outside its scope: %v", calls)
	}
	reasons := map[string]bool{}
	for _, rejected := range plan.Rejected {
		reasons[rejected.Model] = true
	}
	if !reasons["gpt-reserve"] || !reasons["gpt-missing"] {
		t.Fatal("hidden or unlisted selected models were not reported")
	}
	var exposed map[string]any
	_ = json.Unmarshal(plan.Exposed, &exposed)
	groups := exposed["account_models"].(map[string]any)
	if !reflect.DeepEqual(groups["one"], []any{"gpt-6-luna", "gpt-6.2-sol"}) || !reflect.DeepEqual(groups["two"], []any{"gpt-6-luna"}) {
		t.Fatalf("scoped review changed another account: %v", groups)
	}
}

func TestScopedCatalogPlanRejectsInvalidScope(t *testing.T) {
	bindings, discovery, old, metadata := fixturePlan()
	probe := func(context.Context, quota.Binding, Model, string) ProbeResult { return ProbeResult{OK: true} }
	for _, scope := range []*Scope{{AccountID: "three", Models: []string{"gpt-6-astra"}}, {AccountID: "one"}, {AccountID: "one", Models: []string{"gpt-6-astra", "gpt-6-astra"}}, {AccountID: " one", Models: []string{"gpt-6-astra"}}} {
		if _, err := BuildScopedPlan(t.Context(), bindings, discovery, old, metadata, probe, time.Unix(100, 0), scope); err == nil {
			t.Fatalf("invalid scope accepted: %+v", scope)
		}
	}
}

func TestScopedCatalogPlanRefreshesSelectedReviewedModel(t *testing.T) {
	t.Run("transient level retry", func(t *testing.T) {
		bindings, discovery, old, metadata := fixturePlan()
		discovery.Accounts[0].Models[0] = Model{Name: "gpt-6-luna", Window: 272000, Thinking: []string{"max", "ultra"}, Visibility: "list"}
		seen := map[string]int{}
		probe := func(_ context.Context, _ quota.Binding, m Model, level string) ProbeResult {
			seen[level]++
			switch {
			case level == "ultra":
				return ProbeResult{Status: 400}
			case level == "max" && seen[level] == 1:
				return ProbeResult{}
			}
			return ProbeResult{OK: true, ReportedModel: m.Name}
		}
		plan, err := BuildScopedPlan(t.Context(), bindings, discovery, old, metadata, probe, time.Unix(100, 0), &Scope{AccountID: "one", Models: []string{"gpt-6-luna"}})
		if err != nil || seen["max"] != 2 || seen["ultra"] != 1 {
			t.Fatalf("transient retry incorrect: %v %v", err, seen)
		}
		var ctx map[string]any
		_ = json.Unmarshal(plan.Context, &ctx)
		if levels := ctx["models"].(map[string]any)["gpt-6-luna"].(map[string]any)["supported_reasoning_levels"].([]any); len(levels) != 1 {
			t.Fatalf("retried level not recorded: %v", levels)
		}
	})
	bindings, discovery, old, metadata := fixturePlan()
	discovery.Accounts[0].Models[0] = Model{Name: "gpt-6-luna", Window: 500000, Thinking: []string{"high", "max"}, Visibility: "list"}
	calls := 0
	probe := func(_ context.Context, b quota.Binding, m Model, _ string) ProbeResult {
		calls++
		if b.ID != "one" || m.Name != "gpt-6-luna" {
			t.Fatalf("refresh probed outside its scope: %s %s", b.ID, m.Name)
		}
		return ProbeResult{OK: true, ReportedModel: m.Name}
	}
	plan, err := BuildScopedPlan(t.Context(), bindings, discovery, old, metadata, probe, time.Unix(100, 0), &Scope{AccountID: "one", Models: []string{"gpt-6-luna"}})
	if err != nil || len(plan.Added) != 0 || calls != 3 {
		t.Fatalf("selected reviewed model was not refreshed: %v %v %d", err, plan.Added, calls)
	}
	var ctx map[string]any
	_ = json.Unmarshal(plan.Context, &ctx)
	models := ctx["models"].(map[string]any)
	if entry := models["gpt-6-luna"].(map[string]any); entry["context_window"] != float64(500000) || len(entry["supported_reasoning_levels"].([]any)) != 2 || entry["extension"] != "keep" {
		t.Fatalf("refreshed metadata incorrect: %v", entry)
	}
	if entry := models["codex2/gpt-6-luna"].(map[string]any); entry["context_window"] != float64(272000) {
		t.Fatal("refresh changed the other account")
	}
}
