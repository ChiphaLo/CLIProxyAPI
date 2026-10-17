package gateway

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/api"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/clientkeys"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/traffic"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v8/sdk/access"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
)

// LivePaths references existing business files, never release-local snapshots.
// Private copies of these paths are used on the migration validation machine.
type LivePaths struct {
	ConfigDir, AuthDir, KeyStore, AdminEnv string
}

type fileStamp struct {
	path string
	info os.FileInfo
}
type profileContextKey struct{}

// LiveProfile atomically publishes fully validated identity snapshots. A bad
// edit fails closed for new admissions rather than keeping a revoked key alive.
// Old calls retain their account identity for execution and receipt attribution.
type LiveProfile struct {
	paths          LivePaths
	mu             sync.Mutex
	stamps         []fileStamp
	active         atomic.Pointer[LegacyProfile]
	continuations  *ConversationStore
	websocketSlots chan struct{}
	limits         *AdmissionLimits
}

func OpenLiveProfile(paths LivePaths) (*LiveProfile, error) {
	if paths.ConfigDir == "" || paths.AuthDir == "" || paths.KeyStore == "" || paths.AdminEnv == "" {
		return nil, fmt.Errorf("live CPA identity paths must be explicit")
	}
	l := &LiveProfile{paths: paths, continuations: NewConversationStore(), websocketSlots: make(chan struct{}, 8), limits: NewAdmissionLimits(32)}
	if _, err := l.Refresh(); err != nil {
		return nil, err
	}
	return l, nil
}

func (l *LiveProfile) stamp() ([]fileStamp, error) {
	dir, err := os.Lstat(l.paths.AuthDir)
	if err != nil || !dir.IsDir() || dir.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("live auth root is not a real directory")
	}
	entries, err := os.ReadDir(l.paths.AuthDir)
	if err != nil || len(entries) > 512 {
		return nil, fmt.Errorf("live auth directory exceeds its bound")
	}
	paths := []string{l.paths.KeyStore, l.paths.AdminEnv, filepath.Join(l.paths.ConfigDir, "oauth-accounts.json"), filepath.Join(l.paths.ConfigDir, "exposed-codex-models.json")}
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(strings.ToLower(entry.Name()), ".json") {
			paths = append(paths, filepath.Join(l.paths.AuthDir, entry.Name()))
		}
	}
	slices.Sort(paths)
	stamps := []fileStamp{{path: l.paths.AuthDir, info: dir}}
	for _, path := range paths {
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() {
			return nil, fmt.Errorf("live identity file is missing or unsafe")
		}
		stamps = append(stamps, fileStamp{path: path, info: info})
	}
	return stamps, nil
}

func sameStamps(a, b []fileStamp) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].path != b[i].path || !os.SameFile(a[i].info, b[i].info) || a[i].info.Size() != b[i].info.Size() ||
			a[i].info.Mode() != b[i].info.Mode() || !a[i].info.ModTime().Equal(b[i].info.ModTime()) {
			return false
		}
	}
	return true
}

// Refresh performs bounded metadata checks on every new admission. It reads
// credentials only on change, detects atomic GUI rewrites even at equal size,
// and validates the input set again before publishing one coherent snapshot.
func (l *LiveProfile) Refresh() (*LegacyProfile, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for attempt := 0; attempt < 3; attempt++ {
		before, err := l.stamp()
		if err != nil {
			return nil, err
		}
		if current := l.active.Load(); current != nil && sameStamps(l.stamps, before) {
			return current, nil
		}
		admin, err := clientkeys.ReadLegacyAdminKey(l.paths.AdminEnv)
		if err != nil {
			return nil, err
		}
		next, err := LoadLegacyProfile(admin, l.paths.KeyStore, l.paths.ConfigDir, l.paths.AuthDir)
		if err != nil {
			return nil, err
		}
		after, err := l.stamp()
		if err != nil {
			return nil, err
		}
		if !sameStamps(before, after) {
			continue
		}
		l.stamps = after
		l.active.Store(next)
		return next, nil
	}
	return nil, fmt.Errorf("live CPA identity files changed during validation")
}

func (l *LiveProfile) Identifier() string { return clientkeys.ProviderID }

func (l *LiveProfile) Authenticate(ctx context.Context, r *http.Request) (*sdkaccess.Result, *sdkaccess.AuthError) {
	p, err := l.Refresh()
	if err != nil {
		return nil, &sdkaccess.AuthError{Code: sdkaccess.AuthErrorCodeInternal, StatusCode: 503, Message: "CPA identity update is unavailable or invalid"}
	}
	result, authErr := p.Keys.Authenticate(ctx, r)
	if authErr == nil {
		*r = *r.WithContext(context.WithValue(r.Context(), profileContextKey{}, p))
	}
	return result, authErr
}

