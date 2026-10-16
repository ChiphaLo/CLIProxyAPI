package gateway

import (
	"maps"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/thinking"
)

// Project numeric budgets into UI effort choices without changing the runtime
// registry: Antigravity Claude still receives thinkingBudget, never thinkingLevel.
func antigravityModelMetadata(row map[string]any, info *registry.ModelInfo, codex bool) map[string]any {
	result := maps.Clone(row)
	if info == nil {
		return result
	}
	support := info.Thinking
	if support == nil {
		return result
	}
	projected := *support
	projected.Levels = append([]string(nil), support.Levels...)
	if len(projected.Levels) == 0 && support.Max > 0 {
		for _, level := range []string{"minimal", "low", "medium", "high", "xhigh"} {
			budget, ok := thinking.ConvertLevelToBudget(level)
			if ok && budget >= support.Min && budget <= support.Max && (info.MaxCompletionTokens <= 0 || budget < info.MaxCompletionTokens) {
				projected.Levels = append(projected.Levels, level)
			}
		}
	}
	result["supported_reasoning_levels"] = projected.Levels
	result["thinking"] = support
	if codex {
		result["thinking"] = &projected
	}
	return result
}
