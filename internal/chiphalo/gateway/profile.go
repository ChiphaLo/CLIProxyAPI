package gateway

import (
	"fmt"
	"path/filepath"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/api"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/accounts"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/clientkeys"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/traffic"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v8/sdk/access"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
)

// LegacyProfile adapts real persisted CPA identities without replacing their
// IDs, hashing scheme, account prefixes or historical attribution boundaries.
type LegacyProfile struct {
	Keys     *clientkeys.Provider
	Accounts *accounts.Registry
}

func LoadLegacyProfile(admin, keyPath, configDir, authDir string) (*LegacyProfile, error) {
	keys, err := clientkeys.Load(admin, keyPath)
	if err != nil {
		return nil, fmt.Errorf("load existing CPA client keys: %w", err)
	}
	registry, err := accounts.Load(filepath.Join(configDir, "oauth-accounts.json"), authDir, filepath.Join(configDir, "exposed-codex-models.json"))
	if err != nil {
		return nil, fmt.Errorf("load registered CPA OAuth identities: %w", err)
	}
	return &LegacyProfile{Keys: keys, Accounts: registry}, nil
}

func (p *LegacyProfile) Admission() gin.HandlerFunc {
	return AdmissionWithOptions(AdmissionOptions{KeyID: p.Keys.KeyID, Authorize: p.Keys.Authorize, Resolve: p.Accounts.Resolve, Decorate: p.decorateAdmission})
}

// ServerOptions wires the profile into the actual upstream server. The router
// configurator runs before serving starts, allowing the catalog to use the
// server's own handlers (including future configuration reloads), not a stale
// independently constructed capability catalog. Call once per server instance.
func (p *LegacyProfile) ServerOptions() []api.ServerOption {
	var catalog gin.HandlerFunc
	return []api.ServerOption{
		api.WithMiddleware(PublicSurface()),
		api.WithProxyMiddleware(traffic.HTTP(), p.Admission(), func(c *gin.Context) {
			if catalog == nil {
				abort(c, 503, "CPA account catalog not initialized")
				return
			}
			catalog(c)
		}),
		api.WithRouterConfigurator(func(_ *gin.Engine, base *handlers.BaseAPIHandler, _ *config.Config) {
			catalog = p.ModelCatalog(base)
		}),
	}
}

// RegisterAccess is called before constructing the SDK service. Exclusive
// registration prevents the internal CPA backend key becoming a public bypass.
func (p *LegacyProfile) RegisterAccess() {
	sdkaccess.RegisterProvider(clientkeys.ProviderID, p.Keys)
	sdkaccess.SetExclusiveProvider(clientkeys.ProviderID)
}

func (p *LegacyProfile) ReceiptBindings() map[string]AccountBinding {
	items := map[string]AccountBinding{}
	for _, binding := range p.Accounts.Bindings() {
		items[binding.AuthID] = AccountBinding{ID: binding.ID, Prefix: binding.Prefix}
	}
	return items
}