func (l *LiveProfile) RegisterAccess() {
	sdkaccess.RegisterProvider(l.Identifier(), l)
	sdkaccess.SetExclusiveProvider(l.Identifier())
}

// adminRPMExempt verifies the actual credential before skipping the IP quota.
// Reject non-admin candidates from the cached snapshot first, so repeated
// unauthenticated traffic cannot force identity-file scans beyond its IP limit.
// A matching candidate is checked again against refreshed state: rotation or
// invalid identity files must never leave an old admin exemption active.
func (l *LiveProfile) adminRPMExempt(c *gin.Context) bool {
	p := l.active.Load()
	if p == nil {
		return false
	}
	result, authErr := p.Keys.Authenticate(c.Request.Context(), c.Request)
	if authErr != nil || result == nil || result.Principal != "admin" {
		return false
	}
	p, err := l.Refresh()
	if err != nil {
		return false
	}
	result, authErr = p.Keys.Authenticate(c.Request.Context(), c.Request)
	return authErr == nil && result != nil && result.Principal == "admin"
}

// KeyLimit belongs immediately after authentication and before decompression,
// so a single key cannot occupy every global slot with 64 MiB decoded bodies.
func (l *LiveProfile) KeyLimit() gin.HandlerFunc {
	return func(c *gin.Context) {
		p, _ := c.Request.Context().Value(profileContextKey{}).(*LegacyProfile)
		if p == nil {
			abort(c, 503, "CPA request identity snapshot unavailable")
			return
		}
		id := c.GetString("userApiKey")
		websocket := c.Request.Method == http.MethodGet && strings.EqualFold(c.Request.Header.Get("Upgrade"), "websocket")
		rpm, concurrent, ok := p.Keys.Limits(id)
		if !ok {
			abort(c, 403, "CPA key no longer active")
			return
		}
		release, ok := l.limits.AcquireKey(id, rpm, concurrent, !websocket)
		if !ok {
			rateAbort(c, "CPA key rate or concurrency limit reached")
			return
		}
		defer release()
		c.Next()
	}
}

func (l *LiveProfile) Admission() gin.HandlerFunc {
	return func(c *gin.Context) {
		p, _ := c.Request.Context().Value(profileContextKey{}).(*LegacyProfile)
		if p == nil {
			abort(c, 503, "CPA request identity snapshot unavailable")
			return
		}
		var view atomic.Pointer[LegacyProfile]
		view.Store(p)
		var routeView atomic.Pointer[LegacyProfile]
		routeView.Store(p)
		id := c.GetString("userApiKey")
		websocket := c.Request.Method == http.MethodGet && strings.EqualFold(c.Request.Header.Get("Upgrade"), "websocket")
		options := AdmissionOptions{
			Continuations: l.continuations, websocketSlots: l.websocketSlots,
			KeyID: func(principal string) string {
				if websocket {
					fresh, err := l.Refresh()
					if err != nil {
						return ""
					}
					result, authErr := fresh.Keys.Authenticate(c.Request.Context(), c.Request)
					if authErr != nil || result.Principal != principal {
						return ""
					}
					view.Store(fresh)
				}
				return view.Load().Keys.KeyID(principal)
			},
			Authorize: func(id, model string) error { return view.Load().Keys.Authorize(id, model) },
			Resolve: func(model, path string) (string, string, error) {
				current := view.Load()
				if err := current.Keys.Authorize(id, model); err != nil {
					return "", "", err
				}
				routeView.Store(current)
				return current.Accounts.Resolve(model, path)
			},
			SnapshotDecorator: func() func(context.Context, string) context.Context { return routeView.Load().decorateAdmission },
			ChargeTurn: func(keyID string) bool {
				rpm, concurrent, ok := view.Load().Keys.Limits(keyID)
				return ok && l.limits.ChargeTurn(keyID, c.ClientIP(), rpm, concurrent)
			},
		}
		AdmissionWithOptions(options)(c)
	}
}

func (l *LiveProfile) ServerOptions() []api.ServerOption {
	var base *handlers.BaseAPIHandler
	return []api.ServerOption{
		api.WithMiddleware(l.limits.global(l.adminRPMExempt), PublicSurface()),
		api.WithProxyMiddleware(l.KeyLimit(), traffic.HTTP(), l.Admission(), func(c *gin.Context) {
			p, _ := c.Request.Context().Value(profileContextKey{}).(*LegacyProfile)
			if p == nil || base == nil {
				abort(c, 503, "CPA live catalog unavailable")
				return
			}
			p.ModelCatalog(base)(c)
		}),
		api.WithRouterConfigurator(func(_ *gin.Engine, b *handlers.BaseAPIHandler, _ *config.Config) { base = b }),
	}
}
