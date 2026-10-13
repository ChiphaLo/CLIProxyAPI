//go:build linux

package adminhelper

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/catalogsync"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/configbundle"
)

type catalogBackend struct {
	dir              string
	proposal         catalogsync.Publication
	failNew, failOld bool
	newSeen          bool
	reloads          int
	started, release chan struct{}
}

func (b *catalogBackend) Catalog(context.Context) ([]string, error) {
	raw, err := os.ReadFile(filepath.Join(b.dir, catalogFiles[1]))
	if err != nil {
		return nil, err
	}
	if strings.Contains(string(raw), "gpt-new-fixture") {
		return b.proposal.NewModels, nil
	}
	return b.proposal.OldModels, nil
}
func (b *catalogBackend) Await(ctx context.Context, digest string, ids []string) error {
	raw, err := os.ReadFile(filepath.Join(b.dir, configFiles[0]))
	if err != nil || hash(raw) != digest {
		return errors.New("config changed")
	}
	actual, err := b.Catalog(ctx)
	if err != nil || !slices.Equal(actual, ids) {
		return errors.New("catalog mismatch")
	}
	return nil
}
func (b *catalogBackend) ReloadReviewed(ctx context.Context) error {
	b.reloads++
	ids, err := b.Catalog(ctx)
	if err != nil {
		return err
	}
	if slices.Equal(ids, b.proposal.NewModels) {
		b.newSeen = true
		if b.failNew {
			return errors.New("new registration fault")
		}
	} else if b.newSeen && b.failOld {
		return errors.New("old registration fault")
	}
	return nil
}
func (b *catalogBackend) ReviewCatalog(ctx context.Context) (catalogsync.Publication, error) {
	if b.started != nil {
		close(b.started)
		select {
		case <-b.release:
		case <-ctx.Done():
			return catalogsync.Publication{}, ctx.Err()
		}
	}
	return b.proposal, nil
}
func catalogFixture(t *testing.T) (*Service, ApplyOptions, *catalogBackend, map[string][]byte) {
	s, opts, old := applyFixture(t)
	exposed := []byte(`{"models":["gpt-6-astra"],"extension":"keep"}`)
	if err := os.WriteFile(filepath.Join(opts.ConfigDir, catalogFiles[1]), exposed, 0640); err != nil {
		t.Fatal(err)
	}
	accounts, _ := os.ReadFile(filepath.Join(opts.ConfigDir, "oauth-accounts.json"))
	proposal := catalogsync.Publication{Config: hash(old[configFiles[0]]), Accounts: hash(accounts), OldContext: hash(old[configFiles[2]]), OldExposed: hash(exposed), Context: []byte(`{"models":{"provider/old":{},"codex2/gpt-6-astra":{"preserve":true},"gpt-new-fixture":{"source_account_id":"one","source_kind":"live OAuth probe","verified_at":"2026-10-14T00:00:00Z","context_window":272000,"supported_reasoning_levels":[{"effort":"medium"}]}}}`), Exposed: []byte(`{"models":["gpt-6-astra","gpt-new-fixture"],"account_models":{"one":["gpt-new-fixture"],"two":["gpt-6-astra"]},"extension":"keep"}`), OldModels: []string{"codex2/gpt-6-astra", "provider/old"}, NewModels: []string{"codex2/gpt-6-astra", "gpt-new-fixture", "provider/old"}, Added: []string{"gpt-new-fixture"}, Changed: true}
	backend := &catalogBackend{dir: opts.ConfigDir, proposal: proposal}
	opts.Backend = backend
	if err := s.EnableApply(t.Context(), opts); err != nil {
		t.Fatal(err)
	}
	old[catalogFiles[1]] = exposed
	return s, opts, backend, old
}
func TestCatalogTransactionPublishesOnlyTwoFilesAndUsesSDKReload(t *testing.T) {
	s, opts, b, old := catalogFixture(t)
	result := s.publishCatalogJob(t.Context(), b.proposal)
	requireOK(t, result)
	if b.reloads != 1 {
		t.Fatal("publication did not explicitly reload SDK")
	}
	for _, name := range configFiles[:2] {
		raw, _ := os.ReadFile(filepath.Join(opts.ConfigDir, name))
		if string(raw) != string(old[name]) {
			t.Fatal("catalog sync altered API provider configuration")
		}
	}
	raw, _ := os.ReadFile(filepath.Join(opts.Transactions, result["txid"].(string), "journal.json"))
	var j journal
	_ = json.Unmarshal(raw, &j)
	if !slices.Equal(j.Files, catalogFiles) || j.Phase != "committed" {
		t.Fatal("catalog journal not durable and isolated")
	}
	if _, err := os.Stat(filepath.Join(opts.ConfigDir, configbundle.FenceName)); !os.IsNotExist(err) {
		t.Fatal("confirmed catalog remained fenced")
	}
}
func TestCatalogFaultRestoresFilesAndRestartReloadsBeforeUnfencing(t *testing.T) {
	s, opts, b, old := catalogFixture(t)
	b.failNew, b.failOld = true, true
	result := s.publishCatalogJob(t.Context(), b.proposal)
	if result["rollback_failed"] != true {
		t.Fatal("unconfirmed old SDK catalog unfenced", result)
	}
	for name, want := range old {
		raw, _ := os.ReadFile(filepath.Join(opts.ConfigDir, name))
		if string(raw) != string(want) {
			t.Fatal("catalog rollback failed exact byte recovery")
		}
	}
	b.failNew, b.failOld = false, false
	if err := s.EnableApply(t.Context(), opts); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(opts.ConfigDir, configbundle.FenceName)); !os.IsNotExist(err) {
		t.Fatal("verified restart recovery remained fenced")
	}
}
func TestCatalogProposalCASRejectsConcurrentConfigurationEdits(t *testing.T) {
	for _, file := range []string{configFiles[0], "oauth-accounts.json", catalogFiles[0], catalogFiles[1]} {
		t.Run(file, func(t *testing.T) {
			s, opts, b, _ := catalogFixture(t)
			path := filepath.Join(opts.ConfigDir, file)
			current, _ := os.ReadFile(path)
			current = append(current, ' ')
			if err := os.WriteFile(path, current, 0640); err != nil {
				t.Fatal(err)
			}
			result := s.publishCatalogJob(t.Context(), b.proposal)
			if result["ok"] == true || b.reloads != 0 {
				t.Fatal("stale proposal was published")
			}
			raw, _ := os.ReadFile(path)
			if string(raw) != string(current) {
				t.Fatal("stale review overwrote current data")
			}
		})
	}
}
func TestCatalogBackgroundJobIsSingleWorkerAndDoesNotBlockKeyReads(t *testing.T) {
	s, _, b, _ := catalogFixture(t)
	b.started, b.release = make(chan struct{}), make(chan struct{})
	first := s.dispatch("sync_codex", nil)
	requireOK(t, first)
	<-b.started
	second := s.dispatch("sync_codex", nil)
	if second["job_id"] != first["job_id"] {
		t.Fatal("duplicate upstream review worker started")
	}
	read := s.dispatch("read_api_keys", nil)
	requireOK(t, read)
	close(b.release)
	s.catalogWorkers.Wait()
	result := s.dispatch("sync_codex", map[string]json.RawMessage{"job_id": set(first["job_id"])})
	requireOK(t, result)
	if result["pending"] != false || b.reloads != 1 {
		t.Fatal("completed background review status incorrect")
	}
	s.stopCatalogJobs()
}
