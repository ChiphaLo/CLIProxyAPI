//go:build linux

package cutover

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

func confirmCgroupEmpty(unit string) error {
	if !strings.HasSuffix(unit, ".service") {
		return nil
	}
	p := filepath.Join("/sys/fs/cgroup/system.slice", unit, "cgroup.procs")
	raw, err := os.ReadFile(p)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil || len(raw) > 65536 || len(strings.Fields(string(raw))) != 0 {
		return errors.New("service cgroup still contains an unconfirmed writer")
	}
	return nil
}
func (s *System) gatewayDrainCheck(ctx context.Context) error {
	c := exec.CommandContext(ctx, filepath.Join(s.Manifest.ProgramRoot, "cpa-gateway"), "--hosting-manifest", s.HostingPath, "--check-drained")
	c.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: s.Manifest.GatewayUID, Gid: s.Manifest.GatewayGID, Groups: []uint32{s.Manifest.GatewayGID, s.Manifest.ObserveGID}}}
	c.Env = []string{"PATH=/usr/bin:/bin", "HOME=/nonexistent"}
	c.Stdout, c.Stderr = io.Discard, io.Discard
	if c.Run() != nil {
		return errors.New("refresh lease release unconfirmed; legacy restart refused")
	}
	return nil
}
