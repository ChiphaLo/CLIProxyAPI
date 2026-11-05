//go:build linux

package cutover

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/clientkeys"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/configbundle"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/hosting"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/privatefile"
)

const drainDropin = "99-cpa-go-drain.conf"
const drainBody = "[Service]\nTimeoutStopSec=infinity\nSendSIGKILL=no\nKillMode=control-group\n"

var serviceRoles = map[string]string{legacyUnits[0]: "cpa-gateway", legacyUnits[1]: "cpa-gui", legacyUnits[2]: "cpa-admin-helper", legacyUnits[4]: "cpa-quota-collector", legacyUnits[5]: "guard-timer", legacyUnits[6]: "guard-service"}

func (s *System) run(ctx context.Context, name string, args ...string) ([]byte, error) {
	if s.command != nil {
		return s.command(ctx, name, args...)
	}
	if name != "systemctl" && name != "docker" {
		return nil, errors.New("unapproved deployment command")
	}
	return local(ctx, "/usr/bin/"+name, args...)
}
func restartPolicy(old Snapshot) string {
	if old.ContainerRestart == "on-failure" && old.ContainerRetries > 0 {
		return fmt.Sprintf("on-failure:%d", old.ContainerRetries)
	}
	return old.ContainerRestart
}
func (s *System) unitDigest(ctx context.Context, unit string) (string, error) {
	raw, err := s.run(ctx, "systemctl", "cat", unit)
	if err != nil {
		return "", err
	}
	chunks := []string{}
	current := []string{}
	skip := false
	flush := func() {
		if len(current) > 0 && !skip {
			chunks = append(chunks, strings.TrimRight(strings.Join(current, "\n"), "\n"))
		}
		current = nil
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, "# /") {
			flush()
			p := strings.TrimPrefix(line, "# ")
			skip = p == filepath.Join(s.unitsRoot, unit+".d", dropin) || p == filepath.Join(s.unitsRoot, unit+".d", drainDropin) || p == filepath.Join(s.Root, "selected", unit+".conf") || p == filepath.Join(s.bundleRoot("go"), unit+".conf") || p == filepath.Join(s.bundleRoot("legacy"), unit+".conf")
		}
		current = append(current, line)
	}
	flush()
	return hashBytes([]byte(strings.Join(chunks, "\n\n"))), nil
}
func (s *System) verifyOriginalUnits(ctx context.Context, old Snapshot) error {
	for _, u := range legacyUnits {
		actual, err := s.unitDigest(ctx, u)
		if err != nil || actual != old.UnitDigests[u] {
			return errors.New("legacy service definition changed outside this transaction")
		}
	}
	return nil
}
func exclusiveFile(path string, body []byte, mode os.FileMode) error {
	if i, err := os.Lstat(path); !os.IsNotExist(err) {
		if err != nil || !i.Mode().IsRegular() || !rootOwned(i) {
			return errors.New("service metadata target unsafe")
		}
		old, err := privatefile.Read(path, 65536, false)
		if err != nil || !bytes.Equal(old, body) {
			return errors.New("service metadata external edit conflict")
		}
		return nil
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY|syscall.O_NOFOLLOW, mode)
	if err != nil {
		return err
	}
	_, err = f.Write(body)
	if err == nil {
		err = f.Sync()
	}
	if e := f.Close(); err == nil {
		err = e
	}
	if err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}
