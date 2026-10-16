package gateway

import (
	"encoding/json"
	"testing"

	codexmodels "github.com/router-for-me/CLIProxyAPI/v8/internal/client/codex/models"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/thinking"
	_ "github.com/router-for-me/CLIProxyAPI/v8/internal/thinking/provider/antigravity"
	"github.com/tidwall/gjson"
)

func TestAntigravityBudgetCatalogAndWireStayConsistent(t *testing.T) {
	info := &registry.ModelInfo{ID: "claude-fixture", Type: "antigravity", MaxCompletionTokens: 64000, Thinking: &registry.ThinkingSupport{Min: 1024, Max: 64000, DynamicAllowed: true, ZeroAllowed: true}}
	row := map[string]any{"id": "antigravity/claude-fixture"}
	projected := antigravityModelMetadata(row, info, true)
	levels := projected["supported_reasoning_levels"].([]string)
	if len(levels) != 4 || len(info.Thinking.Levels) != 0 || row["thinking"] != nil {
		t.Fatal("runtime budget registry mutated")
	}
	catalog := codexmodels.BuildResponseForClientWithToolCapabilities([]map[string]any{projected}, func(string) []string { return []string{"antigravity"} }, nil, nil, nil, false, "0.170.1")
	raw, _ := json.Marshal(catalog)
	if len(gjson.GetBytes(raw, "models.0.supported_reasoning_levels").Array()) != 4 {
		t.Fatalf("client choices missing: %s", raw)
	}
	for _, level := range levels {
		body := []byte(`{"request":{"generationConfig":{"maxOutputTokens":64000}}}`)
		source := []byte(`{"reasoning":{"effort":"` + level + `"}}`)
		actual, err := thinking.ApplyThinkingWithModelInfo(body, source, info.ID, "openai-response", "antigravity", "antigravity", info)
		budget, _ := thinking.ConvertLevelToBudget(level)
		if err != nil || gjson.GetBytes(actual, "request.generationConfig.thinkingConfig.thinkingBudget").Int() != int64(budget) || gjson.GetBytes(actual, "request.generationConfig.thinkingConfig.thinkingLevel").Exists() {
			t.Fatalf("%s did not map to budget: %s %v", level, actual, err)
		}
	}
	public := antigravityModelMetadata(row, info, false)
	if len(public["thinking"].(*registry.ThinkingSupport).Levels) != 0 {
		t.Fatal("public budget range rewritten")
	}
}

func TestAntigravityDiscreteLevelsPreserved(t *testing.T) {
	info := &registry.ModelInfo{Thinking: &registry.ThinkingSupport{Levels: []string{"low", "medium", "high"}}}
	got := antigravityModelMetadata(map[string]any{}, info, true)
	if len(got["supported_reasoning_levels"].([]string)) != 3 {
		t.Fatal("Gemini levels changed")
	}
}
