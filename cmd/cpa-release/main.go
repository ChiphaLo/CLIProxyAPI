// cpa-release is the isolation-only atomic release switchboard and control CLI.
// Production use is gated until the complete gateway migration passes.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/release"
)

func run() error {
	if len(os.Args) < 2 {
		return fmt.Errorf("usage: cpa-release serve|state|activate|rollback --root DIR --isolation-test")
	}
	action := os.Args[1]
	flags := flag.NewFlagSet(action, flag.ContinueOnError)
	root := flags.String("root", "", "Dedicated private release root")
	id := flags.String("id", "", "Release ID for activation")
	revision := flags.Uint64("expected-revision", 0, "Compare-and-swap revision (required for changing an active release)")
	listen := flags.String("listen", "127.0.0.1:0", "Explicit loopback data-plane listener")
	isolation := flags.Bool("isolation-test", false, "Acknowledge that this controller is not production-ready")
	if err := flags.Parse(os.Args[2:]); err != nil {
		return err
	}
	if !*isolation || *root == "" || flags.NArg() != 0 {
		return fmt.Errorf("this migration runtime requires --isolation-test and a private --root")
	}
	absolute, err := filepath.Abs(*root)
	if err != nil {
		return err
	}
	if action != "serve" {
		if action != "state" && action != "activate" && action != "rollback" {
			return fmt.Errorf("unknown release action")
		}
		client := release.ControlClient(absolute)
		defer client.CloseIdleConnections()
		method, path := http.MethodGet, "/state"
		var body io.Reader
		if action != "state" {
			method, path = http.MethodPost, "/change"
			if action == "activate" && *id == "" || action == "rollback" && *id != "" {
				return fmt.Errorf("activate requires --id; rollback must use the pinned previous release")
			}
			raw, err := json.Marshal(release.Change{Action: action, ID: *id, ExpectedRevision: *revision})
			if err != nil {
				return err
			}
			body = bytes.NewReader(raw)
		}
		req, err := http.NewRequest(method, "http://unix"+path, body)
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		response, err := client.Do(req)
		if err != nil {
			return fmt.Errorf("release control socket unavailable: %w", err)
		}
		defer func() { _ = response.Body.Close() }()
		raw, err := io.ReadAll(io.LimitReader(response.Body, 16385))
		if err != nil || len(raw) > 16384 {
			return fmt.Errorf("invalid release control response")
		}
		var reply release.Reply
		if err := json.Unmarshal(raw, &reply); err != nil {
			return err
		}
		_, _ = fmt.Fprintln(os.Stdout, string(raw))
		if response.StatusCode != http.StatusOK || reply.Error != "" {
			return fmt.Errorf("release transaction was not confirmed")
		}
		return nil
	}
	host, _, err := net.SplitHostPort(*listen)
	if err != nil || host != "127.0.0.1" && host != "::1" {
		return fmt.Errorf("data-plane listener must be explicit loopback")
	}
	controller, err := release.Open(absolute)
	if err != nil {
		return err
	}
	defer func() { _ = controller.Close() }()
	controlListener, err := controller.ListenControl()
	if err != nil {
		return err
	}
	defer func() { _ = controlListener.Close() }()
	listener, err := net.Listen("tcp", *listen)
	if err != nil {
		return err
	}
	defer func() { _ = listener.Close() }()
	control := &http.Server{Handler: controller.ControlHandler()}
	data := &http.Server{Handler: controller}
	errorsCh := make(chan error, 2)
	go func() { errorsCh <- control.Serve(controlListener) }()
	go func() { errorsCh <- data.Serve(listener) }()
	_, _ = fmt.Fprintln(os.Stdout, "isolation release switchboard listening:", listener.Addr())
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	select {
	case <-ctx.Done():
	case err := <-errorsCh:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	}
	// Control is closed first so no release transaction races with data drain.
	if err := control.Shutdown(context.Background()); err != nil {
		return err
	}
	return data.Shutdown(context.Background())
}

func main() {
	if err := run(); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "CPA release:", err)
		os.Exit(1)
	}
}
