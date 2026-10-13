//go:build linux

package adminhelper

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/configbundle"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

type applyBackend struct {
	configDir, oldDigest          string
	failNew, failRollback, sawNew bool
	hook                          func()
}

func (b *applyBackend) Catalog(context.Context) ([]string, error) {
	return []string{"codex2/gpt-6-astra", "provider/old"}, nil
}
func (b *applyBackend) Await(_ context.Context, digest string, models []string) error {
	raw, err := os.ReadFile(filepath.Join(b.configDir, configFiles[0]))
	if err != nil || hash(raw) != digest {
		return errors.New("fixture config digest mismatch")
	}
	cfg, err := config.ParseConfigBytes(raw)
	if err != nil {
		return err
	}
	expected := []string{"codex2/gpt-6-astra"}
	for _, p := range cfg.OpenAICompatibility {
		for _, m := range p.Models {
			expected = append(expected, p.Prefix+"/"+m.Alias)
		}
	}
	slices.Sort(expected)
	if !slices.Equal(expected, models) {
		return errors.New("fixture exact catalog mismatch")
	}
	if digest != b.oldDigest {
		b.sawNew = true
		if b.hook != nil {
			b.hook()
		}
		if b.failNew {
			return errors.New("synthetic new catalog fault")
		}
	}
	if digest == b.oldDigest && b.sawNew && b.failRollback {
		return errors.New("synthetic rollback catalog fault")
	}
	return nil
}

var applyState = json.RawMessage(`{"providers":[{"id":"provider","baseUrl":"https://provider.example/v1","models":[{"clientName":"new","upstreamName":"new-upstream"}]}],"managedPrefixes":["provider"]}`)

