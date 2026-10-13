//go:build linux

// Package privatecontrol authenticates both ends of a local management channel.
// It must never be mounted on a public HTTP listener.
package privatecontrol

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
)

func peer(conn net.Conn, uid uint32) bool {
	c, ok := conn.(*net.UnixConn)
	if !ok {
		return false
	}
	raw, err := c.SyscallConn()
	if err != nil {
		return false
	}
	accepted := false
	if raw.Control(func(fd uintptr) {
		credentials, err := unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
		accepted = err == nil && credentials.Uid == uid
	}) != nil {
		return false
	}
	return accepted
}

func directory(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return errors.New("explicit private control path required")
	}
	resolved, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil || resolved != filepath.Dir(path) {
		return errors.New("control parent must not traverse symlinks")
	}
	info, err := os.Lstat(resolved)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return errors.New("owner-private control parent required")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) {
		return errors.New("control parent owner mismatch")
	}
	return nil
}

type listener struct {
	net.Listener
	uid uint32
}

func (l *listener) Accept() (net.Conn, error) {
	for {
		conn, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		if peer(conn, l.uid) {
			return conn, nil
		}
		_ = conn.Close()
	}
}

// Listen never unlinks an existing path, including a stale socket. The release
// supervisor must supply a new private runtime directory for every process.
func Listen(path string, allowedUID uint32) (net.Listener, error) {
	if err := directory(path); err != nil {
		return nil, err
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		return nil, errors.New("control path already exists")
	}
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, errors.New("cannot create private control listener")
	}
	if os.Chmod(path, 0600) != nil {
		_ = l.Close()
		return nil, errors.New("cannot secure private control listener")
	}
	return &listener{Listener: l, uid: allowedUID}, nil
}

// Client checks SO_PEERCRED after connecting; a substituted socket cannot
// impersonate the configured gateway service UID. No bearer secret is sent.
func Client(path string, expectedUID uint32) *http.Client {
	return &http.Client{Transport: &http.Transport{Proxy: nil, MaxIdleConnsPerHost: 2,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return Dial(ctx, path, expectedUID)
		},
	}, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("private control redirects forbidden") }}
}

func Dial(ctx context.Context, path string, expectedUID uint32) (net.Conn, error) {
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", path)
	if err != nil {
		return nil, errors.New("private control unavailable")
	}
	if !peer(conn, expectedUID) {
		_ = conn.Close()
		return nil, errors.New("private control server identity rejected")
	}
	return conn, nil
}
