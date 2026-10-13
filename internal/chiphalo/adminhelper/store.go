package adminhelper

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/clientkeys"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/privatefile"
)

type Paths struct{ Keys, Vault, Lock, Audit string }

type Service struct {
	paths Paths
	mu    sync.Mutex
	gate  chan struct{}
	now   func() time.Time
	apply *Applier
}

func New(paths Paths) (*Service, error) {
	seen := map[string]bool{}
	for _, path := range []string{paths.Keys, paths.Vault, paths.Lock, paths.Audit} {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path || seen[path] {
			return nil, errors.New("invalid control file paths")
		}
		seen[path] = true
	}
	return &Service{paths: paths, now: time.Now, gate: make(chan struct{}, 1)}, nil
}

func (s *Service) dispatch(op string, payload map[string]json.RawMessage) Result {
	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Second)
	defer cancel()
	return s.dispatchContext(ctx, op, payload)
}
func (s *Service) dispatchContext(ctx context.Context, op string, payload map[string]json.RawMessage) Result {
	switch op {
	case "create_api_key", "update_api_key", "delete_api_key", "read_api_keys", "set_provider_secret", "delete_provider_secret", "apply_bundle":
	default:
		return failure("operation not migrated to Go helper")
	}
	select {
	case s.gate <- struct{}{}:
		defer func() { <-s.gate }()
	case <-ctx.Done():
		return failure("control operation canceled before dispatch")
	}
	f, err := openPrivate(s.paths.Lock)
	if err != nil {
		return failure("control lock unavailable")
	}
	defer func() { _ = f.Close() }()
	if lockContext(ctx, f) != nil {
		return failure("control lock unavailable")
	}
	defer unlock(f)
	if op == "apply_bundle" {
		if s.apply == nil {
			return failure("Go configuration apply is not enabled")
		}
		if err := allowed(payload, "state"); err != nil {
			return failure(err.Error())
		}
		return s.apply.Apply(ctx, payload["state"])
	}
	if ctx.Err() != nil {
		return failure("control operation canceled before mutation")
	}
	if op == "set_provider_secret" || op == "delete_provider_secret" {
		return s.secret(op, payload)
	}
	return s.keys(op, payload)
}

// Existing lock/audit files are opened without following symlinks. Their parent
// directories must be controlled by the deployment owner, not the GUI UID.
func openPrivate(path string) (*os.File, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("private control file unavailable")
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return nil, errors.New("private control file unavailable")
	}
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) {
		_ = f.Close()
		return nil, errors.New("private control file changed")
	}
	return f, nil
}

// replaceJSON preserves unrecognized business fields and ownership. It publishes
// a complete fsynced document by rename. Consumers must reopen the path; legacy
// single-file bind mounts are not compatible with this writer.
func replaceJSON(path string, value any) error {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return errors.New("control database unavailable")
	}
	if _, err := privatefile.Read(path, 2*1024*1024, true); err != nil {
		return errors.New("control database unavailable")
	}
	raw, err := json.Marshal(value)
	if err != nil || len(raw)+1 > 2*1024*1024 {
		return errors.New("control database exceeds limit")
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".cpa-control-*")
	if err != nil {
		return errors.New("cannot stage control database")
	}
	defer func() { _ = f.Close(); _ = os.Remove(f.Name()) }()
	// Set permissions while the staging file still belongs to this process.
	// Restoring a different service UID first would require CAP_FOWNER merely
	// to chmod the file, even though no such capability is otherwise needed.
	if err = f.Chmod(0600); err == nil {
		err = preserveOwner(f, info)
	}
	if err == nil {
		_, err = f.Write(append(raw, '\n'))
	}
	if err == nil {
		err = f.Sync()
	}
	if err == nil {
		err = f.Close()
	}
	if err != nil {
		return errors.New("cannot stage control database")
	}
	current, err := os.Lstat(path)
	if err != nil || !os.SameFile(info, current) {
		return errors.New("control database changed before publication")
	}
	if os.Rename(f.Name(), path) != nil {
		return errors.New("cannot publish control database")
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return errors.New("control database published but durability unconfirmed")
	}
	defer func() { _ = dir.Close() }()
	if dir.Sync() != nil {
		return errors.New("control database published but durability unconfirmed")
	}
	return nil
}

