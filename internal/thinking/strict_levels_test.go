package thinking

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
)

func TestStrictLevelsRejectsBeforeCrossFamilyClamping(t *testing.T) {
	model := &registry.ModelInfo{ID: "swe-2", Type: "openai-compatibility",
		Thinking: &registry.ThinkingSupport{StrictLevels: true, Levels: []string{"medium", "high", "max"}}}
	for _, config := range []ThinkingConfig{
		{Mode: ModeLevel, Level: "low"}, {Mode: ModeLevel, Level: "ultra"},
		{Mode: ModeNone}, {Mode: ModeAuto},
	} {
		if _, err := ValidateConfig(config, model, "openai-response", "openai-response", false); err == nil {
			t.Fatalf("unsupported level was silently changed: %+v", config)
		}
	}
	for _, level := range []ThinkingLevel{"medium", "high", "max"} {
		got, err := ValidateConfig(ThinkingConfig{Mode: ModeLevel, Level: level}, model, "openai-response", "openai-response", false)
		if err != nil || got.Level != level {
			t.Fatalf("supported level changed: %s %+v %v", level, got, err)
		}
	}
	model.Thinking.StrictLevels = false
	if _, err := ValidateConfig(ThinkingConfig{Mode: ModeLevel, Level: "low"}, model, "openai-response", "openai-response", false); err != nil {
		t.Fatalf("default compatibility behavior changed: %v", err)
	}
}
