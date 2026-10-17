package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/api"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/billing"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v8/sdk/access"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

func atomicIdentityWrite(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path+".candidate", data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path+".candidate", path); err != nil {
		t.Fatal(err)
	}
}

func liveFixture(t *testing.T) (*LiveProfile, *profileFixture, *gin.Engine) {
	t.Helper()
	f := newProfileFixture(t, false)
	dir := filepath.Dir(f.path)
	// The fixture creates this key path, but not the literal gateway environment.
	env := filepath.Join(dir, "gateway.env")
	atomicIdentityWrite(t, env, []byte("UNIFIED_GATEWAY_API_KEY=admin-test-key\n"))
	l, err := OpenLiveProfile(LivePaths{ConfigDir: filepath.Join(dir, "config"), AuthDir: filepath.Join(dir, "auth"), KeyStore: filepath.Join(dir, "keys.json"), AdminEnv: env})
	if err != nil {
		t.Fatal(err)
	}
	manager := sdkaccess.NewManager()
	manager.SetProviders([]sdkaccess.Provider{l})
	engine := gin.New()
	_ = engine.SetTrustedProxies(nil)
	engine.Use(l.limits.global(l.adminRPMExempt), api.AuthMiddleware(manager), l.KeyLimit(), l.Admission())
	engine.GET("/v1/models", func(c *gin.Context) { c.Status(200) })
	engine.POST("/v1/responses", func(c *gin.Context) {
		meta := c.Request.Context().Value(metadataKey{}).(requestMetadata)
		c.JSON(200, gin.H{"key_id": meta.KeyID, "model": meta.Model, "account": meta.Account.ID})
	})
	return l, f, engine
}

