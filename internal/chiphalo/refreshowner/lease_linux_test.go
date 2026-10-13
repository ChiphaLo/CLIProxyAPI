//go:build linux

package refreshowner

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func leaseRoot(t *testing.T) string {
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestLeaseExclusiveAndNeverUnlinks(t *testing.T) {
	root := leaseRoot(t)
	lease, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lease.Close(context.Background()) }()
	path := filepath.Join(root, lockName)
	before, err := os.Stat(path)
	if err != nil || before.Mode().Perm() != 0600 {
		t.Fatalf("bad lock mode: %v", err)
	}
	if other, err := Open(root); err == nil {
		_ = other.Close(context.Background())
		t.Fatal("second owner acquired same lock")
	}
	if err := lease.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(path)
	if err != nil || !os.SameFile(before, after) {
		t.Fatal("handoff unlinked or replaced the lock inode")
	}
	other, err := Open(root)
	if err != nil {
		t.Fatal("new startup could not acquire drained owner", err)
	}
	defer func() { _ = other.Close(context.Background()) }()
	if _, err := lease.Begin(context.Background()); err == nil {
		t.Fatal("old owner resumed with stale in-memory tokens")
	}
}

func TestLeaseCanceledDrainRetainsOwnership(t *testing.T) {
	root := leaseRoot(t)
	lease, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	end, err := lease.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { end(); _ = lease.Close(context.Background()) }()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := lease.Close(ctx); err != context.Canceled {
		t.Fatalf("canceled drain: %v", err)
	}
	if _, err := lease.Begin(context.Background()); err == nil {
		t.Fatal("new refresh admitted during drain")
	}
	if other, err := Open(root); err == nil {
		_ = other.Close(context.Background())
		t.Fatal("handoff released ownership before refresh persistence completed")
	}
	end()
	end()
	if err := lease.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	other, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	_ = other.Close(context.Background())
}

func TestLeaseRejectsUnsafePathsAndReplacedLock(t *testing.T) {
	root := leaseRoot(t)
	if _, err := Open("relative"); err == nil {
		t.Fatal("relative root accepted")
	}
	if err := os.Chmod(root, 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(root); err == nil {
		t.Fatal("public root accepted")
	}
	_ = os.Chmod(root, 0700)
	path := filepath.Join(root, lockName)
	if err := os.Symlink(filepath.Join(root, "target"), path); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(root); err == nil {
		t.Fatal("symlink lock accepted")
	}
	_ = os.Remove(path)
	if err := os.WriteFile(path, []byte("existing"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(root); err == nil {
		t.Fatal("unsafe existing lock accepted")
	}
	bytes, _ := os.ReadFile(path)
	info, _ := os.Stat(path)
	if string(bytes) != "existing" || info.Mode().Perm() != 0644 {
		t.Fatal("existing lock bytes or permissions modified")
	}
	_ = os.Chmod(path, 0600)
	lease, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lease.Close(context.Background()) }()
	if err := os.Rename(path, path+".moved"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("replacement"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := lease.Begin(context.Background()); err == nil {
		t.Fatal("split-lock inode admitted a refresh")
	}
}

func TestLeaseCrossProcessCrashRecovery(t *testing.T) {
	if root := os.Getenv("CPA_REFRESH_OWNER_CHILD_ROOT"); root != "" {
		lease, err := Open(root)
		if err != nil {
			os.Exit(19)
		}
		_ = lease
		// Deliberately exit without closing; the kernel must release ownership.
		os.Exit(0)
	}
	root := leaseRoot(t)
	lease, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lease.Close(context.Background()) }()
	child := func() error {
		cmd := exec.Command(os.Args[0], "-test.run=^TestLeaseCrossProcessCrashRecovery$")
		cmd.Env = append(os.Environ(), "CPA_REFRESH_OWNER_CHILD_ROOT="+root)
		return cmd.Run()
	}
	if err := child(); err == nil {
		t.Fatal("separate process acquired a live owner's refresh lock")
	}
	if err := lease.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := child(); err != nil {
		t.Fatal("new process failed after owner drain", err)
	}
	next, err := Open(root)
	if err != nil {
		t.Fatal("crashed owner's lock was not released by kernel", err)
	}
	_ = next.Close(context.Background())
}
