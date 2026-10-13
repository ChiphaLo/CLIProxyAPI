// Package cutover coordinates the initial non-cooperative legacy writer handoff.
// Only program/service selection is journaled; business postimages are never
// restored as part of rollback. The old writer must exit before Go starts.
package cutover

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/configbundle"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/privatefile"
)

type Snapshot struct {
	Units            map[string]bool   `json:"active_units"`
	ContainerRunning bool              `json:"container_running"`
	ContainerRestart string            `json:"container_restart"`
	ContainerRetries int               `json:"container_retries"`
	UnitDigests      map[string]string `json:"unit_sha256"`
	Processes        []Process         `json:"processes"`
}
type Process struct {
	Role  string `json:"role"`
	PID   uint32 `json:"pid"`
	Start uint64 `json:"start_ticks"`
}
type State struct {
	Version  int      `json:"version"`
	Revision uint64   `json:"revision"`
	Phase    string   `json:"phase"`
	PlanSHA  string   `json:"plan_sha256"`
	Legacy   Snapshot `json:"legacy"`
}
type Backend interface {
	Capture(context.Context) (Snapshot, error)
	StopLegacy(context.Context, Snapshot) error
	ConfirmLegacyExit(context.Context, Snapshot) error
	PrepareGo(context.Context, Snapshot) error
	SelectGo(context.Context) error
	StartGo(context.Context) error
	GoReady(context.Context) error
	StopGo(context.Context) error
	ConfirmGoExit(context.Context) error
	SelectLegacy(context.Context, Snapshot) error
	StartLegacy(context.Context, Snapshot) error
	LegacyReady(context.Context, Snapshot) error
}
type Controller struct {
	root       string
	backend    Backend
	mu         sync.Mutex
	lock       *os.File
	state      State
	planSHA    string
	journalSHA string
}

func Open(root string, backend Backend, planSHA string) (*Controller, error) {
	if !digestPattern.MatchString(planSHA) || backend == nil || !filepath.IsAbs(root) || filepath.Clean(root) != root || root == string(filepath.Separator) {
		return nil, errors.New("explicit cutover root/backend required")
	}
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil || resolved != root {
		return nil, errors.New("cutover root must not traverse symlinks")
	}
	lock, err := lockRoot(root)
	if err != nil {
		return nil, err
	}
	c := &Controller{root: root, backend: backend, lock: lock, planSHA: planSHA, state: State{Version: 1, Phase: "empty"}}
	path := filepath.Join(root, "cutover.json")
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		raw, err := privatefile.Read(path, 65536, true)
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.DisallowUnknownFields()
		if err != nil || configbundle.DecodeDraft(raw, &c.state) != nil || decoder.Decode(&c.state) != nil || c.state.Version != 1 || c.state.Revision == 0 || !validPhase(c.state.Phase) || c.state.PlanSHA != planSHA || validateSnapshot(c.state.Legacy) != nil {
			_ = c.Close()
			return nil, errors.New("cutover journal invalid")
		}
		c.journalSHA = hashBytes(raw)
	}
	return c, nil
}
func validPhase(s string) bool {
	switch s {
	case "prepared", "stopping-legacy", "preparing-go", "selecting-go", "starting-go", "committed", "rolling-back", "restored":
		return true
	}
	return false
}
func (c *Controller) Close() error { return c.lock.Close() }
func cloneState(s State) State {
	raw, _ := json.Marshal(s)
	var out State
	_ = json.Unmarshal(raw, &out)
	return out
}
func (c *Controller) State() State { c.mu.Lock(); defer c.mu.Unlock(); return cloneState(c.state) }
func (c *Controller) save(phase string) error {

	path := filepath.Join(c.root, "cutover.json")
	if _, err := os.Lstat(path); os.IsNotExist(err) {
		if c.journalSHA != "" {
			return errors.New("cutover journal disappeared")
		}
	} else {
		raw, err := privatefile.Read(path, 65536, true)
		if err != nil || c.journalSHA == "" || hashBytes(raw) != c.journalSHA {
			return errors.New("cutover journal changed outside this owner")
		}
	}
	next := c.state
	next.Phase = phase
	next.Revision++
	if next.Revision == 0 {
		return errors.New("cutover revision overflow")
	}
	raw, err := json.Marshal(next)
	if err != nil {
		return err
	}
	renamed, err := commit(c.root, append(raw, '\n'))
	if renamed {
		c.state = next
		c.journalSHA = hashBytes(append(raw, '\n'))
	}
	return err
}

