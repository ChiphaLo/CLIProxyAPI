//go:build linux

package main

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/adminhelper"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/hosting"
)

func serveHosted(ctx context.Context, path string) error {
	m, err := hosting.Load(path, "cpa-admin-helper")
	if err != nil {
		return err
	}
	if err := hosting.ConfirmLegacyExit(m); err != nil {
		return err
	}
	if err := hosting.ConfirmRuntimeOwner(m); err != nil {
		return err
	}
	paths := adminhelper.Paths{Keys: filepath.Join(m.BusinessRoot, "data/api-keys.json"), Vault: filepath.Join(m.AdminRoot, "provider-secrets.json"), Lock: filepath.Join(m.AdminRoot, "apply.lock"), Audit: filepath.Join(m.AdminRoot, "audit.jsonl")}
	for _, p := range []string{m.BusinessRoot, filepath.Join(m.BusinessRoot, "config"), m.AdminRoot, filepath.Join(m.AdminRoot, "transactions"), filepath.Dir(m.Helper())} {
		if !controlledDirectory(p) {
			return errors.New("hosted helper root ownership unsafe")
		}
	}
	if !controlledDirectory(filepath.Dir(paths.Keys), m.GatewayUID) {
		return errors.New("hosted key data root ownership unsafe")
	}
	service, err := adminhelper.New(paths)
	if err != nil {
		return err
	}
	if err := service.EnableWorkBuddy(filepath.Join(filepath.Dir(m.AdminRoot), "cpa-workbuddy-onboarding", "worker-access.private.json")); err != nil {
		return err
	}
	backend := adminhelper.NewUnixBackend(m.Control(), m.GatewayUID)
	if err := service.EnableInventory(backend); err != nil {
		return err
	}
	recovery, cancel := context.WithTimeout(ctx, 25*time.Second)
	err = service.EnableApply(recovery, adminhelper.ApplyOptions{ConfigDir: filepath.Join(m.BusinessRoot, "config"), Transactions: filepath.Join(m.AdminRoot, "transactions"), Backend: backend})
	cancel()
	if err != nil {
		return err
	}
	if _, err := os.Lstat(m.Helper()); !os.IsNotExist(err) {
		return errors.New("hosted helper socket already exists; refusing unlink")
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: m.Helper(), Net: "unix"})
	if err != nil {
		return err
	}
	defer func() { _ = listener.Close() }()
	if os.Chown(m.Helper(), 0, m.GUIGID) != nil || os.Chmod(m.Helper(), 0660) != nil {
		return errors.New("cannot secure hosted helper socket")
	}
	return service.Serve(ctx, listener, m.GUIUID)
}
