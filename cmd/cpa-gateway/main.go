// cpa-gateway is the incremental Go replacement for the deployed gateway.
// It is not yet a replacement for the GUI/helper/quota services.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/api"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/clientkeys"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/gateway"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/traffic"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	_ "github.com/router-for-me/CLIProxyAPI/v8/internal/translator"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

func run() error {
	hostedManifest := flag.String("hosting-manifest", "", "Explicit verified hosting manifest; independent of shadow mode")
	drained := flag.Bool("check-drained", false, "Check refresh ownership release without loading credentials or starting a server")
	configPath := flag.String("config", "config.yaml", "Upstream v8 or legacy configuration file")
	receiptsPath := flag.String("receipts", "", "Private usage.jsonl output (required)")
	localModel := flag.Bool("local-model", false, "Use embedded model catalogs")
	isolationTest := flag.Bool("isolation-test", false, "Enable the restricted synthetic-credential integration runtime")
	legacyCandidate := flag.Bool("legacy-candidate", false, "Validate real legacy identity/config wiring using private copies; not a production launch")
	candidateRoot := flag.String("candidate-root", "", "Dedicated real directory containing every candidate input and output")
	candidateListen := flag.String("candidate-listen", "127.0.0.1:18317", "Loopback-only candidate listener")
	controlSocket := flag.String("candidate-control", "", "Optional mutually authenticated private Unix management socket")
	controlUID := flag.Uint("candidate-control-uid", 0, "Explicit authorized private management peer UID")
	auditPath := flag.String("audit", "", "Private legacy audit.jsonl output for candidate validation")
	checkProfile := flag.Bool("check-legacy-profile", false, "Read-only identity preflight; opens no listener and refreshes no tokens")
	configDir := flag.String("legacy-config-dir", "", "Private frozen CPA config directory for preflight")
	authDir := flag.String("legacy-auth-dir", "", "Private frozen OAuth directory for preflight")
	keyStore := flag.String("legacy-key-store", "", "Existing SHA-256 issued-key database for preflight")
	legacyEnv := flag.String("legacy-env", "", "Private literal gateway.env for read-only preflight")
	flag.Parse()
	if *hostedManifest != "" {
		if *legacyCandidate || *checkProfile || *isolationTest {
			return fmt.Errorf("hosted and shadow modes are mutually exclusive")
		}
		if *drained {
			return checkHostedDrain(*hostedManifest)
		}
		return runHosted(*hostedManifest)
	}
	if *drained {
		return fmt.Errorf("refresh drain check requires verified hosting")
	}
	if *controlUID > 4294967295 {
		return fmt.Errorf("invalid private management peer UID")
	}
	if *legacyCandidate {
		if *checkProfile || *isolationTest {
			return fmt.Errorf("candidate, identity check and synthetic modes are mutually exclusive")
		}
		return runLegacyCandidate(candidateOptions{Root: *candidateRoot, Listen: *candidateListen, Config: *configPath, Receipts: *receiptsPath, Audit: *auditPath, ControlSocket: *controlSocket, ControlUID: uint32(*controlUID),
			Paths: gateway.LivePaths{ConfigDir: *configDir, AuthDir: *authDir, KeyStore: *keyStore, AdminEnv: *legacyEnv}})
	}
	if *checkProfile {
		if *configDir == "" || *authDir == "" || *keyStore == "" {
			return fmt.Errorf("identity preflight requires explicit private legacy input paths")
		}
		admin := os.Getenv("UNIFIED_GATEWAY_API_KEY")
		if *legacyEnv != "" {
			var err error
			admin, err = clientkeys.ReadLegacyAdminKey(*legacyEnv)
			if err != nil {
				return err
			}
		}
		profile, err := gateway.LoadLegacyProfile(admin, *keyStore, *configDir, *authDir)
		if err != nil {
			return err
		}
		bindings := profile.Accounts.Bindings()
		prefixes := []string{}
		workspaces := map[string]bool{}
		for _, binding := range bindings {
			prefixes = append(prefixes, binding.Prefix)
			workspaces[binding.WorkspaceID] = true
		}
		return json.NewEncoder(os.Stdout).Encode(map[string]any{"identity_preflight": "passed", "production_ready": false,
			"issued_keys": profile.Keys.Count(), "oauth_accounts": len(bindings), "oauth_prefixes": prefixes,
			"distinct_workspaces": len(workspaces), "public_oauth_models": profile.Accounts.PublicModelIDs(),
			"admin_key_source": "existing_public_admin_key", "credentials_refreshed": false, "files_modified": false})
	}
	// The account registry, DSML policy, GUI and helper migrations are still in
	// progress. Refuse general production use until those gates are complete.
	if !*isolationTest || *receiptsPath == "" {
		return fmt.Errorf("this incremental runtime requires --isolation-test and --receipts; production migration is not complete")
	}
	path, err := filepath.Abs(*configPath)
	if err != nil {
		return err
	}
	cfg, err := config.LoadConfig(path)
	if err != nil {
		return err
	}
	if len(cfg.APIKeys) != 1 || cfg.APIKeys[0] != "wire-client-test-only" || len(cfg.OpenAICompatibility) != 1 {
		return fmt.Errorf("isolation runtime accepts only the synthetic wire fixture")
	}
	provider := cfg.OpenAICompatibility[0]
	if provider.BaseURL != "http://wire-mock:18330/v1" || cfg.RemoteManagement.SecretKey != "" {
		return fmt.Errorf("isolation runtime cannot connect to a real provider or enable management")
	}
	entries, err := os.ReadDir(cfg.AuthDir)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if len(entries) != 0 {
		return fmt.Errorf("isolation runtime refuses nonempty OAuth auth directories")
	}
	sink, err := gateway.OpenReceiptSink(*receiptsPath, nil)
	if err != nil {
		return err
	}
	defer func() { _ = sink.Close() }()
	usage.RegisterNamedPlugin("chiphalo-legacy-receipts", sink)
	registry.SetLocalModelCatalogs(*localModel)
	keyHash := sha256.Sum256([]byte(cfg.APIKeys[0]))
	admission := gateway.Admission(func(principal string) string {
		if sha256.Sum256([]byte(principal)) == keyHash {
			return "admin"
		}
		return ""
	})
	service, err := cliproxy.NewBuilder().WithConfig(cfg).WithConfigPath(path).
		WithServerOptions(api.WithMiddleware(gateway.PublicSurface()), api.WithProxyMiddleware(traffic.HTTP(), admission)).Build()
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	errRun := service.Run(ctx)
	drainCtx, cancelDrain := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelDrain()
	if err := usage.DefaultManager().StopAndWait(drainCtx); err != nil {
		return fmt.Errorf("drain CPA receipts: %w", err)
	}
	if err := sink.Err(); err != nil {
		return err
	}
	if errRun != nil && !errors.Is(errRun, context.Canceled) {
		return errRun
	}
	return nil
}

func main() {
	logging.SetupBaseLogger()
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "CPA Go gateway:", err)
		os.Exit(1)
	}
}
