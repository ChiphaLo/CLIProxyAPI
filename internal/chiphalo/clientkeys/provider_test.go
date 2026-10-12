package clientkeys

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func keyRaw(t *testing.T, token string, changes map[string]any) []byte {
	t.Helper()
	hash := sha256.Sum256([]byte(token))
	entry := map[string]any{"id": "client-1", "hash": hex.EncodeToString(hash[:]), "enabled": true}
	for k, v := range changes {
		entry[k] = v
	}
	raw, err := json.Marshal(map[string]any{"keys": []any{entry}})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func authenticate(p *Provider, token string) (string, bool) {
	req := httptest.NewRequest("GET", "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	result, err := p.Authenticate(context.Background(), req)
	if err != nil {
		return "", false
	}
	return result.Principal, true
}

func TestExistingHashesAdminAndExpiry(t *testing.T) {
	p, err := New("admin-test-only", keyRaw(t, "client-test-only", map[string]any{"expiresAt": int64(1000)}))
	if err != nil {
		t.Fatal(err)
	}
	p.now = func() time.Time { return time.Unix(999, 0) }
	if id, ok := authenticate(p, "admin-test-only"); !ok || id != "admin" {
		t.Fatal("existing admin key lost")
	}
	if id, ok := authenticate(p, "client-test-only"); !ok || id != "client-1" {
		t.Fatal("existing hash not recognized")
	}
	if _, ok := authenticate(p, "client-1"); ok {
		t.Fatal("public key ID became a bearer credential")
	}
	if _, ok := authenticate(p, "wrong-key"); ok {
		t.Fatal("invalid key authorized")
	}
	p.now = func() time.Time { return time.Unix(1000, 0) }
	if _, ok := authenticate(p, "client-test-only"); ok {
		t.Fatal("expiry boundary is not exclusive")
	}
	if err := p.Authorize("client-1", "gpt-6-astra"); err == nil {
		t.Fatal("expiry ignored during admission")
	}
}

func TestDefaultAllModelsAndLegacyPolicy(t *testing.T) {
	p, err := New("admin-test-only", keyRaw(t, "client-test-only", nil))
	if err != nil {
		t.Fatal(err)
	}
	for _, model := range []string{"gpt-6-astra", "codex2/gpt-6-luna", "opencode/deepseek-v4.1-flash"} {
		if p.Authorize("client-1", model) != nil {
			t.Fatalf("default all-model policy rejected %s", model)
		}
	}
	if err := p.Reload(keyRaw(t, "client-test-only", map[string]any{"modelPatterns": []string{"*/gpt-6-*"}, "providerPrefixes": []string{"opencode"}})); err != nil {
		t.Fatal(err)
	}
	for _, model := range []string{"codex2/gpt-6-luna", "other/nested/gpt-6-astra", "opencode/deepseek-v4.1-flash"} {
		if p.Authorize("client-1", model) != nil {
			t.Fatal("legacy pattern or prefix policy changed")
		}
	}
	if p.Authorize("client-1", "other/deepseek") == nil || p.Authorize("revoked", "gpt-6-astra") == nil {
		t.Fatal("restricted or revoked identity authorized")
	}
	if p.Authorize("admin", "other/deepseek") != nil {
		t.Fatal("admin semantics changed")
	}
}

func TestRejectMalformedPoliciesAndKeepPublishedSnapshot(t *testing.T) {
	p, err := New("admin-test-only", keyRaw(t, "client-test-only", nil))
	if err != nil {
		t.Fatal(err)
	}
	for _, changes := range []map[string]any{
		{"id": "admin"}, {"hash": "not-a-digest"}, {"expiresAt": "never"}, {"expiresAt": -1},
		{"rpm": 10001}, {"maxConcurrent": 65}, {"providerPrefixes": []string{"../bad"}},
	} {
		if p.Reload(keyRaw(t, "client-test-only", changes)) == nil {
			t.Fatalf("malformed policy accepted: %v", changes)
		}
		if _, ok := authenticate(p, "client-test-only"); !ok {
			t.Fatal("invalid candidate partially mutated active snapshot")
		}
	}
	for _, raw := range [][]byte{[]byte(`null`), []byte(`{}`), []byte(`{"keys":[]} {}`), keyRaw(t, "admin-test-only", nil)} {
		if p.Reload(raw) == nil {
			t.Fatal("malformed/ambiguous database accepted")
		}
	}
	var duplicate map[string]any
	if json.Unmarshal(keyRaw(t, "client-test-only", nil), &duplicate) != nil {
		t.Fatal("fixture")
	}
	entry := duplicate["keys"].([]any)[0]
	duplicate["keys"] = []any{entry, entry}
	raw, _ := json.Marshal(duplicate)
	if p.Reload(raw) == nil {
		t.Fatal("duplicate key identity/digest accepted")
	}
	if err := p.Reload([]byte(`{"keys":[]}`)); err != nil {
		t.Fatal(err)
	}
	if _, ok := authenticate(p, "client-test-only"); ok {
		t.Fatal("revocation did not take effect")
	}
}

func TestBearerOnlyAndNoSecretPrincipal(t *testing.T) {
	p, err := New("admin-test-only", keyRaw(t, "client-test-only", nil))
	if err != nil {
		t.Fatal(err)
	}
	for _, url := range []string{"/v1/models?key=admin-test-only", "/v1/models?auth_token=client-test-only"} {
		req := httptest.NewRequest("GET", url, nil)
		req.Header.Set("X-Api-Key", "admin-test-only")
		if _, err := p.Authenticate(context.Background(), req); err == nil {
			t.Fatal("legacy bearer policy was widened")
		}
	}
	req := httptest.NewRequest("GET", "/v1/models", nil)
	req.Header.Add("Authorization", "Bearer admin-test-only")
	req.Header.Add("Authorization", "Bearer client-test-only")
	if _, err := p.Authenticate(context.Background(), req); err == nil {
		t.Fatal("ambiguous header accepted")
	}
}

func TestConcurrentAtomicReload(t *testing.T) {
	first, second := keyRaw(t, "first", nil), keyRaw(t, "second", nil)
	p, err := New("admin-test-only", first)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for n := 0; n < 8; n++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				if i%2 == 0 {
					if p.Reload(first) != nil {
						t.Error("reload first")
					}
				} else {
					if p.Reload(second) != nil {
						t.Error("reload second")
					}
				}
				authenticate(p, "first")
				authenticate(p, "second")
			}
		}()
	}
	wg.Wait()
}

