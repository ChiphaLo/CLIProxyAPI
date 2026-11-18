package catalogsync

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/quota"
)

func fixturePlan() ([]quota.Binding, Snapshot, []byte, []byte) {
	bindings := []quota.Binding{{ID: "one", Prefix: "codex", Workspace: "shared-team"}, {ID: "two", Prefix: "codex2", Workspace: "shared-team"}}
	old := []byte(`{"models":["gpt-6-luna","gpt-5.6-sol"],"extension":"keep"}`)
	metadata := []byte(`{"models":{"gpt-6-luna":{"context_window":272000,"supported_reasoning_levels":[{"effort":"high"}],"extension":"keep"},"codex2/gpt-6-luna":{"context_window":272000,"supported_reasoning_levels":[{"effort":"high"}],"extension":"keep"},"1x/gpt-5.6-sol":{"context_window":999999,"extension":"do-not-change"}},"extension":"keep"}`)
	newModel := Model{Name: "gpt-6-astra", Window: 272000, MaxWindow: 1000000, Thinking: []string{"low", "medium", "high", "ultra"}, DefaultThinking: "high", Inputs: []string{"text", "image"}, MultiAgent: "v2", Visibility: "list"}
	discovery := Snapshot{OK: true, Accounts: []Account{{ID: "one", Prefix: "codex", Status: "ok", Models: []Model{{Name: "gpt-6-luna"}, newModel}}, {ID: "two", Prefix: "codex2", Status: "unavailable", Models: []Model{}}}}
	return bindings, discovery, old, metadata
}
func TestCatalogPlanPublishesOfficialCatalogAndKeepsUnrelatedRoutes(t *testing.T) {
	bindings, discovery, old, metadata := fixturePlan()
	plan, err := BuildPlan(t.Context(), bindings, discovery, old, metadata)
	if err != nil || !plan.Changed || !reflect.DeepEqual(plan.Added, []string{"gpt-6-astra"}) || len(plan.Removed) != 2 {
		t.Fatalf("unexpected catalog plan: %v %v", err, plan.Added)
	}
	var exposed, ctx map[string]any
	_ = json.Unmarshal(plan.Exposed, &exposed)
	_ = json.Unmarshal(plan.Context, &ctx)
	groups := exposed["account_models"].(map[string]any)
	if !reflect.DeepEqual(groups["one"], []any{"gpt-6-luna", "gpt-6-astra"}) || !reflect.DeepEqual(groups["two"], []any{"gpt-6-luna"}) || exposed["extension"] != "keep" || ctx["extension"] != "keep" {
		t.Fatal("catalog availability leaked to the failed account or root extensions were lost")
	}
	models := ctx["models"].(map[string]any)
	if models["1x/gpt-5.6-sol"].(map[string]any)["extension"] != "do-not-change" || models["gpt-6-luna"].(map[string]any)["extension"] != "keep" {
		t.Fatal("operator metadata or unrelated API route was rewritten")
	}
	entry := models["gpt-6-astra"].(map[string]any)
	if entry["default_reasoning_level"] != "high" || entry["source_account_id"] != "one" || entry["multi_agent_version"] != "v2" || len(entry["supported_reasoning_levels"].([]any)) != 4 || entry["max_context_window"] != float64(1000000) {
		t.Fatalf("official catalog capabilities were not published: %v", entry)
	}
	repeated, err := BuildPlan(t.Context(), bindings, discovery, plan.Exposed, plan.Context)
	if err != nil || repeated.Changed {
		t.Fatal("an unchanged official catalog produced needless writes")
	}
}
func TestCatalogPlanRejectsNewModelWithoutReasoningLevels(t *testing.T) {
	bindings, discovery, old, metadata := fixturePlan()
	discovery.Accounts[0].Models[1].Thinking = nil
	plan, err := BuildPlan(t.Context(), bindings, discovery, old, metadata)
	if err != nil || len(plan.Added) != 0 || len(plan.Rejected) < 2 {
		t.Fatal("a model without official reasoning levels was advertised")
	}
}
func TestCatalogPlanRejectsOfflineUnregisteredAndCanceledDiscovery(t *testing.T) {
	for _, kind := range []string{"offline", "wrong-prefix", "missing-member", "duplicate-model", "canceled"} {
		t.Run(kind, func(t *testing.T) {
			bindings, discovery, old, metadata := fixturePlan()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			switch kind {
			case "offline":
				discovery.Accounts[0].Status = "unavailable"
			case "wrong-prefix":
				discovery.Accounts[0].Prefix = "codex2"
			case "missing-member":
				discovery.Accounts = discovery.Accounts[:1]
			case "duplicate-model":
				discovery.Accounts[0].Models = append(discovery.Accounts[0].Models, discovery.Accounts[0].Models[0])
			case "canceled":
				cancel()
			}
			if _, err := BuildPlan(ctx, bindings, discovery, old, metadata); err == nil {
				t.Fatal("an ambiguous, unavailable or canceled discovery could be published")
			}
		})
	}
}
func TestCatalogPlanReadsLegacyListAndObjectNames(t *testing.T) {
	bindings, discovery, _, metadata := fixturePlan()
	discovery.Accounts[0].Models = []Model{{Name: "gpt-6-luna"}}
	for _, old := range []string{`["gpt-6-luna"]`, `{"models":[{"id":"codex/gpt-6-luna"}]}`} {
		plan, err := BuildPlan(t.Context(), bindings, discovery, []byte(old), metadata)
		if err != nil || len(plan.Added) != 0 {
			t.Fatal("a supported legacy catalog shape was discarded")
		}
	}
}
