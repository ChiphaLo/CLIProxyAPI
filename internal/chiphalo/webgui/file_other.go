//go:build !linux

package webgui

import "os"

// Non-Linux builds are for compatibility tests, not production durability.
func preserveFileOwner(*os.File, os.FileInfo) error { return nil }
func syncParent(string) error                       { return nil }

func quotaFileIdentity(os.FileInfo) string { return "non-linux" }
