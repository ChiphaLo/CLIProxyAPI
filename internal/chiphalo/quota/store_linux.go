//go:build linux

package quota

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const snapshotName = "oauth-accounts.json"
const historyName = "oauth-quota-history.jsonl"
const exclusionsName = "quota-model-exclusions.json"
const historyLimit = 32 * 1024 * 1024

type Store struct {
	mu         sync.Mutex
	path       string
	dir, lock  *os.File
	info       os.FileInfo
	observeGID uint32
	closed     bool
}

// OpenStore anchors every operation to a directory descriptor. A mutable
// gateway-owned parent cannot redirect a privileged collector through a symlink.
// The process lock is held for the store lifetime; legacy writers must be stopped
// by the release supervisor before enabling this collector.
func OpenStore(ctx context.Context, path string, gatewayUID, observeGID uint32) (*Store, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, errors.New("explicit quota directory required")
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil || resolved != path {
		return nil, errors.New("real quota directory required")
	}
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0022 != 0 {
		return nil, errors.New("quota directory permissions unsafe")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) && stat.Uid != gatewayUID {
		return nil, errors.New("quota directory owner mismatch")
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, errors.New("cannot anchor quota directory")
	}
	dir := os.NewFile(uintptr(fd), path)
	opened, err := dir.Stat()
	if err != nil || !os.SameFile(info, opened) {
		_ = dir.Close()
		return nil, errors.New("quota directory changed while opening")
	}
	s := &Store{path: path, dir: dir, info: info, observeGID: observeGID}
	lock, err := s.open(".cpa-quota-collector.lock", unix.O_RDWR|unix.O_CREAT, 0600, false)
	if err != nil {
		_ = dir.Close()
		return nil, err
	}
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			_ = lock.Close()
			_ = dir.Close()
			return nil, errors.New("collector ownership acquisition canceled")
		}
		err = unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			break
		}
		if err != unix.EAGAIN && err != unix.EWOULDBLOCK {
			_ = lock.Close()
			_ = dir.Close()
			return nil, errors.New("collector ownership unavailable")
		}
		select {
		case <-ctx.Done():
		case <-ticker.C:
		}
	}
	s.lock = lock
	return s, nil
}
func (s *Store) check() error {
	if s.closed {
		return os.ErrClosed
	}
	current, err := os.Lstat(s.path)
	if err != nil || !current.IsDir() || !os.SameFile(current, s.info) || current.Mode().Perm()&0022 != 0 {
		return errors.New("quota directory identity changed")
	}
	return nil
}
func (s *Store) open(name string, flags int, mode uint32, public bool) (*os.File, error) {
	if name != filepath.Base(name) || name == "." || name == ".." {
		return nil, errors.New("quota file name rejected")
	}
	fd, err := unix.Openat(int(s.dir.Fd()), name, flags|unix.O_NOFOLLOW|unix.O_CLOEXEC, mode)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), name)
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, errors.New("quota file status unavailable")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if err != nil || !ok || !info.Mode().IsRegular() || stat.Uid != uint32(os.Geteuid()) || info.Mode().Perm()&0027 != 0 || !public && info.Mode().Perm()&0077 != 0 {
		_ = file.Close()
		return nil, errors.New("quota file ownership/permissions unsafe")
	}
	return file, nil
}
func (s *Store) read(name string, limit int64) ([]byte, error) {
	file, err := s.open(name, unix.O_RDONLY, 0, true)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	raw, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil || int64(len(raw)) > limit {
		return nil, errors.New("quota input exceeds bound")
	}
	return raw, nil
}
func (s *Store) Previous() (Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.check(); err != nil {
		return Snapshot{}, err
	}
	raw, err := s.read(snapshotName, 4*1024*1024)
	if errors.Is(err, os.ErrNotExist) {
		return Snapshot{Accounts: []Account{}}, nil
	}
	if err != nil {
		return Snapshot{}, err
	}
	value, err := DecodeSnapshot(raw)
	if err != nil || value.Accounts == nil {
		return Snapshot{}, errors.New("previous quota snapshot invalid; refusing to reset it")
	}
	return value, nil
}
func randomName(prefix string) (string, error) {
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(nonce), nil
}
func (s *Store) publish(raw []byte) error {
	mode := os.FileMode(0640)
	uid, gid := os.Geteuid(), int(s.observeGID)
	var original os.FileInfo
	old, err := s.open(snapshotName, unix.O_RDONLY, 0, true)
	if err == nil {
		raw, e := io.ReadAll(io.LimitReader(old, 4*1024*1024+1))
		previous, decodeErr := DecodeSnapshot(raw)
		if e != nil || len(raw) > 4*1024*1024 || decodeErr != nil || previous.Accounts == nil {
			_ = old.Close()
			return errors.New("previous quota snapshot invalid; refusing to reset it")
		}
		original, _ = old.Stat()
		_ = old.Close()
		stat := original.Sys().(*syscall.Stat_t)
		mode = original.Mode().Perm()
		uid, gid = int(stat.Uid), int(stat.Gid)
	} else if !errors.Is(err, os.ErrNotExist) {
		return errors.New("previous quota snapshot unsafe")
	}
	name, err := randomName(".oauth-public-")
	if err != nil {
		return err
	}
	file, err := s.open(name, unix.O_RDWR|unix.O_CREAT|unix.O_EXCL, 0600, false)
	if err != nil {
		return errors.New("cannot stage public quota snapshot")
	}
	defer func() { _ = file.Close(); _ = unix.Unlinkat(int(s.dir.Fd()), name, 0) }()
	if err = file.Chown(uid, gid); err == nil {
		err = file.Chmod(mode)
	}
	if err == nil {
		_, err = file.Write(raw)
	}
	if err == nil {
		err = file.Sync()
	}
	if err == nil {
		err = file.Close()
	}
	if err != nil {
		return errors.New("quota snapshot staging failed")
	}
	if original != nil {
		current, err := s.open(snapshotName, unix.O_RDONLY, 0, true)
		if err != nil {
			return errors.New("quota snapshot changed")
		}
		info, e := current.Stat()
		_ = current.Close()
		if e != nil || !os.SameFile(original, info) {
			return errors.New("quota snapshot changed")
		}
	} else {
		if current, err := s.open(snapshotName, unix.O_RDONLY, 0, true); err == nil {
			_ = current.Close()
			return errors.New("quota snapshot appeared during staging")
		} else if !errors.Is(err, os.ErrNotExist) {
			return errors.New("quota snapshot path changed")
		}
	}
	if s.check() != nil || unix.Renameat(int(s.dir.Fd()), name, int(s.dir.Fd()), snapshotName) != nil || s.dir.Sync() != nil {
		return errors.New("quota snapshot publication durability unconfirmed")
	}
	return nil
}
func (s *Store) exclusions() (Exclusions, error) {
	raw, err := s.read(exclusionsName, 1024*1024)
	if errors.Is(err, os.ErrNotExist) {
		return Exclusions{Intervals: []Interval{}}, nil
	}
	if err != nil {
		return Exclusions{}, err
	}
	var value Exclusions
	if json.Unmarshal(raw, &value) != nil || value.Intervals == nil {
		return Exclusions{}, errors.New("history exclusions invalid")
	}
	return value, nil
}
func (s *Store) appendHistory(rows []HistoryRow) error {
	if len(rows) == 0 {
		return nil
	}
	var lines bytes.Buffer
	for _, row := range rows {
		raw, err := json.Marshal(row)
		if err != nil {
			return err
		}
		lines.Write(raw)
		lines.WriteByte('\n')
	}
	history, err := s.open(historyName, unix.O_RDWR|unix.O_APPEND, 0, true)
	mode := os.FileMode(0640)
	uid, gid := os.Geteuid(), int(s.observeGID)
	if err == nil {
		info, e := history.Stat()
		if e != nil {
			_ = history.Close()
			return e
		}
		stat := info.Sys().(*syscall.Stat_t)
		mode, uid, gid = info.Mode().Perm(), int(stat.Uid), int(stat.Gid)
		if info.Size() > 0 {
			var tail [1]byte
			if _, err := history.ReadAt(tail[:], info.Size()-1); err != nil || tail[0] != '\n' {
				_ = history.Close()
				return errors.New("historical quota line incomplete; no data altered")
			}
		}
		if info.Size() > historyLimit {
			if err := history.Sync(); err != nil {
				_ = history.Close()
				return err
			}
			_ = history.Close()
			archive, err := randomName(historyName + ".archive-")
			if err != nil {
				return err
			}
			if unix.Renameat2(int(s.dir.Fd()), historyName, int(s.dir.Fd()), archive, unix.RENAME_NOREPLACE) != nil || s.dir.Sync() != nil {
				return errors.New("quota history archive durability unconfirmed")
			}
			history = nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if history == nil {
		history, err = s.open(historyName, unix.O_RDWR|unix.O_APPEND|unix.O_CREAT|unix.O_EXCL, 0600, false)
		if err != nil {
			return err
		}
		if err = history.Chown(uid, gid); err == nil {
			err = history.Chmod(mode)
		}
		if err != nil {
			_ = history.Close()
			return err
		}
		if s.dir.Sync() != nil {
			_ = history.Close()
			return errors.New("new quota history durability unconfirmed")
		}
	}
	defer func() { _ = history.Close() }()
	if s.check() != nil {
		return errors.New("quota directory changed")
	}
	written, err := history.Write(lines.Bytes())
	if err == nil && written != lines.Len() {
		err = io.ErrShortWrite
	}
	if err != nil {
		return errors.New("quota history append uncertain; no retry performed")
	}
	return history.Sync()
}

// Save publishes the sanitized current snapshot first. A bad history/exclusion
// document is reported separately and never erases historical data or turns a
// successful official fetch into unavailable quota.
func (s *Store) Save(value Snapshot) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.check(); err != nil {
		return err
	}
	raw, err := snapshotBytes(value)
	if err != nil {
		return err
	}
	if err := s.publish(raw); err != nil {
		return err
	}
	return nil
}
func (s *Store) AppendHistory(value Snapshot) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.check(); err != nil {
		return err
	}
	rules, err := s.exclusions()
	if err != nil {
		return err
	}
	rows, err := History(value, rules)
	if err != nil {
		return err
	}
	return s.appendHistory(rows)
}
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	return errors.Join(s.lock.Close(), s.dir.Close())
}
