package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/gin-gonic/gin"
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
	quotaSlot := make(chan struct{}, 1)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		fail := func(status int) {
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "private management unavailable"})
		}
		if r.Method != http.MethodGet {
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
		case "/quota":
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
			snapshot := quota.Collect(r.Context(), bindings, lookup, fetcher, time.Now)
			_ = json.NewEncoder(w).Encode(snapshot)
		default:
			fail(404)
		}
	})
}