func text(payload map[string]json.RawMessage, name string, limit int) (string, error) {
	raw, exists := payload[name]
	if !exists || string(raw) == "null" {
		return "", nil
	}
	var value string
	if json.Unmarshal(raw, &value) != nil {
		return "", fmt.Errorf("%s must be a string", name)
	}
	value = strings.TrimSpace(value)
	runes := []rune(value)
	if len(runes) > limit {
		value = string(runes[:limit])
	}
	return value, nil
}

func allowed(payload map[string]json.RawMessage, fields ...string) error {
	set := map[string]bool{}
	for _, field := range fields {
		set[field] = true
	}
	for field := range payload {
		if !set[field] {
			return errors.New("unknown payload fields")
		}
	}
	return nil
}

func set(value any) json.RawMessage { raw, _ := json.Marshal(value); return raw }

func stringList(raw json.RawMessage, field string) (json.RawMessage, error) {
	if string(raw) == "null" {
		return raw, nil
	}
	var values []string
	if json.Unmarshal(raw, &values) != nil || values == nil || len(values) > 128 {
		return nil, fmt.Errorf("%s must be a bounded list of strings", field)
	}
	out := []string{}
	seen := map[string]bool{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if len([]rune(value)) > 200 {
			return nil, fmt.Errorf("%s item exceeds limit", field)
		}
		if value != "" && !seen[value] {
			out = append(out, value)
			seen[value] = true
		}
	}
	return set(out), nil
}

func patchKey(entry map[string]json.RawMessage, payload map[string]json.RawMessage) error {
	for field, limit := range map[string]int{"name": 120, "note": 500} {
		if _, exists := payload[field]; exists {
			value, err := text(payload, field, limit)
			if err != nil {
				return err
			}
			entry[field] = set(value)
		}
	}
	if raw, exists := payload["enabled"]; exists {
		var value bool
		if string(raw) == "null" || json.Unmarshal(raw, &value) != nil {
			return errors.New("enabled must be boolean")
		}
		entry["enabled"] = set(value)
	}
	for field, ceiling := range map[string]int64{"expiresAt": 0, "rpm": 10000, "maxConcurrent": 64} {
		if raw, exists := payload[field]; exists {
			if string(raw) == "null" {
				entry[field] = raw
				continue
			}
			var value int64
			if json.Unmarshal(raw, &value) != nil || value < 0 || ceiling != 0 && value == 0 {
				return fmt.Errorf("%s must be a valid integer", field)
			}
			if ceiling > 0 && value > ceiling {
				value = ceiling
			}
			entry[field] = set(value)
		}
	}
	for field, old := range map[string]string{"modelPatterns": "model_patterns", "providerPrefixes": "provider_prefixes"} {
		if raw, exists := payload[field]; exists {
			value, err := stringList(raw, field)
			if err != nil {
				return err
			}
			entry[field] = value
			// Clear legacy aliases too, so an explicitly cleared scope cannot be
			// resurrected by the gateway's backwards-compatible reader.
			delete(entry, old)
		}
	}
	return nil
}

func randomHex(bytes int) (string, error) {
	raw := make([]byte, bytes)
	if _, err := rand.Read(raw); err != nil {
		return "", errors.New("secure key generation unavailable")
	}
	return hex.EncodeToString(raw), nil
}

