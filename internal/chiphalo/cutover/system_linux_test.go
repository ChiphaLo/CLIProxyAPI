//go:build linux

package cutover

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/hosting"
)

func systemFixture(t *testing.T) (*System, Snapshot, *int) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("service metadata tests require isolated deployment owner")
	}
	root := t.TempDir()
	_ = os.Chmod(root, 0700)
	units := filepath.Join(root, "units")
	tmpfiles := filepath.Join(root, "tmpfiles")
	_ = os.Mkdir(units, 0755)
	_ = os.Mkdir(tmpfiles, 0755)
	reloads := 0
	s := &System{Root: root, unitsRoot: units, tmpfilesRoot: tmpfiles, HostingPath: "/program/hosting.json", Manifest: &hosting.Manifest{ProgramRoot: "/program", StateRoot: "/copy/state", RuntimeRoot: "/run/copy", BusinessRoot: "/copy/business", GUIRoot: "/copy/gui", AdminRoot: "/copy/admin"}}
	for _, u := range legacyUnits {
		_ = os.WriteFile(filepath.Join(units, u), []byte("[Unit]\nDescription=original\n[Service]\nExecStart=/original\n"), 0644)
	}
	s.command = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name != "systemctl" {
			return nil, errors.New("unexpected host command")
		}
		switch args[0] {
		case "cat":
			u := args[1]
			raw, _ := os.ReadFile(filepath.Join(units, u))
			out := "# " + filepath.Join(units, u) + "\n" + string(raw)
			for _, d := range []string{dropin, drainDropin} {
				p := filepath.Join(units, u+".d", d)
				if raw, err := os.ReadFile(p); err == nil {
					out += "\n# " + p + "\n" + string(raw)
				}
			}
			return []byte(out), nil
		case "daemon-reload":
			reloads++
			return nil, nil
		}
		return nil, errors.New("unapproved fixture command")
	}
	old := snapshotFixture()
	for _, u := range legacyUnits {
		h, err := s.unitDigest(context.Background(), u)
		if err != nil {
			t.Fatal(err)
		}
		old.UnitDigests[u] = h
	}
	return s, old, &reloads
}
func TestSystemAtomicSelectionAndExactLegacyRecovery(t *testing.T) {
	s, old, reloads := systemFixture(t)
	if err := s.installDrain(); err != nil {
		t.Fatal(err)
	}
	if err := s.verifyOriginalUnits(context.Background(), old); err != nil {
		t.Fatal("drain changed legacy authority", err)
	}
	if err := s.SelectGo(context.Background()); err != nil {
		t.Fatal(err)
	}
	if target, err := s.selectionTarget(); err != nil || target != s.bundleRoot("go") {
		t.Fatal("entire bundle not selected")
	}
	for u, role := range serviceRoles {
		raw, err := os.ReadFile(filepath.Join(s.unitsRoot, u+".d", dropin))
		if err != nil || string(raw) != string(s.serviceBody(role)) {
			t.Fatal("mixed service generation")
		}
	}
	if err := s.verifyOriginalUnits(context.Background(), old); err != nil {
		t.Fatal("selector changed original unit identity", err)
	}
	if err := s.SelectLegacy(context.Background(), old); err != nil {
		t.Fatal(err)
	}
	for u := range serviceRoles {
		if _, err := os.Lstat(filepath.Join(s.unitsRoot, u+".d", dropin)); !os.IsNotExist(err) {
			t.Fatal("cutover service override left behind")
		}
	}
	if *reloads != 3 {
		t.Fatal("intermediate bundle was reloaded", *reloads)
	}
	if err := s.verifyOriginalUnits(context.Background(), old); err != nil {
		t.Fatal(err)
	}
}
func TestSystemSelectionExternalEditsFenceRollback(t *testing.T) {
	for _, kind := range []string{"unit", "link", "pointer", "drain", "tmpfiles", "bundle"} {
		t.Run(kind, func(t *testing.T) {
			s, old, _ := systemFixture(t)
			_ = s.installDrain()
			if err := s.SelectGo(context.Background()); err != nil {
				t.Fatal(err)
			}
			var p string
			switch kind {
			case "bundle":
				p = filepath.Join(s.bundleRoot("go"), legacyUnits[0]+".conf")
			case "unit":
				p = filepath.Join(s.unitsRoot, legacyUnits[0])
			case "link":
				p = filepath.Join(s.unitsRoot, legacyUnits[0]+".d", dropin)
				_ = os.Remove(p)
			case "pointer":
				p = filepath.Join(s.Root, "selected")
				_ = os.Remove(p)
			case "drain":
				p = filepath.Join(s.unitsRoot, legacyUnits[0]+".d", drainDropin)
			case "tmpfiles":
				p = filepath.Join(s.tmpfilesRoot, "90-cpa-go.conf")
				_ = os.Remove(p)
			}
			_ = os.WriteFile(p, []byte("external edit"), 0644)
			if err := s.SelectLegacy(context.Background(), old); err == nil {
				t.Fatal("external control edit ignored")
			}
			raw, _ := os.ReadFile(p)
			if string(raw) != "external edit" {
				t.Fatal("external control bytes overwritten")
			}
		})
	}
}
func TestSystemServiceSandboxAndRebootOwnership(t *testing.T) {
	s, _, _ := systemFixture(t)
	for _, role := range []string{"cpa-gateway", "cpa-gui", "cpa-admin-helper", "cpa-quota-collector"} {
		raw := string(s.serviceBody(role))
		for _, want := range []string{"ProtectSystem=strict", "NoNewPrivileges=true", "SendSIGKILL=no", "TimeoutStopSec=infinity", "PrivateDevices=true", "ProtectHome=true", "RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6 AF_NETLINK"} {
			if !strings.Contains(raw, want) {
				t.Fatal("missing service guard", role, want)
			}
		}
		if (role == "cpa-gateway" || role == "cpa-gui") && strings.Contains(raw, "User=root") {
			t.Fatal("public service elevated")
		}
	}
	if !strings.Contains(string(s.serviceBody("guard-timer")), "ConditionPathExists=!"+filepath.Join(s.Root, "selected", "go-owner")) {
		t.Fatal("reboot could restart legacy OAuth writer")
	}
	if safeServicePath("/opt/a%u") || safeServicePath("/opt/a b") || safeServicePath("/opt/a\nb") || !safeServicePath("/opt/cpa-go-releases/r1/hosting.json") {
		t.Fatal("unit path injection accepted")
	}
}
func TestSystemPartialMetadataRecovery(t *testing.T) {
	s, old, _ := systemFixture(t)
	_ = s.installDrain()
	if err := s.SelectLegacy(context.Background(), old); err != nil {
		t.Fatal("failure before selector creation not recoverable", err)
	}
	if err := s.SelectGo(context.Background()); err != nil {
		t.Fatal(err)
	}
	u := legacyUnits[1]
	if err := os.Remove(filepath.Join(s.unitsRoot, u+".d", dropin)); err != nil {
		t.Fatal(err)
	}
	if err := s.SelectLegacy(context.Background(), old); err != nil {
		t.Fatal("partial published link set not recoverable", err)
	}
}
