//go:build !linux

package release

import (
	"fmt"
	"net"
	"os"
)

func ownerListener(listener net.Listener) net.Listener { return listener }

func requirePrivate(_ os.FileInfo) error { return nil }
func lockRoot(_ string) (*os.File, error) {
	return nil, fmt.Errorf("atomic release controller is supported only on Linux")
}
func commitFile(_ string, _ []byte) (bool, error) {
	return false, fmt.Errorf("atomic release controller is supported only on Linux")
}
