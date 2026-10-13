package main

import (
	"context"
	"errors"
	"net"
	"path/filepath"
	"strconv"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/gateway"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/hosting"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/refreshowner"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

// The hosted entrypoint is independently validated; it cannot turn a legacy
// shadow CLI launch into production by removing its root or readiness guards.
func runHosted(path string) error {
	m, err := hosting.Load(path, "cpa-gateway")
	if err != nil {
		return err
	}
	if err := hosting.ConfirmLegacyExit(m); err != nil {
		return err
	}
	if err := hosting.ConfirmRuntimeOwner(m); err != nil {
		return err
	}
	options := candidateOptions{Root: m.StateRoot, Listen: m.GatewayListen, Config: m.Config(), Receipts: filepath.Join(m.BusinessRoot, "data/usage.jsonl"), Audit: filepath.Join(m.BusinessRoot, "data/audit.jsonl"), ControlSocket: m.Control(), ControlUID: 0, Hosted: m,
		Paths: gateway.LivePaths{ConfigDir: filepath.Join(m.BusinessRoot, "config"), AuthDir: filepath.Join(m.BusinessRoot, "auth"), KeyStore: filepath.Join(m.BusinessRoot, "data/api-keys.json"), AdminEnv: filepath.Join(m.BusinessRoot, "gateway.env")}}
	return runLegacyCandidate(options)
}
func hostedTransform(options candidateOptions) (func(*config.Config) error, error) {
	host, port, err := net.SplitHostPort(options.Hosted.GatewayListen)
	n, e := strconv.Atoi(port)
	if err != nil || e != nil {
		return nil, errors.New("invalid hosted listener")
	}
	return func(cfg *config.Config) error {
		cfg.Host, cfg.Port, cfg.AuthDir = host, n, options.Paths.AuthDir
		cfg.TrustedProxies = options.Hosted.TrustedPeers
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

func hostedDrainContext() context.Context { return context.Background() }

func checkHostedDrain(path string) error {
	m, err := hosting.Load(path, "cpa-gateway")
	if err != nil {
		return err
	}
	if err := hosting.ConfirmLegacyExit(m); err != nil {
		return err
	}
	lease, err := refreshowner.Open(m.OAuthHealth())
	if err != nil {
		return err
	}
	return lease.Close(context.Background())
}
