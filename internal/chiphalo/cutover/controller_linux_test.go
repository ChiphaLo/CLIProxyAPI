//go:build linux

package cutover

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

const testPlan = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

type fakeBackend struct {
	calls                []string
	fail                 string
	goAlive, legacyAlive bool
	business             string
}

func snapshotFixture() Snapshot {
	s := Snapshot{Units: map[string]bool{}, UnitDigests: map[string]string{}, ContainerRunning: true, ContainerRestart: "unless-stopped", Processes: []Process{}}
	for _, u := range legacyUnits {
		s.Units[u] = true
		s.UnitDigests[u] = testPlan
	}
	for i, role := range []string{"gateway", "gui", "helper", "oauth"} {
		s.Processes = append(s.Processes, Process{Role: role, PID: uint32(i + 10), Start: 100})
	}
	return s
}
func (f *fakeBackend) call(s string) error {
	f.calls = append(f.calls, s)
	if s == f.fail {
		return errors.New("injected")
	}
	return nil
}
func (f *fakeBackend) Capture(context.Context) (Snapshot, error) {
	return snapshotFixture(), f.call("capture")
}
func (f *fakeBackend) StopLegacy(context.Context, Snapshot) error {
	if err := f.call("stop-legacy"); err != nil {
		return err
	}
	f.legacyAlive = false
	return nil
}
func (f *fakeBackend) ConfirmLegacyExit(context.Context, Snapshot) error {
	if f.legacyAlive {
		return errors.New("legacy alive")
	}
	return f.call("legacy-exit")
}
func (f *fakeBackend) PrepareGo(context.Context, Snapshot) error {
	if os.Getenv("CPA_CUTOVER_CRASH_CHILD") == "1" {
		_, _ = fmt.Fprintln(os.Stdout, "READY")
		_ = syscall.Kill(os.Getpid(), syscall.SIGSTOP)
	}
	return f.call("prepare-go")
}
func (f *fakeBackend) SelectGo(context.Context) error { return f.call("select-go") }
func (f *fakeBackend) StartGo(context.Context) error  { f.goAlive = true; return f.call("start-go") }
func (f *fakeBackend) GoReady(context.Context) error  { return f.call("go-ready") }
func (f *fakeBackend) StopGo(context.Context) error {
	if err := f.call("stop-go"); err != nil {
		return err
	}
	f.goAlive = false
	return nil
}
func (f *fakeBackend) ConfirmGoExit(context.Context) error {
	if f.goAlive {
		return errors.New("Go alive")
	}
	return f.call("go-exit")
}
func (f *fakeBackend) SelectLegacy(context.Context, Snapshot) error { return f.call("select-legacy") }
func (f *fakeBackend) StartLegacy(context.Context, Snapshot) error {
	if f.goAlive {
		return errors.New("overlapping writers")
	}
	f.legacyAlive = true
	return f.call("start-legacy")
}
func (f *fakeBackend) LegacyReady(context.Context, Snapshot) error { return f.call("legacy-ready") }
func openFixture(t *testing.T, f *fakeBackend) (*Controller, string) {
	t.Helper()
	root := t.TempDir()
	_ = os.Chmod(root, 0700)
	c, err := Open(root, f, testPlan)
	if err != nil {
		t.Fatal(err)
	}
	return c, root
}
func TestCutoverCurrentBusinessRollback(t *testing.T) {
	f := &fakeBackend{legacyAlive: true, business: "before"}
	c, root := openFixture(t, f)
	s, err := c.Prepare(context.Background(), testPlan)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 1 || !f.legacyAlive {
		t.Fatal("prepare changed services")
	}
	s, err = c.Apply(context.Background(), s.Revision, testPlan)
	if err != nil || s.Phase != "committed" {
		t.Fatal(err)
	}
	f.business = "new provider, latest OAuth generation and latest receipts"
	_ = c.Close()
	c, err = Open(root, f, testPlan)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	s, err = c.Rollback(context.Background(), c.State().Revision)
	if err != nil || s.Phase != "restored" || f.business != "new provider, latest OAuth generation and latest receipts" || !f.legacyAlive || f.goAlive {
		t.Fatal("rollback did not retain current business")
	}
}
func TestCutoverEveryOperationFailureRecovers(t *testing.T) {
	for _, step := range []string{"stop-legacy", "legacy-exit", "prepare-go", "select-go", "start-go", "go-ready"} {
		t.Run(step, func(t *testing.T) {
			f := &fakeBackend{legacyAlive: true, fail: step}
			c, _ := openFixture(t, f)
			defer func() { _ = c.Close() }()
			s, err := c.Prepare(context.Background(), testPlan)
			if err != nil {
				t.Fatal(err)
			}
			s, err = c.Apply(context.Background(), s.Revision, testPlan)
			if err == nil || s.Phase != "restored" || !f.legacyAlive || f.goAlive {
				t.Fatal("failure recovery unconfirmed", s.Phase, err)
			}
		})
	}
}
func TestCutoverUnconfirmedDrainNeverStartsLegacy(t *testing.T) {
	f := &fakeBackend{legacyAlive: true}
	c, root := openFixture(t, f)
	s, _ := c.Prepare(context.Background(), testPlan)
	s, _ = c.Apply(context.Background(), s.Revision, testPlan)
	f.fail = "stop-go"
	s, err := c.Rollback(context.Background(), s.Revision)
	if err == nil || s.Phase != "rolling-back" || f.legacyAlive {
		t.Fatal("unconfirmed writer was overlapped")
	}
	_ = c.Close()
	f.fail = ""
	c, err = Open(root, f, testPlan)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	s, err = c.Rollback(context.Background(), c.State().Revision)
	if err != nil || s.Phase != "restored" {
		t.Fatal(err)
	}
}
func TestCutoverPartialJournalCrashRecovery(t *testing.T) {
	for _, phase := range []string{"stopping-legacy", "preparing-go", "selecting-go", "starting-go", "rolling-back"} {
		t.Run(phase, func(t *testing.T) {
			f := &fakeBackend{legacyAlive: true}
			c, root := openFixture(t, f)
			_, _ = c.Prepare(context.Background(), testPlan)
			if err := c.save(phase); err != nil {
				t.Fatal(err)
			}
			_ = c.Close()
			c, err := Open(root, f, testPlan)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = c.Close() }()
			s, err := c.Rollback(context.Background(), c.State().Revision)
			if err != nil || s.Phase != "restored" {
				t.Fatal(err)
			}
		})
	}
}
func TestCutoverCASOwnershipAndMalformedJournal(t *testing.T) {
	f := &fakeBackend{legacyAlive: true}
	c, root := openFixture(t, f)
	s, _ := c.Prepare(context.Background(), testPlan)
	s.Legacy.Units[legacyUnits[0]] = false
	s.Legacy.Processes[0].PID = 0
	if !c.State().Legacy.Units[legacyUnits[0]] || c.State().Legacy.Processes[0].PID == 0 {
		t.Fatal("state aliases internal authority")
	}
	if _, err := Open(root, f, testPlan); err == nil {
		t.Fatal("parallel owner accepted")
	}
	if _, err := c.Apply(context.Background(), 999, testPlan); err == nil {
		t.Fatal("stale CAS accepted")
	}
	if _, err := c.Apply(context.Background(), s.Revision, strings.Repeat("b", 64)); err == nil {
		t.Fatal("plan drift accepted")
	}
	_ = c.Close()
	if _, err := Open(root, f, strings.Repeat("b", 64)); err == nil {
		t.Fatal("journal rebound to another plan")
	}
	raw, _ := os.ReadFile(filepath.Join(root, "cutover.json"))
	var v map[string]any
	_ = json.Unmarshal(raw, &v)
	v["unknown"] = true
	raw, _ = json.Marshal(v)
	_ = os.WriteFile(filepath.Join(root, "cutover.json"), raw, 0600)
	if _, err := Open(root, f, testPlan); err == nil {
		t.Fatal("unknown journal authority accepted")
	}
}
func TestCutoverUnsafeFilesystemRejected(t *testing.T) {
	f := &fakeBackend{}
	root := t.TempDir()
	_ = os.Chmod(root, 0777)
	if _, err := Open(root, f, testPlan); err == nil {
		t.Fatal("shared root accepted")
	}
	_ = os.Chmod(root, 0700)
	target := filepath.Join(root, "target")
	_ = os.WriteFile(target, []byte("untouched"), 0600)
	_ = os.Symlink(target, filepath.Join(root, "cutover.json"))
	if _, err := Open(root, f, testPlan); err == nil {
		t.Fatal("journal symlink accepted")
	}
	raw, _ := os.ReadFile(target)
	if string(raw) != "untouched" {
		t.Fatal("unsafe target changed")
	}
}

