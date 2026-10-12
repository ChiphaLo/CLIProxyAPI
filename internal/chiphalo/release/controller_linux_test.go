package release

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func digest(raw []byte) string {
	hash := sha256.Sum256(raw)
	return hex.EncodeToString(hash[:])
}

func releaseRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "releases"), 0700); err != nil {
		t.Fatal(err)
	}
	return root
}

func stage(t *testing.T, root, id, endpoint string) Manifest {
	t.Helper()
	dir := filepath.Join(root, "releases", id)
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	config := []byte("synthetic-config:" + id)
	executable := []byte("synthetic-build:" + id)
	manifest := Manifest{ID: id, Endpoint: endpoint, StateContract: StateContract, ConfigSHA256: digest(config),
		Artifacts: map[string]string{"config.yaml": digest(config), "gateway": digest(executable)}}
	for name, raw := range map[string][]byte{"config.yaml": config, "gateway": executable} {
		if err := os.WriteFile(filepath.Join(dir, name), raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	writeManifest(t, root, manifest)
	return manifest
}

func writeManifest(t *testing.T, root string, manifest Manifest) {
	t.Helper()
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "releases", manifest.ID, "manifest.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
}

func readyReply(w http.ResponseWriter, id string) {
	_ = json.NewEncoder(w).Encode(map[string]any{"ready": true, "release_id": id, "config_sha256": digest([]byte("synthetic-config:" + id)), "state_contract": StateContract})
}

func backend(t *testing.T, id string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/__cpa_release/ready" {
			readyReply(w, id)
			return
		}
		_, _ = io.WriteString(w, id)
	}))
	t.Cleanup(server.Close)
	return server
}

