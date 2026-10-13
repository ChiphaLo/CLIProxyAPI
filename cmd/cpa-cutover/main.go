//go:build linux

package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/cutover"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
)

func run() error {
	if len(os.Args) < 2 {
		return errors.New("usage: cpa-cutover prepare|state|apply|rollback --root DIR --hosting-manifest FILE --production-cutover")
	}
	action := os.Args[1]
	flags := flag.NewFlagSet(action, flag.ContinueOnError)
	root := flags.String("root", "", "Private durable supervisor journal")
	manifest := flags.String("hosting-manifest", "", "Immutable pinned hosting manifest")
	production := flags.Bool("production-cutover", false, "Explicit production opt-in; never inferred from hosting paths")
	rev := flags.Uint64("expected-revision", 0, "Required apply compare-and-swap revision")
	ack := flags.Bool("rollback-acknowledged", false, "Acknowledge the previously displayed one-command rollback")
	if err := flags.Parse(os.Args[2:]); err != nil {
		return err
	}
	if !*production || flags.NArg() != 0 {
		return errors.New("explicit production-cutover acknowledgement required")
	}
	if action != "prepare" && action != "state" && action != "apply" && action != "rollback" {
		return errors.New("unknown cutover action")
	}
	backend, plan, err := cutover.NewSystem(*root, *manifest)
	if err != nil {
		return err
	}
	// This creates only the private journal root, not any service/runtime or
	// business file. No production action runs before prepare has printed recovery.
	if action == "prepare" {
		if err := cutover.CreateJournalRoot(*root); err != nil {
			return err
		}
	}
	c, err := cutover.Open(*root, backend, plan)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if filepath.Clean(exe) != filepath.Join(backend.Manifest.ProgramRoot, "cpa-cutover") {
		return errors.New("invoke the pinned immutable supervisor path")
	}
	command := fmt.Sprintf("%s rollback --root %s --hosting-manifest %s --production-cutover", exe, *root, *manifest)
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	state := c.State()
	switch action {
	case "prepare":
		state, err = c.Prepare(ctx, plan)
	case "apply":
		if !*ack || *rev == 0 {
			return errors.New("apply requires expected revision and rollback acknowledgement")
		}
		// The recovery command is printed before the first stop operation.
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"rollback_command": command, "about_to_stop_legacy": true})
		state, err = c.Apply(ctx, *rev, plan)
	case "rollback":
		state, err = c.Rollback(ctx, state.Revision)
	}
	_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"phase": state.Phase, "revision": state.Revision, "plan_sha256": state.PlanSHA, "rollback_command": command, "business_snapshots_restored": false, "confirmed": err == nil})
	return err
}
func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "CPA cutover:", err)
		os.Exit(1)
	}
}
