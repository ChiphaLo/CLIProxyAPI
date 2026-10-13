//go:build linux

package adminhelper

import (
	"context"
	"errors"
	"net"
	"os"
	"syscall"
	"time"
)

func peerUID(conn *net.UnixConn) (uint32, error) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return 0, err
	}
	var peer *syscall.Ucred
	var errPeer error
	err = raw.Control(func(fd uintptr) {
		peer, errPeer = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	})
	if err != nil || errPeer != nil || peer == nil {
		return 0, errors.New("cannot authenticate peer")
	}
	return peer.Uid, nil
}

func lock(file *os.File) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return lockContext(ctx, file)
}
func lockContext(ctx context.Context, file *os.File) error {
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return errors.New("control lock acquisition canceled")
		}
		err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return nil
		}
		if err != syscall.EWOULDBLOCK && err != syscall.EAGAIN {
			return err
		}
		select {
		case <-ctx.Done():
			return errors.New("control lock acquisition canceled")
		case <-ticker.C:
		}
	}
}
func unlock(file *os.File) { _ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN) }

func preserveOwner(file *os.File, info os.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return errors.New("cannot preserve owner")
	}
	if uint32(os.Geteuid()) == stat.Uid && uint32(os.Getegid()) == stat.Gid {
		return nil
	}
	return file.Chown(int(stat.Uid), int(stat.Gid))
}