func openController(t *testing.T, root string) *Controller {
	t.Helper()
	c, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func selected(t *testing.T, c *Controller, want string) {
	t.Helper()
	recorder := httptest.NewRecorder()
	c.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if recorder.Code != http.StatusOK || recorder.Body.String() != want {
		t.Fatalf("selected response = %d %q, want %q", recorder.Code, recorder.Body.String(), want)
	}
}

func TestAtomicSwitchRollbackRestartAndSharedReceipts(t *testing.T) {
	root := releaseRoot(t)
	stats := filepath.Join(root, "usage.jsonl")
	var mu sync.Mutex
	appendReceipt := func(id string) {
		mu.Lock()
		defer mu.Unlock()
		file, err := os.OpenFile(stats, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
		if err == nil {
			_, err = fmt.Fprintln(file, id)
			_ = file.Close()
		}
		if err != nil {
			t.Error(err)
		}
	}
	finish := make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(finish) }) })
	old := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/__cpa_release/ready" {
			readyReply(w, "old")
			return
		}
		if r.URL.Path == "/stream" {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: old-start\n\n")
			w.(http.Flusher).Flush()
			<-finish
			_, _ = io.WriteString(w, "data: old-complete\n\n")
			appendReceipt("old-finished-after-switch")
			return
		}
		_, _ = io.WriteString(w, "old")
	}))
	defer old.Close()
	newServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/__cpa_release/ready" {
			readyReply(w, "new")
			return
		}
		appendReceipt("new-finished-before-rollback")
		_, _ = io.WriteString(w, "new")
	}))
	defer newServer.Close()
	stage(t, root, "old", old.URL)
	stage(t, root, "new", newServer.URL)
	c := openController(t, root)
	if _, err := c.Activate(context.Background(), "old", 0); err != nil {
		t.Fatal(err)
	}
	edge := httptest.NewServer(c)
	defer edge.Close()
	defer func() { once.Do(func() { close(finish) }) }()
	response, err := http.Get(edge.URL + "/stream")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	reader := bufio.NewReader(response.Body)
	line, err := reader.ReadString('\n')
	if err != nil || line != "data: old-start\n" {
		t.Fatalf("old stream did not flush before completion: %q %v", line, err)
	}
	state, err := c.Activate(context.Background(), "new", 1)
	if err != nil || state.Revision != 2 || state.Previous.ID != "old" {
		t.Fatalf("activate: %+v %v", state, err)
	}
	selected(t, c, "new")
	if _, err := c.Rollback(context.Background(), 2); err != nil {
		t.Fatal(err)
	}
	selected(t, c, "old")
	once.Do(func() { close(finish) })
	rest, err := io.ReadAll(reader)
	if err != nil || !bytes.Contains(rest, []byte("data: old-complete")) || bytes.Contains(rest, []byte("new")) {
		t.Fatalf("old stream corrupted during switch: %q %v", rest, err)
	}
	raw, err := os.ReadFile(stats)
	if err != nil || string(raw) != "new-finished-before-rollback\nold-finished-after-switch\n" {
		t.Fatalf("rollback lost new/late-old receipts: %q %v", raw, err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openController(t, root)
	if reopened.State() != c.State() {
		t.Fatalf("restart lost durable release state")
	}
	selected(t, reopened, "old")
}

func TestUnhealthyCandidateCannotChangeCurrent(t *testing.T) {
	for _, kind := range []string{"wrong-identity", "wrong-config", "wrong-contract", "redirect", "not-ready", "oversized"} {
		t.Run(kind, func(t *testing.T) {
			root := releaseRoot(t)
			old := backend(t, "old")
			bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch kind {
				case "redirect":
					http.Redirect(w, r, old.URL, http.StatusFound)
				case "not-ready":
					w.WriteHeader(http.StatusServiceUnavailable)
				case "oversized":
					_, _ = io.WriteString(w, strings.Repeat("x", 8193))
				default:
					id, hash, contract := "bad", digest([]byte("synthetic-config:bad")), StateContract
					if kind == "wrong-identity" {
						id = "another"
					}
					if kind == "wrong-config" {
						hash = digest([]byte("different"))
					}
					if kind == "wrong-contract" {
						contract = "incompatible-v2"
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"ready": true, "release_id": id, "config_sha256": hash, "state_contract": contract})
				}
			}))
			defer bad.Close()
			stage(t, root, "old", old.URL)
			stage(t, root, "bad", bad.URL)
			c := openController(t, root)
			if _, err := c.Activate(context.Background(), "old", 0); err != nil {
				t.Fatal(err)
			}
			before, _ := os.ReadFile(filepath.Join(root, "current.json"))
			if _, err := c.Activate(context.Background(), "bad", 1); err == nil {
				t.Fatal("unhealthy candidate accepted")
			}
			after, _ := os.ReadFile(filepath.Join(root, "current.json"))
			if !bytes.Equal(before, after) || c.State().Current.ID != "old" {
				t.Fatal("failed preflight changed state")
			}
			selected(t, c, "old")
		})
	}
}

func TestConcurrentCASAndSingleWriter(t *testing.T) {
	root := releaseRoot(t)
	a, b := backend(t, "a"), backend(t, "b")
	stage(t, root, "a", a.URL)
	stage(t, root, "b", b.URL)
	c := openController(t, root)
	if _, err := Open(root); err == nil {
		t.Fatal("second controller acquired writer lock")
	}
	if _, err := c.Activate(context.Background(), "a", 0); err != nil {
		t.Fatal(err)
	}
	var succeeded atomic.Int32
	var group sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 20; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			if _, err := c.Activate(context.Background(), "b", 1); err == nil {
				succeeded.Add(1)
			}
		}()
	}
	close(start)
	group.Wait()
	if succeeded.Load() != 1 || c.State().Revision != 2 {
		t.Fatalf("non-atomic CAS: %d %+v", succeeded.Load(), c.State())
	}
}

