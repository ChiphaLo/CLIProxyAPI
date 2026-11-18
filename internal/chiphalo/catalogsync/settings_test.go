package catalogsync

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/quota"
)

func TestExplicitCatalogSyncKeepsUserDisabledModelsAndAccount(t *testing.T) {
	bindings := []quota.Binding{{ID: "one", Prefix: "codex", Workspace: "team"}, {ID: "two", Prefix: "codex2", Workspace: "team"}}
	old := []byte(`{"models":["gpt-6-luna"],"account_models":{"one":[],"two":[]},"reviewed_account_models":{"one":["gpt-6-luna"],"two":["gpt-6-luna"]},"account_settings":{"one":{"name":"Main","enabled":true,"disabled_models":["gpt-6-luna"]},"two":{"name":"Second","enabled":false,"disabled_models":[]}}}`)
	metadata := []byte(`{"models":{"gpt-6-luna":{"context_window":272000,"supported_reasoning_levels":["high"]},"codex2/gpt-6-luna":{"context_window":272000,"supported_reasoning_levels":["high"]}}}`)
	discovery := Snapshot{OK: true, Accounts: []Account{{ID: "one", Prefix: "codex", Status: "ok", Models: []Model{{Name: "gpt-6-luna"}}}, {ID: "two", Prefix: "codex2", Status: "ok", Models: []Model{{Name: "gpt-6-luna"}}}}}
	p, err := BuildPlan(t.Context(), bindings, discovery, old, metadata)
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Accounts map[string][]string       `json:"account_models"`
		Reviewed map[string][]string       `json:"reviewed_account_models"`
		Settings map[string]AccountSetting `json:"account_settings"`
	}
	if json.Unmarshal(p.Exposed, &out) != nil || len(out.Accounts["one"]) != 0 || len(out.Accounts["two"]) != 0 || !slices.Equal(out.Reviewed["one"], []string{"gpt-6-luna"}) || out.Settings["one"].DisabledModels[0] != "gpt-6-luna" {
		t.Fatalf("settings lost or disabled names re-enabled: %s", p.Exposed)
	}
}
