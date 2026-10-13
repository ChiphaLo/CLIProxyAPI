//go:build linux

package webgui

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStateCrashWorker(t *testing.T) {
	root := os.Getenv("CPA_STATE_CRASH_ROOT")
	if root == "" {
		return
	}
	s, err := New(Options{Data: root, Usage: filepath.Join(root, "usage.jsonl"), Quota: filepath.Join(root, "quota.json"), Exposed: filepath.Join(root, "exposed.json"), Origin: "https://cpa.chipha.de", Helper: fixtureHelper{}})
	if err != nil {
		t.Fatal(err)
	}
	state, err := s.state()
	if err != nil {
		t.Fatal(err)
	}
	state["custom"] = "authorized-child-publication"
	s.stateFault = func(phase string) error {
		if phase == os.Getenv("CPA_STATE_CRASH_PHASE") {
			fmt.Println("state-crash-ready")
			<-time.NewTimer(time.Hour).C
		}
		return nil
	}
	if err := s.saveState(state); err != nil {
		t.Fatal(err)
	}
	t.Fatal("crash hook did not stop publication")
}
func TestActualStateSIGKILLBeforeAndAfterPublication(t *testing.T) {
	for _, phase := range []string{"before-publish", "after-publish"} {
		t.Run(phase, func(t *testing.T) {
			s, _ := fixtureServer(t)
			if _, err := s.state(); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(os.Args[0], "-test.run=^TestStateCrashWorker$", "-test.count=1")
			cmd.Env = append(os.Environ(), "CPA_STATE_CRASH_ROOT="+s.options.Data, "CPA_STATE_CRASH_PHASE="+phase)
			pipe, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() {
				_ = cmd.Process.Kill()
				if cmd.ProcessState == nil {
					_ = cmd.Wait()
				}
			}()
			ready := make(chan bool, 1)
			go func() {
				line, _ := bufio.NewReader(pipe).ReadString('\n')
				ready <- strings.TrimSpace(line) == "state-crash-ready"
			}()
			select {
			case ok := <-ready:
				if !ok {
					t.Fatal("isolated crash worker did not reach publication phase")
				}
			case <-time.After(8 * time.Second):
				t.Fatal("crash worker readiness timed out")
			}
			if err := cmd.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			if err := cmd.Wait(); err == nil {
				t.Fatal("worker was not killed")
			}
			restarted, err := New(s.options)
			if err != nil {
				t.Fatal(err)
			}
			current, err := restarted.state()
			want := "keep"
			if phase == "after-publish" {
				want = "authorized-child-publication"
			}
			if err != nil || current["custom"] != want {
				t.Fatalf("SIGKILL recovery selected wrong state: %v", err)
			}
			if _, err := os.Lstat(filepath.Join(s.options.Data, stateFence)); !os.IsNotExist(err) {
				t.Fatal("confirmed restart left state fence")
			}
		})
	}
}

func TestStateRecoveryRejectsSymlinkTargetsAndDirectories(t *testing.T) {
	s, _ := fixtureServer(t)
	if _, err := s.state(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(s.options.Data, "state.json")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(s.options.Usage)
	if err := os.Symlink(s.options.Usage, path); err != nil {
		t.Fatal(err)
	}
	if _, err := s.state(); err == nil {
		t.Fatal("state symlink followed by recovery")
	}
	after, _ := os.ReadFile(s.options.Usage)
	if string(before) != string(after) {
		t.Fatal("symlink recovery overwrote another file")
	}
	s, _ = fixtureServer(t)
	if _, err := s.state(); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(s.options.Data, "state-recovery")
	if err := os.Rename(dir, filepath.Join(s.options.Data, "original-recovery")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), dir); err != nil {
		t.Fatal(err)
	}
	if _, err := s.state(); err == nil {
		t.Fatal("recovery directory symlink accepted")
	}
}
