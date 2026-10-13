package cutover

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"regexp"
)

var digestPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)
var legacyUnits = []string{"unified-ai-gateway.service", "cpa-gui-shadow.service", "cpa-admin-helper.service", "cpa-oauth-quota.timer", "cpa-oauth-quota.service", "cpa-fix-codex-auth-prefix.timer", "cpa-fix-codex-auth-prefix.service"}

func validateSnapshot(s Snapshot) error {
	if len(s.Units) != len(legacyUnits) || len(s.UnitDigests) != len(legacyUnits) || len(s.Processes) != 4 || !s.ContainerRunning || s.ContainerRetries < 0 {
		return errors.New("legacy writer inventory incomplete")
	}
	for _, u := range legacyUnits {
		if _, ok := s.Units[u]; !ok || !digestPattern.MatchString(s.UnitDigests[u]) {
			return errors.New("legacy unit authority incomplete")
		}
	}
	for _, u := range legacyUnits[:3] {
		if !s.Units[u] {
			return errors.New("legacy core service must be active")
		}
	}
	if s.ContainerRestart != "on-failure" && s.ContainerRetries != 0 {
		return errors.New("restart retry count is inconsistent")
	}
	switch s.ContainerRestart {
	case "no", "always", "unless-stopped", "on-failure":
	default:
		return errors.New("invalid legacy restart policy")
	}
	roles := map[string]bool{"gateway": false, "oauth": false, "gui": false, "helper": false}
	pids := map[uint32]bool{}
	for _, p := range s.Processes {
		seen, known := roles[p.Role]
		if !known || seen || p.PID == 0 || p.Start == 0 || pids[p.PID] {
			return errors.New("legacy writer inventory ambiguous")
		}
		roles[p.Role], pids[p.PID] = true, true
	}
	return nil
}

func hashBytes(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
