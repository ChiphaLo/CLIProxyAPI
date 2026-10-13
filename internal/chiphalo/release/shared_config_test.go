//go:build linux

package release

import (
	"bufio"
	"context"
	"encoding/json"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/privatecontrol"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestSharedBusinessRollbackDoesNotPinOrRestoreOldConfiguration(t *testing.T) {
	root := releaseRoot(t)
	business := filepath.Join(root, "business-live.json")
	if err := os.WriteFile(business, []byte(`{"provider":"current","receipts":2}`), 0600); err != nil {
		t.Fatal(err)
	}
	stageShared := func(id string) *httptest.Server {
		handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/__cpa_release/ready" {
				raw, _ := os.ReadFile(business)
				_ = json.NewEncoder(w).Encode(map[string]any{"ready": true, "production_ready": true, "configuration_ready": true, "config_apply_pending": false, "release_id": id, "config_sha256": digest(raw), "state_contract": StateContract})
				return
			}
			_, _ = w.Write([]byte(id))
		})
		server := httptest.NewServer(handler)
		t.Cleanup(server.Close)
		dir := filepath.Join(root, "releases", id)
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
		controlDir := t.TempDir()
		if err := os.Chmod(controlDir, 0700); err != nil {
			t.Fatal(err)
		}
		socket := filepath.Join(controlDir, "control.sock")
		listener, err := privatecontrol.Listen(socket, uint32(os.Geteuid()))
		if err != nil {
			t.Fatal(err)
		}
		private := &http.Server{Handler: handler}
		go func() { _ = private.Serve(listener) }()
		t.Cleanup(func() { _ = private.Close() })
		manifest := Manifest{ID: id, Endpoint: server.URL, StateContract: StateContract, SharedBusinessConfig: true, PrivateControlSocket: socket, GatewayUID: uint32(os.Geteuid()), Artifacts: map[string]string{}}
		for name, raw := range map[string][]byte{"hosting.json": []byte("fixed hosting:" + id), "gateway": []byte("fixed binary:" + id)} {
			if err := os.WriteFile(filepath.Join(dir, name), raw, 0600); err != nil {
				t.Fatal(err)
			}
			manifest.Artifacts[name] = digest(raw)
		}
		writeManifest(t, root, manifest)
		return server
	}
	stageShared("previous-shared")
	stageShared("next-shared")
	c := openController(t, root)
	if _, err := c.Activate(context.Background(), "previous-shared", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Activate(context.Background(), "next-shared", 1); err != nil {
		t.Fatal(err)
	}
	current := []byte(`{"provider":"edited-after-deploy","receipts":999,"new_account":"preserve"}`)
	if err := os.WriteFile(business, current, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Rollback(context.Background(), 2); err != nil {
		t.Fatal("rollback wrongly required an old business snapshot", err)
	}
	selected(t, c, "previous-shared")
	raw, _ := os.ReadFile(business)
	if string(raw) != string(current) {
		t.Fatal("rollback overwrote shared business state")
	}
}

func TestRealCLISharedBusinessOneCommandRollback(t *testing.T) {
	bin := os.Getenv("CPA_RELEASE_BINARY")
	if bin == "" {
		t.Skip("set CPA_RELEASE_BINARY for actual CLI evidence")
	}
	root := releaseRoot(t)
	data := filepath.Join(root, "business.json")
	initial := []byte(`{"providers":3,"receipts":1}`)
	if err := os.WriteFile(data, initial, 0600); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"old-current-data", "new-current-data"} {
		id := id
		handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw, _ := os.ReadFile(data)
			_ = json.NewEncoder(w).Encode(map[string]any{"ready": true, "production_ready": true, "configuration_ready": true, "release_id": id, "config_sha256": digest(raw), "state_contract": StateContract})
		})
		public := httptest.NewServer(handler)
		t.Cleanup(public.Close)
		privateDir := t.TempDir()
		if err := os.Chmod(privateDir, 0700); err != nil {
			t.Fatal(err)
		}
		socket := filepath.Join(privateDir, "control.sock")
		listener, err := privatecontrol.Listen(socket, uint32(os.Geteuid()))
		if err != nil {
			t.Fatal(err)
		}
		private := &http.Server{Handler: handler}
		go func() { _ = private.Serve(listener) }()
		t.Cleanup(func() { _ = private.Close() })
		dir := filepath.Join(root, "releases", id)
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
		m := Manifest{ID: id, Endpoint: public.URL, StateContract: StateContract, SharedBusinessConfig: true, PrivateControlSocket: socket, GatewayUID: uint32(os.Geteuid()), Artifacts: map[string]string{}}
		for name, raw := range map[string][]byte{"hosting.json": []byte("immutable hosting:" + id), "gateway": []byte("immutable program:" + id)} {
			if err := os.WriteFile(filepath.Join(dir, name), raw, 0600); err != nil {
				t.Fatal(err)
			}
			m.Artifacts[name] = digest(raw)
		}
		writeManifest(t, root, m)
	}
	command := exec.Command(bin, "serve", "--root", root, "--isolation-test", "--listen", "127.0.0.1:0")
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	command.Stderr = os.Stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = command.Process.Kill(); _ = command.Wait() })
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || !strings.Contains(line, "switchboard listening:") {
		t.Fatal("CLI did not start")
	}
	change := func(action string, args ...string) State {
		raw, err := exec.Command(bin, append([]string{action, "--root", root, "--isolation-test"}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("CLI change failed: %s", raw)
		}
		var reply Reply
		if json.Unmarshal(raw, &reply) != nil || reply.Error != "" {
			t.Fatal("CLI reply invalid")
		}
		return reply.State
	}
	change("activate", "--id", "old-current-data", "--expected-revision", "0")
	change("activate", "--id", "new-current-data", "--expected-revision", "1")
	current := []byte(`{"providers":4,"receipts":999,"account":"added-after-deploy"}`)
	if err := os.WriteFile(data, current, 0600); err != nil {
		t.Fatal(err)
	}
	state := change("rollback", "--expected-revision", "2")
	if state.Current.ID != "old-current-data" || state.Revision != 3 {
		t.Fatal("one-command rollback selected wrong program")
	}
	raw, _ := os.ReadFile(data)
	if string(raw) != string(current) {
		t.Fatal("actual CLI rollback restored old business data")
	}
	if err := command.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err := command.Wait(); err != nil {
		t.Fatal(err)
	}
}