func rootOwned(i os.FileInfo) bool {
	st, ok := i.Sys().(*syscall.Stat_t)
	return ok && st.Uid == 0 && i.Mode().Perm()&0022 == 0
}
func (s *System) installDrain() error {
	// This temporary metadata changes only stop behavior, never program or data.
	// It also protects legacy writers whose original units permit SIGKILL.
	for _, u := range legacyUnits {
		if !strings.HasSuffix(u, ".service") {
			continue
		}
		d := filepath.Join(s.unitsRoot, u+".d")
		if err := ownedDirectory(d, 0, 0, 0755); err != nil {
			return err
		}
		if err := exclusiveFile(filepath.Join(d, drainDropin), []byte(drainBody), 0644); err != nil {
			return err
		}
	}
	_, err := s.run(context.Background(), "systemctl", "daemon-reload")
	return err
}
func (s *System) removeDrain() error {
	for _, u := range legacyUnits {
		p := filepath.Join(s.unitsRoot, u+".d", drainDropin)
		if _, err := os.Lstat(p); os.IsNotExist(err) {
			continue
		}
		if err := exclusiveFile(p, []byte(drainBody), 0644); err != nil {
			return err
		}
		if err := os.Remove(p); err != nil {
			return err
		}
		if err := syncDir(filepath.Dir(p)); err != nil {
			return err
		}
	}
	return nil
}
func (s *System) serviceBody(role string) []byte {
	m := s.Manifest
	if strings.HasPrefix(role, "guard-") {
		return []byte("[Unit]\nConditionPathExists=!" + filepath.Join(s.Root, "selected", "go-owner") + "\n")
	}
	user, group, supp, typ, extra := "root", "root", "", "simple", ""
	cap := "CAP_CHOWN CAP_DAC_OVERRIDE CAP_FOWNER"
	if role == "cpa-gateway" {
		user, group, supp = "cpa-gateway", "cpa-gateway", "cpa-observe"
		cap = ""
	}
	if role == "cpa-gui" {
		user, group, supp = "cpa-gui", "cpa-gui", "cpa-observe"
		cap = ""
	}
	write := m.RuntimeRoot + " " + m.StateRoot
	readOnly := ""
	if role == "cpa-gateway" {
		write += " " + filepath.Join(m.BusinessRoot, "auth") + " " + filepath.Join(m.BusinessRoot, "data")
	}
	if role == "cpa-gui" {
		write = filepath.Join(m.GUIRoot, "data")
	}
	if role == "cpa-admin-helper" {
		// Atomic key publication needs a writable parent, not a single-file bind
		// mount. Keep the other business data mounts read-only in this namespace.
		data := filepath.Join(m.BusinessRoot, "data")
		write += " " + m.AdminRoot + " " + filepath.Join(m.BusinessRoot, "config") + " " + data
		protected := []string{"usage.jsonl", "audit.jsonl", "oauth-quota-history.jsonl", "quota-model-exclusions.json", "oauth-accounts.json", ".cpa-quota-collector.lock", "opencode-debug", "opencode-reasoning"}
		for _, name := range protected {
			readOnly += " -" + filepath.Join(data, name)
		}
	}
	if role == "cpa-quota-collector" {
		typ, extra = "oneshot", " --once"
		write = filepath.Join(m.BusinessRoot, "data")
	}
	body := fmt.Sprintf("[Service]\nType=%s\nPIDFile=\nExecStart=\nExecStartPre=\nExecStartPost=\nExecStop=\nExecStopPost=\nExecReload=\nExecStart=%s --hosting-manifest %s%s\nUser=%s\nGroup=%s\nSupplementaryGroups=\nSupplementaryGroups=%s\nEnvironment=\nEnvironmentFile=\nProtectSystem=strict\nReadWritePaths=\nReadWritePaths=%s\nCapabilityBoundingSet=\nCapabilityBoundingSet=%s\nAmbientCapabilities=\nNoNewPrivileges=true\nPrivateTmp=true\nPrivateDevices=true\nProtectHome=true\nProtectKernelTunables=true\nProtectKernelModules=true\nProtectControlGroups=true\nRestrictSUIDSGID=true\nRestrictAddressFamilies=AF_UNIX AF_INET AF_INET6 AF_NETLINK\nUMask=0027\nTimeoutStopSec=infinity\nKillMode=control-group\nSendSIGKILL=no\n", typ, filepath.Join(m.ProgramRoot, role), s.HostingPath, extra, user, group, supp, write, cap)
	if readOnly != "" {
		body += "ReadOnlyPaths=" + strings.TrimSpace(readOnly) + "\n"
	}
	return []byte(body)
}
func (s *System) bundleRoot(kind string) string { return filepath.Join(s.Root, "services-"+kind) }
func (s *System) selectionTarget() (string, error) {
	p := filepath.Join(s.Root, "selected")
	i, err := os.Lstat(p)
	if err != nil || i.Mode()&os.ModeSymlink == 0 {
		return "", errors.New("atomic service selector unavailable")
	}
	st, ok := i.Sys().(*syscall.Stat_t)
	if !ok || st.Uid != 0 {
		return "", errors.New("service selector owner invalid")
	}
	target, err := os.Readlink(p)
	if err != nil || target != s.bundleRoot("go") && target != s.bundleRoot("legacy") {
		return "", errors.New("service selector external edit conflict")
	}
	return target, nil
}
func (s *System) switchSelection(kind string) error {
	if _, err := s.selectionTarget(); err != nil {
		return err
	}
	temp := filepath.Join(s.Root, ".selected-stage")
	if _, err := os.Lstat(temp); !os.IsNotExist(err) {
		target, err := os.Readlink(temp)
		if err != nil || target != s.bundleRoot(kind) {
			return errors.New("selector staging conflict")
		}
		if err := os.Remove(temp); err != nil {
			return err
		}
	}
	if err := os.Symlink(s.bundleRoot(kind), temp); err != nil {
		return err
	}
	if err := os.Rename(temp, filepath.Join(s.Root, "selected")); err != nil {
		return err
	}
	return syncDir(s.Root)
}
func (s *System) verifyServiceLink(unit string, missingOK bool) error {
	p := filepath.Join(s.unitsRoot, unit+".d", dropin)
	i, err := os.Lstat(p)
	if os.IsNotExist(err) && missingOK {
		return nil
	}
	if err != nil || i.Mode()&os.ModeSymlink == 0 {
		return errors.New("service selection link external edit conflict")
	}
	st, ok := i.Sys().(*syscall.Stat_t)
	target, e := os.Readlink(p)
	if !ok || st.Uid != 0 || e != nil || target != filepath.Join(s.Root, "selected", unit+".conf") {
		return errors.New("service selection link external edit conflict")
	}
	return nil
}
func (s *System) SelectGo(ctx context.Context) error {
	for _, kind := range []string{"legacy", "go"} {
		dir := s.bundleRoot(kind)
		if err := ownedDirectory(dir, 0, 0, 0700); err != nil {
			return err
		}
		if kind == "go" {
			if err := exclusiveFile(filepath.Join(dir, "go-owner"), []byte("Go owns OAuth refresh.\n"), 0644); err != nil {
				return err
			}
		}
		for unit, role := range serviceRoles {
			body := []byte("# Legacy program selected; no business-data snapshot is restored.\n")
			if kind == "go" {
				body = s.serviceBody(role)
			}
			if err := exclusiveFile(filepath.Join(dir, unit+".conf"), body, 0644); err != nil {
				return err
			}
		}
		body := []byte("# Legacy runtime bootstrap unchanged.\n")
		if kind == "go" {
			body = []byte(fmt.Sprintf("d %s 0750 root cpa-observe -\nd %s 0700 cpa-gateway cpa-gateway -\nd %s 0750 root cpa-gui -\n", s.Manifest.RuntimeRoot, filepath.Dir(s.Manifest.Control()), filepath.Dir(s.Manifest.Helper())))
		}
		if err := exclusiveFile(filepath.Join(dir, "tmpfiles.conf"), body, 0644); err != nil {
			return err
		}
	}
	pointer := filepath.Join(s.Root, "selected")
	if _, err := os.Lstat(pointer); os.IsNotExist(err) {
		if err := os.Symlink(s.bundleRoot("legacy"), pointer); err != nil {
			return err
		}
		if err := syncDir(s.Root); err != nil {
			return err
		}
	}
	if _, err := s.selectionTarget(); err != nil {
		return err
	}
	for unit := range serviceRoles {
		if err := ownedDirectory(filepath.Join(s.unitsRoot, unit+".d"), 0, 0, 0755); err != nil {
			return err
		}
		p := filepath.Join(s.unitsRoot, unit+".d", dropin)
		if _, err := os.Lstat(p); os.IsNotExist(err) {
			if err := os.Symlink(filepath.Join(s.Root, "selected", unit+".conf"), p); err != nil {
				return err
			}
			if err := syncDir(filepath.Dir(p)); err != nil {
				return err
			}
		}
		if err := s.verifyServiceLink(unit, false); err != nil {
			return err
		}
	}
	if err := s.tmpfilesLink(false); err != nil {
		return err
	}
	// All services are stopped. One rename selects the entire program bundle,
	// and a single daemon-reload consumes that coherent set of definitions.
	if err := s.switchSelection("go"); err != nil {
		return err
	}
	_, err := s.run(ctx, "systemctl", "daemon-reload")
	return err
}
func (s *System) SelectLegacy(ctx context.Context, old Snapshot) error {
	if err := s.verifyBundles(); err != nil {
		return err
	}
	if err := s.tmpfilesLink(true); err != nil {
		return err
	}
	for unit := range serviceRoles {
		if err := s.verifyServiceLink(unit, true); err != nil {
			return err
		}
	}
	if err := s.verifyOriginalUnits(ctx, old); err != nil {
		return err
	}
	if _, err := os.Lstat(filepath.Join(s.Root, "selected")); !os.IsNotExist(err) {
		if err := s.switchSelection("legacy"); err != nil {
			return err
		}
	}
	for unit := range serviceRoles {
		p := filepath.Join(s.unitsRoot, unit+".d", dropin)
		if _, err := os.Lstat(p); os.IsNotExist(err) {
			continue
		}
		if err := s.verifyServiceLink(unit, false); err != nil {
			return err
		}
		if err := os.Remove(p); err != nil {
			return err
		}
		if err := syncDir(filepath.Dir(p)); err != nil {
			return err
		}
	}
	if err := s.removeDrain(); err != nil {
		return err
	}
	p := filepath.Join(s.tmpfilesRoot, "90-cpa-go.conf")
	if _, err := os.Lstat(p); !os.IsNotExist(err) {
		if err := os.Remove(p); err != nil {
			return err
		}
		if err := syncDir(s.tmpfilesRoot); err != nil {
			return err
		}
	}
	_, err := s.run(ctx, "systemctl", "daemon-reload")
	return err
}
func (s *System) tmpfilesLink(checkOnly bool) error {
	p := filepath.Join(s.tmpfilesRoot, "90-cpa-go.conf")
	target := filepath.Join(s.Root, "selected", "tmpfiles.conf")
	i, err := os.Lstat(p)
	if os.IsNotExist(err) {
		if checkOnly {
			return nil
		}
		if err := os.Symlink(target, p); err != nil {
			return err
		}
		return syncDir(s.tmpfilesRoot)
	}
	if err != nil || i.Mode()&os.ModeSymlink == 0 {
		return errors.New("runtime bootstrap metadata conflict")
	}
	st, ok := i.Sys().(*syscall.Stat_t)
	actual, e := os.Readlink(p)
	if !ok || st.Uid != 0 || e != nil || actual != target {
		return errors.New("runtime bootstrap metadata external edit")
	}
	return nil
}
func (s *System) guiCommand(ctx context.Context, args ...string) error {
	c := exec.CommandContext(ctx, filepath.Join(s.Manifest.ProgramRoot, "cpa-gui"), args...)
	c.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: s.Manifest.GUIUID, Gid: uint32(s.Manifest.GUIGID), Groups: []uint32{uint32(s.Manifest.GUIGID), s.Manifest.ObserveGID}}}
	c.Env = []string{"PATH=/usr/bin:/bin", "HOME=/nonexistent"}
	c.Stdout, c.Stderr = io.Discard, io.Discard
	if c.Run() != nil {
		return errors.New("GUI handoff/readiness unconfirmed")
	}
	return nil
}
func (s *System) adoptGUI(ctx context.Context) error {
	data := filepath.Join(s.Manifest.GUIRoot, "data")
	raw, err := privatefile.Read(filepath.Join(data, "state.json"), 4*1024*1024, false)
	if err != nil {
		return err
	}
	metadata := "missing"
	p := filepath.Join(data, ".state-recovery.json")
	if _, err := os.Lstat(p); !os.IsNotExist(err) {
		raw, err := privatefile.Read(p, 4096, true)
		if err != nil {
			return err
		}
		metadata = hashBytes(raw)
	}
	t := hosting.GUIAdoption{Version: 1, ReleaseID: s.Manifest.ReleaseID, StateSHA: hashBytes(raw), MetadataSHA: metadata}
	ticket, _ := json.Marshal(t)
	p = filepath.Join(s.Manifest.StateRoot, "gui-adoption.json")
	if err := publish(p, append(ticket, '\n'), 0640); err != nil {
		return err
	}
	if err := os.Chown(p, 0, int(s.Manifest.ObserveGID)); err != nil {
		return err
	}
	return s.guiCommand(ctx, "--hosting-manifest", s.HostingPath, "--adopt-current-state")
}
func (s *System) checkGUI(ctx context.Context) error {
	return s.guiCommand(ctx, "--hosting-manifest", s.HostingPath, "--check-ready")
}
func (s *System) checkLegacyHTTP(ctx context.Context) error {
	if err := s.ValidateCurrentDocuments(); err != nil {
		return err
	}
	admin, err := clientkeys.ReadLegacyAdminKey(filepath.Join(s.Manifest.BusinessRoot, "gateway.env"))
	if err != nil {
		return err
	}
	client := &http.Client{Transport: &http.Transport{Proxy: nil}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	defer client.CloseIdleConnections()
	for _, probe := range []struct {
		url    string
		auth   bool
		status int
	}{{"http://" + s.Manifest.GatewayListen + "/v1/models", true, 200}, {"http://" + s.Manifest.GUIListen + "/", false, 200}} {
		r, _ := http.NewRequestWithContext(ctx, "GET", probe.url, nil)
		if probe.auth {
			r.Header.Set("Authorization", "Bearer "+admin)
		}
		resp, err := client.Do(r)
		if err != nil {
			return errors.New("legacy local HTTP restore unavailable")
		}
		raw, e := io.ReadAll(io.LimitReader(resp.Body, 2*1024*1024+1))
		_ = resp.Body.Close()
		if e != nil || len(raw) > 2*1024*1024 || resp.StatusCode != probe.status {
			return errors.New("legacy local HTTP restore rejected")
		}
		if probe.auth {
			var value struct {
				Data []json.RawMessage `json:"data"`
			}
			if configbundle.DecodeDraft(raw, &value) != nil || len(value.Data) == 0 {
				return errors.New("legacy model directory restore unavailable")
			}
		}
	}
	return nil
}

// verifyBundles prevents recovery from consuming externally edited program
// metadata. Missing files are allowed only in a not-yet-selected partial bundle.
func (s *System) verifyBundles() error {
	selected := ""
	if _, err := os.Lstat(filepath.Join(s.Root, "selected")); !os.IsNotExist(err) {
		target, e := s.selectionTarget()
		if e != nil {
			return e
		}
		selected = target
	}
	for _, kind := range []string{"legacy", "go"} {
		bodies := map[string][]byte{}
		for unit, role := range serviceRoles {
			body := []byte("# Legacy program selected; no business-data snapshot is restored.\n")
			if kind == "go" {
				body = s.serviceBody(role)
			}
			bodies[unit+".conf"] = body
		}
		body := []byte("# Legacy runtime bootstrap unchanged.\n")
		if kind == "go" {
			body = []byte(fmt.Sprintf("d %s 0750 root cpa-observe -\nd %s 0700 cpa-gateway cpa-gateway -\nd %s 0750 root cpa-gui -\n", s.Manifest.RuntimeRoot, filepath.Dir(s.Manifest.Control()), filepath.Dir(s.Manifest.Helper())))
			bodies["go-owner"] = []byte("Go owns OAuth refresh.\n")
		}
		bodies["tmpfiles.conf"] = body
		for name, want := range bodies {
			p := filepath.Join(s.bundleRoot(kind), name)
			i, err := os.Lstat(p)
			if os.IsNotExist(err) && selected != s.bundleRoot("go") {
				continue
			}
			if err != nil || !i.Mode().IsRegular() || !rootOwned(i) {
				return errors.New("program bundle metadata incomplete or unsafe")
			}
			raw, err := privatefile.Read(p, 65536, false)
			if err != nil || !bytes.Equal(raw, want) {
				return errors.New("program bundle metadata external edit conflict")
			}
		}
	}
	return nil
}
