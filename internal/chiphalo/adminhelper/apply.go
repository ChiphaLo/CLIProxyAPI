package adminhelper

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/configbundle"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/privatefile"
)

var configFiles = []string{"cliproxyapi.yaml", "generated-models.yaml", "context-windows.json"}

// Backend is the local candidate control plane, never an external inference API.
type Backend interface {
	Catalog(context.Context) ([]string, error)
	Await(context.Context, string, []string) error
}
type ApplyOptions struct {
	ConfigDir, Transactions string
	Backend                 Backend
	Resolve                 configbundle.Resolver
}
type Applier struct {
	service *Service
	options ApplyOptions
}
type journal struct {
	Version   int               `json:"version"`
	ID        string            `json:"id"`
	Phase     string            `json:"phase"`
	OldHashes map[string]string `json:"old_hashes"`
	NewHashes map[string]string `json:"new_hashes"`
	OldModels []string          `json:"old_models"`
	NewModels []string          `json:"new_models"`
}

func hash(raw []byte) string { sum := sha256.Sum256(raw); return hex.EncodeToString(sum[:]) }
func syncDirectory(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	return f.Sync()
}
func writePrivate(path string, raw []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".cpa-tx-*")
	if err != nil {
		return errors.New("cannot stage transaction state")
	}
	defer func() { _ = f.Close(); _ = os.Remove(f.Name()) }()
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(raw)
	}
	if err == nil {
		err = f.Sync()
	}
	if err == nil {
		err = f.Close()
	}
	if err != nil {
		return errors.New("cannot persist transaction state")
	}
	if os.Rename(f.Name(), path) != nil || syncDirectory(filepath.Dir(path)) != nil {
		return errors.New("transaction state durability is unconfirmed")
	}
	return nil
}
func replaceConfig(path string, raw []byte) error {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return errors.New("published config path is unsafe")
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".cpa-publish-*")
	if err != nil {
		return errors.New("cannot stage configuration")
	}
	defer func() { _ = f.Close(); _ = os.Remove(f.Name()) }()
	if err = f.Chmod(info.Mode().Perm()); err == nil {
		err = preserveOwner(f, info)
	}
	if err == nil {
		_, err = f.Write(raw)
	}
	if err == nil {
		err = f.Sync()
	}
	if err == nil {
		err = f.Close()
	}
	if err != nil {
		return errors.New("cannot stage configuration")
	}
	current, err := os.Lstat(path)
	if err != nil || !os.SameFile(info, current) {
		return errors.New("published configuration changed during staging")
	}
	if os.Rename(f.Name(), path) != nil || syncDirectory(filepath.Dir(path)) != nil {
		return errors.New("configuration publication durability is unconfirmed")
	}
	return nil
}

func (s *Service) EnableApply(ctx context.Context, options ApplyOptions) error {
	for _, path := range []string{options.ConfigDir, options.Transactions} {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path {
			return errors.New("explicit controlled apply paths required")
		}
	}
	if options.Backend == nil {
		return errors.New("local candidate backend required")
	}
	p := &Applier{service: s, options: options}
	f, err := openPrivate(s.paths.Lock)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	if err = lockContext(ctx, f); err != nil {
		return err
	}
	defer unlock(f)
	if err = p.recover(ctx); err != nil {
		return err
	}
	s.apply = p
	return nil
}
func (p *Applier) fence() string { return filepath.Join(p.options.ConfigDir, configbundle.FenceName) }
func (p *Applier) save(j *journal) error {
	raw, _ := json.Marshal(j)
	return writePrivate(filepath.Join(p.options.Transactions, j.ID, "journal.json"), append(raw, '\n'))
}
func (p *Applier) clearFence() error {
	if os.Remove(p.fence()) != nil || syncDirectory(p.options.ConfigDir) != nil {
		return errors.New("configuration fence removal is unconfirmed")
	}
	return nil
}

