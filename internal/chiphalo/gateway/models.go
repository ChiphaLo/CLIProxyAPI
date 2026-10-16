package gateway

import (
	"maps"
	"net/http"
	"slices"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/modelidentity"
	codexmodels "github.com/router-for-me/CLIProxyAPI/v8/internal/client/codex/models"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	responsesclaude "github.com/router-for-me/CLIProxyAPI/v8/internal/translator/claude/openai/responses"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
)

// ModelCatalog is installed after authentication alongside Admission. Public
// aliases are only a projection: the upstream registry and credential routing
// are not mutated. Codex capability serialization remains owned by upstream.
func (p *LegacyProfile) ModelCatalog(base *handlers.BaseAPIHandler) gin.HandlerFunc {
	geminiCatalog := p.GeminiCatalog(base)
	return func(c *gin.Context) {
		if strings.HasPrefix(c.Request.URL.Path, "/v1beta/") {
			geminiCatalog(c)
			return
		}
		if c.Request.Method != http.MethodGet || c.Request.URL.Path != "/v1/models" {
			c.Next()
			return
		}
		if base == nil || base.AuthManager == nil || p == nil || p.Accounts == nil || p.Keys == nil {
			abort(c, http.StatusServiceUnavailable, "CPA account catalog unavailable")
			return
		}
		keyID := p.Keys.KeyID(c.GetString("userApiKey"))
		if keyID == "" {
			abort(c, http.StatusForbidden, "authenticated key has no CPA audit identity")
			return
		}
		modelRegistry := registry.GetGlobalRegistry()
		available := map[string]map[string]any{}
		for _, model := range modelRegistry.GetAvailableModels("openai") {
			if id, ok := model["id"].(string); ok {
				available[id] = model
			}
		}
		models := []map[string]any{}
		sources := map[string]string{}
		publicIDs := p.Accounts.PublicModelIDs()
		for _, binding := range p.Accounts.Bindings() {
			auth, found := base.AuthManager.GetByID(binding.AuthID)
			if !found || p.Accounts.ValidateAuth(auth) != nil {
				continue
			}
			registered := map[string]bool{}
			for _, model := range modelRegistry.GetModelsForClient(binding.AuthID) {
				if model != nil {
					registered[model.ID] = true
				}
			}
			for _, public := range publicIDs {
				routed, authID, err := p.Accounts.Resolve(public, "/v1/responses")
				if err != nil || authID != binding.AuthID || !registered[routed] || available[routed] == nil || p.Keys.Authorize(keyID, public) != nil {
					continue
				}
				row := maps.Clone(available[routed])
				row["id"] = public
				models = append(models, row)
				sources[public] = routed
			}
		}
		for id, row := range available {
			if canonical := modelidentity.Expected(id); canonical != id && available[canonical] != nil && p.Keys.Authorize(keyID, canonical) == nil {
				continue // Keep legacy routing, but list only the verified response ID.
			}
			// Never publish unregistered or unpublished credentials from an OAuth
			// pool, including a duplicate primary codex/ group.
			if slices.Contains(modelRegistry.GetModelProviders(id), "codex") || sources[id] != "" || p.Keys.Authorize(keyID, id) != nil {
				continue
			}
			models = append(models, maps.Clone(row))
			sources[id] = id
		}
		slices.SortFunc(models, func(a, b map[string]any) int { return strings.Compare(a["id"].(string), b["id"].(string)) })
		resolve := func(id string) string {
			if source := sources[id]; source != "" {
				return source
			}
			return id
		}
		_, codexClient := c.Request.URL.Query()["client_version"]
		for i, row := range models {
			id, _ := row["id"].(string)
			if slices.Contains(modelRegistry.GetModelProviders(resolve(id)), "antigravity") {
				models[i] = antigravityModelMetadata(row, modelRegistry.GetModelInfo(resolve(id), "antigravity"), codexClient)
			}
		}
		if _, codex := c.Request.URL.Query()["client_version"]; codex {
			var patch codexmodels.ApplyPatchCapabilityForModelFunc
			var search codexmodels.ToolSearchCapabilityForModelFunc
			v2 := base.Cfg != nil && base.Cfg.Client.Codex.OptimizeMultiAgentV2
			if base.Cfg != nil && base.Cfg.Client.Codex.EnableApplyPatch {
				patch = func(id string) bool { return base.SupportsApplyPatchModel(resolve(id)) }
			}
			if responsesclaude.ClientToolSearchEnabled() {
				search = func(id string) bool { return base.SupportsNativeToolSearchModel(resolve(id)) }
			}
			payload := codexmodels.BuildResponseForClientWithToolCapabilities(models,
				func(id string) []string { return modelRegistry.GetModelProviders(resolve(id)) },
				func(id string) *bool { return modelRegistry.GetResponsesWebSearchCapability(resolve(id)) },
				patch, search, v2, c.Query("client_version"))
			base.WriteModelListResponse(c, "openai", payload)
		} else {
			base.WriteModelListResponse(c, "openai", gin.H{"object": "list", "data": models})
		}
		c.Abort()
	}
}
