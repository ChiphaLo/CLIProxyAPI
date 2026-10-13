//go:build !linux

package cutover

import (
	"errors"
	"os"
)

func lockRoot(string) (*os.File, error) { return nil, errors.New("cutover ownership requires Linux") }
func commit(string, []byte) (bool, error) {
	return false, errors.New("cutover durability requires Linux")
}
