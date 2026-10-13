//go:build linux

package adminhelper

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/clientkeys"
)

func fixture(t *testing.T) *Service {
	t.Helper()
	dir := t.TempDir()
	paths := Paths{Keys: filepath.Join(dir, "keys.json"), Vault: filepath.Join(dir, "vault.json"), Lock: filepath.Join(dir, "apply.lock"), Audit: filepath.Join(dir, "audit.jsonl")}
	digest := sha256.Sum256([]byte("legacy-fixture-key"))
	doc := map[string]any{"version": 7, "future_root": map[string]any{"preserve": true}, "keys": []any{map[string]any{"id": "key_existing", "hash": hex.EncodeToString(digest[:]), "enabled": true, "name": "existing", "future_metadata": "keep", "lastUsedAt": 123}}}
	raw, _ := json.Marshal(doc)
	for path, value := range map[string][]byte{paths.Keys: raw, paths.Vault: []byte(`{"existing":"private-existing-secret","unknown_metadata":{"keep":true}}`), paths.Lock: nil, paths.Audit: nil} {
		if err := os.WriteFile(path, value, 0600); err != nil {
			t.Fatal(err)
		}
	}
	s, err := New(paths)
	if err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return time.Unix(1780000000, 0) }
	return s
}

func call(s *Service, op string, payload map[string]any) Result {
	raw := map[string]json.RawMessage{}
	for field, value := range payload {
		raw[field] = set(value)
	}
	return s.dispatch(op, raw)
}

func requireOK(t *testing.T, result Result) {
	t.Helper()
	if result["ok"] != true {
		t.Fatalf("control failed: %v", result["error"])
	}
}

func TestKeysPreserveSchemaAndAuthenticate(t *testing.T) {
	s := fixture(t)
	result := call(s, "create_api_key", map[string]any{"name": "测试", "rpm": 12})
	requireOK(t, result)
	key, id := result["key"].(string), result["id"].(string)
	if !strings.HasPrefix(key, "sk-cpa-") || len(key) != 47 {
		t.Fatal("unexpected key format")
	}
	raw, err := os.ReadFile(s.paths.Keys)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte(key)) {
		t.Fatal("plaintext key persisted")
	}
	provider, err := clientkeys.New("admin-fixture", raw)
	if err != nil {
		t.Fatal(err)
	}
	request, _ := newKeyRequest(key)
	auth, errAuth := provider.Authenticate(context.Background(), request)
	if errAuth != nil || auth.Principal != id || provider.Authorize(id, "any/model") != nil {
		t.Fatal("issued key not accepted with default all-model access")
	}
	requireOK(t, call(s, "update_api_key", map[string]any{"key_id": id, "enabled": false, "note": "changed"}))
	raw, _ = os.ReadFile(s.paths.Keys)
	if provider.Reload(raw) != nil {
		t.Fatal("updated key invalid")
	}
	if _, errAuth := provider.Authenticate(context.Background(), request); errAuth == nil {
		t.Fatal("disabled key accepted")
	}
	requireOK(t, call(s, "delete_api_key", map[string]any{"key_id": id}))
	raw, _ = os.ReadFile(s.paths.Keys)
	var doc map[string]any
	_ = json.Unmarshal(raw, &doc)
	entries := doc["keys"].([]any)
	if len(entries) != 1 || doc["version"] != float64(7) || doc["future_root"] == nil || entries[0].(map[string]any)["future_metadata"] != "keep" || entries[0].(map[string]any)["lastUsedAt"] != float64(123) {
		t.Fatal("unrelated history or schema lost")
	}
	read := call(s, "read_api_keys", nil)
	requireOK(t, read)
	safe, _ := json.Marshal(read)
	if bytes.Contains(safe, []byte(`"hash"`)) {
		t.Fatal("read leaked key digest")
	}
	info, _ := os.Stat(s.paths.Keys)
	if info.Mode().Perm() != 0600 {
		t.Fatal("unsafe key mode")
	}
}

func TestInvalidDatabaseNeverResetsOrMutates(t *testing.T) {
	for _, raw := range []string{`{"keys":null}`, `{"keys":[]`, `{"keys":[{"id":"bad"}]}`, `{"keys":[],"keys":[]}`} {
		s := fixture(t)
		_ = os.WriteFile(s.paths.Keys, []byte(raw), 0600)
		if call(s, "create_api_key", nil)["ok"] != false {
			t.Fatal("invalid database accepted")
		}
		after, _ := os.ReadFile(s.paths.Keys)
		if string(after) != raw {
			t.Fatal("invalid database overwritten")
		}
	}
	s := fixture(t)
	before, _ := os.ReadFile(s.paths.Keys)
	for _, payload := range []map[string]any{{"key_id": "key_existing", "name": "modified", "rpm": -1}, {"key_id": "key_existing", "exec": "unsafe"}, {"key_id": "missing", "name": "modified"}} {
		if call(s, "update_api_key", payload)["ok"] != false {
			t.Fatal("bad update accepted")
		}
		after, _ := os.ReadFile(s.paths.Keys)
		if !bytes.Equal(after, before) {
			t.Fatal("failed update partially mutated database")
		}
	}
}

