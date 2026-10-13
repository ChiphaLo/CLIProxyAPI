//go:build !linux

package hosting

import "errors"

func ConfirmLegacyExit(*Manifest) error { return errors.New("production handoff requires Linux") }
