// Package appendlog appends legacy JSONL without changing existing permissions,
// restoring snapshots, or continuing to write to an obsolete rotated inode.
package appendlog

import (
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"sync"
)

type Log struct {
	mu     sync.Mutex
	path   string
	file   *os.File
	closed bool
}

func (l *Log) Name() string { return l.path }

func Open(path string) (*Log, error) {
	l := &Log{path: path}
	if err := l.reopen(); err != nil {
		return nil, err
	}
	return l, nil
}

func eligible(info os.FileInfo) bool {
	return info.Mode().IsRegular() && (runtime.GOOS == "windows" || info.Mode().Perm()&0027 == 0)
}

// reopen creates only a missing file, never chmods or truncates an existing one.
// Lstat plus SameFile rejects symlinks and replacement races while opening.
func (l *Log) reopen() error {
	for attempt := 0; attempt < 3; attempt++ {
		info, err := os.Lstat(l.path)
		flags := os.O_RDWR | os.O_APPEND
		if errors.Is(err, os.ErrNotExist) {
			flags |= os.O_CREATE | os.O_EXCL
		} else if err != nil || !eligible(info) {
			return fmt.Errorf("append destination must be a protected regular file")
		}
		file, err := os.OpenFile(l.path, flags, 0600)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("cannot open append destination: %w", err)
		}
		opened, err := file.Stat()
		current, errCurrent := os.Lstat(l.path)
		if err != nil || errCurrent != nil || !eligible(opened) || !eligible(current) ||
			!os.SameFile(opened, current) || info != nil && !os.SameFile(info, opened) {
			_ = file.Close()
			return fmt.Errorf("append destination changed while opening")
		}
		if opened.Size() > 0 {
			var tail [1]byte
			if _, err := file.ReadAt(tail[:], opened.Size()-1); err != nil || tail[0] != '\n' {
				_ = file.Close()
				return fmt.Errorf("append destination has an incomplete historical line; refusing to alter it")
			}
		}
		if l.file != nil {
			if err := l.file.Sync(); err != nil {
				_ = file.Close()
				return err
			}
			if err := l.file.Close(); err != nil {
				_ = file.Close()
				return err
			}
		}
		l.file = file
		return nil
	}
	return fmt.Errorf("append destination repeatedly changed while opening")
}

// Append issues one O_APPEND write per complete JSON line. It does not retry a
// partial write: replaying one could duplicate a receipt or corrupt old data.
func (l *Log) Append(line []byte) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return os.ErrClosed
	}
	if len(line) == 0 || line[len(line)-1] != '\n' {
		return fmt.Errorf("append requires a complete line")
	}
	current, err := os.Lstat(l.path)
	if err != nil || !eligible(current) {
		return fmt.Errorf("append destination missing or unsafe")
	}
	opened, err := l.file.Stat()
	if err != nil {
		return err
	}
	if !os.SameFile(current, opened) {
		if err := l.reopen(); err != nil {
			return err
		}
	}
	n, err := l.file.Write(line)
	if err == nil && n != len(line) {
		return io.ErrShortWrite
	}
	return err
}

func (l *Log) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil
	}
	l.closed = true
	return errors.Join(l.file.Sync(), l.file.Close())
}
