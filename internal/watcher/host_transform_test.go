package watcher

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestHostTransformAppliesBeforeSynthesisAndRejectsWithoutMutation(t *testing.T) {
	dir := t.TempDir()
	auth := filepath.Join(dir, "auth")
	if err := os.Mkdir(auth, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.yaml")
	raw := "host: 0.0.0.0\nport: 18318\nauth-dir: /old-container/auth\nremote-management:\n  secret-key: host-policy-fixture-only\n"
	if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	w := &Watcher{configPath: path, authDir: auth, lastAuthHashes: map[string]string{}}
	w.SetConfigTransform(func(cfg *config.Config) error {
		if cfg.Port == 13 {
			return errors.New("fixture policy rejection")
		}
		cfg.Host = "127.0.0.1"
		cfg.Port = 18317
		cfg.AuthDir = auth
		return nil
	})
	if !w.reloadConfig() {
		t.Fatal("accepted transform did not reload")
	}
	if w.config.AuthDir != auth || w.config.Port != 18317 || w.config.Host != "127.0.0.1" {
		t.Fatal("host constraint lost before auth synthesis")
	}
	saved, _ := os.ReadFile(path)
	if string(saved) != raw {
		t.Fatal("runtime transform rewrote shared source configuration")
	}
	old := w.config
	if err := os.WriteFile(path, []byte("port: 13\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if w.reloadConfig() {
		t.Fatal("host rejection still published a reload")
	}
	if w.config != old {
		t.Fatal("rejected edit partially replaced active configuration")
	}
}