func (s *Service) keys(op string, payload map[string]json.RawMessage) Result {
	fields := []string{"key_id"}
	if op == "create_api_key" || op == "update_api_key" {
		fields = []string{"name", "note", "expiresAt", "modelPatterns", "providerPrefixes", "rpm", "maxConcurrent"}
		if op == "update_api_key" {
			fields = append(fields, "key_id", "enabled")
		}
	}
	if op == "read_api_keys" {
		fields = nil
	}
	if err := allowed(payload, fields...); err != nil {
		return failure(err.Error())
	}
	raw, err := privatefile.Read(s.paths.Keys, 2*1024*1024, true)
	if err != nil || clientkeys.ValidateDatabase(raw) != nil {
		return failure("issued-key database unavailable or invalid; refusing to reset it")
	}
	doc, err := object(raw)
	if err != nil {
		return failure("invalid issued-key database")
	}
	var rows []json.RawMessage
	if json.Unmarshal(doc["keys"], &rows) != nil {
		return failure("invalid issued-key database")
	}
	entries := make([]map[string]json.RawMessage, 0, len(rows))
	for _, row := range rows {
		entry, err := object(row)
		if err != nil {
			return failure("invalid issued-key database")
		}
		entries = append(entries, entry)
	}
	if op == "read_api_keys" {
		for _, entry := range entries {
			delete(entry, "hash")
		}
		return Result{"ok": true, "keys": entries}
	}
	result := Result{"ok": true}
	if op == "create_api_key" {
		key, errKey := randomHex(20)
		id, errID := randomHex(8)
		if errKey != nil || errID != nil {
			return failure("secure key generation unavailable")
		}
		key, id = "sk-cpa-"+key, "key_"+id
		digest := sha256.Sum256([]byte(key))
		entry := map[string]json.RawMessage{"id": set(id), "name": set("Unnamed Key"), "hash": set(hex.EncodeToString(digest[:])), "prefix": set(key[:16]), "enabled": set(true), "createdAt": set(s.now().Unix()), "lastUsedAt": set(nil), "note": set(""), "expiresAt": set(nil)}
		if err := patchKey(entry, payload); err != nil {
			return failure(err.Error())
		}
		if string(entry["name"]) == `""` {
			entry["name"] = set("Unnamed Key")
		}
		entries = append(entries, entry)
		safe := map[string]json.RawMessage{}
		for field, value := range entry {
			if field != "hash" {
				safe[field] = value
			}
		}
		result["key"], result["id"], result["entry"] = key, id, safe
	} else {
		id, errID := text(payload, "key_id", 129)
		if errID != nil || !identity.MatchString(id) || id == "admin" {
			return failure("invalid key id")
		}
		found := -1
		for i, entry := range entries {
			var current string
			_ = json.Unmarshal(entry["id"], &current)
			if current == id {
				found = i
				break
			}
		}
		if found < 0 {
			return failure("key not found")
		}
		if op == "delete_api_key" {
			entries = append(entries[:found], entries[found+1:]...)
		} else if err := patchKey(entries[found], payload); err != nil {
			return failure(err.Error())
		}
	}
	doc["keys"] = set(entries)
	next, err := json.Marshal(doc)
	if err != nil || clientkeys.ValidateDatabase(next) != nil {
		return failure("invalid issued-key policy")
	}
	if err := replaceJSON(s.paths.Keys, doc); err != nil {
		return failure(err.Error())
	}
	return result
}

func (s *Service) secret(op string, payload map[string]json.RawMessage) Result {
	fields := []string{"provider_id"}
	if op == "set_provider_secret" {
		fields = append(fields, "secret")
	}
	if err := allowed(payload, fields...); err != nil {
		return failure(err.Error())
	}
	id, err := text(payload, "provider_id", 129)
	if err != nil || !identity.MatchString(id) {
		return failure("invalid provider id")
	}
	raw, err := privatefile.Read(s.paths.Vault, 2*1024*1024, true)
	if err != nil {
		return failure("provider vault unavailable; refusing to reset it")
	}
	doc, err := object(raw)
	if err != nil {
		return failure("invalid provider vault")
	}
	if op == "set_provider_secret" {
		secret, err := text(payload, "secret", 4097)
		if err != nil || secret == "" || len(secret) > 4096 {
			return failure("invalid provider secret request")
		}
		doc[id] = set(secret)
	} else {
		delete(doc, id)
	}
	if err := replaceJSON(s.paths.Vault, doc); err != nil {
		return failure(err.Error())
	}
	return Result{"ok": true, "provider_id": id, "hasApiKey": op == "set_provider_secret"}
}

func (s *Service) audit(id, op, phase string, ok bool, uid uint32) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := openPrivate(s.paths.Audit)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	if lock(f) != nil {
		return errors.New("audit lock unavailable")
	}
	defer unlock(f)
	if _, err := f.Seek(0, 2); err != nil {
		return err
	}
	raw, _ := json.Marshal(map[string]any{"ts": s.now().Unix(), "txid": id, "op": op, "phase": phase, "ok": ok, "peer_uid": uid})
	if _, err := f.Write(append(raw, '\n')); err != nil {
		return err
	}
	return f.Sync()
}
