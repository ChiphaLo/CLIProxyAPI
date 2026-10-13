//go:build linux

package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/adminhelper"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/privatefile"
)

func controlledDirectory(path string, owners ...uint32) bool {
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0022 != 0 {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return false
	}
	if stat.Uid == uint32(os.Geteuid()) {
		return true
	}
	for _, owner := range owners {
		if stat.Uid == owner {
			return true
		}
	}
	return false
}

func serve(ctx context.Context, root, socket string, uid uint32, gid int, origin string) error {
	resolved, err := filepath.EvalSymlinks(root)
	if !filepath.IsAbs(root) || filepath.Clean(root) != root || root == "/" || !controlledDirectory(root) {
		return fmt.Errorf("a controlled private shadow root is required; production is unsupported")
	}
	if err != nil || resolved != root {
		return fmt.Errorf("shadow root must not traverse symlinks")
	}
	marker, err := privatefile.Read(filepath.Join(root, ".cpa-shadow-root"), 128, true)
	if err != nil || strings.TrimSpace(string(marker)) != "cpa-shadow-validation-v1" {
		return fmt.Errorf("shadow isolation marker required")
	}
	paths := adminhelper.Paths{
		Keys:  filepath.Join(root, "opt/unified-ai-gateway/data/api-keys.json"),
		Vault: filepath.Join(root, "var/lib/cpa-admin/provider-secrets.json"),
		Lock:  filepath.Join(root, "var/lib/cpa-admin/apply.lock"),
		Audit: filepath.Join(root, "var/lib/cpa-admin/audit.jsonl"),
	}
	// The captured key directory belongs to cpa-gateway, not root. Preserve that
	// ownership; only this fixed service identity may own the key parent.
	gateway, err := user.Lookup("cpa-gateway")
	if err != nil {
		return fmt.Errorf("captured gateway service identity required")
	}
	gatewayUID, err := strconv.ParseUint(gateway.Uid, 10, 32)
	if err != nil || uint32(gatewayUID) == uid {
		return fmt.Errorf("GUI and gateway identities must differ")
	}
	for _, file := range []string{paths.Keys, paths.Vault, paths.Lock, paths.Audit} {
		for dir := filepath.Dir(file); dir != filepath.Dir(root); dir = filepath.Dir(dir) {
			owners := []uint32{}
			if file == paths.Keys && dir == filepath.Dir(paths.Keys) {
				owners = append(owners, uint32(gatewayUID))
			}
			if !controlledDirectory(dir, owners...) {
				return fmt.Errorf("shadow control parents must be deployment-owner controlled")
			}
		}
	}
	socketParent, err := filepath.EvalSymlinks(filepath.Dir(socket))
	if err != nil || socketParent != filepath.Dir(socket) || !filepath.IsAbs(socket) || filepath.Clean(socket) != socket || !controlledDirectory(filepath.Dir(socket)) {
		return fmt.Errorf("controlled isolated socket directory required")
	}
	if _, err := os.Lstat(socket); !os.IsNotExist(err) {
		return fmt.Errorf("socket path already exists; refusing to unlink it")
	}
	service, err := adminhelper.New(paths)
	if err != nil {
		return err
	}
	if origin != "" {
		configDir := filepath.Join(root, "opt/unified-ai-gateway/config")
		transactions := filepath.Join(root, "var/lib/cpa-admin/transactions")
		for _, dir := range []string{configDir, transactions} {
			if !controlledDirectory(dir) {
				return fmt.Errorf("controlled configuration and transaction directories required")
			}
		}
		backend, err := adminhelper.NewHTTPBackend(origin, filepath.Join(root, "opt/unified-ai-gateway/gateway.env"))
		if err != nil {
			return err
		}
		recoveryCtx, cancel := context.WithTimeout(ctx, 25*time.Second)
		err = service.EnableApply(recoveryCtx, adminhelper.ApplyOptions{ConfigDir: configDir, Transactions: transactions, Backend: backend})
		cancel()
		if err != nil {
			return err
		}
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
	if err != nil {
		return fmt.Errorf("cannot create isolated control socket")
	}
	defer func() { _ = listener.Close() }()
	if os.Chown(socket, os.Geteuid(), gid) != nil || os.Chmod(socket, 0660) != nil {
		return fmt.Errorf("cannot secure isolated control socket")
	}
	return service.Serve(ctx, listener, uid)
}
