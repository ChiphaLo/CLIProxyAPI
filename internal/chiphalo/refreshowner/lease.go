// Package refreshowner provides a process-lifetime OAuth refresh ownership
// lease. It never reads, restores or rewrites credential business files.
package refreshowner

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

const lockName = ".oauth-refresh-owner.lock"

type Lease struct {
	mu                 sync.Mutex
	root               string
	file               *os.File
	rootInfo, fileInfo os.FileInfo
	active             int
	draining, closed   bool
	done               chan struct{}
	closeErr           error
}

// Begin is suitable for Manager.SetCredentialRefreshGuard. A lease must be
// acquired BEFORE loading shared auth files. It cannot be resumed after drain:
// the next owner must restart and load the latest persisted token generation.
func (l *Lease) Begin(ctx context.Context) (func(), error) {
	if l == nil {
		return nil, fmt.Errorf("OAuth refresh owner is unavailable")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.draining || l.closed || ctx.Err() != nil {
		return nil, fmt.Errorf("OAuth refresh owner is draining or unavailable")
	}
	root, errRoot := os.Lstat(l.root)
	info, errFile := os.Lstat(filepath.Join(l.root, lockName))
	if errRoot != nil || errFile != nil || !os.SameFile(root, l.rootInfo) || !os.SameFile(info, l.fileInfo) ||
		!root.IsDir() || !info.Mode().IsRegular() || !isPrivate(root) || !isPrivate(info) {
		return nil, fmt.Errorf("OAuth refresh ownership path changed or is unsafe")
	}
	l.active++
	var once sync.Once
	return func() {
		once.Do(func() {
			l.mu.Lock()
			defer l.mu.Unlock()
			l.active--
			if l.draining && l.active == 0 {
				close(l.done)
			}
		})
	}, nil
}

// Close first rejects new refreshes, then waits for all refresh-and-persist
// transactions before releasing the OS lock. A canceled wait retains ownership
// and remains fail-closed; callers may retry Close but must not force handoff.
// The lock file is never removed, avoiding split-lock inode races.
func (l *Lease) Close(ctx context.Context) error {
	if l == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	l.mu.Lock()
	if l.closed {
		err := l.closeErr
		l.mu.Unlock()
		return err
	}
	if !l.draining {
		l.draining = true
		if l.active == 0 {
			close(l.done)
		}
	}
	done := l.done
	l.mu.Unlock()
	select {
	case <-done:
	case <-ctx.Done():
		return ctx.Err()
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.closed {
		l.closed = true
		l.closeErr = l.file.Close()
	}
	return l.closeErr
}
