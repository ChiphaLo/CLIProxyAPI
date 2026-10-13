package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/api"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/configbundle"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/durableauth"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/gateway"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/privatecontrol"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/privatefile"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/quota"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v8/sdk/access"
	sdkauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

type candidateOptions struct {
	Root, Listen, Config, Receipts, Audit string
	ControlSocket                         string
	ControlUID                            uint32
	Paths                                 gateway.LivePaths
}

// shadowStore locks the watcher's auth root to the explicit private copy even
// when GUI YAML still contains the old container-internal auth-dir value.
type shadowStore struct {
	*sdkauth.FileTokenStore
	root string
}

func (s *shadowStore) AuthDir() string { return s.root }

func withinCandidate(root, path string) error {
	if path == "" || !filepath.IsAbs(path) {
		return fmt.Errorf("candidate paths must be explicit and absolute")
	}
	for _, production := range []string{"/opt/unified-ai-gateway", "/opt/cpa-gui-shadow", "/var/lib/cpa-admin"} {
		if path == production || strings.HasPrefix(path, production+"/") {
			return fmt.Errorf("shadow runtime refuses known live production data paths")
		}
	}
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("candidate input/output must stay inside its dedicated root")
	}
	current := root
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("candidate paths must already exist without symlinks")
		}
	}
	return nil
}

func candidateTransform(options candidateOptions) (func(*config.Config) error, error) {
	root, err := filepath.Abs(options.Root)
	if err != nil || options.Root == "" {
		return nil, fmt.Errorf("candidate root must be explicit")
	}
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil || resolved != root || root == filepath.VolumeName(root)+string(filepath.Separator) {
		return nil, fmt.Errorf("candidate root must be a dedicated real directory")
	}
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() {
		return nil, fmt.Errorf("candidate root is not a directory")
	}
	marker, err := privatefile.Read(filepath.Join(root, ".cpa-shadow-root"), 4096, true)
	if err != nil || strings.TrimSpace(string(marker)) != "cpa-shadow-validation-v1" {
		return nil, fmt.Errorf("candidate root requires a private shadow-validation marker")
	}
	for _, path := range []string{options.Config, options.Receipts, options.Audit, options.Paths.ConfigDir, options.Paths.AuthDir, options.Paths.KeyStore, options.Paths.AdminEnv} {
		if err := withinCandidate(root, path); err != nil {
			return nil, err
		}
	}
	host, portText, err := net.SplitHostPort(options.Listen)
	port, errPort := strconv.Atoi(portText)
	if err != nil || errPort != nil || host != "127.0.0.1" && host != "::1" || port < 1024 || port > 65535 {
		return nil, fmt.Errorf("candidate listener must be explicit unprivileged loopback")
	}
	return func(cfg *config.Config) error {
		// These are runtime-only hosting constraints, not provider payload rules.
		// Existing GUI-owned YAML, providers, keys and account prefixes stay intact.
		cfg.Host, cfg.Port, cfg.AuthDir = host, port, options.Paths.AuthDir
		cfg.TrustedProxies = nil
		cfg.TLS.Enable = false
		cfg.RemoteManagement.SecretKey = ""
		cfg.RemoteManagement.AllowRemote = false
		cfg.RemoteManagement.DisableControlPanel = true
		cfg.Pprof.Enable = false
		cfg.Discovery.Enabled = false
		cfg.Plugins.Enabled = false
		cfg.Debug = false
		cfg.RequestLog = false
		cfg.LoggingToFile = false
		cfg.CommercialMode = true
		cfg.UsageStatisticsEnabled = false
		cfg.SaveCooldownStatus = false
		cfg.WebsocketAuth = true
		return nil
	}, nil
}

