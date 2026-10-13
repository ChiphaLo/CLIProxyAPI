//go:build !linux

package cutover

import (
	"errors"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/hosting"
)

type System struct{ Manifest *hosting.Manifest }

func NewSystem(root, path string) (*System, string, error) {
	return nil, "", errors.New("cutover requires Linux")
}
func CreateJournalRoot(string) error { return errors.New("cutover requires Linux") }
