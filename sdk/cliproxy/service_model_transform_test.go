package cliproxy

import (
	"context"
	"errors"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

func TestOptionalModelTransformRegistersReviewedDefinitionsBeforePrefixes(t *testing.T) {
	reg := registry.GetGlobalRegistry()
	auth := &coreauth.Auth{ID: "model-transform-fixture", Provider: "codex", Prefix: "codex2", Status: coreauth.StatusActive, Attributes: map[string]string{"plan_type": "team"}}
	t.Cleanup(func() { reg.UnregisterClient(auth.ID) })
	called := 0
	cfg := &config.Config{}
	cfg.ForceModelPrefix = true
	s := &Service{cfg: cfg, modelCatalogTransform: func(_ context.Context, selected *coreauth.Auth, baseline []*ModelInfo) ([]*ModelInfo, error) {
		called++
		if selected.ID != auth.ID || len(baseline) == 0 {
			t.Fatal("transform lost the exact member or baseline")
		}
		selected.Prefix = "must-not-mutate-original"
		return []*ModelInfo{{ID: "gpt-reviewed-fixture", Object: "model", OwnedBy: "openai", Type: "codex", ContextLength: 272000, ClientContextWindow: 272000, ClientMaxContextWindow: 1000000, ClientDefaultReasoningLevel: "ultra", Thinking: &registry.ThinkingSupport{Levels: []string{"high", "ultra"}}}}, nil
	}}
	s.registerModelsForAuth(t.Context(), auth)
	models := reg.GetModelsForClient(auth.ID)
	if called != 1 || len(models) != 1 || models[0].ID != "codex2/gpt-reviewed-fixture" || models[0].ClientMaxContextWindow != 1000000 || auth.Prefix != "codex2" {
		t.Fatal("reviewed registration bypassed stock prefixing or mutated auth")
	}
	s.modelCatalogTransform = func(context.Context, *coreauth.Auth, []*ModelInfo) ([]*ModelInfo, error) {
		return nil, errors.New("private source diagnostic")
	}
	s.registerModelsForAuth(t.Context(), auth)
	if len(reg.GetModelsForClient(auth.ID)) != 0 {
		t.Fatal("a rejected reviewed catalog retained stale registrations")
	}
}
func TestModelTransformBuilderIsExplicitAndDefaultNil(t *testing.T) {
	b := NewBuilder()
	if b.modelCatalogTransform != nil {
		t.Fatal("stock builder enabled an account-specific transform")
	}
	b.WithModelCatalogTransform(func(context.Context, *coreauth.Auth, []*ModelInfo) ([]*ModelInfo, error) { return nil, nil })
	if b.modelCatalogTransform == nil {
		t.Fatal("explicit registration adapter ignored")
	}
}

func TestReviewedCatalogReloadKeepsAuthAndRefreshUntouched(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	auth := &coreauth.Auth{ID: "reload-model-fixture", Provider: "codex", Prefix: "codex2", Status: coreauth.StatusActive, Attributes: map[string]string{"plan_type": "team"}}
	if _, err := manager.Register(t.Context(), auth); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
	cfg := &config.Config{}
	cfg.ForceModelPrefix = true
	name := "gpt-old-reviewed"
	s := &Service{cfg: cfg, coreManager: manager, modelCatalogTransform: func(context.Context, *coreauth.Auth, []*ModelInfo) ([]*ModelInfo, error) {
		return []*ModelInfo{{ID: name, Type: "codex"}}, nil
	}}
	before, _ := manager.GetByID(auth.ID)
	if err := s.ReloadReviewedModelCatalog(t.Context()); err != nil {
		t.Fatal(err)
	}
	name = "gpt-new-reviewed"
	if err := s.ReloadReviewedModelCatalog(t.Context()); err != nil {
		t.Fatal(err)
	}
	after, _ := manager.GetByID(auth.ID)
	reg := registry.GetGlobalRegistry()
	if reg.ClientSupportsModel(auth.ID, "codex2/gpt-old-reviewed") || !reg.ClientSupportsModel(auth.ID, "codex2/gpt-new-reviewed") || before.Generation != after.Generation || before.Prefix != after.Prefix {
		t.Fatal("reviewed reload retained old model or changed auth generation")
	}
	s.modelCatalogTransform = nil
	if err := s.ReloadReviewedModelCatalog(t.Context()); err == nil {
		t.Fatal("stock SDK enabled custom reload by default")
	}
}
