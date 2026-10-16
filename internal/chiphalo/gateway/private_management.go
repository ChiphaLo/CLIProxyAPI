package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/catalogsync"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/configbundle"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/privatefile"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/quota"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// PrivateManagement is an allowlisted projection, not a reverse proxy to the
// deprecated management API. Host it only on a mutually authenticated Unix socket.
func (l *LiveProfile) PrivateManagement(configPath string, manager *coreauth.Manager, ready func() (int, map[string]any), fetchers ...quota.Fetcher) http.Handler {
	fetcher := quota.Fetcher{}
	if len(fetchers) > 0 {
		fetcher = fetchers[0]
	}
	return l.PrivateManagementWithActions(configPath, manager, ready, fetcher, CatalogActions{})
}

func (l *LiveProfile) PrivateManagementWithActions(configPath string, manager *coreauth.Manager, ready func() (int, map[string]any), fetcher quota.Fetcher, actions CatalogActions) http.Handler {
	quotaSlot := make(chan struct{}, 1)
	catalogSlot := make(chan struct{}, 1)
	consumeSlot := make(chan struct{}, 1)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		fail := func(status int) {
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "private management unavailable"})
		}
		if r.Method != http.MethodGet {
			if r.Method == http.MethodPost && r.URL.Path == "/consume-reset-credit" && r.URL.RawQuery == "" && actions.AllowResetConsume {
				select {
				case consumeSlot <- struct{}{}:
					defer func() { <-consumeSlot }()
				default:
					fail(429)
					return
				}
				body, err := io.ReadAll(io.LimitReader(r.Body, 2049))
				if err != nil {
					fail(400)
					return
				}
				result, err := l.ConsumeReset(r.Context(), configPath, manager, fetcher, body)
				if err != nil {
					fail(400)
					return
				}
				_ = json.NewEncoder(w).Encode(result)
				return
			}
			if r.Method == http.MethodPost && (r.URL.Path == "/catalog-review" || r.URL.Path == "/reload-reviewed-models") && r.URL.RawQuery == "" && actions.Reload != nil {
				body, err := io.ReadAll(io.LimitReader(r.Body, 3))
				if err != nil || len(body) != 0 {
					fail(400)
					return
				}
				if r.URL.Path == "/reload-reviewed-models" {
					if err := actions.Reload(r.Context()); err != nil {
						fail(503)
						return
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
					return
				}
				select {
				case catalogSlot <- struct{}{}:
					defer func() { <-catalogSlot }()
				default:
					fail(429)
					return
				}
				if _, err := os.Lstat(filepath.Join(l.paths.ConfigDir, configbundle.FenceName)); !os.IsNotExist(err) {
					fail(503)
					return
				}
				publication, err := l.ReviewCatalog(r.Context(), configPath, manager, fetcher)
				if err != nil {
					fail(503)
					return
				}
				_ = json.NewEncoder(w).Encode(publication)
				return
			}
			fail(405)
			return
		}
		if r.URL.Path == "/__cpa_release/ready" {
			status, value := ready()
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(value)
			return
		}
		p, err := l.Refresh()
		if err != nil {
			fail(503)
			return
		}
		raw, err := privatefile.Read(configPath, 4*1024*1024, true)
		if err != nil {
			fail(503)
			return
		}
		cfg, err := config.ParseConfigBytes(raw)
		if err != nil {
			fail(503)
			return
		}
		switch r.URL.Path {
		case "/v1/models":
			c, _ := gin.CreateTestContext(w)
			c.Request = r
			c.Set("userApiKey", "admin")
			p.ModelCatalog(handlers.NewBaseAPIHandlers(&cfg.SDKConfig, manager))(c)
		case "/seed":
			if _, err := os.Lstat(filepath.Join(l.paths.ConfigDir, configbundle.FenceName)); !os.IsNotExist(err) {
				fail(503)
				return
			}
			contextRaw, e1 := privatefile.Read(filepath.Join(l.paths.ConfigDir, "context-windows.json"), 4*1024*1024, false)
			exposedRaw, e2 := privatefile.Read(filepath.Join(l.paths.ConfigDir, "exposed-codex-models.json"), 1024*1024, false)
			value, err := configbundle.Inventory(raw, contextRaw, exposedRaw)
			if e1 != nil || e2 != nil || err != nil {
				fail(503)
				return
			}
			_ = json.NewEncoder(w).Encode(value)
		case "/accounts":
			rows := []map[string]any{}
			for _, b := range p.Accounts.Bindings() {
				rows = append(rows, map[string]any{"id": b.ID, "prefix": b.Prefix, "label": b.Label, "enabled": b.Enabled()})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "accounts": rows})
		case "/quota", "/reset-credits", "/codex-models":
			if r.URL.RawQuery != "" || manager == nil {
				fail(400)
				return
			}
			select {
			case quotaSlot <- struct{}{}:
				defer func() { <-quotaSlot }()
			default:
				fail(429)
				return
			}
			bindings := []quota.Binding{}
			byID := map[string]string{}
			enabled := map[string]bool{}
			for _, b := range p.Accounts.Bindings() {
				bindings = append(bindings, quota.Binding{ID: b.ID, Prefix: b.Prefix, Label: b.Label, Workspace: b.WorkspaceID, LegacySince: b.LegacySince})
				byID[b.ID] = b.AuthID
				enabled[b.ID] = b.Enabled()
			}
			lookup := func(ctx context.Context, b quota.Binding) (quota.Credential, error) {
				if ctx.Err() != nil {
					return quota.Credential{}, errors.New("quota acquisition canceled")
				}
				auth, found := manager.GetByID(byID[b.ID])
				if !enabled[b.ID] || !found || p.Accounts.ValidateAuth(auth) != nil {
					return quota.Credential{}, errors.New("registered credential unavailable")
				}
				token, _ := auth.Metadata["access_token"].(string)
				proxy := auth.ProxyURL
				if proxy == "" {
					proxy = cfg.ProxyURL
				}
				return quota.Credential{Token: token, Workspace: b.Workspace, Proxy: proxy}, nil
			}
			if r.URL.Path == "/codex-models" {
				models := catalogsync.Fetcher{Transport: fetcher.Transport, ClientVersion: catalogsync.ClientVersion(cfg.CodexHeaderDefaults.UserAgent)}
				_ = json.NewEncoder(w).Encode(catalogsync.Discover(r.Context(), bindings, lookup, models, time.Now))
			} else if r.URL.Path == "/reset-credits" {
				_ = json.NewEncoder(w).Encode(quota.CollectResetCredits(r.Context(), bindings, lookup, fetcher, time.Now))
			} else {
				snapshot := quota.Collect(r.Context(), bindings, lookup, fetcher, time.Now)
				snapshot.Antigravity = collectAntigravityQuota(r.Context(), manager, cfg.ProxyURL, fetcher)
				_ = json.NewEncoder(w).Encode(snapshot)
			}
		default:
			fail(404)
		}
	})
}
