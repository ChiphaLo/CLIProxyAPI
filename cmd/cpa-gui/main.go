package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/privatefile"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/webgui"
)

func run() error {
	root := flag.String("shadow-root", "", "Copied-environment root; production hosting is not enabled")
	listen := flag.String("listen", "127.0.0.1:8320", "Explicit loopback-only GUI listener")
	socket := flag.String("helper", "", "Private helper Unix socket")
	uid := flag.Uint("helper-uid", 0, "Expected helper service UID")
	peers := flag.String("trusted-peers", "", "Comma-separated explicit loopback reverse proxy peers")
	flag.Parse()
	if !filepath.IsAbs(*root) || filepath.Clean(*root) != *root || *root == "/" || *root == "" || *uid > 4294967295 {
		return errors.New("explicit private shadow root required")
	}
	for _, live := range []string{"/opt/unified-ai-gateway", "/opt/cpa-gui-shadow", "/var/lib/cpa-admin"} {
		if *root == live || strings.HasPrefix(*root, live+"/") {
			return errors.New("GUI shadow refuses known production data paths")
		}
	}
	resolved, err := filepath.EvalSymlinks(*root)
	if err != nil || resolved != *root {
		return errors.New("real shadow root required")
	}
	marker, err := privatefile.Read(filepath.Join(*root, ".cpa-shadow-root"), 128, false)
	if err != nil || strings.TrimSpace(string(marker)) != "cpa-shadow-validation-v1" {
		return errors.New("shadow marker required")
	}
	host, _, err := net.SplitHostPort(*listen)
	if err != nil || host != "127.0.0.1" && host != "::1" {
		return errors.New("loopback GUI listener required")
	}
	var trusted []string
	if *peers != "" {
		trusted = strings.Split(*peers, ",")
	}
	base := filepath.Join(*root, "opt/unified-ai-gateway")
	server, err := webgui.New(webgui.Options{Data: filepath.Join(*root, "opt/cpa-gui-shadow/data"), Usage: filepath.Join(base, "data/usage.jsonl"), Quota: filepath.Join(base, "data/oauth-accounts.json"), Exposed: filepath.Join(base, "config/exposed-codex-models.json"), Origin: "https://cpa.chipha.de", Helper: webgui.UnixHelper{Path: *socket, UID: uint32(*uid)}, TrustedPeers: trusted})
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", *listen)
	if err != nil {
		return errors.New("cannot open shadow GUI listener")
	}
	service := &http.Server{Handler: server, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16384}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	go func() { <-ctx.Done(); _ = service.Close() }()
	err = service.Serve(listener)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "CPA Go GUI:", err)
		os.Exit(1)
	}
}