func liveRequest(engine *gin.Engine, token, model string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	if model != "" {
		req = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"`+model+`","input":"fixture"}`))
	}
	req.RemoteAddr = "192.0.2.1:1234"
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)
	return w
}

func TestLiveKeysRotateRevokeFailClosedAndRecoverWithoutRestart(t *testing.T) {
	l, _, engine := liveFixture(t)
	first, err := l.Refresh()
	if err != nil {
		t.Fatal(err)
	}
	if again, _ := l.Refresh(); again != first {
		t.Fatal("unchanged files reparsed")
	}
	if w := liveRequest(engine, "issued-test-key", identityModel); w.Code != 200 {
		t.Fatalf("old key missing: %d %s", w.Code, w.Body.String())
	}
	hash := sha256.Sum256([]byte("rotated-test-key"))
	raw, _ := json.Marshal(map[string]any{"keys": []any{map[string]any{"id": "issued-fixture", "hash": hex.EncodeToString(hash[:]), "enabled": true, "rpm": 2, "maxConcurrent": 1}}})
	atomicIdentityWrite(t, l.paths.KeyStore, raw)
	if w := liveRequest(engine, "issued-test-key", identityModel); w.Code != 401 {
		t.Fatalf("old hash remained authorized: %d", w.Code)
	}
	if w := liveRequest(engine, "rotated-test-key", identityModel); w.Code != 200 {
		t.Fatalf("rotated hash not applied: %d %s", w.Code, w.Body.String())
	}
	if w := liveRequest(engine, "rotated-test-key", identityModel); w.Code != 429 {
		t.Fatalf("reload reset previous RPM accounting: %d", w.Code)
	}
	atomicIdentityWrite(t, l.paths.KeyStore, []byte(`{"keys":`))
	if w := liveRequest(engine, "admin-test-key", ""); w.Code != 503 {
		t.Fatal("malformed policy reused old authorization")
	}
	atomicIdentityWrite(t, l.paths.KeyStore, []byte(`{"keys":[]}`))
	if w := liveRequest(engine, "rotated-test-key", identityModel); w.Code != 401 {
		t.Fatal("revoked key still accepted")
	}
	if w := liveRequest(engine, "admin-test-key", ""); w.Code != 200 {
		t.Fatalf("valid repair did not recover: %d", w.Code)
	}
	atomicIdentityWrite(t, l.paths.AdminEnv, []byte("UNIFIED_GATEWAY_API_KEY=new-admin-test-key\n"))
	if w := liveRequest(engine, "admin-test-key", ""); w.Code != 401 {
		t.Fatal("old admin key survived rotation")
	}
	if w := liveRequest(engine, "new-admin-test-key", ""); w.Code != 200 {
		t.Fatal("new admin key missing")
	}
}

func TestLiveCatalogAndAccountAttributionKeepInFlightSnapshot(t *testing.T) {
	l, _, engine := liveFixture(t)
	first, _ := l.Refresh()
	stamp := time.Unix(1800000000, 0)
	old := first.decorateAdmission(context.WithValue(context.Background(), metadataKey{}, requestMetadata{Model: identityModel, KeyID: "admin", Started: stamp}), "codex-fixture.json")
	path := filepath.Join(l.paths.ConfigDir, "oauth-accounts.json")
	raw, _ := os.ReadFile(path)
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	doc["accounts"].([]any)[0].(map[string]any)["id"] = "renamed-display-account"
	raw, _ = json.Marshal(doc)
	atomicIdentityWrite(t, path, raw)
	if w := liveRequest(engine, "admin-test-key", identityModel); w.Code != 200 || !strings.Contains(w.Body.String(), "renamed-display-account") {
		t.Fatalf("new account projection not applied: %d %s", w.Code, w.Body.String())
	}
	second, err := l.Refresh()
	if err != nil {
		t.Fatal(err)
	}
	fresh := second.decorateAdmission(context.WithValue(context.Background(), metadataKey{}, requestMetadata{Model: identityModel, KeyID: "admin", Started: stamp}), "codex-fixture.json")
	usagePath := filepath.Join(t.TempDir(), "usage.jsonl")
	history := "{\"ts\":1,\"model\":\"historical\"}\n"
	if err := os.WriteFile(usagePath, []byte(history), 0640); err != nil {
		t.Fatal(err)
	}
	sink, err := OpenReceiptSink(usagePath, first.ReceiptBindings())
	if err != nil {
		t.Fatal(err)
	}
	record := usage.Record{AuthID: "codex-fixture.json", Model: identityModel, RequestedAt: stamp, Latency: time.Second, RawUsageFormat: "responses", RawUsage: []byte(`{"input_tokens":10,"output_tokens":8}`)}
	sink.HandleUsage(old, record)
	sink.HandleUsage(fresh, record)
	if err := sink.Err(); err != nil {
		t.Fatal(err)
	}
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}
	result, _ := os.ReadFile(usagePath)
	if !strings.HasPrefix(string(result), history) {
		t.Fatal("historical rows overwritten")
	}
	lines := strings.Split(strings.TrimSpace(string(result)), "\n")
	before, _ := billing.DecodeRow([]byte(lines[1]))
	after, _ := billing.DecodeRow([]byte(lines[2]))
	if before.OAuthAccountID != "codex-member" || after.OAuthAccountID != "renamed-display-account" {
		t.Fatalf("late accounting crossed account edit: before=%s after=%s", before.OAuthAccountID, after.OAuthAccountID)
	}
	atomicIdentityWrite(t, filepath.Join(l.paths.ConfigDir, "exposed-codex-models.json"), []byte(`{"models":["new-fixture"]}`))
	if w := liveRequest(engine, "admin-test-key", "codex/"+identityModel); w.Code != 400 {
		t.Fatal("removed model remained routed")
	}
	third, _ := l.Refresh()
	if _, _, err := third.Accounts.Resolve("new-fixture", "/v1/responses"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := first.Accounts.Resolve(identityModel, "/v1/responses"); err != nil {
		t.Fatal("old snapshot mutated")
	}
}

func TestLiveAccountDisableAndMemberMismatchNeverFallBack(t *testing.T) {
	l, _, engine := liveFixture(t)
	path := filepath.Join(l.paths.AuthDir, "codex-fixture.json")
	raw, _ := os.ReadFile(path)
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	doc["disabled"] = true
	raw, _ = json.Marshal(doc)
	atomicIdentityWrite(t, path, raw)
	if w := liveRequest(engine, "admin-test-key", identityModel); w.Code != 400 {
		t.Fatal("disabled primary account fell back")
	}
	if w := liveRequest(engine, "admin-test-key", "codex2/"+identityModel); w.Code != 200 {
		t.Fatalf("disabled primary affected secondary: %d", w.Code)
	}
	doc["email"] = "different-member@example.invalid"
	raw, _ = json.Marshal(doc)
	atomicIdentityWrite(t, path, raw)
	if w := liveRequest(engine, "admin-test-key", "codex2/"+identityModel); w.Code != 503 {
		t.Fatal("inconsistent account transaction accepted")
	}
}

func TestLiveRefreshConcurrentReadersPublishOneSnapshot(t *testing.T) {
	l, _, _ := liveFixture(t)
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := 0; n < 5; n++ {
				p, err := l.Refresh()
				if err != nil || p == nil {
					t.Error("concurrent refresh failed")
				}
			}
		}()
	}
	wg.Wait()
}
