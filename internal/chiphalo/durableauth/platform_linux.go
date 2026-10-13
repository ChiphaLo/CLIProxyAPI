//go:build linux

package durableauth

import (
	"os"
	"path/filepath"
	"syscall"
)

func durabilitySupported() bool { return true }

func privateHealthRoot(path string) bool {
	info, err := os.Lstat(path)
	if err != nil {
		return false
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && st.Uid == uint32(os.Geteuid()) && info.Mode().Perm()&0077 == 0
}

func syncDirectory(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	return f.Sync()
}

func confirmFile(path string, before os.FileInfo) error {
	file, err := os.OpenFile(path, os.O_RDWR|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	opened, err := file.Stat()
	named, errNamed := os.Lstat(path)
	if err != nil || errNamed != nil || !opened.Mode().IsRegular() || !os.SameFile(opened, named) {
		return ErrUnsafePath
	}
	if before != nil {
		old, okOld := before.Sys().(*syscall.Stat_t)
		current, okCurrent := opened.Sys().(*syscall.Stat_t)
		if !okOld || !okCurrent {
			return ErrUnsafePath
		}
		if old.Uid != current.Uid || old.Gid != current.Gid {
			if err := file.Chown(int(old.Uid), int(old.Gid)); err != nil {
				return err
			}
		}
		if err := file.Chmod(before.Mode().Perm()); err != nil {
			return err
		}
	}
	if err := file.Sync(); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(path))
}