func TestPythonGlobCases(t *testing.T) {
	for _, tc := range []struct {
		pattern, model string
		match          bool
	}{
		{"*", "provider/model", true}, {"a?", "ab", true}, {"gpt-[6-9]*", "gpt-6-astra", true},
		{"[!a]*", "b/model", true}, {"[!a]*", "a/model", false}, {"[[]", "[", true},
		{"a[", "a[", true}, {"gpt-*", "other/gpt-6-astra", false}, {"GPT-*", "gpt-6-astra", false},
	} {
		t.Run(tc.pattern+tc.model, func(t *testing.T) {
			pattern, err := compileGlob(tc.pattern)
			if err != nil || pattern.MatchString(tc.model) != tc.match {
				t.Fatalf("glob changed: %v", err)
			}
		})
	}
}

func TestLegacyEnvReadIsLiteral(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.env")
	if err := os.WriteFile(path, []byte("INNER_GATEWAY_API_KEY=do-not-use\nUNIFIED_GATEWAY_API_KEY='literal-$(never-execute)'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	key, err := ReadLegacyAdminKey(path)
	if err != nil || key != "literal-$(never-execute)" {
		t.Fatal("environment was expanded/executed or wrong key selected")
	}
	if err := os.WriteFile(path, []byte("INNER_GATEWAY_API_KEY=do-not-use\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = ReadLegacyAdminKey(path); err == nil {
		t.Fatal("backend key used as public fallback")
	}
}
