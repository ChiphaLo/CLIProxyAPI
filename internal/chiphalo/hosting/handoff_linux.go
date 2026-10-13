//go:build linux

package hosting

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/configbundle"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/privatefile"
)

type LegacyProcess struct {
	Role       string `json:"role"`
	PID        uint32 `json:"pid"`
	StartTicks uint64 `json:"start_ticks"`
}
type LegacyExit struct {
	Version     int             `json:"version"`
	Processes   []LegacyProcess `json:"processes"`
	ConfirmedAt string          `json:"confirmed_at"`
}

// ConfirmLegacyExit refuses cooperative-lease assumptions about the original
// Python/7.x processes. A root-controlled handoff proof must name all old writer
// process identities, and those exact PID/start generations must have exited.
// It never stops a process or writes a proof. Rehearsals do not acquire refresh.
func ConfirmLegacyExit(m *Manifest) error {
	if !m.Production() {
		return nil
	}
	path := filepath.Join(m.StateRoot, "legacy-writer-exit.json")
	if !deploymentOwned(path) || !deploymentParents(path) {
		return errors.New("root-owned legacy writer handoff proof required")
	}
	raw, err := privatefile.Read(path, 8192, true)
	var proof LegacyExit
	if err != nil || configbundle.DecodeDraft(raw, &proof) != nil || proof.Version != 1 || len(proof.Processes) != 4 {
		return errors.New("legacy writer proof invalid")
	}
	return validateLegacyExit(proof, func(pid uint32) ([]byte, error) {
		return os.ReadFile("/proc/" + strconv.FormatUint(uint64(pid), 10) + "/stat")
	})
}
func validateLegacyExit(proof LegacyExit, readProcess func(uint32) ([]byte, error)) error {
	if proof.Version != 1 || len(proof.Processes) != 4 || readProcess == nil {
		return errors.New("legacy writer proof invalid")
	}
	if _, err := time.Parse(time.RFC3339Nano, proof.ConfirmedAt); err != nil {
		return errors.New("legacy writer proof timestamp invalid")
	}
	roles := map[string]bool{"gateway": false, "oauth": false, "gui": false, "helper": false}
	pids := map[uint32]bool{}
	for _, p := range proof.Processes {
		seen, known := roles[p.Role]
		if !known || seen || p.PID == 0 || p.StartTicks == 0 || pids[p.PID] {
			return errors.New("legacy writer proof incomplete")
		}
		roles[p.Role] = true
		pids[p.PID] = true
		raw, err := readProcess(p.PID)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return errors.New("legacy writer liveness unconfirmed")
		}
		end := strings.LastIndexByte(string(raw), ')')
		if end < 0 {
			return errors.New("legacy writer liveness invalid")
		}
		fields := strings.Fields(string(raw[end+1:]))
		if len(fields) < 20 {
			return errors.New("legacy writer liveness invalid")
		}
		start, err := strconv.ParseUint(fields[19], 10, 64)
		if err != nil || start == p.StartTicks {
			return errors.New("legacy writer is still present; ownership handoff refused")
		}
	}
	return nil
}