// Prepare provides the rollback identity before stopping any service. Repeated
// preparation cannot silently adopt a different legacy process or code plan.
func (c *Controller) Prepare(ctx context.Context, planSHA string) (State, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !digestPattern.MatchString(planSHA) || planSHA != c.planSHA {
		return cloneState(c.state), errors.New("invalid immutable cutover plan digest")
	}
	if c.state.Phase != "empty" && c.state.Phase != "restored" {
		if c.state.Phase == "prepared" && c.state.PlanSHA == planSHA {
			return cloneState(c.state), nil
		}
		return cloneState(c.state), errors.New("cutover journal already initialized")
	}
	old, err := c.backend.Capture(ctx)
	if err != nil {
		return cloneState(c.state), err
	}
	if err := validateSnapshot(old); err != nil {
		return cloneState(c.state), err
	}
	c.state.PlanSHA, c.state.Legacy = planSHA, cloneState(State{Legacy: old}).Legacy
	if err := c.save("prepared"); err != nil {
		return cloneState(c.state), err
	}
	return cloneState(c.state), nil
}

func (c *Controller) Apply(ctx context.Context, revision uint64, planSHA string) (State, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state.Revision != revision || c.state.Phase != "prepared" || c.state.PlanSHA != planSHA {
		return cloneState(c.state), errors.New("cutover CAS or immutable plan conflict")
	}
	if err := c.save("stopping-legacy"); err != nil {
		return cloneState(c.state), err
	}
	steps := []struct {
		phase string
		name  string
		run   func() error
	}{
		{"", "stop-legacy", func() error { return c.backend.StopLegacy(ctx, c.state.Legacy) }},
		{"", "legacy-exit", func() error { return c.backend.ConfirmLegacyExit(ctx, c.state.Legacy) }},
		{"preparing-go", "prepare-go", func() error { return c.backend.PrepareGo(ctx, c.state.Legacy) }},
		{"selecting-go", "select-go", func() error { return c.backend.SelectGo(ctx) }},
		{"starting-go", "start-go", func() error { return c.backend.StartGo(ctx) }},
		{"", "go-ready", func() error { return c.backend.GoReady(ctx) }},
	}
	for _, step := range steps {
		if step.phase != "" {
			if err := c.save(step.phase); err != nil {
				return cloneState(c.state), err
			}
		}
		if err := step.run(); err != nil {
			// Recovery is independent of a disconnected/canceled apply request. Do
			// not impose a force-kill deadline on refresh-and-persist draining.
			if rollbackErr := c.rollback(context.WithoutCancel(ctx)); rollbackErr != nil {
				return cloneState(c.state), fmt.Errorf("cutover %s unconfirmed (%v); recovery unconfirmed (%w); do not restart another writer", step.name, err, rollbackErr)
			}
			return cloneState(c.state), fmt.Errorf("cutover %s failed (%w); legacy program restored using current business data", step.name, err)
		}
	}
	if err := c.save("committed"); err != nil {
		return cloneState(c.state), err
	}
	return cloneState(c.state), nil
}

// Rollback also recovers a partially applied durable journal. It never starts
// the old writer until every Go writer is confirmed exited.
func (c *Controller) Rollback(ctx context.Context, revision uint64) (State, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state.Revision != revision || c.state.Phase == "empty" {
		return cloneState(c.state), errors.New("cutover rollback CAS conflict")
	}
	if c.state.Phase == "prepared" || c.state.Phase == "restored" {
		return cloneState(c.state), nil
	}
	if err := c.rollback(ctx); err != nil {
		return cloneState(c.state), err
	}
	return cloneState(c.state), nil
}
func (c *Controller) rollback(ctx context.Context) error {
	if err := c.save("rolling-back"); err != nil {
		return err
	}
	for _, step := range []func() error{
		func() error { return c.backend.StopGo(ctx) }, func() error { return c.backend.ConfirmGoExit(ctx) },
		func() error { return c.backend.SelectLegacy(ctx, c.state.Legacy) }, func() error { return c.backend.StartLegacy(ctx, c.state.Legacy) },
		func() error { return c.backend.LegacyReady(ctx, c.state.Legacy) },
	} {
		if err := step(); err != nil {
			return fmt.Errorf("legacy recovery unconfirmed; writer handoff remains fenced: %w", err)
		}
	}
	return c.save("restored")
}
