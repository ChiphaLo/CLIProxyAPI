//go:build !linux

package adminhelper

import (
	"context"
	"errors"
	"net"
	"os"
)

func peerUID(*net.UnixConn) (uint32, error)       { return 0, errors.New("Linux peer credentials required") }
func lock(*os.File) error                         { return errors.New("Linux file locking required") }
func lockContext(context.Context, *os.File) error { return errors.New("Linux file locking required") }
func unlock(*os.File)                             {}
func preserveOwner(*os.File, os.FileInfo) error   { return nil }
