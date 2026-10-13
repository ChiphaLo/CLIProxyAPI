//go:build linux

package cutover

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

func private(info os.FileInfo) bool {
	s, ok := info.Sys().(*syscall.Stat_t)
	return ok && s.Uid == uint32(os.Geteuid()) && info.Mode().Perm()&0077 == 0
}
func lockRoot(root string) (*os.File, error) {
	i, err := os.Lstat(root)
	if err != nil || !i.IsDir() || !private(i) {
		return nil, errors.New("private owned cutover root required")
	}
	f, err := os.OpenFile(filepath.Join(root, ".cutover.lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	i, err = f.Stat()
	if err != nil || !i.Mode().IsRegular() || !private(i) {
		_ = f.Close()
		return nil, errors.New("cutover lock unsafe")
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, errors.New("another cutover controller owns this root")
	}
	return f, nil
}
func commit(root string, raw []byte) (bool, error) {
	p := filepath.Join(root, "cutover.json")
	if i, err := os.Lstat(p); !os.IsNotExist(err) && (err != nil || !i.Mode().IsRegular() || !private(i)) {
		return false, errors.New("cutover state target unsafe")
	}
	f, err := os.CreateTemp(root, ".cutover-stage-*")
	if err != nil {
		return false, err
	}
	defer func() { _ = f.Close(); _ = os.Remove(f.Name()) }()
	if _, err := f.Write(raw); err != nil {
		return false, err
	}
	if err := f.Sync(); err != nil {
		return false, err
	}
	if err := f.Close(); err != nil {
		return false, err
	}
	if err := os.Rename(f.Name(), p); err != nil {
		return false, err
	}
	dir, err := os.Open(root)
	if err != nil {
		return true, err
	}
	defer func() { _ = dir.Close() }()
	return true, dir.Sync()
}
