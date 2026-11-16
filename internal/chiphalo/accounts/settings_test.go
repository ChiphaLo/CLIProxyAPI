package accounts

import (
	"encoding/json"
	"testing"
)

func TestUserDisabledCodexModelsDoNotFallBackToOtherMember(t *testing.T) {
	dir, rows, _ := fixture(t)
	raw, _ := json.Marshal(map[string]any{"accounts": rows})
	catalog := []byte(`{"models":["gpt-6-astra","gpt-6-luna"],"account_models":{"primary-member":[],"second-member":["gpt-6-astra"]},"reviewed_account_models":{"primary-member":["gpt-6-astra","gpt-6-luna"],"second-member":["gpt-6-astra"]},"account_settings":{"primary-member":{"name":"Main","enabled":false,"disabled_models":[]}}}`)
	r, err := Parse(raw, dir, catalog)
	if err != nil {
		t.Fatal(err)
	}
	for _, model := range []string{"gpt-6-astra", "gpt-6-luna", "codex/gpt-6-astra", "codex/gpt-6-luna"} {
		if _, authID, err := r.Resolve(model, "/v1/responses"); err == nil || authID != "" {
			t.Fatalf("disabled request fell through: %s %s %v", model, authID, err)
		}
	}
	if _, id, err := r.Resolve("codex2/gpt-6-astra", "/v1/responses"); err != nil || id != "second.json" {
		t.Fatal("second member affected", id, err)
	}
}
