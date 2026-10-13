//go:build linux

package cutover

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/hosting"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/privatefile"
)

func (s *System) runtimeOwner(phase string) error {
	raw, err := privatefile.Read(s.HostingPath, 65536, true)
	if err != nil {
		return err
	}
	value := hosting.RuntimeOwner{Version: 1, ReleaseID: s.Manifest.ReleaseID, ManifestSHA: hashBytes(raw), Phase: phase}
	body, err := json.Marshal(value)
	if err != nil {
		return err
	}
	p := filepath.Join(s.Manifest.StateRoot, "runtime-owner.json")
	if err := publish(p, append(body, '\n'), 0640); err != nil {
		return err
	}
	if err := os.Chown(p, 0, int(s.Manifest.ObserveGID)); err != nil {
		return err
	}
	return syncDir(s.Manifest.StateRoot)
}
