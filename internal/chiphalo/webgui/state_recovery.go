package webgui

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/configbundle"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/privatefile"
)

const stateManifest = ".state-recovery.json"
const stateFence = ".state-write-pending.json"

type stateCheckpoint struct {
	Version int    `json:"version"`
	Digest  string `json:"sha256"`
	Image   string `json:"image"`
}
type statePublication struct {
	Version int    `json:"version"`
	Before  string `json:"before"`
	After   string `json:"after"`
	Image   string `json:"image"`
}

var stateDigestPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)
var stateImagePattern = regexp.MustCompile(`^state-image-[a-zA-Z0-9]+\.json$`)

func stateDigest(raw []byte) string {
	hash := sha256.Sum256(raw)
	return hex.EncodeToString(hash[:])
}
func decodeState(raw []byte) (map[string]any, error) {
	var value map[string]any
	if configbundle.DecodeDraft(raw, &value) != nil || value == nil {
		return nil, errors.New("invalid subscription document")
	}
	if _, ok := value["providers"].([]any); !ok {
		return nil, errors.New("invalid subscription inventory")
	}
	return value, nil
}
func stateBytes(value any) ([]byte, error) {
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil || len(raw)+1 > 4*1024*1024 {
		return nil, errors.New("subscription document exceeds bound")
	}
	return append(raw, '\n'), nil
}