func applyFixture(t *testing.T) (*Service, ApplyOptions, map[string][]byte) {
	t.Helper()
	s := fixture(t)
	dir := filepath.Dir(s.paths.Keys)
	cfg := filepath.Join(dir, "config")
	tx := filepath.Join(dir, "transactions")
	for _, path := range []string{cfg, tx} {
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	old := map[string][]byte{
		configFiles[0]: []byte("api-keys: [fixture-inner]\nopenai-compatibility:\n  - name: cpa-gui-provider-chat\n    prefix: provider\n    base-url: https://provider.example/v1\n    api-key-entries: [{api-key: old-fixture}]\n    models: [{name: old-upstream, alias: old}]\n"),
		configFiles[1]: []byte("chat_models: [{id: provider/old}]\nresponses_models: [{id: provider/old}]\n"),
		configFiles[2]: []byte(`{"models":{"provider/old":{},"codex2/gpt-6-astra":{"preserve":true}}}`),
	}
	for name, raw := range old {
		if err := os.WriteFile(filepath.Join(cfg, name), raw, 0640); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(cfg, "oauth-accounts.json"), []byte(`{"accounts":[{"prefix":"codex"},{"prefix":"codex2"}]}`), 0640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.paths.Vault, []byte(`{"provider":"new-fixture","other":"preserve"}`), 0600); err != nil {
		t.Fatal(err)
	}
	backend := &applyBackend{configDir: cfg, oldDigest: hash(old[configFiles[0]])}
	options := ApplyOptions{ConfigDir: cfg, Transactions: tx, Backend: backend, Resolve: func(context.Context, string) ([]netip.Addr, error) { return nil, errors.New("offline") }}
	if err := s.EnableApply(context.Background(), options); err != nil {
		t.Fatal(err)
	}
	return s, options, old
}
func runApply(s *Service, state json.RawMessage) Result {
	return s.dispatch("apply_bundle", map[string]json.RawMessage{"state": state})
}
func restored(t *testing.T, options ApplyOptions, old map[string][]byte) {
	t.Helper()
	for name, want := range old {
		raw, _ := os.ReadFile(filepath.Join(options.ConfigDir, name))
		if string(raw) != string(want) {
			t.Fatalf("configuration not exactly restored: %s", name)
		}
	}
}
func pending(t *testing.T, options ApplyOptions, want bool) {
	t.Helper()
	_, err := os.Lstat(filepath.Join(options.ConfigDir, configbundle.FenceName))
	if (!os.IsNotExist(err)) != want {
		t.Fatal("unexpected apply fence state")
	}
}

func TestApplyCommitsOnlyConfigurationAndPreservesMetadata(t *testing.T) {
	s, options, old := applyFixture(t)
	keys, _ := os.ReadFile(s.paths.Keys)
	vault, _ := os.ReadFile(s.paths.Vault)
	result := runApply(s, applyState)
	requireOK(t, result)
	pending(t, options, false)
	if result["providers"] != 1 || result["models"] != 1 {
		t.Fatal("unexpected committed bundle counts")
	}
	for name, raw := range old {
		now, _ := os.ReadFile(filepath.Join(options.ConfigDir, name))
		if string(now) == string(raw) {
			t.Fatal("configuration was not published")
		}
		info, _ := os.Stat(filepath.Join(options.ConfigDir, name))
		if info.Mode().Perm() != 0640 {
			t.Fatal("configuration permissions changed")
		}
	}
	nowKeys, _ := os.ReadFile(s.paths.Keys)
	nowVault, _ := os.ReadFile(s.paths.Vault)
	if string(keys) != string(nowKeys) || string(vault) != string(nowVault) {
		t.Fatal("key or vault database changed during apply")
	}
	jraw, _ := os.ReadFile(filepath.Join(options.Transactions, result["txid"].(string), "journal.json"))
	var j journal
	_ = json.Unmarshal(jraw, &j)
	if j.Phase != "committed" {
		t.Fatal("durable commit journal missing")
	}
}

func TestApplyStaticFailureNeverPublishes(t *testing.T) {
	s, options, old := applyFixture(t)
	state := json.RawMessage(strings.ReplaceAll(string(applyState), `"clientName":"new"`, `"clientName":"bad/name"`))
	if runApply(s, state)["ok"] != false {
		t.Fatal("invalid draft accepted")
	}
	restored(t, options, old)
	pending(t, options, false)
	entries, _ := os.ReadDir(options.Transactions)
	if len(entries) != 0 {
		t.Fatal("invalid draft created a transaction")
	}
}

func TestApplyCatalogFaultRollsBackExactFiles(t *testing.T) {
	s, options, old := applyFixture(t)
	options.Backend.(*applyBackend).failNew = true
	result := runApply(s, applyState)
	if result["ok"] != false || result["rollback"] != true {
		t.Fatal("failed new catalog was not rolled back")
	}
	restored(t, options, old)
	pending(t, options, false)
}

func TestRollbackFaultRetainsFenceAndRestartRecovers(t *testing.T) {
	s, options, old := applyFixture(t)
	backend := options.Backend.(*applyBackend)
	backend.failNew = true
	backend.failRollback = true
	result := runApply(s, applyState)
	if result["rollback_failed"] != true {
		t.Fatal("rollback health fault hidden")
	}
	pending(t, options, true)
	if runApply(s, applyState)["ok"] != false {
		t.Fatal("accepted another transaction before recovery")
	}
	backend.failRollback = false
	fresh, err := New(s.paths)
	if err != nil {
		t.Fatal(err)
	}
	if err = fresh.EnableApply(context.Background(), options); err != nil {
		t.Fatal(err)
	}
	restored(t, options, old)
	pending(t, options, false)
}

func TestExternalEditsAreNeverOverwrittenByRecovery(t *testing.T) {
	s, options, _ := applyFixture(t)
	backend := options.Backend.(*applyBackend)
	backend.failNew = true
	path := filepath.Join(options.ConfigDir, configFiles[2])
	backend.hook = func() {
		if err := os.WriteFile(path, []byte(`{"external":"keep"}`), 0640); err != nil {
			t.Fatal(err)
		}
	}
	result := runApply(s, applyState)
	if result["rollback_failed"] != true {
		t.Fatal("external edit did not fence recovery")
	}
	pending(t, options, true)
	fresh, _ := New(s.paths)
	if fresh.EnableApply(context.Background(), options) == nil {
		t.Fatal("restart overwrote an external edit")
	}
	raw, _ := os.ReadFile(path)
	if string(raw) != `{"external":"keep"}` {
		t.Fatal("external edit lost")
	}
}

func TestCommittedRecoveryDoesNotRestoreOldSnapshot(t *testing.T) {
	s, options, _ := applyFixture(t)
	result := runApply(s, applyState)
	requireOK(t, result)
	before := map[string]string{}
	for _, name := range configFiles {
		raw, _ := os.ReadFile(filepath.Join(options.ConfigDir, name))
		before[name] = hash(raw)
	}
	marker, _ := json.Marshal(map[string]any{"version": 1, "transaction_id": result["txid"]})
	if err := writePrivate(filepath.Join(options.ConfigDir, configbundle.FenceName), marker); err != nil {
		t.Fatal(err)
	}
	fresh, _ := New(s.paths)
	if err := fresh.EnableApply(context.Background(), options); err != nil {
		t.Fatal(err)
	}
	pending(t, options, false)
	for name, want := range before {
		raw, _ := os.ReadFile(filepath.Join(options.ConfigDir, name))
		if hash(raw) != want {
			t.Fatal("committed recovery restored stale configuration")
		}
	}
}

func TestQueuedMutationCancelsBeforeWriting(t *testing.T) {
	s := fixture(t)
	before, _ := os.ReadFile(s.paths.Keys)
	s.gate <- struct{}{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result := s.dispatchContext(ctx, "create_api_key", nil)
	<-s.gate
	if result["ok"] != false {
		t.Fatal("canceled queued mutation succeeded")
	}
	after, _ := os.ReadFile(s.paths.Keys)
	if string(after) != string(before) {
		t.Fatal("canceled request wrote a key")
	}
}

func TestApplyCrashWorker(t *testing.T) {
	dir := os.Getenv("CPA_APPLY_CRASH_WORKER")
	if dir == "" {
		return
	}
	s, err := New(Paths{Keys: filepath.Join(dir, "keys.json"), Vault: filepath.Join(dir, "vault.json"), Lock: filepath.Join(dir, "apply.lock"), Audit: filepath.Join(dir, "audit.jsonl")})
	if err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(dir, "config")
	raw, _ := os.ReadFile(filepath.Join(cfg, configFiles[0]))
	backend := &applyBackend{configDir: cfg, oldDigest: hash(raw), hook: func() { fmt.Println("published-awaiting-commit"); select {} }}
	options := ApplyOptions{ConfigDir: cfg, Transactions: filepath.Join(dir, "transactions"), Backend: backend, Resolve: func(context.Context, string) ([]netip.Addr, error) { return nil, errors.New("offline") }}
	if err := s.EnableApply(context.Background(), options); err != nil {
		t.Fatal(err)
	}
	_ = runApply(s, applyState)
	t.Fatal("worker unexpectedly returned")
}

func TestActualSIGKILLBeforeCommitRestoresOnRestart(t *testing.T) {
	s, options, old := applyFixture(t)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, "-test.run=^TestApplyCrashWorker$", "-test.timeout=30s")
	cmd.Env = append(os.Environ(), "CPA_APPLY_CRASH_WORKER="+filepath.Dir(s.paths.Keys))
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	scanner := bufio.NewScanner(pipe)
	ready := false
	for scanner.Scan() {
		if scanner.Text() == "published-awaiting-commit" {
			ready = true
			break
		}
	}
	if !ready {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatal("crash worker failed to reach publication")
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	pending(t, options, true)
	fresh, _ := New(s.paths)
	if err := fresh.EnableApply(context.Background(), options); err != nil {
		t.Fatal(err)
	}
	restored(t, options, old)
	pending(t, options, false)
}

func TestPartialThreeFilePublicationRecoversFromPreparedJournal(t *testing.T) {
	s, options, old := applyFixture(t)
	accounts, _ := os.ReadFile(filepath.Join(options.ConfigDir, "oauth-accounts.json"))
	vault, _ := os.ReadFile(s.paths.Vault)
	out, err := configbundle.Build(context.Background(), configbundle.Input{Config: old[configFiles[0]], Generated: old[configFiles[1]], Context: old[configFiles[2]], Accounts: accounts, Vault: vault, State: applyState, Resolve: options.Resolve, Now: s.now()})
	if err != nil {
		t.Fatal(err)
	}
	j := &journal{Version: 1, ID: "tx-" + strings.Repeat("1", 32), Phase: "prepared", OldHashes: map[string]string{}, NewHashes: map[string]string{}, OldModels: []string{"codex2/gpt-6-astra", "provider/old"}, NewModels: []string{"codex2/gpt-6-astra", "provider/new"}}
	dir := filepath.Join(options.Transactions, j.ID)
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	next := map[string][]byte{configFiles[0]: out.Config, configFiles[1]: out.Generated, configFiles[2]: out.Context}
	for _, name := range configFiles {
		j.OldHashes[name] = hash(old[name])
		j.NewHashes[name] = hash(next[name])
		if writePrivate(filepath.Join(dir, name+".old"), old[name]) != nil || writePrivate(filepath.Join(dir, name+".new"), next[name]) != nil {
			t.Fatal("cannot stage partial transaction")
		}
	}
	if s.apply.save(j) != nil {
		t.Fatal("cannot stage journal")
	}
	marker, _ := json.Marshal(map[string]any{"version": 1, "transaction_id": j.ID})
	if writePrivate(s.apply.fence(), marker) != nil {
		t.Fatal("cannot stage fence")
	}
	if err := replaceConfig(filepath.Join(options.ConfigDir, configFiles[0]), out.Config); err != nil {
		t.Fatal(err)
	}
	fresh, _ := New(s.paths)
	if err := fresh.EnableApply(context.Background(), options); err != nil {
		t.Fatal(err)
	}
	restored(t, options, old)
	pending(t, options, false)
}

func TestCorruptPrivateBackupFencesRecoveryWithoutRestoringIt(t *testing.T) {
	s, options, old := applyFixture(t)
	backend := options.Backend.(*applyBackend)
	backend.failNew = true
	backend.failRollback = true
	result := runApply(s, applyState)
	if result["rollback_failed"] != true {
		t.Fatal("fault setup failed")
	}
	path := filepath.Join(options.Transactions, result["txid"].(string), configFiles[0]+".old")
	if os.WriteFile(path, []byte("corrupted"), 0600) != nil {
		t.Fatal("backup corruption setup failed")
	}
	fresh, _ := New(s.paths)
	if fresh.EnableApply(context.Background(), options) == nil {
		t.Fatal("corrupt backup accepted")
	}
	restored(t, options, old)
	pending(t, options, true)
}
