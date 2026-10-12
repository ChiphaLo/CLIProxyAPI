//go:build linux

package release

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"syscall"
)

type privateListener struct{ net.Listener }

func ownerListener(listener net.Listener) net.Listener { return &privateListener{listener} }

func (l *privateListener) Accept() (net.Conn, error) {
	for {
		conn, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		unix, ok := conn.(*net.UnixConn)
		if !ok {
			_ = conn.Close()
			continue
		}
		var peer *syscall.Ucred
		var errPeer error
		raw, err := unix.SyscallConn()
		if err == nil {
			err = raw.Control(func(fd uintptr) {
				peer, errPeer = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
			})
		}
		if err == nil && errPeer == nil && peer != nil && peer.Uid == uint32(os.Geteuid()) {
			return conn, nil
		}
		_ = conn.Close()
	}
}

func requirePrivate(info os.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) || info.Mode().Perm()&0077 != 0 {
		return fmt.Errorf("release state and artifacts must be private and owned by the controller UID")
	}
	return nil
}

func lockRoot(root string) (*os.File, error) {
	file, err := os.OpenFile(filepath.Join(root, ".controller.lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || requirePrivate(info) != nil {
		_ = file.Close()
		return nil, fmt.Errorf("invalid controller lock")
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("another release controller owns this root")
	}
	return file, nil
}

func commitFile(root string, raw []byte) (renamed bool, err error) {
	file, err := os.CreateTemp(root, ".current-*")
	if err != nil {
		return false, err
	}
	name := file.Name()
	defer func() {
		_ = file.Close()
		_ = os.Remove(name)
	}()
	if _, err := file.Write(raw); err != nil {
		return false, err
	}
	if err := file.Sync(); err != nil {
		return false, err
	}
	if err := file.Close(); err != nil {
		return false, err
	}
	// The temp file and state live on the same filesystem. Linux rename replaces
	// the entire manifest atomically, not a sequence of code/config copies.
	if err := os.Rename(name, filepath.Join(root, "current.json")); err != nil {
		return false, err
	}
	directory, err := os.Open(root)
	if err != nil {
		return true, fmt.Errorf("state replaced but directory durability is unconfirmed: %w", err)
	}
	defer func() { _ = directory.Close() }()
	if err := directory.Sync(); err != nil {
		return true, fmt.Errorf("state replaced but directory fsync failed: %w", err)
	}
	return true, nil
}
