package gateway

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
)

func TestReviewedModelHintsKeepWindowsDefaultsAndMemberCapabilities(t *testing.T) {
	live, f, _ := liveFixture(t)
	path := filepath.Join(filepath.Dir(f.path), "config/context-windows.json")
	document := map[string]any{"models": map[string]any{identityModel: map[string]any{"context_window": 272000, "max_context_window": 1000000, "default_reasoning_level": "ultra", "supported_reasoning_levels": []map[string]string{{"effort": "high"}, {"effort": "ultra"}}, "input_modalities": []string{"text", "image"}, "source_account_id": "codex-member"}, "codex2/" + identityModel: map[string]any{"context_window": 200000, "supported_reasoning_levels": []string{"medium"}, "input_modalities": []string{"text"}, "source_account_id": "codex2-member"}}}
	raw, _ := json.Marshal(document)
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	baseline := []*registry.ModelInfo{{ID: identityModel, ContextLength: 128000, Thinking: &registry.ThinkingSupport{Levels: []string{"low"}}, SupportedInputModalities: []string{"text"}}}
	first, err := live.ModelHints(t.Context(), f.auths["codex"], baseline)
	if err != nil || len(first) != 1 || first[0].ClientContextWindow != 272000 || first[0].ClientMaxContextWindow != 1000000 || first[0].ClientDefaultReasoningLevel != "ultra" || !reflect.DeepEqual(first[0].SupportedInputModalities, []string{"text", "image"}) {
		t.Fatalf("reviewed primary capability lost: %v", err)
	}
	second, err := live.ModelHints(t.Context(), f.auths["codex2"], baseline)
	if err != nil || len(second) != 1 || second[0].ContextLength != 200000 || !reflect.DeepEqual(second[0].Thinking.Levels, []string{"medium"}) || baseline[0].ContextLength != 128000 {
		t.Fatal("capabilities leaked between members or mutated baseline")
	}
	document["models"].(map[string]any)["codex2/"+identityModel].(map[string]any)["source_account_id"] = "codex-member"
	raw, _ = json.Marshal(document)
	_ = os.WriteFile(path, raw, 0600)
	if _, err := live.ModelHints(t.Context(), f.auths["codex2"], baseline); err == nil {
		t.Fatal("a different member's model capability was accepted")
	}
}
func TestNewSDKModelNeedsAnExactMemberLiveReview(t *testing.T) {
	live, f, _ := liveFixture(t)
	dir := filepath.Join(filepath.Dir(f.path), "config")
	atomicIdentityWrite(t, filepath.Join(dir, "exposed-codex-models.json"), []byte(`{"models":["gpt-new-fixture"],"account_models":{"codex-member":["gpt-new-fixture"],"codex2-member":[]}}`))
	document := map[string]any{"models": map[string]any{"gpt-new-fixture": map[string]any{"context_window": 272000, "supported_reasoning_levels": []string{"medium"}, "source_account_id": "codex-member", "source_kind": "live OAuth probe", "verified_at": "2026-10-14T00:00:00Z"}}}
	raw, _ := json.Marshal(document)
	if err := os.WriteFile(filepath.Join(dir, "context-windows.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	first, err := live.ModelHints(t.Context(), f.auths["codex"], nil)
	second, errSecond := live.ModelHints(t.Context(), f.auths["codex2"], nil)
	if err != nil || errSecond != nil || len(first) != 1 || len(second) != 0 || first[0].ID != "gpt-new-fixture" {
		t.Fatal("reviewed new model did not remain member-local")
	}
	document["models"].(map[string]any)["gpt-new-fixture"].(map[string]any)["verified_at"] = ""
	raw, _ = json.Marshal(document)
	_ = os.WriteFile(filepath.Join(dir, "context-windows.json"), raw, 0600)
	if _, err := live.ModelHints(t.Context(), f.auths["codex"], nil); err == nil {
		t.Fatal("unreviewed new model was registered")
	}
	entry := document["models"].(map[string]any)["gpt-new-fixture"].(map[string]any)
	entry["verified_at"] = "not-a-review-timestamp"
	raw, _ = json.Marshal(document)
	_ = os.WriteFile(filepath.Join(dir, "context-windows.json"), raw, 0600)
	if _, err := live.ModelHints(t.Context(), f.auths["codex"], nil); err == nil {
		t.Fatal("invalid live review timestamp was accepted")
	}
	entry["verified_at"] = "2026-10-14T00:00:00Z"
	entry["supported_reasoning_levels"] = []string{}
	raw, _ = json.Marshal(document)
	_ = os.WriteFile(filepath.Join(dir, "context-windows.json"), raw, 0600)
	if _, err := live.ModelHints(t.Context(), f.auths["codex"], nil); err == nil {
		t.Fatal("unknown model without reviewed reasoning was activated")
	}
}

func TestReviewedModelHintsLegacyStaleAllowlistDoesNotWithholdLiveModels(t *testing.T) {
	live, f, _ := liveFixture(t)
	dir := filepath.Join(filepath.Dir(f.path), "config")
	atomicIdentityWrite(t, filepath.Join(dir, "exposed-codex-models.json"), []byte(`{"models":["gpt-6-astra","gpt-5.4","gpt-unreviewed-fixture"]}`))
	raw := []byte(`{"models":{"gpt-6-astra":{"context_window":272000,"supported_reasoning_levels":[{"effort":"medium"}]},"gpt-5.4":{"context_window":272000,"supported_reasoning_levels":[{"effort":"medium"}]}}}`)
	if err := os.WriteFile(filepath.Join(dir, "context-windows.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	for _, auth := range f.auths {
		hints, err := live.ModelHints(t.Context(), auth, stockCodexCatalog(auth))
		if err != nil || len(hints) != 1 || hints[0].ID != "gpt-6-astra" {
			t.Fatalf("stale legacy names hid valid models or activated unreviewed names: %v", err)
		}
		for _, hint := range hints {
			hint.ID = auth.Prefix + "/" + hint.ID
		}
		registry.GetGlobalRegistry().RegisterClient(auth.ID, "codex", hints)
	}
	if r := live.ReviewedReadiness(t.Context(), &config.Config{}, f.manager); !r.Ready {
		t.Fatal("legacy stock intersection was not ready", r)
	}
	atomicIdentityWrite(t, filepath.Join(dir, "exposed-codex-models.json"), []byte(`{"models":["gpt-6-astra","gpt-unreviewed-fixture"],"account_models":{"codex-member":["gpt-6-astra","gpt-unreviewed-fixture"],"codex2-member":["gpt-6-astra"]}}`))
	if _, err := live.ModelHints(t.Context(), f.auths["codex"], stockCodexCatalog(f.auths["codex"])); err == nil {
		t.Fatal("explicit reviewed account catalog bypassed missing live review")
	}
	if r := live.ReviewedReadiness(t.Context(), &config.Config{}, f.manager); r.Ready {
		t.Fatal("incomplete reviewed publication was ready")
	}
}

func TestReviewedReadinessDetectsStaleCapabilitiesWithSameNames(t *testing.T) {
	live, f, _ := liveFixture(t)
	dir := filepath.Join(filepath.Dir(f.path), "config")
	atomicIdentityWrite(t, filepath.Join(dir, "exposed-codex-models.json"), []byte(`{"models":["gpt-identity-fixture"],"account_models":{"codex-member":["gpt-identity-fixture"],"codex2-member":["gpt-identity-fixture"]}}`))
	document := map[string]any{"models": map[string]any{identityModel: map[string]any{"context_window": 272000, "supported_reasoning_levels": []string{"high"}, "source_account_id": "codex-member", "source_kind": "live OAuth probe", "verified_at": "2026-10-14T00:00:00Z"}, "codex2/" + identityModel: map[string]any{"context_window": 272000, "supported_reasoning_levels": []string{"high"}, "source_account_id": "codex2-member", "source_kind": "live OAuth probe", "verified_at": "2026-10-14T00:00:00Z"}}}
	raw, _ := json.Marshal(document)
	if err := os.WriteFile(filepath.Join(dir, "context-windows.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	if value := live.ReviewedReadiness(t.Context(), &config.Config{}, f.manager); value.Ready {
		t.Fatal("registered names alone proved reviewed metadata readiness")
	}
	for prefix, auth := range f.auths {
		hints, err := live.ModelHints(t.Context(), auth, []*registry.ModelInfo{{ID: identityModel}})
		if err != nil {
			t.Fatal(err)
		}
		for _, hint := range hints {
			hint.ID = prefix + "/" + hint.ID
		}
		registry.GetGlobalRegistry().RegisterClient(auth.ID, "codex", hints)
	}
	if value := live.ReviewedReadiness(t.Context(), &config.Config{}, f.manager); !value.Ready {
		t.Fatal("actual reviewed SDK metadata rejected", value)
	}
	document["models"].(map[string]any)[identityModel].(map[string]any)["context_window"] = 320000
	raw, _ = json.Marshal(document)
	_ = os.WriteFile(filepath.Join(dir, "context-windows.json"), raw, 0600)
	if value := live.ReviewedReadiness(t.Context(), &config.Config{}, f.manager); value.Ready {
		t.Fatal("changed capabilities with an unchanged model ID reused stale readiness")
	}
}
