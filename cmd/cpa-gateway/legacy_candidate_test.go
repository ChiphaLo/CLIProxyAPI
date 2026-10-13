package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/gateway"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestCandidateRequiresPrivateRootAndPinsPathsAcrossReload(t *testing.T) {
	root := t.TempDir()
	options := candidateOptions{Root: root, Listen: "127.0.0.1:18317", Config: filepath.Join(root, "config.yaml"), Receipts: filepath.Join(root, "usage.jsonl"), Audit: filepath.Join(root, "audit.jsonl"), Paths: gateway.LivePaths{ConfigDir: filepath.Join(root, "config"), AuthDir: filepath.Join(root, "auth"), KeyStore: filepath.Join(root, "keys.json"), AdminEnv: filepath.Join(root, "gateway.env")}}
	for _, dir := range []string{options.Paths.ConfigDir, options.Paths.AuthDir} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{options.Config, options.Receipts, options.Audit, options.Paths.KeyStore, options.Paths.AdminEnv} {
		if err := os.WriteFile(path, nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := candidateTransform(options); err == nil {
		t.Fatal("unmarked root accepted")
	}
	if err := os.WriteFile(filepath.Join(root, ".cpa-shadow-root"), []byte("cpa-shadow-validation-v1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	// Windows temporary roots may be returned with a short-name alias. The
	// deployment policy is verified on Linux; normalize the test root as well.
	real, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" && real != root {
		t.Skip("candidate root uses a Windows alias")
	}
	transform, err := candidateTransform(options)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Host: "0.0.0.0", Port: 8318, AuthDir: "/old-container/auth", RemoteManagement: config.RemoteManagement{SecretKey: "private-fixture"}}
	cfg.APIKeys = []string{"internal-fixture"}
	if err := transform(cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Host != "127.0.0.1" || cfg.Port != 18317 || cfg.AuthDir != options.Paths.AuthDir || cfg.RemoteManagement.SecretKey != "" || !cfg.WebsocketAuth || cfg.APIKeys[0] != "internal-fixture" {
		t.Fatal("candidate host policy changed credentials or escaped isolation")
	}
	outside := options
	outside.Receipts = filepath.Join(filepath.Dir(root), "outside.jsonl")
	if _, err := candidateTransform(outside); err == nil {
		t.Fatal("candidate output escaped private root")
	}
	public := options
	public.Listen = "0.0.0.0:18317"
	if _, err := candidateTransform(public); err == nil {
		t.Fatal("candidate listener exposed publicly")
	}
	if err := withinCandidate("/opt", "/opt/unified-ai-gateway/data/usage.jsonl"); err == nil {
		t.Fatal("known production data accepted by shadow mode")
	}
}
