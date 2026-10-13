package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/hosting"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/privatecontrol"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/privatefile"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/quota"
)

func run() error {
	hosted := flag.String("hosting-manifest", "", "Explicit verified hosting manifest; independent of shadow mode")
	root := flag.String("shadow-root", "", "Copied-environment root; production mode is not enabled")
	socket := flag.String("control", "", "Authenticated SDK management socket inside shadow root")
	uid := flag.Uint("gateway-uid", 0, "Expected private gateway peer UID")
	once := flag.Bool("once", false, "Collect one snapshot and exit")
	interval := flag.Duration("interval", 60*time.Second, "Single-writer collection interval")
	flag.Parse()
	if *interval < 30*time.Second {
		return errors.New("bounded polling rate required")
	}
	var host *hosting.Manifest
	if *hosted != "" {
		if *root != "" || *socket != "" {
			return errors.New("hosted and shadow quota modes are mutually exclusive")
		}
		var err error
		host, err = hosting.Load(*hosted, "cpa-quota-collector")
		if err != nil {
			return err
		}
		if err := hosting.ConfirmLegacyExit(host); err != nil {
			return err
		}
		*socket = host.Control()
		*uid = uint(host.GatewayUID)
	}
	if host == nil {
		if !filepath.IsAbs(*root) || filepath.Clean(*root) != *root || *root == "/" || *uid > 4294967295 || *interval < 30*time.Second {
			return errors.New("explicit private shadow root and bounded polling rate required")
		}
		for _, live := range []string{"/opt/unified-ai-gateway", "/opt/cpa-gui-shadow", "/var/lib/cpa-admin"} {
			if *root == live || strings.HasPrefix(*root, live+"/") {
				return errors.New("quota shadow refuses production data paths")
			}
		}
		resolved, err := filepath.EvalSymlinks(*root)
		if err != nil || resolved != *root {
			return errors.New("real shadow root required")
		}
		marker, err := privatefile.Read(filepath.Join(*root, ".cpa-shadow-root"), 128, true)
		if err != nil || strings.TrimSpace(string(marker)) != "cpa-shadow-validation-v1" {
			return errors.New("private shadow marker required")
		}
		if !filepath.IsAbs(*socket) || filepath.Clean(*socket) != *socket || !strings.HasPrefix(*socket, *root+string(filepath.Separator)) {
			return errors.New("private quota control socket must remain inside shadow root")
		}
	}
	gateway, err := user.Lookup("cpa-gateway")
	if err != nil {
		return errors.New("captured gateway identity required")
	}
	dataUID, err := strconv.ParseUint(gateway.Uid, 10, 32)
	if err != nil {
		return err
	}
	observe, err := user.LookupGroup("cpa-observe")
	if err != nil {
		return errors.New("captured observation group required")
	}
	observeGID, err := strconv.ParseUint(observe.Gid, 10, 32)
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	data := filepath.Join(*root, "opt/unified-ai-gateway/data")
	if host != nil {
		data = filepath.Join(host.BusinessRoot, "data")
		if uint32(dataUID) != host.GatewayUID || uint32(observeGID) != host.ObserveGID {
			return errors.New("hosted quota identities differ from service database")
		}
	}
	store, err := quota.OpenStore(ctx, data, uint32(dataUID), uint32(observeGID))
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()
	client := privatecontrol.Client(*socket, uint32(*uid))
	defer client.CloseIdleConnections()
	collect := func() error {
		previous, err := store.Previous()
		if err != nil {
			return err
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://cpa-control/quota", nil)
		if err != nil {
			return err
		}
		response, err := client.Do(request)
		if err != nil {
			return errors.New("private quota acquisition unavailable")
		}
		defer func() { _ = response.Body.Close() }()
		if response.StatusCode != 200 {
			return errors.New("private quota acquisition rejected")
		}
		raw, err := io.ReadAll(io.LimitReader(response.Body, 1024*1024+1))
		if err != nil || len(raw) > 1024*1024 {
			return errors.New("private quota response unavailable")
		}
		current, err := quota.DecodeSnapshot(raw)
		if err != nil {
			return errors.New("private quota projection invalid")
		}
		current = quota.CarryPrevious(current, previous)
		if err := store.Save(current); err != nil {
			return err
		}
		historyErr := store.AppendHistory(current)
		rows := []map[string]any{}
		for _, account := range current.Accounts {
			rows = append(rows, map[string]any{"prefix": account.Prefix, "status": account.Status, "updated_at": account.Updated})
		}
		return json.NewEncoder(os.Stdout).Encode(map[string]any{"accounts": rows, "history_available": historyErr == nil, "production_cutover": false})
	}
	if *once {
		return collect()
	}
	ticker := time.NewTicker(*interval)
	defer ticker.Stop()
	for {
		if err := collect(); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}
func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "CPA Go quota collector:", err)
		os.Exit(1)
	}
}
