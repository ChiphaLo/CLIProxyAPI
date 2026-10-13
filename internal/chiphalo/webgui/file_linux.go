//go:build linux

package webgui

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

func quotaFileIdentity(info os.FileInfo) string {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return "unknown"
	}
	return fmt.Sprintf("%d:%d", stat.Dev, stat.Ino)
}

func preserveFileOwner(file *os.File, info os.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return errors.New("GUI file ownership unavailable")
	}
	return file.Chown(int(stat.Uid), int(stat.Gid))
}
func syncParent(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	return dir.Sync()
}