func TestRejectMutatedRollbackAndActiveArtifacts(t *testing.T) {
	root := releaseRoot(t)
	a, b := backend(t, "a"), backend(t, "b")
	manifest := stage(t, root, "a", a.URL)
	stage(t, root, "b", b.URL)
	c := openController(t, root)
	if _, err := c.Activate(context.Background(), "a", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Activate(context.Background(), "b", 1); err != nil {
		t.Fatal(err)
	}
	manifest.Endpoint = b.URL
	writeManifest(t, root, manifest)
	if _, err := c.Rollback(context.Background(), 2); err == nil {
		t.Fatal("changed rollback manifest accepted")
	}
	if c.State().Revision != 2 {
		t.Fatal("failed rollback changed revision")
	}
	if err := os.WriteFile(filepath.Join(root, "releases", "b", "gateway"), []byte("tamper"), 0600); err != nil {
		t.Fatal(err)
	}
	_ = c.Close()
	if reopened, err := Open(root); err == nil {
		_ = reopened.Close()
		t.Fatal("restart accepted changed executable")
	}
}

func TestManifestBoundaries(t *testing.T) {
	for _, kind := range []string{"symlink", "public-mode", "remote-url", "traversal", "contract", "config-digest"} {
		t.Run(kind, func(t *testing.T) {
			root := releaseRoot(t)
			server := backend(t, "a")
			manifest := stage(t, root, "a", server.URL)
			switch kind {
			case "symlink":
				path := filepath.Join(root, "releases", "a", "gateway")
				_ = os.Remove(path)
				if err := os.Symlink("config.yaml", path); err != nil {
					t.Fatal(err)
				}
			case "public-mode":
				if err := os.Chmod(filepath.Join(root, "releases", "a", "config.yaml"), 0644); err != nil {
					t.Fatal(err)
				}
			case "remote-url":
				manifest.Endpoint = "http://example.com:8317"
			case "traversal":
				manifest.Artifacts["../gateway"] = manifest.Artifacts["gateway"]
			case "contract":
				manifest.StateContract = "new-incompatible-contract"
			case "config-digest":
				manifest.ConfigSHA256 = digest([]byte("wrong"))
			}
			writeManifest(t, root, manifest)
			c := openController(t, root)
			if _, err := c.Activate(context.Background(), "a", 0); err == nil {
				t.Fatal("unsafe manifest accepted")
			}
			if c.State().Revision != 0 {
				t.Fatal("unsafe manifest was committed")
			}
		})
	}
}

func TestOwnerSocketControlAndHiddenDataPlane(t *testing.T) {
	root := releaseRoot(t)
	server := backend(t, "a")
	stage(t, root, "a", server.URL)
	c := openController(t, root)
	listener, err := c.ListenControl()
	if err != nil {
		t.Fatal(err)
	}
	control := &http.Server{Handler: c.ControlHandler()}
	go func() { _ = control.Serve(listener) }()
	defer func() { _ = control.Close() }()
	info, err := os.Stat(SocketPath(root))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("control socket is not private")
	}
	if _, err := c.ListenControl(); err == nil {
		t.Fatal("second listener replaced the live socket")
	}
	client := ControlClient(root)
	defer client.CloseIdleConnections()
	response, err := client.Post("http://unix/change", "application/json", strings.NewReader(`{"action":"activate","release_id":"a","expected_revision":0}`))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != 200 || c.State().Revision != 1 {
		t.Fatalf("control activation failed: %s", raw)
	}
	recorder := httptest.NewRecorder()
	c.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/__cpa_release/ready", nil))
	if recorder.Code != http.StatusNotFound {
		t.Fatal("readiness exposed on data plane")
	}
}

func TestFailedDiskCommitKeepsPreviousRoute(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("this permission fault requires an unprivileged controller")
	}
	root := releaseRoot(t)
	a, b := backend(t, "old"), backend(t, "new")
	stage(t, root, "old", a.URL)
	stage(t, root, "new", b.URL)
	c := openController(t, root)
	if _, err := c.Activate(context.Background(), "old", 0); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(root, "current.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0500); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(root, 0700) }()
	if _, err := c.Activate(context.Background(), "new", 1); err == nil {
		t.Fatal("read-only state root accepted a commit")
	}
	after, err := os.ReadFile(filepath.Join(root, "current.json"))
	if err != nil || !bytes.Equal(before, after) || c.State().Revision != 1 {
		t.Fatal("failed disk commit changed durable/live state")
	}
	selected(t, c, "old")
}
