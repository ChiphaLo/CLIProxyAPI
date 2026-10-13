package gateway

import (
	"context"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/watcher/synthesizer"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestReadinessRequiresBothOAuthMembersAndTheirCatalogs(t *testing.T) {
	f := newProfileFixture(t, false)
	cfg := &config.Config{}
	if result := f.profile.CheckCatalogReadiness(cfg, f.manager); !result.Ready {
		t.Fatal("loaded fixture rejected", result)
	}
	id := f.auths["codex2"].ID
	models := registry.GetGlobalRegistry()
	models.UnregisterClient(id)
	if result := f.profile.CheckCatalogReadiness(cfg, f.manager); result.Ready || result.MissingCatalogs != 1 {
		t.Fatal("open listener/file inventory was treated as a loaded catalog", result)
	}
	models.RegisterClient(id, "codex", []*registry.ModelInfo{{ID: "codex2/" + identityModel}})
	f.manager.Remove(context.Background(), id)
	if result := f.profile.CheckCatalogReadiness(cfg, f.manager); result.Ready || result.MissingCredentials != 1 {
		t.Fatal("the other team member substituted for the missing account", result)
	}
	auth := f.auths["codex2"].Clone()
	auth.Prefix = "codex"
	if _, err := f.manager.Register(context.Background(), auth); err != nil {
		t.Fatal(err)
	}
	if result := f.profile.CheckCatalogReadiness(cfg, f.manager); result.Ready {
		t.Fatal("misbound account advertised readiness")
	}
}

func TestReadinessDetectsStaleConfigKeysPoliciesMappingsAndRemoval(t *testing.T) {
	f := newProfileFixture(t, false)
	cfg := &config.Config{OpenAICompatibility: []config.OpenAICompatibility{{Name: "ready-fixture", Prefix: "ready", BaseURL: "https://example.invalid/v1",
		APIKeyEntries: []config.OpenAICompatibilityAPIKey{{APIKey: "synthetic-ready-key", RequestCompression: "gzip"}},
		Models:        []config.OpenAICompatibilityModel{{Name: "upstream-fixture", Alias: "one"}},
	}}}
	synthesize := func() *coreauth.Auth {
		items, err := synthesizer.NewConfigSynthesizer().Synthesize(&synthesizer.SynthesisContext{Config: cfg, IDGenerator: synthesizer.NewStableIDGenerator()})
		if err != nil || len(items) != 1 {
			t.Fatalf("fixture synthesis: %v", err)
		}
		return items[0]
	}
	auth := synthesize()
	if result := f.profile.CheckCatalogReadiness(cfg, f.manager); result.Ready || result.MissingCredentials != 1 {
		t.Fatal("file config accepted before SDK credential registration", result)
	}
	if _, err := f.manager.Register(context.Background(), auth); err != nil {
		t.Fatal(err)
	}
	models := registry.GetGlobalRegistry()
	t.Cleanup(func() { models.UnregisterClient(auth.ID) })
	models.RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: "ready/one", MetadataModelID: "upstream-fixture"}})
	if result := f.profile.CheckCatalogReadiness(cfg, f.manager); !result.Ready {
		t.Fatal(result)
	}
	cfg.OpenAICompatibility[0].APIKeyEntries[0].APIKey = "changed-synthetic-key"
	if result := f.profile.CheckCatalogReadiness(cfg, f.manager); result.Ready || result.MissingCredentials != 1 || result.UnexpectedCredentials != 1 {
		t.Fatal("old upstream key remained ready after replacement", result)
	}
	cfg.OpenAICompatibility[0].APIKeyEntries[0].APIKey = "synthetic-ready-key"
	cfg.OpenAICompatibility[0].APIKeyEntries[0].RequestCompression = ""
	if result := f.profile.CheckCatalogReadiness(cfg, f.manager); result.Ready {
		t.Fatal("removed policy survived in readiness")
	}
	cfg.OpenAICompatibility[0].APIKeyEntries[0].RequestCompression = "gzip"
	cfg.OpenAICompatibility[0].Models[0].Alias = "two"
	updated := synthesize()
	if _, err := f.manager.Register(context.Background(), updated); err != nil {
		t.Fatal(err)
	}
	if result := f.profile.CheckCatalogReadiness(cfg, f.manager); result.Ready || result.MissingCatalogs != 1 {
		t.Fatal("new credential metadata accepted while old model mapping remained", result)
	}
	models.RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: "ready/two", MetadataModelID: "upstream-fixture"}})
	if result := f.profile.CheckCatalogReadiness(cfg, f.manager); !result.Ready {
		t.Fatal(result)
	}
	cfg.OpenAICompatibility = nil
	if result := f.profile.CheckCatalogReadiness(cfg, f.manager); result.Ready || result.UnexpectedCredentials != 1 {
		t.Fatal("removed provider still loaded in SDK", result)
	}
}

