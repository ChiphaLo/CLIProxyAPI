//go:build linux

package cutover

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/configbundle"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/hosting"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/privatecontrol"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/privatefile"
)

const legacyContainer = "unified-ai-gateway-cliproxyapi"
const dropin = "90-cpa-go-cutover.conf"

type System struct {
	Manifest     *hosting.Manifest
	HostingPath  string
	Root         string
	unitsRoot    string
	tmpfilesRoot string
	command      func(context.Context, string, ...string) ([]byte, error)
}

// NewSystem is never used by rehearsal tests: it binds commands to the exact
// established host. No caller-controlled unit/container/command strings exist.
func NewSystem(root, path string) (*System, string, error) {
	if os.Geteuid() != 0 {
		return nil, "", errors.New("production supervisor requires deployment owner")
	}
	if root != "/var/lib/cpa-cutover" {
		return nil, "", errors.New("fixed deployment journal root required")
	}
	m, err := hosting.LoadSupervisor(path)
	if err != nil || !m.Production() {
		return nil, "", errors.New("verified production hosting manifest required")
	}
	for _, p := range []string{root, path, m.ProgramRoot, m.BusinessRoot, m.GUIRoot, m.AdminRoot, m.RuntimeRoot, m.StateRoot} {
		if !safeServicePath(p) {
			return nil, "", errors.New("unsafe service metadata path")
		}
	}
	raw, err := privatefile.Read(path, 65536, true)
	if err != nil {
		return nil, "", err
	}
	h := sha256.Sum256(raw)
	return &System{Manifest: m, HostingPath: path, Root: root, unitsRoot: "/etc/systemd/system", tmpfilesRoot: "/etc/tmpfiles.d"}, hex.EncodeToString(h[:]), nil
}
func local(ctx context.Context, name string, args ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, name, args...)
	var output bytes.Buffer
	command.Stdout = &output
	command.Stderr = io.Discard
	if err := command.Run(); err != nil {
		return nil, errors.New("local service operation failed")
	}
	if output.Len() > 65536 {
		return nil, errors.New("local service response exceeds bound")
	}
	return output.Bytes(), nil
}
func (s *System) unitState(ctx context.Context, unit string) (bool, uint32, error) {
	raw, err := s.run(ctx, "systemctl", "show", unit, "--property=ActiveState,MainPID")
	if err != nil {
		return false, 0, err
	}
	state := ""
	pid := uint64(0)
	for _, line := range strings.Split(string(raw), "\n") {
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		if k == "ActiveState" {
			state = v
		}
		if k == "MainPID" {
			pid, err = strconv.ParseUint(v, 10, 32)
			if err != nil {
				return false, 0, errors.New("service PID invalid")
			}
		}
	}
	return state == "active" || state == "activating", uint32(pid), nil
}
func processStart(pid uint32) (uint64, error) {
	raw, err := os.ReadFile("/proc/" + strconv.FormatUint(uint64(pid), 10) + "/stat")
	if err != nil {
		return 0, err
	}
	end := strings.LastIndexByte(string(raw), ')')
	if end < 0 {
		return 0, errors.New("process identity invalid")
	}
	f := strings.Fields(string(raw[end+1:]))
	if len(f) < 20 {
		return 0, errors.New("process identity truncated")
	}
	return strconv.ParseUint(f[19], 10, 64)
}
func (s *System) Capture(ctx context.Context) (Snapshot, error) {
	if _, err := os.Lstat(filepath.Join(s.tmpfilesRoot, "90-cpa-go.conf")); !os.IsNotExist(err) {
		return Snapshot{}, errors.New("runtime bootstrap metadata already exists")
	}
	out := Snapshot{Units: map[string]bool{}, UnitDigests: map[string]string{}, Processes: []Process{}}
	for _, unit := range legacyUnits {
		active, pid, err := s.unitState(ctx, unit)
		if err != nil {
			return out, err
		}
		out.Units[unit] = active
		digest, err := s.unitDigest(ctx, unit)
		if err != nil {
			return out, err
		}
		out.UnitDigests[unit] = digest
		role := ""
		switch unit {
		case "unified-ai-gateway.service":
			role = "gateway"
		case "cpa-gui-shadow.service":
			role = "gui"
		case "cpa-admin-helper.service":
			role = "helper"
		}
		if role != "" {
			if !active || pid == 0 {
				return out, errors.New("initial legacy writer is not active")
			}
			start, err := processStart(pid)
			if err != nil {
				return out, err
			}
			out.Processes = append(out.Processes, Process{Role: role, PID: pid, Start: start})
		}
		if _, err := os.Lstat(filepath.Join(s.unitsRoot, unit+".d", dropin)); !os.IsNotExist(err) {
			return out, errors.New("cutover override already exists; refusing to overwrite")
		}
	}
	raw, err := s.run(ctx, "docker", "inspect", "--format", `{{json .State}}`, legacyContainer)
	if err != nil {
		return out, err
	}
	var state struct {
		Running bool
		Pid     uint32
	}
	if json.Unmarshal(raw, &state) != nil || !state.Running || state.Pid == 0 {
		return out, errors.New("legacy SDK writer unavailable")
	}
	start, err := processStart(state.Pid)
	if err != nil {
		return out, err
	}
	out.Processes = append(out.Processes, Process{Role: "oauth", PID: state.Pid, Start: start})
	out.ContainerRunning = true
	raw, err = s.run(ctx, "docker", "inspect", "--format", `{{.HostConfig.RestartPolicy.Name}}`, legacyContainer)
	if err != nil {
		return out, err
	}
	out.ContainerRestart = strings.TrimSpace(string(raw))
	switch out.ContainerRestart {
	case "no", "always", "unless-stopped", "on-failure":
	default:
		return out, errors.New("legacy container restart policy unsupported")
	}
	raw, err = s.run(ctx, "docker", "inspect", "--format", `{{.HostConfig.RestartPolicy.MaximumRetryCount}}`, legacyContainer)
	if err != nil {
		return out, err
	}
	out.ContainerRetries, err = strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		return out, errors.New("invalid retry policy")
	}
	return out, validateSnapshot(out)
}
func (s *System) StopLegacy(ctx context.Context, old Snapshot) error {
	for _, p := range old.Processes {
		start, err := processStart(p.PID)
		if err != nil || start != p.Start {
			return errors.New("legacy writer identity changed after preparation")
		}
	}
	if err := s.verifyOriginalUnits(ctx, old); err != nil {
		return err
	}
	if err := s.installDrain(); err != nil {
		return err
	}
	// Timer owners must stop before services; none of these legacy writers
	// participate in the new refresh/collector locks. Never force-kill them.
	for _, unit := range []string{legacyUnits[3], legacyUnits[5], legacyUnits[4], legacyUnits[6], legacyUnits[1], legacyUnits[2], legacyUnits[0]} {
		if _, err := s.run(ctx, "systemctl", "stop", unit); err != nil {
			return err
		}
	}
	if _, err := s.run(ctx, "docker", "update", "--restart=no", legacyContainer); err != nil {
		return err
	}
	_, err := s.run(ctx, "docker", "stop", "--time=-1", legacyContainer)
	return err
}
func (s *System) ConfirmLegacyExit(ctx context.Context, old Snapshot) error {
	for _, unit := range legacyUnits {
		active, pid, err := s.unitState(ctx, unit)
		if err != nil || active || pid != 0 {
			return errors.New("legacy service writer exit unconfirmed")
		}
		if err := confirmCgroupEmpty(unit); err != nil {
			return err
		}
	}
	for _, p := range old.Processes {
		start, err := processStart(p.PID)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil || start == p.Start {
			return errors.New("legacy process generation still present")
		}
	}
	raw, err := s.run(ctx, "docker", "inspect", "--format", `{{.State.Running}}`, legacyContainer)
	if err != nil || strings.TrimSpace(string(raw)) != "false" {
		return errors.New("legacy SDK writer exit unconfirmed")
	}
	return nil
}
func syncDir(p string) error {
	f, err := os.Open(p)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	return f.Sync()
}
func publish(path string, raw []byte, mode os.FileMode) error {
	if i, err := os.Lstat(path); !os.IsNotExist(err) && (err != nil || !i.Mode().IsRegular()) {
		return errors.New("cutover target unsafe")
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".cpa-service-*")
	if err != nil {
		return err
	}
	defer func() { _ = f.Close(); _ = os.Remove(f.Name()) }()
	if err := f.Chmod(mode); err != nil {
		return err
	}
	if _, err := f.Write(raw); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}
