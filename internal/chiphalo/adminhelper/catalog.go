package adminhelper

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/catalogsync"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/configbundle"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/privatefile"
)

type catalogReviewer interface {
	ReviewCatalog(context.Context) (catalogsync.Publication, error)
}
type catalogJob struct {
	id      string
	pending bool
	result  Result
	cancel  context.CancelFunc
}

func (s *Service) syncCatalog(payload map[string]json.RawMessage) Result {
	if allowed(payload, "job_id") != nil {
		return failure("unknown catalog sync fields")
	}
	jobID := ""
	if raw, ok := payload["job_id"]; ok {
		if json.Unmarshal(raw, &jobID) != nil || !requestIdentity.MatchString(jobID) {
			return failure("invalid catalog job identity")
		}
	}
	s.catalogMu.Lock()
	defer s.catalogMu.Unlock()
	if jobID != "" {
		if s.catalogJob == nil || s.catalogJob.id != jobID {
			return failure("catalog job unavailable; inspect published catalog before retrying")
		}
		if s.catalogJob.pending {
			return Result{"ok": true, "pending": true, "job_id": jobID, "output": "Catalog review in progress"}
		}
		return s.catalogJob.result
	}
	if s.catalogJob != nil && s.catalogJob.pending {
		return Result{"ok": true, "pending": true, "job_id": s.catalogJob.id, "output": "Existing catalog review in progress"}
	}
	if s.apply == nil {
		return failure("catalog publisher unavailable")
	}
	reviewer, ok := s.apply.options.Backend.(catalogReviewer)
	if !ok {
		return failure("private catalog reviewer unavailable")
	}
	token, err := randomHex(16)
	if err != nil {
		return failure("catalog job identity unavailable")
	}
	ctx, cancel := context.WithCancel(context.Background())
	job := &catalogJob{id: "catalog-" + token, pending: true, cancel: cancel}
	s.catalogJob = job
	s.catalogWorkers.Add(1)
	go func() {
		defer s.catalogWorkers.Done()
		defer cancel()
		result := failure("catalog proposal unavailable; published files were not changed")
		if s.audit(job.id, "sync_codex", "job_started", false, 0) == nil {
			proposal, err := reviewer.ReviewCatalog(ctx)
			if err == nil {
				result = s.publishCatalogJob(ctx, proposal)
			}
		}
		if s.audit(job.id, "sync_codex", "job_completed", result["ok"] == true, 0) != nil {
			result = failure("catalog job audit unavailable; inspect published state")
		}
		result["pending"], result["job_id"] = false, job.id
		if result["output"] == nil {
			result["output"] = result["error"]
		}
		s.catalogMu.Lock()
		job.pending = false
		job.result = result
		s.catalogMu.Unlock()
	}()
	return Result{"ok": true, "pending": true, "job_id": job.id, "output": "Catalog review started"}
}
func (s *Service) stopCatalogJobs() {
	s.catalogMu.Lock()
	if s.catalogJob != nil {
		s.catalogJob.cancel()
	}
	s.catalogMu.Unlock()
	s.catalogWorkers.Wait()
}
func (s *Service) publishCatalogJob(parent context.Context, proposal catalogsync.Publication) Result {
	// This deadline bounds only local publication/registration, not the upstream
	// discovery or model probes, which run independently of GUI reply deadlines.
	ctx, cancel := context.WithTimeout(parent, 70*time.Second)
	defer cancel()
	select {
	case s.gate <- struct{}{}:
		defer func() { <-s.gate }()
	case <-ctx.Done():
		return failure("catalog publication canceled")
	}
	f, err := openPrivate(s.paths.Lock)
	if err != nil {
		return failure("catalog lock unavailable")
	}
	defer func() { _ = f.Close() }()
	if lockContext(ctx, f) != nil {
		return failure("catalog lock unavailable")
	}
	defer unlock(f)
	return s.apply.PublishCatalog(ctx, proposal)
}
func (p *Applier) PublishCatalog(ctx context.Context, proposal catalogsync.Publication) Result {
	if ctx.Err() != nil {
		return failure("catalog publication canceled")
	}
	if _, err := os.Lstat(p.fence()); !os.IsNotExist(err) {
		return failure("configuration transaction requires recovery")
	}
	for _, digest := range []string{proposal.Config, proposal.Accounts, proposal.OldExposed, proposal.OldContext} {
		if !validDigest(digest) {
			return failure("catalog proposal lacks fixed input hashes")
		}
	}
	old := map[string][]byte{}
	for name, want := range map[string]string{configFiles[0]: proposal.Config, "oauth-accounts.json": proposal.Accounts, catalogFiles[0]: proposal.OldContext, catalogFiles[1]: proposal.OldExposed} {
		raw, err := privatefile.Read(filepath.Join(p.options.ConfigDir, name), 4*1024*1024, name == configFiles[0])
		if err != nil || hash(raw) != want {
			return failure("catalog proposal became stale; published files were not changed")
		}
		old[name] = raw
	}
	var exposed map[string]any
	var metadata map[string]any
	if len(proposal.Exposed) > 1024*1024 || configbundle.DecodeDraft(proposal.Exposed, &exposed) != nil || configbundle.DecodeDraft(proposal.Context, &metadata) != nil || exposed["account_models"] == nil || metadata["models"] == nil {
		return failure("reviewed catalog documents invalid")
	}
	actual, err := p.options.Backend.Catalog(ctx)
	if err != nil || !slices.Equal(actual, proposal.OldModels) {
		return failure("catalog proposal no longer matches actual registrations")
	}
	if err = p.options.Backend.Await(ctx, proposal.Config, actual); err != nil {
		return failure("published SDK catalog not ready")
	}
	if !proposal.Changed {
		return Result{"ok": true, "output": "Catalog already reviewed; no files changed"}
	}
	next := map[string][]byte{catalogFiles[0]: proposal.Context, catalogFiles[1]: proposal.Exposed}
	extra := Result{"added_count": len(proposal.Added), "removed_count": len(proposal.Removed), "rejected_count": len(proposal.Rejected), "more_pending": proposal.MorePending, "output": fmt.Sprintf("Added %d, removed %d, review rejected %d", len(proposal.Added), len(proposal.Removed), len(proposal.Rejected))}
	return p.publish(ctx, old, next, actual, proposal.NewModels, catalogFiles, proposal.Config, proposal.Accounts, extra)
}

func (b *HTTPBackend) catalogPost(ctx context.Context, path string, target any, limit int64) error {
	if !b.private || path != "/catalog-review" && path != "/reload-reviewed-models" {
		return errors.New("private catalog action unavailable")
	}
	r, err := http.NewRequestWithContext(ctx, "POST", b.origin+path, nil)
	if err != nil {
		return errors.New("private catalog request unavailable")
	}
	resp, err := b.client.Do(r)
	if err != nil {
		return errors.New("private catalog action unavailable")
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil || int64(len(raw)) > limit || resp.StatusCode != 200 || json.Unmarshal(raw, target) != nil {
		return errors.New("private catalog result invalid")
	}
	return nil
}
func (b *HTTPBackend) ReviewCatalog(ctx context.Context) (catalogsync.Publication, error) {
	var p catalogsync.Publication
	err := b.catalogPost(ctx, "/catalog-review", &p, 8*1024*1024)
	return p, err
}
func (b *HTTPBackend) ReloadReviewed(ctx context.Context) error {
	var value Result
	if err := b.catalogPost(ctx, "/reload-reviewed-models", &value, 4096); err != nil {
		return err
	}
	if value["ok"] != true {
		return errors.New("reviewed SDK reload unavailable")
	}
	return nil
}
