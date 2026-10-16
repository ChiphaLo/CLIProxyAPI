package cliproxy

import (
	"context"
	"slices"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/gateway"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/modelidentity"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

type reportedIdentityCapture struct {
	mockBatchTestExecutor
	model string
}

func (e *reportedIdentityCapture) Execute(_ context.Context, _ *coreauth.Auth, req coreexecutor.Request, _ coreexecutor.Options) (coreexecutor.Response, error) {
	e.model = req.Model
	return coreexecutor.Response{Payload: []byte(`{"ok":true}`)}, nil
}

func TestAntigravityReportedAliasSurvivesFencedCatalogRefresh(t *testing.T) {
	cfg := &config.Config{SDKConfig: config.SDKConfig{ForceModelPrefix: true}}
	gateway.ApplyAntigravityPolicy(cfg)
	auth := &coreauth.Auth{ID: "reported-identity-fixture", Prefix: "antigravity", Provider: "antigravity"}
	svc := &Service{cfg: cfg}
	hints := antigravityModelCapabilityHints{ModelIDs: map[string]struct{}{modelidentity.AntigravityFlashRoute: {}}}
	models := svc.antigravityModelsForHintsWithConfig(auth, hints, cfg)
	if len(models) != 2 {
		t.Fatalf("alias/legacy route missing: %+v", models)
	}
	var original, alias *ModelInfo
	for _, m := range models {
		switch m.ID {
		case "antigravity/" + modelidentity.AntigravityFlashRoute:
			original = m
		case "antigravity/" + modelidentity.AntigravityFlashReported:
			alias = m
		}
	}
	if original == nil || alias == nil || alias.MetadataModelID != modelidentity.AntigravityFlashRoute || alias.Thinking == nil || !slices.Equal(alias.Thinking.Levels, []string{"low", "medium", "high"}) || !slices.Equal(alias.SupportedInputModalities, original.SupportedInputModalities) {
		t.Fatalf("alias lost upstream source or capabilities: %+v", alias)
	}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.SetConfig(cfg)
	manager.SetOAuthModelAlias(cfg.OAuthModelAlias)
	capture := &reportedIdentityCapture{mockBatchTestExecutor: mockBatchTestExecutor{provider: "antigravity"}}
	manager.RegisterExecutor(capture)
	if _, err := manager.Register(t.Context(), auth); err != nil {
		t.Fatal(err)
	}
	GlobalModelRegistry().RegisterClient(auth.ID, "antigravity", models)
	t.Cleanup(func() { GlobalModelRegistry().UnregisterClient(auth.ID) })
	for _, name := range []string{modelidentity.AntigravityFlashRoute, modelidentity.AntigravityFlashReported} {
		for _, suffix := range []string{"", "(high)"} {
			_, err := manager.Execute(t.Context(), []string{"antigravity"}, coreexecutor.Request{Model: "antigravity/" + name + suffix, Payload: []byte(`{"input":"fixture"}`)}, coreexecutor.Options{})
			if err != nil || capture.model != modelidentity.AntigravityFlashRoute+suffix {
				t.Fatalf("unsupported upstream route %q: %v", capture.model, err)
			}
		}
	}
}
