// Package hosting validates explicit service identity and business-data paths.
// It is separate from the shadow CLI guards and never relocates/restores data.
package hosting

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/netip"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/configbundle"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/privatefile"
)

var releaseID = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,79}$`)

type Manifest struct {
	Version       int               `json:"version"`
	Mode          string            `json:"mode"`
	ReleaseID     string            `json:"release_id"`
	MachineSHA    string            `json:"machine_id_sha256"`
	ProgramRoot   string            `json:"program_root"`
	BusinessRoot  string            `json:"business_root"`
	GUIRoot       string            `json:"gui_root"`
	AdminRoot     string            `json:"admin_root"`
	StateRoot     string            `json:"state_root"`
	RuntimeRoot   string            `json:"runtime_root"`
	GatewayListen string            `json:"gateway_listen"`
	GUIListen     string            `json:"gui_listen"`
	GatewayUID    uint32            `json:"gateway_uid"`
	GatewayGID    uint32            `json:"gateway_gid"`
	GUIUID        uint32            `json:"gui_uid"`
	GUIGID        int               `json:"gui_gid"`
	ObserveGID    uint32            `json:"observe_gid"`
	TrustedPeers  []string          `json:"trusted_peers"`
	Artifacts     map[string]string `json:"artifacts"`
	SupervisorSHA string            `json:"supervisor_sha256,omitempty"`
	sourceSHA     string
}

func (m *Manifest) Production() bool    { return m.Mode == "production" }
func (m *Manifest) Config() string      { return filepath.Join(m.BusinessRoot, "config/cliproxyapi.yaml") }
func (m *Manifest) Control() string     { return filepath.Join(m.RuntimeRoot, "gateway/control.sock") }
func (m *Manifest) Helper() string      { return filepath.Join(m.RuntimeRoot, "helper/control.sock") }
func (m *Manifest) OAuthHealth() string { return filepath.Join(m.StateRoot, "oauth") }
func (m *Manifest) HealthContract() string {
	if m.Production() {
		return "cpa-legacy-v1"
	}
	return "cpa-shadow-validation-v1"
}

func fileHash(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 256*1024*1024 {
		return "", errors.New("hosting artifact unsafe")
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return "", errors.New("hosting artifact changed")
	}
	h := sha256.New()
	if _, err := io.Copy(h, io.LimitReader(f, 256*1024*1024+1)); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
func controlledPath(path string) bool {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == string(filepath.Separator) {
		return false
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil || resolved != path {
		return false
	}
	info, err := os.Lstat(path)
	return err == nil && info.IsDir() && info.Mode().Perm()&0022 == 0
}
func loopback(raw string) bool {
	host, port, err := net.SplitHostPort(raw)
	if err != nil || host != "127.0.0.1" && host != "::1" {
		return false
	}
	n, err := strconv.Atoi(port)
	return err == nil && n >= 1024 && n <= 65535
}

// Load accepts only owner-controlled immutable code manifests. Rehearsals must
// have no non-loopback interfaces and cannot name known production data roots.
// The old shadow flags still refuse production paths; they are not bypassed.
func Load(path, role string) (*Manifest, error) {
	return load(path, role, false)
}

// LoadSupervisor verifies the complete immutable service bundle without
// pretending that the deployment controller is one of its service binaries.
// Only the two new runtime/state roots may be absent before first handoff.
func LoadSupervisor(path string) (*Manifest, error) {
	if os.Geteuid() != 0 {
		return nil, errors.New("deployment owner required")
	}
	return load(path, "cpa-admin-helper", true)
}

func load(path, role string, supervisor bool) (*Manifest, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, errors.New("explicit hosting manifest required")
	}
	raw, err := privatefile.Read(path, 65536, true)
	if err != nil {
		return nil, errors.New("hosting manifest unavailable")
	}
	var m Manifest
	hash := sha256.Sum256(raw)
	m.sourceSHA = hex.EncodeToString(hash[:])
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if configbundle.DecodeDraft(raw, &m) != nil || d.Decode(&m) != nil || m.Version != 1 || !releaseID.MatchString(m.ReleaseID) || m.Mode != "production" && m.Mode != "rehearsal" || m.GatewayUID == 0 || m.GatewayGID == 0 || m.GUIUID == 0 || m.GatewayUID == m.GUIUID || m.GUIGID <= 0 || m.ObserveGID == 0 || !loopback(m.GatewayListen) || !loopback(m.GUIListen) || m.GatewayListen == m.GUIListen {
		return nil, errors.New("hosting manifest invalid")
	}
	gw, e1 := user.Lookup("cpa-gateway")
	gui, e2 := user.Lookup("cpa-gui")
	observe, e3 := user.LookupGroup("cpa-observe")
	if e1 != nil || e2 != nil || e3 != nil || gw.Uid != strconv.FormatUint(uint64(m.GatewayUID), 10) || gw.Gid != strconv.FormatUint(uint64(m.GatewayGID), 10) || gui.Uid != strconv.FormatUint(uint64(m.GUIUID), 10) || gui.Gid != strconv.Itoa(m.GUIGID) || observe.Gid != strconv.FormatUint(uint64(m.ObserveGID), 10) {
		return nil, errors.New("hosting identities differ from the service database")
	}
	if len(m.TrustedPeers) > 8 {
		return nil, errors.New("hosting trusted ingress exceeds limit")
	}
	seen := map[netip.Addr]bool{}
	for _, peer := range m.TrustedPeers {
		a, err := netip.ParseAddr(peer)
		if err != nil || !a.IsLoopback() || seen[a.Unmap()] {
			return nil, errors.New("hosting trusted peers must be distinct exact loopback addresses")
		}
		seen[a.Unmap()] = true
	}
	for _, p := range []string{m.ProgramRoot, m.BusinessRoot, m.GUIRoot, m.AdminRoot, m.StateRoot, m.RuntimeRoot} {
		if !filepath.IsAbs(p) || filepath.Clean(p) != p || p == "/" {
			return nil, errors.New("explicit hosting roots required")
		}
	}
	roots := []string{m.ProgramRoot, m.BusinessRoot, m.StateRoot, m.RuntimeRoot}
	if role == "cpa-gui" || role == "cpa-admin-helper" {
		roots = append(roots, m.GUIRoot)
	}
	if role == "cpa-admin-helper" {
		roots = append(roots, m.AdminRoot)
	}
	for _, p := range roots {
		if supervisor && (p == m.StateRoot || p == m.RuntimeRoot) {
			if _, err := os.Lstat(p); os.IsNotExist(err) && controlledPath(filepath.Dir(p)) && deploymentParents(p) {
				continue
			}
		}
		if !controlledPath(p) {
			return nil, errors.New("hosting roots must exist without writable parents or symlinks")
		}
	}
	if !strings.HasPrefix(path, m.ProgramRoot+string(filepath.Separator)) || !deploymentOwned(path) || !deploymentParents(path) {
		return nil, errors.New("manifest outside verified deployment ownership")
	}
	if m.Production() {
		if m.BusinessRoot != "/opt/unified-ai-gateway" || m.GUIRoot != "/opt/cpa-gui-shadow" || m.AdminRoot != "/var/lib/cpa-admin" || m.StateRoot != "/var/lib/cpa-go" || m.RuntimeRoot != "/run/cpa-go" {
			return nil, errors.New("production business paths must match the established host")
		}
		machine, err := os.ReadFile("/etc/machine-id")
		h := sha256.Sum256([]byte(strings.TrimSpace(string(machine))))
		if err != nil || m.MachineSHA != hex.EncodeToString(h[:]) {
			return nil, errors.New("production manifest belongs to another host")
		}
	} else {
		for _, path := range []string{m.BusinessRoot, m.GUIRoot, m.AdminRoot, m.StateRoot, m.RuntimeRoot} {
			for _, live := range []string{"/opt/unified-ai-gateway", "/opt/cpa-gui-shadow", "/var/lib/cpa-admin", "/var/lib/cpa-go", "/run/cpa-go"} {
				if path == live || strings.HasPrefix(path, live+"/") {
					return nil, errors.New("rehearsal cannot name production paths")
				}
			}
		}
		interfaces, err := net.Interfaces()
		if err != nil {
			return nil, errors.New("rehearsal network unavailable")
		}
		for _, i := range interfaces {
			if i.Flags&net.FlagLoopback == 0 {
				return nil, errors.New("rehearsal must use a network-none namespace")
			}
		}
	}
	actors := map[string]uint32{"cpa-gateway": m.GatewayUID, "cpa-gui": m.GUIUID, "cpa-admin-helper": 0, "cpa-quota-collector": 0}
	uid, known := actors[role]
	if !known || os.Geteuid() != int(uid) {
		return nil, errors.New("hosting actor identity mismatch")
	}
	if len(m.Artifacts) != 4 {
		return nil, errors.New("hosting executable inventory incomplete")
	}
	for name, want := range m.Artifacts {
		if _, ok := actors[name]; !ok {
			return nil, errors.New("unknown hosting artifact")
		}
		p := filepath.Join(m.ProgramRoot, name)
		if !deploymentOwned(p) || !deploymentParents(p) {
			return nil, errors.New("hosting executable owner invalid")
		}
		actual, err := fileHash(p)
		if err != nil || actual != want {
			return nil, errors.New("hosting executable digest mismatch")
		}
	}
	if supervisor {
		p := filepath.Join(m.ProgramRoot, "cpa-cutover")
		if !adoptionDigest.MatchString(m.SupervisorSHA) || !deploymentOwned(p) || !deploymentParents(p) {
			return nil, errors.New("pinned supervisor required")
		}
		actual, err := fileHash(p)
		if err != nil || actual != m.SupervisorSHA {
			return nil, errors.New("supervisor digest mismatch")
		}
		exe, err := os.Executable()
		if err != nil {
			return nil, err
		}
		actual, err = fileHash(exe)
		if err != nil || actual != m.SupervisorSHA {
			return nil, errors.New("running supervisor differs from manifest")
		}
		return &m, nil
	}
	exe, err := os.Executable()
	if err != nil {
		return nil, errors.New("hosting executable unavailable")
	}
	actual, err := fileHash(exe)
	if err != nil || actual != m.Artifacts[role] {
		return nil, errors.New("running executable differs from hosting manifest")
	}
	return &m, nil
}