func (p *Applier) Apply(parent context.Context, state []byte) Result {
	// This bounds local control/DNS acquisition, not established provider calls.
	ctx, cancel := context.WithTimeout(parent, 45*time.Second)
	defer cancel()
	if _, err := os.Lstat(p.fence()); !os.IsNotExist(err) {
		return failure("configuration transaction requires recovery before another apply")
	}
	in := configbundle.Input{State: state, Resolve: p.options.Resolve, Now: p.service.now()}
	old := map[string][]byte{}
	for _, name := range configFiles {
		raw, err := privatefile.Read(filepath.Join(p.options.ConfigDir, name), 4*1024*1024, name == "cliproxyapi.yaml")
		if err != nil {
			return failure("published configuration unavailable; refusing to reset it")
		}
		old[name] = raw
	}
	in.Config, in.Generated, in.Context = old[configFiles[0]], old[configFiles[1]], old[configFiles[2]]
	var err error
	in.Accounts, err = privatefile.Read(filepath.Join(p.options.ConfigDir, "oauth-accounts.json"), 1024*1024, false)
	if err != nil {
		return failure("OAuth account registry unavailable")
	}
	in.Vault, err = privatefile.Read(p.service.paths.Vault, 2*1024*1024, true)
	if err != nil {
		return failure("trusted provider vault unavailable")
	}
	out, err := configbundle.Build(ctx, in)
	if err != nil {
		return failure(err.Error())
	}
	oldModels, err := p.options.Backend.Catalog(ctx)
	if err != nil {
		return failure("local candidate catalog unavailable; published files were not changed")
	}
	if err = p.options.Backend.Await(ctx, hash(in.Config), oldModels); err != nil {
		return failure("local candidate does not match the currently published configuration")
	}
	set := map[string]bool{}
	for _, id := range oldModels {
		set[id] = true
	}
	for _, id := range out.ReplacedModels {
		delete(set, id)
	}
	for _, id := range out.Models {
		set[id] = true
	}
	newModels := []string{}
	for id := range set {
		newModels = append(newModels, id)
	}
	sort.Strings(oldModels)
	sort.Strings(newModels)
	token, err := randomHex(16)
	if err != nil {
		return failure("transaction identity unavailable")
	}
	j := &journal{Version: 1, ID: "tx-" + token, Phase: "prepared", OldHashes: map[string]string{}, NewHashes: map[string]string{}, OldModels: oldModels, NewModels: newModels}
	dir := filepath.Join(p.options.Transactions, j.ID)
	if os.Mkdir(dir, 0700) != nil || syncDirectory(p.options.Transactions) != nil {
		return failure("cannot create durable configuration transaction")
	}
	next := map[string][]byte{configFiles[0]: out.Config, configFiles[1]: out.Generated, configFiles[2]: out.Context}
	for _, name := range configFiles {
		j.OldHashes[name] = hash(old[name])
		j.NewHashes[name] = hash(next[name])
		if writePrivate(filepath.Join(dir, name+".old"), old[name]) != nil || writePrivate(filepath.Join(dir, name+".new"), next[name]) != nil {
			return failure("cannot persist private configuration backup")
		}
	}
	if p.save(j) != nil {
		return failure("cannot prepare configuration journal")
	}
	marker, _ := json.Marshal(map[string]any{"version": 1, "transaction_id": j.ID})
	if err = writePrivate(p.fence(), marker); err != nil {
		return failure("configuration fence durability is unconfirmed; recovery may be required")
	}
	steps := []string{"static verification passed", "durable configuration journal prepared"}
	for _, name := range configFiles {
		current, errRead := privatefile.Read(filepath.Join(p.options.ConfigDir, name), 4*1024*1024, name == configFiles[0])
		if errRead != nil || hash(current) != j.OldHashes[name] {
			return p.abort(j, steps, "published configuration changed before publication")
		}
	}
	for _, name := range configFiles {
		if err = replaceConfig(filepath.Join(p.options.ConfigDir, name), next[name]); err != nil {
			return p.abort(j, steps, "configuration publication failed")
		}
	}
	j.Phase = "published"
	if p.save(j) != nil {
		return p.abort(j, steps, "configuration journal update failed")
	}
	steps = append(steps, "configuration files published")
	if err = p.options.Backend.Await(ctx, j.NewHashes[configFiles[0]], newModels); err != nil {
		return p.abort(j, steps, "new configuration did not reach actual catalog readiness")
	}
	steps = append(steps, "actual SDK and public catalog verification passed")
	j.Phase = "committed"
	if p.save(j) != nil {
		return Result{"ok": false, "uncertain": true, "error": "commit durability is unconfirmed; configuration fence retained", "txid": j.ID, "steps": steps}
	}
	if p.clearFence() != nil {
		return Result{"ok": false, "uncertain": true, "error": "configuration committed but fence finalization is unconfirmed", "txid": j.ID, "steps": steps}
	}
	return Result{"ok": true, "txid": j.ID, "models": len(out.Models), "providers": out.Providers, "localStatus": 200, "steps": steps}
}

