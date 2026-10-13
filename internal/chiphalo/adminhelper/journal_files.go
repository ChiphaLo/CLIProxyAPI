package adminhelper

import (
	"context"
	"errors"
	"path/filepath"
	"slices"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/privatefile"
)

var catalogFiles = []string{"context-windows.json", "exposed-codex-models.json"}

func journalFiles(j *journal) []string {
	if len(j.Files) == 0 {
		return configFiles
	}
	return j.Files
}
func validDigest(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
func validJournalFiles(j *journal) bool {
	if len(j.Files) == 0 || slices.Equal(j.Files, configFiles) {
		return j.ConfigDigest == "" && j.AccountDigest == ""
	}
	return slices.Equal(j.Files, catalogFiles) && validDigest(j.ConfigDigest) && validDigest(j.AccountDigest)
}

type reviewedReloader interface{ ReloadReviewed(context.Context) error }

func (p *Applier) awaitJournal(ctx context.Context, j *journal, old bool) error {
	digest, models := j.NewHashes[configFiles[0]], j.NewModels
	if old {
		digest, models = j.OldHashes[configFiles[0]], j.OldModels
	}
	if slices.Equal(j.Files, catalogFiles) {
		for name, want := range map[string]string{configFiles[0]: j.ConfigDigest, "oauth-accounts.json": j.AccountDigest} {
			raw, err := privatefile.Read(filepath.Join(p.options.ConfigDir, name), 4*1024*1024, name == configFiles[0])
			if err != nil || hash(raw) != want {
				return errors.New("catalog transaction fixed inputs changed")
			}
		}
		reloader, ok := p.options.Backend.(reviewedReloader)
		if !ok {
			return errors.New("reviewed SDK registration control unavailable")
		}
		if err := reloader.ReloadReviewed(ctx); err != nil {
			return err
		}
		digest = j.ConfigDigest
	}
	return p.options.Backend.Await(ctx, digest, models)
}
