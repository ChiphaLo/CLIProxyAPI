//go:build !linux

package refreshowner

import (
	"fmt"
	"os"
)

func isPrivate(os.FileInfo) bool { return false }
func Open(string) (*Lease, error) {
	return nil, fmt.Errorf("OAuth refresh ownership lease is supported only on Linux")
}
