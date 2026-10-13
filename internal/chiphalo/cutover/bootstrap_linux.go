//go:build linux

package cutover

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
)

func CreateJournalRoot(path string) error {
	if os.Geteuid() != 0 || path != "/var/lib/cpa-cutover" {
		return errors.New("fixed root-owned deployment journal required")
	}
	return ownedDirectory(path, 0, 0, 0700)
}
func safeServicePath(p string) bool {
	if !filepath.IsAbs(p) || filepath.Clean(p) != p {
		return false
	}
	return regexp.MustCompile(`^/[a-zA-Z0-9._/-]+$`).MatchString(p)
}
