package gateway

import (
	"slices"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/modelidentity"
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

func TestAntigravityPolicyAddsReportedAliasWithoutMutatingConfig(t *testing.T) {
	original := map[string][]config.OAuthModelAlias{"antigravity": {{Name: "gemini-3-flash", Alias: "custom-flash"}}, "codex": {{Name: "gpt-fixture", Alias: "custom-gpt"}}}
	cfg := &config.Config{OAuthModelAlias: original}
	ApplyAntigravityPolicy(cfg)
	ApplyAntigravityPolicy(cfg)
	entries := cfg.OAuthModelAlias["antigravity"]
	if len(entries) != 2 || len(original["antigravity"]) != 1 || cfg.OAuthModelAlias["codex"][0].Alias != "custom-gpt" {
		t.Fatal("aliases duplicated or existing configuration mutated")
	}
	entry := entries[1]
	if entry.Name != modelidentity.AntigravityFlashRoute || entry.Alias != modelidentity.AntigravityFlashReported || !entry.Fork || entry.ForceMapping {
		t.Fatal("legacy routing or raw response identity lost")
	}
}