// publishStateFile supports a missing file only when its absence was captured.
// It never follows a target symlink or overwrites an unexpected concurrent edit.
func publishStateFile(path string, raw []byte, expected []byte, private bool) error {
	info, err := os.Lstat(path)
	missing := os.IsNotExist(err)
	if !missing && (err != nil || !info.Mode().IsRegular()) {
		return errors.New("unsafe subscription publication target")
	}
	if missing != (expected == nil) {
		return errors.New("subscription publication target changed")
	}
	if !missing {
		before, err := privatefile.Read(path, 4*1024*1024, private)
		if err != nil || stateDigest(before) != stateDigest(expected) {
			return errors.New("subscription publication target changed")
		}
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".state-stage-*")
	if err != nil {
		return errors.New("cannot stage subscription publication")
	}
	defer func() { _ = file.Close(); _ = os.Remove(file.Name()) }()
	mode := os.FileMode(0600)
	if !missing && !private {
		mode = info.Mode().Perm()
	}
	if err = file.Chmod(mode); err == nil && !missing {
		err = preserveFileOwner(file, info)
	}
	if err == nil {
		_, err = file.Write(raw)
	}
	if err == nil {
		err = file.Sync()
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return errors.New("subscription staging durability unconfirmed")
	}
	current, statErr := os.Lstat(path)
	if missing {
		if !os.IsNotExist(statErr) {
			return errors.New("subscription target appeared during publication")
		}
	} else {
		before, readErr := privatefile.Read(path, 4*1024*1024, private)
		if statErr != nil || !os.SameFile(info, current) || readErr != nil || stateDigest(before) != stateDigest(expected) {
			return errors.New("subscription target changed during publication")
		}
	}
	if os.Rename(file.Name(), path) != nil || syncParent(filepath.Dir(path)) != nil {
		return errors.New("subscription publication durability unconfirmed")
	}
	return nil
}
func (s *Server) stateMetadata(name string, value any) error {
	path := filepath.Join(s.options.Data, name)
	var expected []byte
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		var readErr error
		expected, readErr = privatefile.Read(path, 4096, true)
		if readErr != nil {
			return errors.New("subscription metadata unavailable")
		}
	}
	raw, err := stateBytes(value)
	if err != nil || len(raw) > 4096 {
		return errors.New("subscription metadata exceeds bound")
	}
	return publishStateFile(path, raw, expected, true)
}
func (s *Server) stateImage(raw []byte) (string, error) {
	dir := filepath.Join(s.options.Data, "state-recovery")
	if err := os.Mkdir(dir, 0700); err != nil && !os.IsExist(err) {
		return "", errors.New("cannot create subscription recovery directory")
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || runtime.GOOS != "windows" && info.Mode().Perm()&0077 != 0 {
		return "", errors.New("unsafe subscription recovery directory")
	}
	file, err := os.CreateTemp(dir, "state-image-*.json")
	if err != nil {
		return "", errors.New("cannot stage subscription recovery image")
	}
	name := file.Name()
	if _, err = file.Write(raw); err == nil {
		err = file.Sync()
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil || syncParent(dir) != nil || syncParent(s.options.Data) != nil {
		return "", errors.New("subscription recovery image durability unconfirmed")
	}
	return filepath.Base(name), nil
}
func (s *Server) confirmedStateImage(record stateCheckpoint) ([]byte, error) {
	if record.Version != 1 || !stateDigestPattern.MatchString(record.Digest) || !stateImagePattern.MatchString(record.Image) {
		return nil, errors.New("invalid subscription recovery checkpoint")
	}
	dir := filepath.Join(s.options.Data, "state-recovery")
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || runtime.GOOS != "windows" && info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("unsafe subscription recovery checkpoint")
	}
	raw, err := privatefile.Read(filepath.Join(dir, record.Image), 4*1024*1024, true)
	if err != nil || stateDigest(raw) != record.Digest {
		return nil, errors.New("subscription recovery image invalid")
	}
	if _, err := decodeState(raw); err != nil {
		return nil, err
	}
	return raw, nil
}
func (s *Server) clearStateFence() error {
	if os.Remove(filepath.Join(s.options.Data, stateFence)) != nil || syncParent(s.options.Data) != nil {
		return errors.New("subscription publication fence durability unconfirmed")
	}
	return nil
}
func (s *Server) resolveStatePublication() error {
	path := filepath.Join(s.options.Data, stateFence)
	if _, err := os.Lstat(path); os.IsNotExist(err) {
		return nil
	}
	var pending statePublication
	if readJSON(path, 4096, true, &pending) != nil || pending.Version != 1 || !stateImagePattern.MatchString(pending.Image) || !stateDigestPattern.MatchString(pending.After) || pending.Before != "missing" && !stateDigestPattern.MatchString(pending.Before) {
		return errors.New("subscription publication journal invalid")
	}
	raw, err := privatefile.Read(filepath.Join(s.options.Data, "state.json"), 4*1024*1024, false)
	current := "missing"
	if err == nil {
		current = stateDigest(raw)
	} else if _, err := os.Lstat(filepath.Join(s.options.Data, "state.json")); !os.IsNotExist(err) {
		return errors.New("subscription publication outcome unavailable")
	}
	if current == pending.Before {
		return s.clearStateFence()
	}
	if current != pending.After {
		return errors.New("subscription publication conflicts with an external edit")
	}
	record := stateCheckpoint{Version: 1, Digest: pending.After, Image: pending.Image}
	if _, err := s.confirmedStateImage(record); err != nil {
		return err
	}
	if err := s.stateMetadata(stateManifest, record); err != nil {
		return err
	}
	return s.clearStateFence()
}
func (s *Server) commitState(state map[string]any, before []byte) error {
	if err := s.resolveStatePublication(); err != nil {
		return err
	}
	raw, err := stateBytes(state)
	if err != nil {
		return err
	}
	if _, err := decodeState(raw); err != nil {
		return err
	}
	image, err := s.stateImage(raw)
	if err != nil {
		return err
	}
	oldDigest := "missing"
	if before != nil {
		oldDigest = stateDigest(before)
	}
	pending := statePublication{Version: 1, Before: oldDigest, After: stateDigest(raw), Image: image}
	if err := s.stateMetadata(stateFence, pending); err != nil {
		return err
	}
	if s.stateFault != nil {
		if err := s.stateFault("before-publish"); err != nil {
			return err
		}
	}
	if err := publishStateFile(filepath.Join(s.options.Data, "state.json"), raw, before, false); err != nil {
		return err
	}
	if s.stateFault != nil {
		if err := s.stateFault("after-publish"); err != nil {
			return err
		}
	}
	if err := s.stateMetadata(stateManifest, stateCheckpoint{Version: 1, Digest: pending.After, Image: image}); err != nil {
		return err
	}
	if err := s.clearStateFence(); err != nil {
		return err
	}
	s.pruneStateImages(image)
	return nil
}
func (s *Server) pruneStateImages(keep string) {
	dir := filepath.Join(s.options.Data, "state-recovery")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	names := []string{}
	for _, entry := range entries {
		if entry.Type().IsRegular() && stateImagePattern.MatchString(entry.Name()) {
			names = append(names, entry.Name())
		}
	}
	slices.SortFunc(names, func(a, b string) int {
		ia, e1 := os.Lstat(filepath.Join(dir, a))
		ib, e2 := os.Lstat(filepath.Join(dir, b))
		if e1 != nil || e2 != nil {
			return strings.Compare(a, b)
		}
		return ia.ModTime().Compare(ib.ModTime())
	})
	for _, name := range names[:max(0, len(names)-20)] {
		if name != keep {
			_ = os.Remove(filepath.Join(dir, name))
		}
	}
	_ = syncParent(dir)
}
func (s *Server) loadRecoverableState() (map[string]any, error) {
	if err := s.resolveStatePublication(); err != nil {
		return nil, err
	}
	var record stateCheckpoint
	meta := filepath.Join(s.options.Data, stateManifest)
	_, metaErr := os.Lstat(meta)
	hasCheckpoint := !os.IsNotExist(metaErr)
	if hasCheckpoint && readJSON(meta, 4096, true, &record) != nil {
		return nil, errors.New("subscription recovery checkpoint unavailable")
	}
	path := filepath.Join(s.options.Data, "state.json")
	raw, readErr := privatefile.Read(path, 4*1024*1024, false)
	state, decodeErr := decodeState(raw)
	if readErr == nil && decodeErr == nil {
		if hasCheckpoint {
			if record.Version != 1 || record.Digest != stateDigest(raw) || !stateImagePattern.MatchString(record.Image) {
				return nil, errors.New("subscription state changed outside the confirmed transaction")
			}
			if _, err := s.confirmedStateImage(record); err != nil {
				return nil, err
			}
		} else {
			image, err := s.stateImage(raw)
			if err != nil {
				return nil, err
			}
			if err := s.stateMetadata(stateManifest, stateCheckpoint{Version: 1, Digest: stateDigest(raw), Image: image}); err != nil {
				return nil, err
			}
		}
		return state, nil
	}
	if !hasCheckpoint {
		return nil, errors.New("GUI state unavailable; no confirmed recovery checkpoint")
	}
	info, statErr := os.Lstat(path)
	if !os.IsNotExist(statErr) && (statErr != nil || !info.Mode().IsRegular() || readErr != nil) {
		return nil, errors.New("unsafe GUI state target; automatic recovery refused")
	}
	image, err := s.confirmedStateImage(record)
	if err != nil {
		return nil, err
	}
	state, _ = decodeState(image)
	state["recovery"] = map[string]any{"reason": "confirmed-checkpoint", "recoveredAt": s.options.Now().Unix(), "providers": len(state["providers"].([]any)), "fromBackup": record.Image, "fromGateway": 0, "errors": []string{}}
	if os.IsNotExist(statErr) {
		raw = nil
	} else {
		dir := filepath.Join(s.options.Data, "state-recovery")
		file, err := os.CreateTemp(dir, "corrupt-state-*")
		if err != nil {
			return nil, errors.New("cannot preserve damaged subscription evidence")
		}
		if _, err = file.Write(raw); err == nil {
			err = file.Sync()
		}
		if closeErr := file.Close(); err == nil {
			err = closeErr
		}
		if err != nil || syncParent(dir) != nil {
			return nil, errors.New("damaged subscription evidence durability unconfirmed")
		}
		state["recovery"].(map[string]any)["damagedStateBackup"] = filepath.Base(file.Name())
	}
	if err := s.commitState(state, raw); err != nil {
		return nil, err
	}
	return state, nil
}
