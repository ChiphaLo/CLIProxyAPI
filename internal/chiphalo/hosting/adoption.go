package hosting

import (
	"errors"
	"path/filepath"
	"regexp"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/configbundle"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/privatefile"
)

type GUIAdoption struct {
	Version     int    `json:"version"`
	ReleaseID   string `json:"release_id"`
	StateSHA    string `json:"state_sha256"`
	MetadataSHA string `json:"metadata_sha256"`
}

var adoptionDigest = regexp.MustCompile(`^[a-f0-9]{64}$`)

// ReadGUIAdoption accepts only a root-authorized, immutable CAS ticket. This
// CLI-only path is not exposed through GUI/helper HTTP or IPC operations.
func ReadGUIAdoption(m *Manifest) (GUIAdoption, error) {
	var t GUIAdoption
	p := filepath.Join(m.StateRoot, "gui-adoption.json")
	if !deploymentOwned(p) || !deploymentParents(p) {
		return t, errors.New("deployment-owned GUI adoption ticket required")
	}
	raw, err := privatefile.Read(p, 4096, true)
	if err != nil || configbundle.DecodeDraft(raw, &t) != nil || t.Version != 1 || t.ReleaseID != m.ReleaseID || !adoptionDigest.MatchString(t.StateSHA) || t.MetadataSHA != "missing" && !adoptionDigest.MatchString(t.MetadataSHA) {
		return t, errors.New("invalid GUI adoption ticket")
	}
	return t, nil
}