func TestReadinessRejectsStaleCapabilitiesWithUnchangedAlias(t *testing.T) {
	f := newProfileFixture(t, false)
	cfg := &config.Config{OpenAICompatibility: []config.OpenAICompatibility{{Name: "capability-ready", Prefix: "cap", BaseURL: "https://example.invalid/v1", APIKeyEntries: []config.OpenAICompatibilityAPIKey{{APIKey: "synthetic-capability-key"}}, Models: []config.OpenAICompatibilityModel{{Name: "upstream", Alias: "same", DisplayName: "Configured", MaxContextLength: 300000, InputModalities: []string{"text", "image"}, OutputModalities: []string{"text"}, Thinking: &registry.ThinkingSupport{Levels: []string{"high", "ultra"}}}}}}}
	auths, err := synthesizer.NewConfigSynthesizer().Synthesize(&synthesizer.SynthesisContext{Config: cfg, IDGenerator: synthesizer.NewStableIDGenerator()})
	if err != nil || len(auths) != 1 {
		t.Fatal("synthetic capability synthesis failed")
	}
	auth := auths[0]
	if _, err := f.manager.Register(context.Background(), auth); err != nil {
		t.Fatal(err)
	}
	models := registry.GetGlobalRegistry()
	t.Cleanup(func() { models.UnregisterClient(auth.ID) })
	good := &registry.ModelInfo{ID: "cap/same", MetadataModelID: "upstream", DisplayName: "Configured", MaxContextLength: 300000, SupportedInputModalities: []string{"text", "image"}, SupportedOutputModalities: []string{"text"}, Thinking: &registry.ThinkingSupport{Levels: []string{"high", "ultra"}}}
	models.RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{good})
	if !f.profile.CheckCatalogReadiness(cfg, f.manager).Ready {
		t.Fatal("current capability metadata rejected")
	}
	for name, mutate := range map[string]func(*registry.ModelInfo){"upstream": func(m *registry.ModelInfo) { m.MetadataModelID = "old-upstream" }, "context": func(m *registry.ModelInfo) { m.MaxContextLength = 200000 }, "thinking": func(m *registry.ModelInfo) { m.Thinking = &registry.ThinkingSupport{Levels: []string{"low"}} }, "image": func(m *registry.ModelInfo) { m.SupportedInputModalities = []string{"text"} }, "display": func(m *registry.ModelInfo) { m.DisplayName = "old-display" }} {
		t.Run(name, func(t *testing.T) {
			stale := *good
			mutate(&stale)
			models.RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{&stale})
			if f.profile.CheckCatalogReadiness(cfg, f.manager).Ready {
				t.Fatal("stale same-alias capability accepted")
			}
		})
	}
	t.Run("canonical modality metadata", func(t *testing.T) {
		cfg.OpenAICompatibility[0].Models[0].InputModalities = []string{" Text ", "IMAGE", "image"}
		models.RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{good})
		if !f.profile.CheckCatalogReadiness(cfg, f.manager).Ready {
			t.Fatal("canonicalized capability metadata rejected")
		}
	})
}

func TestReadinessDoesNotRequireAnIntentionallyDisabledOAuthAccount(t *testing.T) {
	f := newProfileFixture(t, true)
	f.manager.Remove(context.Background(), f.auths["codex"].ID)
	registry.GetGlobalRegistry().UnregisterClient(f.auths["codex"].ID)
	if result := f.profile.CheckCatalogReadiness(&config.Config{}, f.manager); !result.Ready {
		t.Fatal("intentional disable blocked unrelated routes", result)
	}
}