func TestVaultPreservesOtherProvidersAndRejectsSymlinks(t *testing.T) {
	s := fixture(t)
	secret := "new-provider-secret"
	requireOK(t, call(s, "set_provider_secret", map[string]any{"provider_id": "provider_new", "secret": secret}))
	result := call(s, "delete_provider_secret", map[string]any{"provider_id": "provider_new"})
	requireOK(t, result)
	raw, _ := os.ReadFile(s.paths.Vault)
	if !bytes.Contains(raw, []byte("private-existing-secret")) || !bytes.Contains(raw, []byte("unknown_metadata")) || bytes.Contains(raw, []byte(secret)) {
		t.Fatal("vault retention failure")
	}
	outside := filepath.Join(t.TempDir(), "outside")
	_ = os.WriteFile(outside, []byte("{}"), 0600)
	_ = os.Remove(s.paths.Vault)
	if os.Symlink(outside, s.paths.Vault) != nil {
		t.Fatal("symlink fixture failed")
	}
	if call(s, "set_provider_secret", map[string]any{"provider_id": "provider_new", "secret": secret})["ok"] != false {
		t.Fatal("followed vault symlink")
	}
	after, _ := os.ReadFile(outside)
	if string(after) != "{}" {
		t.Fatal("wrote outside vault")
	}
}

func TestConcurrentHelpersDoNotLoseWrites(t *testing.T) {
	s := fixture(t)
	other, err := New(s.paths)
	if err != nil {
		t.Fatal(err)
	}
	var workers sync.WaitGroup
	for i := 0; i < 24; i++ {
		workers.Add(1)
		go func(i int) {
			defer workers.Done()
			current := s
			if i%2 != 0 {
				current = other
			}
			if call(current, "create_api_key", nil)["ok"] != true {
				t.Error("concurrent create failed")
			}
		}(i)
	}
	workers.Wait()
	raw, _ := os.ReadFile(s.paths.Keys)
	var doc struct {
		Keys []any `json:"keys"`
	}
	_ = json.Unmarshal(raw, &doc)
	if len(doc.Keys) != 25 {
		t.Fatal("lost cross-instance writes")
	}
}

func TestSocketPeerAuthenticationBoundsAndAudit(t *testing.T) {
	s := fixture(t)
	path := filepath.Join(t.TempDir(), "control.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx, listener, uint32(os.Geteuid())) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	for _, raw := range []string{
		`{"op":"create_api_key","request_id":"request_1234","payload":{"name":"fixture-secret-name"}}` + "\n",
		`{"op":"apply_bundle","request_id":"request_5678","payload":{}}` + "\n",
		strings.Repeat("x", MaxRequest+1),
	} {
		conn, err := net.Dial("unix", path)
		if err != nil {
			t.Fatal(err)
		}
		_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
		_, _ = conn.Write([]byte(raw))
		response := readResponse(t, conn)
		_ = conn.Close()
		if strings.Contains(raw, "create_api_key") {
			if response["ok"] != true {
				t.Fatal("authorized request failed")
			}
		} else if response["ok"] != false {
			t.Fatal("bad/unsupported request succeeded")
		}
	}
	audit, _ := os.ReadFile(s.paths.Audit)
	if bytes.Contains(audit, []byte("fixture-secret-name")) || bytes.Contains(audit, []byte("sk-cpa-")) || !bytes.Contains(audit, []byte("request_1234")) {
		t.Fatal("audit leaked payload or omitted request")
	}
	denied, err := net.ListenUnix("unix", &net.UnixAddr{Name: filepath.Join(t.TempDir(), "denied.sock"), Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	deniedCtx, deniedCancel := context.WithCancel(context.Background())
	deniedDone := make(chan error, 1)
	go func() { deniedDone <- s.Serve(deniedCtx, denied, uint32(os.Geteuid()+1)) }()
	conn, _ := net.Dial("unix", denied.Addr().String())
	response := readResponse(t, conn)
	_ = conn.Close()
	deniedCancel()
	<-deniedDone
	if response["ok"] != false || response["error"] != "peer not allowed" {
		t.Fatal("wrong UID accepted")
	}
}

func TestAuditFaultBlocksMutationBeforeDispatch(t *testing.T) {
	s := fixture(t)
	before, _ := os.ReadFile(s.paths.Keys)
	if err := os.Remove(s.paths.Audit); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "control.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx, listener, uint32(os.Geteuid())) }()
	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = conn.Write([]byte("{\"op\":\"create_api_key\",\"request_id\":\"audit_1234\",\"payload\":{}}\n"))
	result := readResponse(t, conn)
	_ = conn.Close()
	cancel()
	<-done
	if result["ok"] != false || result["error"] != "audit unavailable; operation not started" {
		t.Fatal("audit fault did not fail closed")
	}
	after, _ := os.ReadFile(s.paths.Keys)
	if !bytes.Equal(before, after) {
		t.Fatal("mutated keys without start audit")
	}
}
