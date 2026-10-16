package gateway

import (
	"slices"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestAntigravityPolicyRetainsOtherAccountsAndExistingExclusions(t *testing.T) {
	original := map[string][]string{"antigravity": {"old-exclusion"}, "codex": {"gpt-private"}}
	cfg := &config.Config{OAuthExcludedModels: original}
	ApplyAntigravityPolicy(cfg)
	ApplyAntigravityPolicy(cfg)
	if len(cfg.OAuthExcludedModels["antigravity"]) != 3 || !slices.Contains(cfg.OAuthExcludedModels["antigravity"], "claude-*") || !slices.Contains(cfg.OAuthExcludedModels["antigravity"], "gpt-*") || len(original["antigravity"]) != 1 || cfg.OAuthExcludedModels["codex"][0] != "gpt-private" {
		t.Fatal("policy mutated source/other account or lost exclusions")
	}
	cfg = &config.Config{}
	ApplyAntigravityPolicy(cfg)
	if len(cfg.OAuthExcludedModels["antigravity"]) != 2 {
		t.Fatal("empty config not covered")
	}
}
