//go:build linux

package release

import (
	"context"
	"encoding/json"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/privatecontrol"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
