package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/catalogsync"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/configbundle"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/privatefile"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/quota"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

type CatalogActions struct{ Reload func(context.Context) error }

func catalogHash(raw []byte) string { value := sha256.Sum256(raw); return hex.EncodeToString(value[:]) }

func (p *LegacyProfile) privateCatalogIDs(cfg *config.Config, manager *coreauth.Manager) ([]string, error) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/v1/models", nil)
	c.Set("userApiKey", "admin")
	p.ModelCatalog(handlers.NewBaseAPIHandlers(&cfg.SDKConfig, manager))(c)
	var value struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &value) != nil || value.Data == nil || len(value.Data) > 5000 {
		return nil, errors.New("current public catalog unavailable")
	}
	ids := []string{}
	for _, row := range value.Data {
		ids = append(ids, row.ID)
	}
	slices.Sort(ids)
	return ids, nil
}

// ReviewCatalog is an explicit read/probe operation. It creates a CAS proposal,
// never writes files or registers provisional probe models into the live SDK.
func (l *LiveProfile) ReviewCatalog(ctx context.Context, configPath string, manager *coreauth.Manager, fetcher quota.Fetcher) (catalogsync.Publication, error) {
	var publication catalogsync.Publication
	profile, err := l.Refresh()
	if err != nil {
		return publication, err
	}
	paths := map[string]string{"config": configPath, "accounts": filepath.Join(l.paths.ConfigDir, "oauth-accounts.json"), "exposed": filepath.Join(l.paths.ConfigDir, "exposed-codex-models.json"), "context": filepath.Join(l.paths.ConfigDir, "context-windows.json")}
	raws := map[string][]byte{}
	for key, path := range paths {
		raw, err := privatefile.Read(path, 4*1024*1024, key == "config")
		if err != nil {
			return publication, errors.New("published review inputs unavailable")
		}
		raws[key] = raw
	}
	cfg, err := config.ParseConfigBytes(raws["config"])
	if err != nil || !l.ReviewedReadiness(ctx, cfg, manager).Ready {
		return publication, errors.New("published SDK catalog not ready for review")
	}
	oldModels, err := profile.privateCatalogIDs(cfg, manager)
	if err != nil {
		return publication, err
	}
	bindings := []quota.Binding{}
	byID := map[string]string{}
	enabled := map[string]bool{}
	for _, b := range profile.Accounts.Bindings() {
		bindings = append(bindings, quota.Binding{ID: b.ID, Prefix: b.Prefix, Label: b.Label, Workspace: b.WorkspaceID})
		byID[b.ID] = b.AuthID
		enabled[b.ID] = b.Enabled()
	}
	lookup := func(ctx context.Context, b quota.Binding) (quota.Credential, error) {
		a, found := manager.GetByID(byID[b.ID])
		if ctx.Err() != nil || !enabled[b.ID] || !found || profile.Accounts.ValidateAuth(a) != nil {
			return quota.Credential{}, errors.New("fixed catalog credential unavailable")
		}
		token, _ := a.Metadata["access_token"].(string)
		proxy := a.ProxyURL
		if proxy == "" {
			proxy = cfg.ProxyURL
		}
		return quota.Credential{Token: token, Workspace: b.Workspace, Proxy: proxy}, nil
	}
	seed := raws["exposed"]
	if !profile.Accounts.HasAccountCatalog() {
		var value any
		if configbundle.DecodeDraft(seed, &value) != nil {
			return publication, errors.New("legacy catalog invalid")
		}
		object, ok := value.(map[string]any)
		if !ok {
			object = map[string]any{"models": value}
		}
		groups := map[string][]string{}
		for _, b := range profile.Accounts.Bindings() {
			groups[b.ID] = []string{}
			for _, name := range profile.Accounts.ModelNamesForAuth(b.AuthID) {
				if registry.GetGlobalRegistry().ClientSupportsModel(b.AuthID, b.Prefix+"/"+name) {
					groups[b.ID] = append(groups[b.ID], name)
				}
			}
		}
		object["account_models"] = groups
		seed, _ = json.Marshal(object)
	}
	discovery := catalogsync.Discover(ctx, bindings, lookup, catalogsync.Fetcher{Transport: fetcher.Transport, ClientVersion: catalogsync.ClientVersion(cfg.CodexHeaderDefaults.UserAgent)}, time.Now)
	plan, err := catalogsync.BuildPlan(ctx, bindings, discovery, seed, raws["context"], l.CatalogProbe(cfg, manager, fetcher.Transport), time.Now())
	if err != nil {
		return publication, err
	}
	var exposed struct {
		Models   []string            `json:"models"`
		Accounts map[string][]string `json:"account_models"`
	}
	if configbundle.DecodeDraft(plan.Exposed, &exposed) != nil {
		return publication, errors.New("reviewed publication invalid")
	}
	set := map[string]bool{}
	for _, id := range oldModels {
		_, authID, err := profile.Accounts.Resolve(id, "/v1/responses")
		if err == nil && authID == "" {
			set[id] = true
		}
	}
	for _, b := range profile.Accounts.Bindings() {
		if !b.Enabled() {
			continue
		}
		for _, name := range exposed.Accounts[b.ID] {
			id := b.Prefix + "/" + name
			if b.Prefix == "codex" {
				id = name
			}
			set[id] = true
		}
	}
	newModels := []string{}
	for id := range set {
		newModels = append(newModels, id)
	}
	slices.Sort(newModels)
	for key, path := range paths {
		raw, err := privatefile.Read(path, 4*1024*1024, key == "config")
		if err != nil || catalogHash(raw) != catalogHash(raws[key]) {
			return publication, errors.New("catalog review inputs changed; proposal discarded")
		}
	}
	if ctx.Err() != nil {
		return publication, errors.New("catalog review canceled")
	}
	publication = catalogsync.Publication{Exposed: plan.Exposed, Context: plan.Context, OldExposed: catalogHash(raws["exposed"]), OldContext: catalogHash(raws["context"]), Config: catalogHash(raws["config"]), Accounts: catalogHash(raws["accounts"]), OldModels: oldModels, NewModels: newModels, Added: plan.Added, Removed: plan.Removed, Rejected: plan.Rejected, MorePending: plan.MorePending, Changed: strings.TrimSpace(string(raws["exposed"])) != strings.TrimSpace(string(plan.Exposed)) || plan.Changed}
	return publication, nil
}
