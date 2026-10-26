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
	"strings"
	"syscall"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/privatefile"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/workbuddycli"
)

func run() error {
	var cfg workbuddycli.Config
	var listen, keyFile, fallback, fallbackModels string
	var nativeURL string
	var exportSource, exportDestination, exportUID string
	var maxQueued int
	flag.StringVar(&listen, "listen", "127.0.0.1:8333", "Loopback-only listener")
	flag.IntVar(&maxQueued, "max-queued", 8, "Bounded waiting requests behind the single official-client worker (0 disables waiting)")
	flag.StringVar(&nativeURL, "native-url", "", "Isolated loopback native Go upstream for the Intl DeepSeek model")
	flag.StringVar(&cfg.Node, "node", "/usr/bin/node", "Official client Node runtime")
	flag.StringVar(&cfg.Entry, "entry", "", "Unmodified official CLI launcher")
	flag.StringVar(&cfg.ProductFile, "product-file", "", "Read-only official product profile")
	flag.StringVar(&cfg.CredentialFile, "credential-file", "", "Existing refresh-owner credential; access token read only")
	flag.StringVar(&cfg.Home, "home", "", "Private worker workspace parent")
	flag.StringVar(&keyFile, "api-key-file", "", "Private downstream worker key file")
	flag.StringVar(&fallback, "legacy-url", "", "Original loopback WorkBuddy plugin origin")
	flag.StringVar(&fallbackModels, "legacy-models", "", "Comma-separated pre-existing model allowlist")
	flag.StringVar(&cfg.ExpectedUID, "account-uid", "", "Pinned WorkBuddy Intl account identity")
	flag.StringVar(&exportSource, "export-source", "", "Access-only projection source; exporter mode never runs the CLI")
	flag.StringVar(&exportDestination, "export-destination", "", "Protected access-only projection destination")
	flag.StringVar(&exportUID, "export-account-uid", "", "Pinned identity for the access projection")
	flag.Parse()
	if exportSource != "" {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		return workbuddycli.RunAccessExporter(ctx, exportSource, exportDestination, exportUID)
	}
	host, _, err := net.SplitHostPort(listen)
	if err != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() {
		return errors.New("official-client adapter must bind an explicit loopback IP")
	}
	raw, err := privatefile.Read(keyFile, 4096, true)
	if err != nil {
		return err
	}
	handler, err := workbuddycli.NewServerWithQueue(cfg, strings.TrimSpace(string(raw)), maxQueued)
	if err != nil {
		return err
	}
	if nativeURL != "" {
		if err = handler.SetNativeUpstream(nativeURL); err != nil {
			return err
		}
	}
	if fallback != "" {
		if err = handler.SetFallback(fallback, strings.Split(fallbackModels, ",")); err != nil {
			return err
		}
	}
	server := &http.Server{Addr: listen, Handler: handler, ReadHeaderTimeout: 10 * time.Second, MaxHeaderBytes: 32 << 10}
	signals, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	drained := make(chan struct{})
	go func() { <-signals.Done(); _ = server.Shutdown(context.Background()); close(drained) }()
	err = server.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		<-drained
		return nil
	}
	return err
}

func main() {
	if err := run(); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "official-client adapter:", err)
		os.Exit(1)
	}
}