func TestSharedBusinessReleaseRefusesFrozenConfigAndShadowReadiness(t *testing.T) {
	root := releaseRoot(t)
	s := backend(t, "shared-invalid")
	m := stage(t, root, "shared-invalid", s.URL)
	m.SharedBusinessConfig = true
	writeManifest(t, root, m)
	c := openController(t, root)
	if _, err := c.Activate(t.Context(), m.ID, 0); err == nil {
		t.Fatal("shared contract accepted a frozen business config artifact")
	}
	dir := filepath.Join(root, "releases", m.ID)
	if err := os.Remove(filepath.Join(dir, "config.yaml")); err != nil {
		t.Fatal(err)
	}
	delete(m.Artifacts, "config.yaml")
	m.ConfigSHA256 = ""
	controlDir := t.TempDir()
	if err := os.Chmod(controlDir, 0700); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(controlDir, "control.sock")
	listener, err := privatecontrol.Listen(socket, uint32(os.Geteuid()))
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { readyReply(w, m.ID) })}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	m.PrivateControlSocket, m.GatewayUID = socket, uint32(os.Geteuid())
	m.Artifacts["hosting.json"] = digest([]byte("hosting"))
	if err := os.WriteFile(filepath.Join(dir, "hosting.json"), []byte("hosting"), 0600); err != nil {
		t.Fatal(err)
	}
	writeManifest(t, root, m)
	if _, err := c.Activate(t.Context(), m.ID, 0); err == nil {
		t.Fatal("shared contract promoted nonproduction readiness")
	}
}