func runLegacyCandidate(options candidateOptions) error {
	transform, err := candidateTransform(options)
	if err != nil {
		return err
	}
	raw, err := privatefile.Read(options.Config, 4*1024*1024, true)
	if err != nil {
		return err
	}
	live, err := gateway.OpenLiveProfile(options.Paths)
	if err != nil {
		return err
	}
	cfg, err := config.ParseConfigBytes(raw)
	if err != nil {
		return fmt.Errorf("cannot load private legacy candidate YAML")
	}
	sink, err := gateway.OpenReceiptSink(options.Receipts, nil)
	if err != nil {
		return err
	}
	defer func() { _ = sink.Close() }()
	audit, err := gateway.OpenAuditSink(options.Audit)
	if err != nil {
		return err
	}
	defer func() { _ = audit.Close() }()
	store := &shadowStore{FileTokenStore: sdkauth.NewFileTokenStore(), root: options.Paths.AuthDir}
	store.SetBaseDir(options.Paths.AuthDir)
	guardedStore, err := durableauth.Open(options.Paths.AuthDir, options.Root, store, true)
	if err != nil {
		return err
	}
	sdkauth.RegisterTokenStore(guardedStore)
	manager := coreauth.NewManager(guardedStore, nil, nil)
	// A copied refresh token is still the same upstream secret. No shadow
	// runtime may rotate it or race the live production token owner.
	manager.DisableCredentialRefresh()
	live.RegisterAccess()
	accessManager := sdkaccess.NewManager()
	if err := accessManager.PinProvider(live); err != nil {
		return err
	}
	usage.RegisterNamedPlugin("chiphalo-legacy-receipts", sink)
	usage.RegisterNamedPlugin("chiphalo-legacy-audit", audit)
	registry.SetLocalModelCatalogs(true)
	ready := func() (int, map[string]any) {
		_, identityErr := live.Refresh()
		readiness := gateway.CatalogReadiness{}
		currentRaw, errRead := privatefile.Read(options.Config, 4*1024*1024, true)
		currentCfg, errParse := config.ParseConfigBytes(currentRaw)
		if identityErr == nil && errRead == nil && errParse == nil {
			readiness = live.ReviewedReadiness(context.Background(), currentCfg, manager)
		}
		ok := identityErr == nil && errRead == nil && errParse == nil && readiness.Ready && sink.Err() == nil && audit.Err() == nil && guardedStore.Err() == nil && !guardedStore.Pending()
		configurationReady := ok
		_, errFence := os.Lstat(filepath.Join(options.Paths.ConfigDir, configbundle.FenceName))
		pending := !os.IsNotExist(errFence)
		ok = ok && !pending
		digest := sha256.Sum256(currentRaw)
		status := http.StatusServiceUnavailable
		if ok {
			status = http.StatusOK
		}
		// Deliberately incompatible with the production release controller.
		return status, map[string]any{"ready": ok, "configuration_ready": configurationReady, "config_apply_pending": pending, "production_ready": false, "state_contract": "cpa-shadow-validation-v1", "config_sha256": hex.EncodeToString(digest[:]), "credential_refresh": false, "catalog": readiness}
	}
	serverOptions := []api.ServerOption{api.WithMiddleware(audit.Middleware(), func(c *gin.Context) {
		if c.Request.Method == http.MethodGet && c.Request.URL.Path == "/__cpa_release/ready" {
			status, value := ready()
			c.AbortWithStatusJSON(status, value)
			return
		}
		_, errFence := os.Lstat(filepath.Join(options.Paths.ConfigDir, configbundle.FenceName))
		if !os.IsNotExist(errFence) && !(c.Request.Method == http.MethodGet && c.Request.URL.Path == "/v1/models") {
			c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": gin.H{"type": "gateway_error", "message": "CPA configuration transaction is pending recovery"}})
			return
		}
		if guardedStore.Err() != nil {
			c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": gin.H{"type": "gateway_error", "message": "OAuth credential persistence is unconfirmed"}})
			return
		}
		c.Next()
	})}
	serverOptions = append(serverOptions, live.ServerOptions()...)
	service, err := cliproxy.NewBuilder().WithConfig(cfg).WithConfigPath(options.Config).WithCoreAuthManager(manager).WithRequestAccessManager(accessManager).
		WithConfigTransform(transform).WithModelCatalogTransform(live.ModelHints).WithServerOptions(serverOptions...).Build()
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	if options.ControlSocket != "" {
		if err := withinCandidate(options.Root, filepath.Dir(options.ControlSocket)); err != nil {
			return err
		}
		listener, err := privatecontrol.Listen(options.ControlSocket, options.ControlUID)
		if err != nil {
			return err
		}
		server := &http.Server{Handler: live.PrivateManagementWithActions(options.Config, manager, ready, quota.Fetcher{}, gateway.CatalogActions{Reload: service.ReloadReviewedModelCatalog}), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 10 * time.Second, MaxHeaderBytes: 8192}
		defer func() { _ = server.Close() }()
		go func() {
			if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
				cancel()
			}
		}()
	}
	errRun := service.Run(ctx)
	drain, cancelDrain := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelDrain()
	if err := usage.DefaultManager().StopAndWait(drain); err != nil {
		return err
	}
	if err := sink.Err(); err != nil {
		return err
	}
	if err := audit.Err(); err != nil {
		return err
	}
	if errRun != nil && !errors.Is(errRun, context.Canceled) {
		return errRun
	}
	return nil
}
