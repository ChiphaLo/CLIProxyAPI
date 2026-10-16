package gateway

import (
	"maps"
	"slices"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

// ApplyAntigravityPolicy uses the SDK's native exclusions on every config load,
// including GUI apply. It does not rewrite credentials or persisted YAML.
func ApplyAntigravityPolicy(cfg *config.Config) {
	excluded := maps.Clone(cfg.OAuthExcludedModels)
	if excluded == nil {
		excluded = map[string][]string{}
	}
	models := append([]string(nil), excluded["antigravity"]...)
	for _, pattern := range []string{"claude-*", "gpt-*"} {
		if !slices.Contains(models, pattern) {
			models = append(models, pattern)
		}
	}
	excluded["antigravity"] = models
	cfg.OAuthExcludedModels = excluded
}
