package release

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
)

func TestRealCLIActivateOneCommandRollbackAndRestart(t *testing.T) {
	bin := os.Getenv("CPA_RELEASE_BINARY")
	if bin == "" {
		t.Skip("set CPA_RELEASE_BINARY to exercise the actual control CLI")
	}
	root := releaseRoot(t)
	a, b := backend(t, "old"), backend(t, "new")
	stage(t, root, "old", a.URL)
	stage(t, root, "new", b.URL)
	start := func() *exec.Cmd {
		command := exec.Command(bin, "serve", "--root", root, "--isolation-test", "--listen", "127.0.0.1:0")
		stdout, err := command.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		command.Stderr = os.Stderr
		if err := command.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = command.Process.Kill() })
		line, err := bufio.NewReader(stdout).ReadString('\n')
		if err != nil || !strings.Contains(line, "switchboard listening:") {
			t.Fatalf("CLI startup: %s %v", line, err)
		}
		return command
	}
	change := func(action string, args ...string) State {
		args = append([]string{action, "--root", root, "--isolation-test"}, args...)
		raw, err := exec.CommandContext(context.Background(), bin, args...).CombinedOutput()
		if err != nil {
			t.Fatalf("CLI %s: %s %v", action, raw, err)
		}
		var reply Reply
		if err := json.Unmarshal(raw, &reply); err != nil || reply.Error != "" {
			t.Fatalf("CLI invalid reply: %s", raw)
		}
		return reply.State
	}
	stop := func(command *exec.Cmd) {
		if err := command.Process.Signal(syscall.SIGTERM); err != nil {
			t.Fatal(err)
		}
		if err := command.Wait(); err != nil {
			t.Fatalf("switchboard did not drain cleanly: %v", err)
		}
	}
	command := start()
	if s := change("activate", "--id", "old", "--expected-revision", "0"); s.Current.ID != "old" {
		t.Fatal(s)
	}
	if s := change("activate", "--id", "new", "--expected-revision", "1"); s.Current.ID != "new" || s.Previous.ID != "old" {
		t.Fatal(s)
	}
	// This is the exact one-command rollback form. A pinned revision prevents
	// accidentally rolling back a later, unrelated deployment.
	if s := change("rollback", "--expected-revision", "2"); s.Current.ID != "old" || s.Revision != 3 {
		t.Fatal(s)
	}
	stop(command)
	command = start()
	if s := change("state"); s.Current.ID != "old" || s.Revision != 3 {
		t.Fatal("restart lost rollback", s)
	}
	stop(command)
}

func TestRealCLICommittedSwitchSurvivesSIGKILL(t *testing.T) {
	bin := os.Getenv("CPA_RELEASE_BINARY")
	if bin == "" {
		t.Skip("set CPA_RELEASE_BINARY to exercise crash durability")
	}
	root := releaseRoot(t)
	a, b := backend(t, "old"), backend(t, "new")
	stage(t, root, "old", a.URL)
	stage(t, root, "new", b.URL)
	command := exec.Command(bin, "serve", "--root", root, "--isolation-test", "--listen", "127.0.0.1:0")
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	command.Stderr = os.Stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = command.Process.Kill() })
	if line, err := bufio.NewReader(stdout).ReadString('\n'); err != nil || !strings.Contains(line, "switchboard listening:") {
		t.Fatalf("crash fixture startup: %s %v", line, err)
	}
	for i, id := range []string{"old", "new"} {
		args := []string{"activate", "--root", root, "--isolation-test", "--id", id, "--expected-revision", fmt.Sprint(i)}
		if raw, err := exec.Command(bin, args...).CombinedOutput(); err != nil {
			t.Fatalf("activation: %s %v", raw, err)
		}
	}
	if err := command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := command.Wait(); err == nil {
		t.Fatal("SIGKILL was not observed")
	}
	// The process cannot unlink its socket after SIGKILL. Reacquiring the root
	// lock allows recovery of this specific, private, connection-refused socket.
	c := openController(t, root)
	if s := c.State(); s.Current.ID != "new" || s.Previous.ID != "old" || s.Revision != 2 {
		t.Fatalf("crash lost committed transaction: %+v", s)
	}
	selected(t, c, "new")
	listener, err := c.ListenControl()
	if err != nil {
		t.Fatalf("crash left the control endpoint unrecoverable: %v", err)
	}
	server := &http.Server{Handler: c.ControlHandler()}
	go func() { _ = server.Serve(listener) }()
	defer func() { _ = server.Close() }()
	args := []string{"rollback", "--root", root, "--isolation-test", "--expected-revision", "2"}
	if raw, err := exec.Command(bin, args...).CombinedOutput(); err != nil {
		t.Fatalf("one-command rollback after crash: %s %v", raw, err)
	}
	if s := c.State(); s.Current.ID != "old" || s.Revision != 3 {
		t.Fatalf("crash rollback failed: %+v", s)
	}
}
