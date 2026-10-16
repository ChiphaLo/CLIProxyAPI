// Package durableauth adds an opt-in persistence barrier around the upstream
// Codex token store. It does not replace its JSON schema or merge semantics.
package durableauth

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/codex"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/privatefile"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

var ErrUnconfirmed = errors.New("OAuth credential persistence is unconfirmed")
var ErrReadOnly = errors.New("shadow OAuth store is read-only")
var ErrUnsafePath = errors.New("OAuth credential path is unsafe")

const markerName = ".cpa-auth-save-unconfirmed"

type Store struct {
	inner               coreauth.Store
	authDir, healthRoot string
	readOnly            bool
	mu                  sync.Mutex
	failed, busy        atomic.Bool
}

// Open refuses a previous unconfirmed persistence attempt. The health root is
// shared across releases but separate from their immutable code artifacts.
// This marker fences persistence/restart, not an upstream rotation occurring
// before Save. A full handoff still requires the refresh owner's drain barrier.
func Open(authDir, healthRoot string, inner coreauth.Store, readOnly bool) (*Store, error) {
	if inner == nil || !realDirectory(authDir) || !realDirectory(healthRoot) || !privateHealthRoot(healthRoot) {
		return nil, ErrUnsafePath
	}
	if !readOnly && !durabilitySupported() {
		return nil, errors.New("durable OAuth persistence requires Linux")
	}
	if _, err := os.Lstat(filepath.Join(healthRoot, markerName)); !os.IsNotExist(err) {
		return nil, ErrUnconfirmed
	}
	if setter, ok := inner.(interface{ SetBaseDir(string) }); ok {
		setter.SetBaseDir(authDir)
	}
	return &Store{inner: inner, authDir: authDir, healthRoot: healthRoot, readOnly: readOnly}, nil
}

func realDirectory(path string) bool {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return false
	}
	resolved, err := filepath.EvalSymlinks(path)
	info, errInfo := os.Lstat(path)
	return err == nil && errInfo == nil && resolved == path && info.IsDir()
}

// AuthDir and SetBaseDir keep GUI-owned YAML from redirecting the host store.
func (s *Store) AuthDir() string   { return s.authDir }
func (s *Store) SetBaseDir(string) {}

func (s *Store) Err() error {
	if s.failed.Load() {
		return ErrUnconfirmed
	}
	if !s.busy.Load() {
		if _, err := os.Lstat(filepath.Join(s.healthRoot, markerName)); !os.IsNotExist(err) {
			return ErrUnconfirmed
		}
	}
	return nil
}

func (s *Store) Pending() bool { return s.busy.Load() }

func (s *Store) List(ctx context.Context) ([]*coreauth.Auth, error) {
	if err := s.Err(); err != nil {
		return nil, err
	}
	return s.inner.List(ctx)
}

// Deletion/import is an account transaction, not a refresh-store operation.
func (s *Store) Delete(context.Context, string) error { return ErrReadOnly }

func (s *Store) path(auth *coreauth.Auth) (string, error) {
	if auth == nil || !coreauth.IsCodexAuthRecord(auth) && !(auth.Provider == "antigravity" && auth.Metadata["type"] == "antigravity") {
		return "", ErrUnsafePath
	}
	name := strings.TrimSpace(auth.Attributes[coreauth.AttributePath])
	if name == "" {
		name = strings.TrimSpace(auth.FileName)
	}
	if name == "" {
		name = strings.TrimSpace(auth.ID)
	}
	if !filepath.IsAbs(name) {
		name = filepath.Join(s.authDir, name)
	}
	if filepath.Clean(name) != name || filepath.Dir(name) != s.authDir || !strings.EqualFold(filepath.Ext(name), ".json") {
		return "", ErrUnsafePath
	}
	info, err := os.Lstat(name)
	if err == nil && (!info.Mode().IsRegular() || info.Mode().Perm()&0027 != 0) {
		return "", ErrUnsafePath
	}
	if err != nil && !os.IsNotExist(err) {
		return "", ErrUnsafePath
	}
	return name, nil
}

func (s *Store) Save(ctx context.Context, auth *coreauth.Auth) (string, error) {
	if s.readOnly {
		return "", ErrReadOnly
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.Err(); err != nil {
		return "", err
	}
	path, err := s.path(auth)
	if err != nil {
		s.failed.Store(true)
		_ = s.arm()
		return "", ErrUnconfirmed
	}
	before, errBefore := os.Lstat(path)
	if errBefore != nil && (!os.IsNotExist(errBefore) || !coreauth.HasAuthCreationIntent(ctx)) {
		s.failed.Store(true)
		_ = s.arm()
		return "", ErrUnconfirmed
	}
	s.busy.Store(true)
	defer s.busy.Store(false)
	// Arm and sync before allowing any mutation. A crash during Save leaves a
	// startup fence instead of silently handing the next owner an unknown token.
	if err := s.arm(); err != nil {
		s.failed.Store(true)
		return "", ErrUnconfirmed
	}
	returned, errSave := s.inner.Save(ctx, auth)
	if errSave != nil || returned != path {
		s.failed.Store(true)
		return "", ErrUnconfirmed
	}
	// Use upstream's same inter-process file lock for read-back, attribute
	// restoration and sync. Do not nest it around inner.Save's own lock.
	errConfirm := codex.WithFileLock(path, func() error {
		raw, err := privatefile.Read(path, 4*1024*1024, true)
		if err != nil {
			return ErrUnconfirmed
		}
		var disk map[string]any
		if json.Unmarshal(raw, &disk) != nil || disk == nil {
			return ErrUnconfirmed
		}
		coreauth.NormalizeCredentialMetadata(disk)
		keys := []string{"type", "prefix", "account_id", "email", "access_token", "refresh_token", "id_token", "expired", "disabled"}
		if auth.Provider == "antigravity" {
			keys = append(keys, "project_id", "proxy_url", "antigravity_capability_hints")
		}
		for _, key := range keys {
			if value, found := auth.Metadata[key]; found && !reflect.DeepEqual(value, disk[key]) {
				return ErrUnconfirmed
			}
		}
		return confirmFile(path, before)
	})
	if errConfirm != nil || s.disarm() != nil {
		s.failed.Store(true)
		return "", ErrUnconfirmed
	}
	return path, nil
}

func (s *Store) arm() error {
	file, err := os.OpenFile(filepath.Join(s.healthRoot, markerName), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	if _, err := file.WriteString("credential-persistence-unconfirmed-v1\n"); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	return syncDirectory(s.healthRoot)
}

func (s *Store) disarm() error {
	if err := os.Remove(filepath.Join(s.healthRoot, markerName)); err != nil {
		return err
	}
	if err := syncDirectory(s.healthRoot); err != nil {
		// Best-effort re-arm after an unconfirmed deletion; readiness remains
		// sticky-failed even when a failing filesystem cannot store the fence.
		_ = s.arm()
		return err
	}
	return nil
}
