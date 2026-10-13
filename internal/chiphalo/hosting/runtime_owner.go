package hosting

import (
	"errors"
	"path/filepath"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/configbundle"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/privatefile"
)

type RuntimeOwner struct {
	Version     int    `json:"version"`
	ReleaseID   string `json:"release_id"`
	ManifestSHA string `json:"manifest_sha256"`
	Phase       string `json:"phase"`
}

// ConfirmRuntimeOwner prevents an enabled Go unit from re-entering ownership
// during rollback/reboot or after legacy has been restored. PID exit proof alone
// cannot identify a newly restarted, non-cooperative legacy writer generation.
func ConfirmRuntimeOwner(m *Manifest) error {
	if !m.Production() {
		return nil
	}
	p := filepath.Join(m.StateRoot, "runtime-owner.json")
	if !deploymentOwned(p) || !deploymentParents(p) {
		return errors.New("deployment-owned runtime handoff required")
	}
	raw, err := privatefile.Read(p, 4096, true)
	var owner RuntimeOwner
	if err != nil || configbundle.DecodeDraft(raw, &owner) != nil {
		return errors.New("Go runtime ownership is not selected")
	}
	return validateRuntimeOwner(m, owner)
}
func validateRuntimeOwner(m *Manifest, owner RuntimeOwner) error {
	if owner.Version != 1 || owner.ReleaseID != m.ReleaseID || owner.ManifestSHA != m.sourceSHA || owner.Phase != "starting-go" && owner.Phase != "committed" {
		return errors.New("Go runtime ownership is not selected")
	}
	return nil
}