func (p *Applier) abort(j *journal, steps []string, message string) Result {
	// A failed/canceled publication must finish recovery independently of the
	// original request context; otherwise partial files could be left exposed.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := p.rollback(ctx, j); err != nil {
		return Result{"ok": false, "rollback": false, "rollback_failed": true, "error": "configuration recovery unconfirmed; admissions remain fenced", "txid": j.ID, "steps": steps}
	}
	return Result{"ok": false, "rollback": true, "error": message, "txid": j.ID, "steps": append(steps, "previous configuration and actual catalog restored")}
}
func (p *Applier) validateFiles(j *journal) error {
	for _, name := range configFiles {
		for _, suffix := range []string{"old", "new"} {
			raw, err := privatefile.Read(filepath.Join(p.options.Transactions, j.ID, name+"."+suffix), 4*1024*1024, true)
			want := j.OldHashes[name]
			if suffix == "new" {
				want = j.NewHashes[name]
			}
			if err != nil || hash(raw) != want {
				return errors.New("private configuration backup verification failed")
			}
		}
		current, err := privatefile.Read(filepath.Join(p.options.ConfigDir, name), 4*1024*1024, name == configFiles[0])
		if err != nil || hash(current) != j.OldHashes[name] && hash(current) != j.NewHashes[name] {
			return errors.New("external configuration edit detected; automatic recovery refused")
		}
	}
	return nil
}
func (p *Applier) rollback(ctx context.Context, j *journal) error {
	if err := p.validateFiles(j); err != nil {
		return err
	}
	j.Phase = "rolling_back"
	if err := p.save(j); err != nil {
		return err
	}
	for _, name := range configFiles {
		raw, err := privatefile.Read(filepath.Join(p.options.Transactions, j.ID, name+".old"), 4*1024*1024, true)
		if err != nil {
			return err
		}
		if err = replaceConfig(filepath.Join(p.options.ConfigDir, name), raw); err != nil {
			return err
		}
	}
	if err := p.options.Backend.Await(ctx, j.OldHashes[configFiles[0]], j.OldModels); err != nil {
		return err
	}
	j.Phase = "rolled_back"
	if err := p.save(j); err != nil {
		return err
	}
	return p.clearFence()
}
func (p *Applier) recover(ctx context.Context) error {
	if _, err := os.Lstat(p.fence()); os.IsNotExist(err) {
		return nil
	}
	raw, err := privatefile.Read(p.fence(), 4096, true)
	if err != nil {
		return errors.New("configuration fence is unsafe")
	}
	var marker struct {
		Version int    `json:"version"`
		ID      string `json:"transaction_id"`
	}
	if privatefile.Decode(raw, &marker) != nil || marker.Version != 1 || !strings.HasPrefix(marker.ID, "tx-") || len(marker.ID) != 35 || !identity.MatchString(marker.ID) {
		return errors.New("configuration fence is invalid")
	}
	journalRaw, err := privatefile.Read(filepath.Join(p.options.Transactions, marker.ID, "journal.json"), 1024*1024, true)
	if err != nil {
		return errors.New("pending configuration journal unavailable")
	}
	var j journal
	if privatefile.Decode(journalRaw, &j) != nil || j.Version != 1 || j.ID != marker.ID {
		return errors.New("pending configuration journal invalid")
	}
	if err = p.validateFiles(&j); err != nil {
		return err
	}
	if j.Phase == "committed" {
		for _, name := range configFiles {
			raw, err := privatefile.Read(filepath.Join(p.options.ConfigDir, name), 4*1024*1024, name == configFiles[0])
			if err != nil || hash(raw) != j.NewHashes[name] {
				return errors.New("committed configuration differs from journal")
			}
		}
		if err = p.options.Backend.Await(ctx, j.NewHashes[configFiles[0]], j.NewModels); err != nil {
			return errors.New("committed catalog recovery is unconfirmed")
		}
		return p.clearFence()
	}
	switch j.Phase {
	case "prepared", "published", "rolling_back", "rolled_back":
		return p.rollback(ctx, &j)
	default:
		return errors.New("unknown configuration transaction phase")
	}
}
