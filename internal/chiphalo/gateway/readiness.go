package gateway

import (
	"maps"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/modelconfig"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/watcher/synthesizer"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// CatalogReadiness exposes counts only: neither credential IDs nor values are
// suitable for an unauthenticated loopback readiness response.
type CatalogReadiness struct {
	Ready                 bool `json:"ready"`
	MissingCredentials    int  `json:"missing_credentials"`
	MissingCatalogs       int  `json:"missing_catalogs"`
	UnexpectedCredentials int  `json:"unexpected_credentials"`
}

// CheckCatalogReadiness checks actual SDK registrations, not the listener or
// file inventory. The SDK starts HTTP before watcher-driven model registration.
// Config credentials are synthesized using upstream's own ID/policy logic so a
// changed key, endpoint, proxy, prefix or model mapping cannot reuse old readiness.
func (p *LegacyProfile) CheckCatalogReadiness(cfg *config.Config, manager *coreauth.Manager) CatalogReadiness {
	result := CatalogReadiness{}
	if p == nil || p.Accounts == nil || cfg == nil || manager == nil {
		result.MissingCredentials = 1
		return result
	}
	models := registry.GetGlobalRegistry()
	for _, binding := range p.Accounts.Bindings() {
		if !binding.Enabled() {
			continue
		}
		auth, found := manager.GetByID(binding.AuthID)
		if !found || p.Accounts.ValidateAuth(auth) != nil {
			result.MissingCredentials++
			continue
		}
		if len(models.GetModelsForClient(binding.AuthID)) == 0 {
			result.MissingCatalogs++
		}
	}
	expected, err := synthesizer.NewConfigSynthesizer().Synthesize(&synthesizer.SynthesisContext{
		Config: cfg, Now: time.Time{}, IDGenerator: synthesizer.NewStableIDGenerator(),
	})
	if err != nil {
		result.MissingCredentials++
		return result
	}
	wanted := map[string]bool{}
	for _, auth := range expected {
		wanted[auth.ID] = true
		loaded, found := manager.GetByID(auth.ID)
		if !found || !sameConfigCredential(auth, loaded) {
			result.MissingCredentials++
			continue
		}
		if auth.Attributes[coreauth.AttributeWeight] != "0" && !configCatalogLoaded(cfg, auth, models) {
			result.MissingCatalogs++
		}
	}
	for _, auth := range manager.List() {
		if auth != nil && strings.HasPrefix(auth.Attributes["source"], "config:") && !wanted[auth.ID] {
			result.UnexpectedCredentials++
		}
	}
	result.Ready = result.MissingCredentials == 0 && result.MissingCatalogs == 0 && result.UnexpectedCredentials == 0
	return result
}

func sameConfigCredential(expected, loaded *coreauth.Auth) bool {
	if loaded == nil || expected.Provider != loaded.Provider || expected.Prefix != loaded.Prefix || expected.ProxyURL != loaded.ProxyURL || expected.Disabled != loaded.Disabled {
		return false
	}
	return maps.Equal(expected.Attributes, loaded.Attributes)
}

func configCatalogLoaded(cfg *config.Config, auth *coreauth.Auth, models *registry.ModelRegistry) bool {
	if name := auth.Attributes["compat_name"]; name != "" {
		for _, provider := range cfg.OpenAICompatibility {
			if provider.Name != name || provider.Disabled {
				continue
			}
			registered := map[string]*registry.ModelInfo{}
			for _, info := range models.GetModelsForClient(auth.ID) {
				if info != nil {
					registered[info.ID] = info
				}
			}
			for _, model := range provider.Models {
				alias := strings.TrimSpace(model.Alias)
				if alias == "" {
					alias = strings.TrimSpace(model.Name)
				}
				metadataID := strings.TrimSpace(model.Name)
				if metadataID == "" {
					metadataID = alias
				}
				if auth.Prefix != "" {
					alias = auth.Prefix + "/" + alias
				}
				info := registered[alias]
				if !models.ClientSupportsModel(auth.ID, alias) || info == nil || info.MetadataModelID != metadataID {
					return false
				}
				// An unchanged alias is insufficient: context, reasoning, image
				// capabilities and display metadata also reload asynchronously.
				if model.MaxContextLength > 0 && info.MaxContextLength != model.MaxContextLength {
					return false
				}
				if model.DisplayName != "" && info.DisplayName != strings.TrimSpace(model.DisplayName) {
					return false
				}
				if model.Thinking != nil && !reflect.DeepEqual(info.Thinking, modelconfig.NormalizeThinkingSupport(model.Thinking)) {
					return false
				}
				if len(model.InputModalities) > 0 && !slices.Equal(info.SupportedInputModalities, readinessModalities(model.InputModalities)) {
					return false
				}
				if len(model.OutputModalities) > 0 && !slices.Equal(info.SupportedOutputModalities, readinessModalities(model.OutputModalities)) {
					return false
				}
			}
			// A provider with no declared models is intentionally non-callable.
			return true
		}
		return false
	}
	return len(models.GetModelsForClient(auth.ID)) > 0
}

// Match upstream's trim/lowercase/dedup serialization without changing config.
func readinessModalities(raw []string) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, item := range raw {
		item = strings.ToLower(strings.TrimSpace(item))
		if item != "" && !seen[item] {
			out = append(out, item)
			seen[item] = true
		}
	}
	return out
}
