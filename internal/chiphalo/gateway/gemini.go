package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/nativegemini"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

// URL model identity is authoritative. Reject duplicate and conflicting body
// identities, including CountTokensRequest's embedded GenerateContentRequest.
func validateGeminiBody(raw []byte, model, action string) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if _, err := decoder.Token(); err != nil {
		return err
	}
	seen := map[string]bool{}
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			return err
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return err
		}
		if key != "model" && key != "generateContentRequest" && key != "generate_content_request" {
			continue
		}
		name := key.(string)
		if name == "generate_content_request" {
			name = "generateContentRequest"
		}
		if seen[name] {
			return fmt.Errorf("ambiguous Gemini request model")
		}
		seen[name] = true
		if name == "model" {
			var identity string
			if json.Unmarshal(value, &identity) != nil || (identity != model && identity != "models/"+model) {
				return fmt.Errorf("Gemini body model conflicts with URL resource")
			}
		} else {
			if action != "countTokens" || !gjson.ParseBytes(value).IsObject() {
				return fmt.Errorf("invalid embedded Gemini generation request")
			}
			if err := validateGeminiBody(value, model, "generateContent"); err != nil {
				return err
			}
		}
	}
	return nil
}

func (p *LegacyProfile) decorateAdmission(ctx context.Context, expectedAuthID string) context.Context {
	meta, _ := ctx.Value(metadataKey{}).(requestMetadata)
	if _, _, native := nativegemini.Parse(http.MethodPost, meta.Path); native {
		return coreauth.WithExecutionAuthValidator(ctx, func(auth *coreauth.Auth) error {
			if auth == nil || expectedAuthID != "" || !nativegemini.Provider(auth.Provider) || auth.Disabled || auth.Status == coreauth.StatusDisabled {
				return fmt.Errorf("native Gemini credential policy rejected this route")
			}
			if strings.Contains(meta.Model, "/") && auth.Prefix != "" && !strings.HasPrefix(meta.Model, auth.Prefix+"/") {
				return fmt.Errorf("native Gemini credential namespace mismatch")
			}
			for _, info := range registry.GetGlobalRegistry().GetModelsForClient(auth.ID) {
				if info != nil && info.ID == meta.Model {
					return nil
				}
			}
			return fmt.Errorf("native Gemini model is not registered for the selected credential")
		})
	}
	if expectedAuthID != "" {
		if binding, err := p.Accounts.BindingForAuth(expectedAuthID); err == nil {
			meta.AccountAuthID = expectedAuthID
			meta.Account = AccountBinding{ID: binding.ID, Prefix: binding.Prefix}
			ctx = context.WithValue(ctx, metadataKey{}, meta)
		}
	}
	return p.Accounts.Decorate(ctx, expectedAuthID)
}

// GeminiCatalog filters against active registered native credentials and CPA
// key policy. The upstream registry is protocol-agnostic and may contain Codex
// and Claude models; serializing it as Gemini alone is not a routing boundary.
func (p *LegacyProfile) GeminiCatalog(base *handlers.BaseAPIHandler) gin.HandlerFunc {
	return func(c *gin.Context) {
		model, action, native := nativegemini.Parse(c.Request.Method, c.Request.URL.Path)
		if !native {
			c.Next()
			return
		}
		if base == nil || base.AuthManager == nil || p == nil || p.Keys == nil {
			abort(c, http.StatusServiceUnavailable, "CPA Gemini catalog unavailable")
			return
		}
		keyID := p.Keys.KeyID(c.GetString("userApiKey"))
		if keyID == "" {
			abort(c, http.StatusForbidden, "authenticated key has no CPA audit identity")
			return
		}
		query, err := url.ParseQuery(c.Request.URL.RawQuery)
		if err != nil || len(query["alt"]) > 1 || query.Has("alt") && (action != "streamGenerateContent" || query.Get("alt") != "sse") {
			abort(c, http.StatusBadRequest, "unsupported Gemini response encoding")
			return
		}
		modelRegistry := registry.GetGlobalRegistry()
		routable := map[string]bool{}
		for _, auth := range base.AuthManager.List() {
			if !nativegemini.Provider(auth.Provider) || auth.Disabled || auth.Status == coreauth.StatusDisabled {
				continue
			}
			for _, info := range modelRegistry.GetModelsForClient(auth.ID) {
				if info != nil && (!strings.Contains(info.ID, "/") || auth.Prefix == "" || strings.HasPrefix(info.ID, auth.Prefix+"/")) {
					routable[info.ID] = true
				}
			}
		}
		rows := []map[string]any{}
		for _, info := range modelRegistry.GetAvailableModelInfos() {
			name := info.ID
			if _, _, valid := nativegemini.Parse(http.MethodGet, "/v1beta/models/"+name); !valid || !routable[name] || p.Keys.Authorize(keyID, name) != nil {
				continue
			}
			// If the shared route can select a non-native backend, do not publish
			// it as native; the execution guard remains authoritative on retries.
			providers := modelRegistry.GetModelProviders(name)
			if len(providers) == 0 || slices.ContainsFunc(providers, func(provider string) bool { return !nativegemini.Provider(provider) }) {
				continue
			}
			copy := geminiMetadata(info)
			// Keep upstream capability declarations. Stream uses the same
			// generateContent capability; do not invent token-count support.
			if copy["supportedGenerationMethods"] == nil {
				copy["supportedGenerationMethods"] = []string{"generateContent"}
			}
			if model == name && action != "list" {
				if action == "get" {
					c.AbortWithStatusJSON(http.StatusOK, copy)
					return
				}
				method := action
				if method == "streamGenerateContent" {
					method = "generateContent"
				}
				methods, _ := copy["supportedGenerationMethods"].([]string)
				if !slices.Contains(methods, method) {
					abort(c, http.StatusBadRequest, "Gemini model does not advertise this operation")
					return
				}
				c.Next()
				return
			}
			rows = append(rows, copy)
		}
		if action != "list" {
			abort(c, http.StatusNotFound, "Gemini model is not available for this CPA key")
			return
		}
		slices.SortFunc(rows, func(a, b map[string]any) int { return strings.Compare(a["name"].(string), b["name"].(string)) })
		base.WriteModelListResponse(c, "gemini", gin.H{"models": rows})
		c.Abort()
	}
}

// Resource names use the registered route ID, not ModelInfo.Name, which may
// remain canonical after upstream prefixing. This preserves account namespaces.
func geminiMetadata(info *registry.ModelInfo) map[string]any {
	row := map[string]any{"name": "models/" + info.ID, "displayName": info.ID, "description": info.ID}
	for key, value := range map[string]string{"version": info.Version, "displayName": info.DisplayName, "description": info.Description} {
		if value != "" {
			row[key] = value
		}
	}
	for key, value := range map[string]int{"inputTokenLimit": info.InputTokenLimit, "outputTokenLimit": info.OutputTokenLimit} {
		if value > 0 {
			row[key] = value
		}
	}
	for key, value := range map[string][]string{"supportedGenerationMethods": info.SupportedGenerationMethods, "supportedInputModalities": info.SupportedInputModalities, "supportedOutputModalities": info.SupportedOutputModalities} {
		if len(value) > 0 {
			row[key] = slices.Clone(value)
		}
	}
	return row
}
