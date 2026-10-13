//go:build linux

package refreshowner

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

func isPrivate(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Geteuid()) && info.Mode().Perm()&0077 == 0
}

// Open acquires one nonblocking exclusive lock for the shared OAuth store.
// All participating releases must use the same private, stable lease root.
// Legacy runtimes that do not acquire this lock must be stopped and verified
// exited before starting a new refresh owner; flock alone cannot fence them.
func Open(root string) (*Lease, error) {
	if !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return nil, fmt.Errorf("OAuth ownership root must be an explicit absolute directory")
	}
	resolved, err := filepath.EvalSymlinks(root)
	info, errInfo := os.Lstat(root)
	if err != nil || errInfo != nil || resolved != root || !info.IsDir() || !isPrivate(info) {
		return nil, fmt.Errorf("OAuth ownership root must be private, owned and without symlinks")
	}
	path := filepath.Join(root, lockName)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW|syscall.O_CLOEXEC|syscall.O_NONBLOCK, 0600)
	if err != nil {
		return nil, fmt.Errorf("cannot open OAuth ownership lock")
	}
	opened, errOpened := file.Stat()
	named, errNamed := os.Lstat(path)
	if errOpened != nil || errNamed != nil || !opened.Mode().IsRegular() || !isPrivate(opened) || !os.SameFile(opened, named) {
		_ = file.Close()
		return nil, fmt.Errorf("OAuth ownership lock is unsafe")
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("another runtime owns OAuth credential refresh")
	}
	current, errCurrent := os.Lstat(root)
	currentFile, errCurrentFile := os.Lstat(path)
	if errCurrent != nil || errCurrentFile != nil || !os.SameFile(info, current) || !os.SameFile(opened, currentFile) {
		_ = file.Close()
		return nil, fmt.Errorf("OAuth ownership path changed while acquiring lock")
	}
	return &Lease{root: root, file: file, rootInfo: info, fileInfo: opened, done: make(chan struct{})}, nil
}
