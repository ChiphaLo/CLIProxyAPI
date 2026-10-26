package cliproxy

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

func TestBuilderConfigTransformOnlyChangesRuntimeNotSource(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	source := "host: 0.0.0.0\nport: 8318\n"
	if err := os.WriteFile(path, []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Host: "0.0.0.0", Port: 8318, AuthDir: dir}
	called := 0
	s, err := NewBuilder().WithConfig(cfg).WithConfigPath(path).WithConfigTransform(func(c *config.Config) error { called++; c.Host = "127.0.0.1"; c.Port = 18317; return nil }).Build()
	if err != nil {
		t.Fatal(err)
	}
	if called != 1 || s.cfg.Host != "127.0.0.1" || s.cfg.Port != 18317 {
		t.Fatal("startup host transform not applied")
	}
	raw, _ := os.ReadFile(path)
	if string(raw) != source {
		t.Fatal("builder overwrote the shared configuration")
	}
}

func TestTransformedWatcherRetainsUpstreamAuthReconciliationCallbacks(t *testing.T) {
	dir := t.TempDir()
	w, err := newWatcherFactory(func(*config.Config) error { return nil })(filepath.Join(dir, "config.yaml"), dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if w.snapshotAuthFile == nil || w.rereadDiscardedFileSnapshot == nil || w.dispatchPersistedAuthWithRev == nil {
		t.Fatal("embedded watcher lost upstream credential reconciliation callbacks")
	}
}
