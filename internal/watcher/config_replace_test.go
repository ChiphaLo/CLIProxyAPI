package watcher

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestConfigWatcherSurvivesRepeatedAtomicGUIReplacement(t *testing.T) {
	dir := t.TempDir()
	auth := filepath.Join(dir, "auth")
	if err := os.Mkdir(auth, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.yaml")
	initial := "port: 18317\nauth-dir: " + strconv.Quote(auth) + "\n"
	if err := os.WriteFile(path, []byte(initial), 0600); err != nil {
		t.Fatal(err)
	}
	updates := make(chan int, 16)
	w, err := NewWatcher(path, auth, func(cfg *config.Config) { updates <- cfg.Port })
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Stop() }()
	w.SetConfigTransform(func(c *config.Config) error { c.AuthDir = auth; return nil })
	w.SetConfig(&config.Config{AuthDir: auth, Port: 18317})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := w.Start(ctx); err != nil {
		t.Fatal(err)
	}
	wait := func(port int) {
		t.Helper()
		deadline := time.NewTimer(5 * time.Second)
		defer deadline.Stop()
		for {
			select {
			case got := <-updates:
				if got == port {
					return
				}
			case <-deadline.C:
				t.Fatalf("no reload after atomic publication %d", port)
			}
		}
	}
	wait(18317)
	for _, port := range []int{18319, 18320, 18317} {
		body := "port: " + strconv.Itoa(port) + "\nauth-dir: " + strconv.Quote(auth) + "\n"
		if err := os.WriteFile(path+".publish", []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(path+".publish", path); err != nil {
			t.Fatal(err)
		}
		wait(port)
	}
}
