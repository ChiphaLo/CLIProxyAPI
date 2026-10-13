//go:build linux

package webgui

import (
	"errors"
	"os"
	"syscall"
)

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
