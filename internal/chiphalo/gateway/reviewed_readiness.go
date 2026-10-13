package gateway

import (
	"context"
	"reflect"
	"slices"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// ReviewedReadiness requires the actual SDK's per-member catalog to reflect the
// reviewed capability metadata, not merely contain one unrelated registered ID.
func (l *LiveProfile) ReviewedReadiness(ctx context.Context, cfg *config.Config, manager *coreauth.Manager) CatalogReadiness {
	profile, err := l.Refresh()
	if err != nil {
		return CatalogReadiness{MissingCredentials: 1}
	}
	result := profile.CheckCatalogReadiness(cfg, manager)
	if !result.Ready {
		return result
	}
	for _, binding := range profile.Accounts.Bindings() {
		if !binding.Enabled() {
			continue
		}
		auth, found := manager.GetByID(binding.AuthID)
		if !found {
			result.MissingCredentials++
			continue
		}
		actual := map[string]*registry.ModelInfo{}
		for _, info := range registry.GetGlobalRegistry().GetModelsForClient(binding.AuthID) {
			if info != nil {
				actual[info.ID] = info
			}
		}
		expected, err := l.ModelHints(ctx, auth, stockCodexCatalog(auth))
		if err != nil {
			result.MissingCatalogs++
			continue
		}
		for _, info := range expected {
			registered := actual[binding.Prefix+"/"+info.ID]
			if registered == nil || registered.ContextLength != info.ContextLength || registered.MaxContextLength != info.MaxContextLength || registered.ClientContextWindow != info.ClientContextWindow || registered.ClientMaxContextWindow != info.ClientMaxContextWindow || registered.ClientDefaultReasoningLevel != info.ClientDefaultReasoningLevel || !reflect.DeepEqual(registered.Thinking, info.Thinking) || !slices.Equal(registered.SupportedInputModalities, info.SupportedInputModalities) || registered.ExplicitInputModalities != info.ExplicitInputModalities || registered.ExplicitThinking != info.ExplicitThinking {
				result.MissingCatalogs++
				break
			}
		}
	}
	result.Ready = result.MissingCredentials == 0 && result.MissingCatalogs == 0 && result.UnexpectedCredentials == 0
	return result
}
