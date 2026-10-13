package catalogsync

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

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
func TestCatalogPlanKeepsMembershipMetadataAndUnrelatedRoutes(t *testing.T) {
	bindings, discovery, old, metadata := fixturePlan()
	calls := []string{}
	probe := func(_ context.Context, b quota.Binding, m Model, level string) ProbeResult {
		calls = append(calls, b.ID+":"+m.Name+":"+level)
		return ProbeResult{OK: level != "high", Status: 200, ReportedModel: "provider/" + m.Name}
	}
	plan, err := BuildPlan(t.Context(), bindings, discovery, old, metadata, probe, time.Unix(100, 0))
	if err != nil || !plan.Changed || !reflect.DeepEqual(plan.Added, []string{"gpt-6-astra"}) || len(plan.Removed) != 2 || len(calls) != 5 {
		t.Fatalf("unexpected catalog plan: %v", err)
	}
	for _, call := range calls {
		if !strings.HasPrefix(call, "one:gpt-6-astra:") {
			t.Fatal("old metadata was reprobed or another account was used")
		}
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
	if entry := models["gpt-6-astra"].(map[string]any); entry["default_reasoning_level"] != "medium" || entry["source_account_id"] != "one" || entry["multi_agent_version"] != "v2" {
		t.Fatal("verified capabilities or source attribution were not retained")
	}
	repeated, err := BuildPlan(t.Context(), bindings, discovery, plan.Exposed, plan.Context, func(context.Context, quota.Binding, Model, string) ProbeResult {
		t.Error("unchanged reviewed models were reprobed")
		return ProbeResult{}
	}, time.Unix(200, 0))
	if err != nil || repeated.Changed {
		t.Fatal("an unchanged reviewed catalog produced needless writes")
	}
}
func TestCatalogPlanRejectsDifferentModelAndUnprovenReasoning(t *testing.T) {
	for _, kind := range []string{"different-model", "no-reasoning"} {
		t.Run(kind, func(t *testing.T) {
			bindings, discovery, old, metadata := fixturePlan()
			plan, err := BuildPlan(t.Context(), bindings, discovery, old, metadata, func(_ context.Context, _ quota.Binding, _ Model, level string) ProbeResult {
				if kind == "different-model" {
					return ProbeResult{OK: true, ReportedModel: "gpt-6-luna"}
				}
				return ProbeResult{OK: level == "", ReportedModel: "gpt-6-astra"}
			}, time.Unix(100, 0))
			if err != nil || len(plan.Added) != 0 || len(plan.Rejected) < 2 {
				t.Fatal("a mismatched or unproven capability was advertised")
			}
		})
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
			if _, err := BuildPlan(ctx, bindings, discovery, old, metadata, func(context.Context, quota.Binding, Model, string) ProbeResult { return ProbeResult{OK: true} }, time.Unix(100, 0)); err == nil {
				t.Fatal("an ambiguous, unavailable or canceled discovery could be published")
			}
		})
	}
}
func TestCatalogPlanReadsLegacyListAndObjectNames(t *testing.T) {
	bindings, discovery, _, metadata := fixturePlan()
	discovery.Accounts[0].Models = []Model{{Name: "gpt-6-luna"}}
	for _, old := range []string{`["gpt-6-luna"]`, `{"models":[{"id":"codex/gpt-6-luna"}]}`} {
		plan, err := BuildPlan(t.Context(), bindings, discovery, []byte(old), metadata, func(context.Context, quota.Binding, Model, string) ProbeResult {
			t.Error("valid existing metadata was not preserved")
			return ProbeResult{}
		}, time.Unix(100, 0))
		if err != nil || len(plan.Added) != 0 {
			t.Fatal("a supported legacy catalog shape was discarded")
		}
	}
}
