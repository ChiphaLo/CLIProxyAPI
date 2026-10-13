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
	models.RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: "ready/one"}})
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
	models.RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: "ready/two"}})
	if result := f.profile.CheckCatalogReadiness(cfg, f.manager); !result.Ready {
		t.Fatal(result)
	}
	cfg.OpenAICompatibility = nil
	if result := f.profile.CheckCatalogReadiness(cfg, f.manager); result.Ready || result.UnexpectedCredentials != 1 {
		t.Fatal("removed provider still loaded in SDK", result)
	}
}

func TestReadinessDoesNotRequireAnIntentionallyDisabledOAuthAccount(t *testing.T) {
	f := newProfileFixture(t, true)
	f.manager.Remove(context.Background(), f.auths["codex"].ID)
	registry.GetGlobalRegistry().UnregisterClient(f.auths["codex"].ID)
	if result := f.profile.CheckCatalogReadiness(&config.Config{}, f.manager); !result.Ready {
		t.Fatal("intentional disable blocked unrelated routes", result)
	}
}
