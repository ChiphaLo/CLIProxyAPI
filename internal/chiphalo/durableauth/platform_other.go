//go:build !linux

package durableauth

import "os"

func durabilitySupported() bool             { return false }
func privateHealthRoot(string) bool         { return true }
func syncDirectory(string) error            { return ErrUnconfirmed }
func confirmFile(string, os.FileInfo) error { return ErrUnconfirmed }
