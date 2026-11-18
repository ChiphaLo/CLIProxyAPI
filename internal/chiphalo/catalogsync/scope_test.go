package catalogsync

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestScopedCatalogPlanAddsOnlySelectedAccountModels(t *testing.T) {
	bindings, discovery, old, metadata := fixturePlan()
	discovery.Accounts[0].Models = append(discovery.Accounts[0].Models, Model{Name: "gpt-6.2-sol", Thinking: []string{"low", "max"}, Visibility: "list"}, Model{Name: "gpt-reserve", Thinking: []string{"low"}, Visibility: "hide"})
	discovery.Accounts[1] = Account{ID: "two", Prefix: "codex2", Status: "ok", Models: []Model{{Name: "gpt-6-luna", Thinking: []string{"low"}}, {Name: "gpt-6.2-sol", Thinking: []string{"low"}}}}
	scope := &Scope{AccountID: "one", Models: []string{"gpt-6.2-sol", "gpt-reserve", "gpt-missing"}}
	plan, err := BuildScopedPlan(t.Context(), bindings, discovery, old, metadata, scope)
	if err != nil || !reflect.DeepEqual(plan.Added, []string{"gpt-6.2-sol"}) {
		t.Fatalf("scoped plan did not add only the selected model: %v %v", err, plan.Added)
	}
	reasons := map[string]bool{}
	for _, rejected := range plan.Rejected {
		reasons[rejected.Model] = true
	}
	if !reasons["gpt-reserve"] || !reasons["gpt-missing"] {
		t.Fatal("hidden or unlisted selected models were not reported")
	}
	var exposed, ctx map[string]any
	_ = json.Unmarshal(plan.Exposed, &exposed)
	_ = json.Unmarshal(plan.Context, &ctx)
	groups := exposed["account_models"].(map[string]any)
	if !reflect.DeepEqual(groups["one"], []any{"gpt-6-luna", "gpt-6.2-sol"}) || !reflect.DeepEqual(groups["two"], []any{"gpt-6-luna"}) {
		t.Fatalf("scoped review changed another account: %v", groups)
	}
	if entry := ctx["models"].(map[string]any)["codex2/gpt-6-luna"].(map[string]any); len(entry["supported_reasoning_levels"].([]any)) != 1 || entry["supported_reasoning_levels"].([]any)[0].(map[string]any)["effort"] != "high" {
		t.Fatal("scoped review refreshed another account")
	}
}

func TestScopedCatalogPlanRejectsInvalidScope(t *testing.T) {
	bindings, discovery, old, metadata := fixturePlan()
	for _, scope := range []*Scope{{AccountID: "three", Models: []string{"gpt-6-astra"}}, {AccountID: "one"}, {AccountID: "one", Models: []string{"gpt-6-astra", "gpt-6-astra"}}, {AccountID: " one", Models: []string{"gpt-6-astra"}}} {
		if _, err := BuildScopedPlan(t.Context(), bindings, discovery, old, metadata, scope); err == nil {
			t.Fatalf("invalid scope accepted: %+v", scope)
		}
	}
}

func TestScopedCatalogPlanRefreshesListedModelsFromOfficialCatalog(t *testing.T) {
	bindings, discovery, old, metadata := fixturePlan()
	discovery.Accounts[0].Models[0] = Model{Name: "gpt-6-luna", Window: 500000, Thinking: []string{"high", "max"}, Visibility: "list"}
	plan, err := BuildScopedPlan(t.Context(), bindings, discovery, old, metadata, &Scope{AccountID: "one", Models: []string{"gpt-6-luna"}})
	if err != nil || len(plan.Added) != 0 {
		t.Fatalf("listed model was not refreshed: %v %v", err, plan.Added)
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