func ownedDirectory(path string, uid, gid int, mode os.FileMode) error {
	if err := os.Mkdir(path, mode); err != nil && !os.IsExist(err) {
		return err
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil || resolved != path {
		return errors.New("hosting directory unsafe")
	}
	i, err := os.Lstat(path)
	if err != nil || !i.IsDir() {
		return errors.New("hosting directory unavailable")
	}
	st, ok := i.Sys().(*syscall.Stat_t)
	if !ok || (st.Uid != 0 && st.Uid != uint32(uid)) {
		return errors.New("hosting directory belongs to another owner")
	}
	if err := os.Chown(path, uid, gid); err != nil {
		return err
	}
	if err := os.Chmod(path, mode); err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}
func (s *System) PrepareGo(ctx context.Context, old Snapshot) error {
	if err := s.ConfirmLegacyExit(ctx, old); err != nil {
		return err
	}
	if err := s.ValidateCurrentDocuments(); err != nil {
		return err
	}
	m := s.Manifest
	for _, d := range []struct {
		path     string
		uid, gid int
		mode     os.FileMode
	}{
		{m.StateRoot, 0, int(m.ObserveGID), 0750}, {m.OAuthHealth(), int(m.GatewayUID), int(m.GatewayGID), 0700}, {m.RuntimeRoot, 0, int(m.ObserveGID), 0750}, {filepath.Dir(m.Control()), int(m.GatewayUID), int(m.GatewayGID), 0700}, {filepath.Dir(m.Helper()), 0, m.GUIGID, 0750},
	} {
		if err := ownedDirectory(d.path, d.uid, d.gid, d.mode); err != nil {
			return err
		}
	}
	proof := hosting.LegacyExit{Version: 1, ConfirmedAt: time.Now().UTC().Format(time.RFC3339Nano), Processes: []hosting.LegacyProcess{}}
	for _, p := range old.Processes {
		proof.Processes = append(proof.Processes, hosting.LegacyProcess{Role: p.Role, PID: p.PID, StartTicks: p.Start})
	}
	raw, _ := json.Marshal(proof)
	path := filepath.Join(m.StateRoot, "legacy-writer-exit.json")
	if err := publish(path, append(raw, '\n'), 0640); err != nil {
		return err
	}
	if err := os.Chown(path, 0, int(m.ObserveGID)); err != nil {
		return err
	}
	return s.adoptGUI(ctx)
}
func (s *System) StartGo(ctx context.Context) error {
	if err := s.runtimeOwner("starting-go"); err != nil {
		return err
	}
	for _, unit := range []string{legacyUnits[0], legacyUnits[2], legacyUnits[1], legacyUnits[3]} {
		if _, err := s.run(ctx, "systemctl", "start", unit); err != nil {
			return err
		}
	}
	return nil
}
func (s *System) GoReady(ctx context.Context) error {
	client := privatecontrol.Client(s.Manifest.Control(), s.Manifest.GatewayUID)
	defer client.CloseIdleConnections()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		for _, unit := range []string{legacyUnits[0], legacyUnits[1], legacyUnits[2]} {
			active, pid, err := s.unitState(ctx, unit)
			if err != nil || !active || pid == 0 {
				return errors.New("Go service terminated before readiness")
			}
		}
		r, _ := http.NewRequestWithContext(ctx, "GET", "http://cpa-control/__cpa_release/ready", nil)
		resp, err := client.Do(r)
		if err == nil {
			raw, e := io.ReadAll(io.LimitReader(resp.Body, 8193))
			_ = resp.Body.Close()
			var value struct {
				Ready      bool   `json:"ready"`
				Production bool   `json:"production_ready"`
				ID         string `json:"release_id"`
			}
			if e == nil && len(raw) <= 8192 && resp.StatusCode == 200 && json.Unmarshal(raw, &value) == nil && value.Ready && value.Production && value.ID == s.Manifest.ReleaseID {
				if err := s.checkGUI(ctx); err == nil {
					return s.runtimeOwner("committed")
				}
			}
		}
		select {
		case <-ctx.Done():
			return errors.New("Go readiness not confirmed")
		case <-ticker.C:
		}
	}
}
func (s *System) StopGo(ctx context.Context) error {
	if _, err := os.Lstat(s.Manifest.StateRoot); err == nil {
		if err := s.runtimeOwner("draining"); err != nil {
			return err
		}
	}
	if err := s.installDrain(); err != nil {
		return err
	}
	for _, unit := range []string{legacyUnits[3], legacyUnits[4], legacyUnits[1], legacyUnits[2], legacyUnits[0]} {
		if _, err := s.run(ctx, "systemctl", "stop", unit); err != nil {
			return err
		}
	}
	return nil
}
func (s *System) ConfirmGoExit(ctx context.Context) error {
	for _, unit := range []string{legacyUnits[0], legacyUnits[1], legacyUnits[2], legacyUnits[3], legacyUnits[4]} {
		a, p, e := s.unitState(ctx, unit)
		if e != nil || a || p != 0 {
			return errors.New("Go writer drain/exit unconfirmed")
		}
		if err := confirmCgroupEmpty(unit); err != nil {
			return err
		}
	}
	if _, err := os.Lstat(filepath.Join(s.Manifest.OAuthHealth(), ".cpa-auth-save-unconfirmed")); !os.IsNotExist(err) {
		return errors.New("OAuth persistence unconfirmed; legacy restart refused")
	}
	for _, p := range []string{filepath.Join(s.Manifest.BusinessRoot, "config", configbundle.FenceName), filepath.Join(s.Manifest.GUIRoot, "data/.state-write-pending.json")} {
		if _, err := os.Lstat(p); !os.IsNotExist(err) {
			return errors.New("business publication outcome unconfirmed; legacy restart refused")
		}
	}
	if _, err := os.Lstat(filepath.Join(s.Manifest.OAuthHealth(), ".oauth-refresh-owner.lock")); !os.IsNotExist(err) {
		return s.gatewayDrainCheck(ctx)
	}
	return nil
}
func (s *System) StartLegacy(ctx context.Context, old Snapshot) error {
	if _, err := os.Lstat(s.Manifest.StateRoot); err == nil {
		if err := s.runtimeOwner("legacy"); err != nil {
			return err
		}
	}
	if _, err := s.run(ctx, "docker", "update", "--restart="+restartPolicy(old), legacyContainer); err != nil {
		return err
	}
	if old.ContainerRunning {
		if _, err := s.run(ctx, "docker", "start", legacyContainer); err != nil {
			return err
		}
	}
	for _, unit := range []string{legacyUnits[2], legacyUnits[0], legacyUnits[1], legacyUnits[4], legacyUnits[6], legacyUnits[3], legacyUnits[5]} {
		if old.Units[unit] {
			if _, err := s.run(ctx, "systemctl", "start", unit); err != nil {
				return err
			}
		}
	}
	return nil
}
func (s *System) LegacyReady(ctx context.Context, old Snapshot) error {
	if err := s.verifyOriginalUnits(ctx, old); err != nil {
		return err
	}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		for _, unit := range []string{legacyUnits[0], legacyUnits[1], legacyUnits[2]} {
			a, p, e := s.unitState(ctx, unit)
			if e != nil || old.Units[unit] && (!a || p == 0) {
				return errors.New("legacy service restore unconfirmed")
			}
		}
		if err := s.checkLegacyHTTP(ctx); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return errors.New("legacy HTTP/catalog readiness unconfirmed")
		case <-ticker.C:
		}
	}
}

// ValidateCurrentDocuments is read-only, not a restoration routine. It rejects
// damaged business configuration after handoff instead of copying old data back.
func (s *System) ValidateCurrentDocuments() error {
	for _, p := range []string{filepath.Join(s.Manifest.BusinessRoot, "config/oauth-accounts.json"), filepath.Join(s.Manifest.GUIRoot, "data/state.json")} {
		raw, err := privatefile.Read(p, 4*1024*1024, false)
		var value map[string]any
		if err != nil || configbundle.DecodeDraft(raw, &value) != nil || value == nil {
			return errors.New("current business document invalid")
		}
	}
	return nil
}