func TestCutoverCrashChildHelper(t *testing.T) {
	if os.Getenv("CPA_CUTOVER_CRASH_CHILD") != "1" {
		return
	}
	root := os.Getenv("CPA_CUTOVER_CRASH_ROOT")
	f := &fakeBackend{legacyAlive: true}
	c, err := Open(root, f, testPlan)
	if err != nil {
		t.Fatal(err)
	}
	state, err := c.Prepare(context.Background(), testPlan)
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Apply(context.Background(), state.Revision, testPlan)
	if err != nil {
		t.Fatal(err)
	}
}
func TestCutoverActualKilledControllerRecoversDurableJournal(t *testing.T) {
	root := t.TempDir()
	_ = os.Chmod(root, 0700)
	child := exec.Command(os.Args[0], "-test.run=^TestCutoverCrashChildHelper$")
	child.Env = append(os.Environ(), "CPA_CUTOVER_CRASH_CHILD=1", "CPA_CUTOVER_CRASH_ROOT="+root)
	stdout, err := child.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = child.Process.Kill(); _ = child.Wait() }()
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || line != "READY\n" {
		t.Fatal("child did not reach durable handoff point", err)
	}
	if err := child.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = child.Wait()
	f := &fakeBackend{}
	c, err := Open(root, f, testPlan)
	if err != nil {
		t.Fatal("dead controller retained ownership", err)
	}
	defer func() { _ = c.Close() }()
	if c.State().Phase != "preparing-go" {
		t.Fatal("crash lost durable phase")
	}
	state, err := c.Rollback(context.Background(), c.State().Revision)
	if err != nil || state.Phase != "restored" || !f.legacyAlive {
		t.Fatal("partial crash not recoverable", err)
	}
}
func TestCutoverJournalExternalEditDoesNotGetOverwritten(t *testing.T) {
	f := &fakeBackend{legacyAlive: true}
	c, root := openFixture(t, f)
	defer func() { _ = c.Close() }()
	s, err := c.Prepare(context.Background(), testPlan)
	if err != nil {
		t.Fatal(err)
	}
	raw := []byte("external journal edit\n")
	_ = os.WriteFile(filepath.Join(root, "cutover.json"), raw, 0600)
	if _, err := c.Apply(context.Background(), s.Revision, testPlan); err == nil {
		t.Fatal("external journal mutation ignored")
	}
	after, _ := os.ReadFile(filepath.Join(root, "cutover.json"))
	if string(after) != string(raw) || len(f.calls) != 1 {
		t.Fatal("external metadata overwritten or service changed")
	}
}
