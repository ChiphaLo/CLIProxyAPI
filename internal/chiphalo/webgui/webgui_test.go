package webgui

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/pbkdf2"
)

type fixtureHelper struct{}

func (fixtureHelper) Call(context.Context, string, map[string]any) (map[string]any, error) {
	return map[string]any{"ok": true, "keys": []any{}, "models": []any{}, "provider_secrets": map[string]any{}, "status": map[string]any{"gateway": "active"}, "codex_context": map[string]any{}}, nil
}
func fixtureServer(t *testing.T) (*Server, *time.Time) {
	t.Helper()
	dir := t.TempDir()
	now := time.Unix(1791892314, 0)
	salt := "legacy-salt"
	hash := pbkdf2.Key([]byte("fixture-password"), []byte(salt), 200000, 32, sha256.New)
	write := func(name string, value any) {
		raw, _ := json.Marshal(value)
		if err := os.WriteFile(filepath.Join(dir, name), raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("gui.json", map[string]any{"admin": map[string]any{"salt": salt, "hash": hex.EncodeToString(hash)}, "secret": strings.Repeat("s", 64), "session_epoch": 1, "custom": "keep", "prices": map[string]any{"gpt-6-astra": map[string]any{"input": 10, "cached": 1, "output": 50}}})
	write("sessions.json", map[string]any{})
	write("state.json", map[string]any{"providers": []any{}, "managedPrefixes": []any{}, "custom": "keep"})
	write("quota.json", map[string]any{"accounts": []any{}})
	write("exposed.json", map[string]any{"models": []string{"gpt-6-astra"}})
	if err := os.WriteFile(filepath.Join(dir, "usage.jsonl"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	s, err := New(Options{Data: dir, Usage: filepath.Join(dir, "usage.jsonl"), Quota: filepath.Join(dir, "quota.json"), Exposed: filepath.Join(dir, "exposed.json"), Origin: "https://cpa.chipha.de", Helper: fixtureHelper{}, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	return s, &now
}
func TestLegacyLoginPersistentSessionAndRevocation(t *testing.T) {
	s, clock := fixtureServer(t)
	token, err := s.auth.login("127.0.0.1", "fixture-password")
	if err != nil {
		t.Fatal(err)
	}
	if !s.auth.verify(token) {
		t.Fatal("login invalid")
	}
	restarted, err := New(s.options)
	if err != nil || !restarted.auth.verify(token) {
		t.Fatal("session lost on restart")
	}
	if restarted.auth.verify(token + "x") {
		t.Fatal("tampered session accepted")
	}
	if err := s.auth.changePassword("fixture-password", "new-long-fixture-password"); err != nil {
		t.Fatal(err)
	}
	if s.auth.verify(token) {
		t.Fatal("old session survived password change")
	}
	if _, err := s.auth.login("127.0.0.1", "new-long-fixture-password"); err != nil {
		t.Fatal("argon2 login failed")
	}
	var doc map[string]any
	if readJSON(s.auth.config, 2*1024*1024, true, &doc) != nil || doc["custom"] != "keep" || doc["prices"] == nil {
		t.Fatal("password change reset pricing/unknown data")
	}
	*clock = clock.Add(31 * time.Minute)
	if restarted.auth.verify(token) {
		t.Fatal("idle token accepted")
	}
}
func TestLoginRateLimitAndTrustedIP(t *testing.T) {
	s, _ := fixtureServer(t)
	for i := 0; i < 8; i++ {
		_, _ = s.auth.login("192.0.2.1", "wrong")
	}
	if _, err := s.auth.login("192.0.2.1", "fixture-password"); err == nil || err.Error() != "too many attempts" {
		t.Fatal("login not limited")
	}
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "127.0.0.1:5000"
	r.Header.Set("X-Forwarded-For", "8.8.8.8")
	if s.ip(r) != "127.0.0.1" {
		t.Fatal("untrusted forwarding header accepted")
	}
	s.trusted[netip.MustParseAddr("127.0.0.1")] = true
	if s.ip(r) != "8.8.8.8" {
		t.Fatal("trusted forwarding ignored")
	}
}
func TestGUIOriginCSRFAndSessionRefresh(t *testing.T) {
	s, _ := fixtureServer(t)
	token, err := s.auth.login("local", "fixture-password")
	if err != nil {
		t.Fatal(err)
	}
	check := func(path, method, origin, csrf string, authenticated bool) int {
		r := httptest.NewRequest(method, path, strings.NewReader(`{}`))
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		r.Header.Set("X-CSRF-Token", csrf)
		if authenticated {
			r.AddCookie(&http.Cookie{Name: "cpa_session", Value: token})
		}
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		return w.Code
	}
	if check("/gui/api/state", "GET", "", "", true) != 200 {
		t.Fatal("refresh did not retain login")
	}
	if check("/gui/api/state", "GET", "", "", false) != 401 {
		t.Fatal("unauthenticated state readable")
	}
	if check("/gui/api/keys", "POST", "https://evil.example", s.auth.csrf(token), true) != 403 {
		t.Fatal("cross-origin write accepted")
	}
	if check("/gui/api/keys", "POST", s.options.Origin, "", true) != 403 {
		t.Fatal("missing CSRF accepted")
	}
	if check("/gui/api/keys", "POST", s.options.Origin, s.auth.csrf(token), true) != 200 {
		t.Fatal("authorized write rejected")
	}
}
func TestUsageWallClockGapsAndCosts(t *testing.T) {
	s, _ := fixtureServer(t)
	path := s.options.Usage
	data := `{"ts":1000,"model":"gpt-6-astra","input":100,"cached":20,"output":10,"duration_ms":2000,"ttft_ms":100,"output_tps":5}
{"ts":2200,"model":"gpt-6-astra","input":0,"cached":0,"output":0,"error":"failed"}
{"ts":900,"model":"old-outside-window","input":1000}
`
	if os.WriteFile(path, []byte(data), 0600) != nil {
		t.Fatal("write failed")
	}
	result, err := s.usage(url.Values{"start": {"1000"}, "end": {"2500"}, "bucket": {"300"}})
	if err != nil {
		t.Fatal(err)
	}
	buckets := result["byBucket"].([]map[string]any)
	if len(buckets) != 6 || buckets[1]["requests"] != int64(0) || buckets[1]["ts"] != int64(1200) {
		t.Fatal("empty time gaps removed")
	}
	models := result["models"].([]string)
	if len(models) != 1 || models[0] != "gpt-6-astra" {
		t.Fatal("model filter ignores time")
	}
	cost := result["totals"].(map[string]any)["cost"].(float64)
	if cost != 0.00132 {
		t.Fatalf("wrong cost %f", cost)
	}
	recent := result["recent"].([]map[string]any)
	if len(recent) != 2 || recent[0]["error"] != "failed" {
		t.Fatal("failed request dropped")
	}
}
func TestArgonRejectsUnboundedHashParameters(t *testing.T) {
	var c authConfig
	c.Admin.Scheme = "argon2id"
	c.Admin.Hash = "$argon2id$v=19$m=4294967295,t=8,p=4$aaaaaaaaaaaa$bbbbbbbbbbbbbbbbbbbbbb"
	if verifyPassword(c, "anything") {
		t.Fatal("unsafe hash accepted")
	}
}

func TestProviderDraftPersistencePreservesUnknownsAndBackup(t *testing.T) {
	s, _ := fixtureServer(t)
	s.options.Resolve = func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
	}
	token, err := s.auth.login("local", "fixture-password")
	if err != nil {
		t.Fatal(err)
	}
	request := func(path, method, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Origin", s.options.Origin)
		r.Header.Set("X-CSRF-Token", s.auth.csrf(token))
		r.AddCookie(&http.Cookie{Name: "cpa_session", Value: token})
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		return w
	}
	body := `{"id":"fixture","prefix":"fixture","name":"Fixture","baseUrl":"https://provider.example/v1","apiKey":"never-store-plaintext","models":[{"clientName":"m","upstreamName":"m","thinking":["high","ultra"],"inputModalities":["text","image"]}]}`
	if w := request("/api/providers", "POST", body); w.Code != 200 {
		t.Fatalf("create failed: %d", w.Code)
	}
	state, err := s.state()
	if err != nil || state["custom"] != "keep" {
		t.Fatal("state root fields lost")
	}
	raw, _ := os.ReadFile(filepath.Join(s.options.Data, "state.json"))
	if strings.Contains(string(raw), "never-store-plaintext") {
		t.Fatal("provider secret persisted in GUI state")
	}
	if w := request("/api/providers/fixture", "PUT", `{"models":[{"clientName":"bad/path","upstreamName":"m"}]}`); w.Code != 400 {
		t.Fatal("invalid local model persisted")
	}
	after, _ := os.ReadFile(filepath.Join(s.options.Data, "state.json"))
	if string(after) != string(raw) {
		t.Fatal("rejected edit changed inventory")
	}
	if w := request("/api/providers/fixture", "PUT", `{"models":[{"clientName":"m","upstreamName":"m","enabled":false,"thinking":[],"inputModalities":["text"]}]}`); w.Code != 200 {
		t.Fatal("valid model edit failed")
	}
	if w := request("/api/providers/fixture", "DELETE", `{}`); w.Code != 200 {
		t.Fatal("delete failed")
	}
	state, _ = s.state()
	if len(state["providers"].([]any)) != 0 || len(state["removedProviders"].([]any)) != 1 {
		t.Fatal("provider tombstone lost")
	}
	files, _ := os.ReadDir(filepath.Join(s.options.Data, "backups"))
	if len(files) != 3 {
		t.Fatal("state backups missing")
	}
}
func TestGUIRejectsDuplicateInputBeforeHelper(t *testing.T) {
	s, _ := fixtureServer(t)
	r := httptest.NewRequest("POST", "/api/login", strings.NewReader(`{"password":"first","password":"second"}`))
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 400 {
		t.Fatal("duplicate request field accepted")
	}
}
func TestQuotaBoundaryDoesNotExposeUnknownSecrets(t *testing.T) {
	value := map[string]any{"id": "one", "prefix": "codex", "status": "ok", "access_token": "private", "quota": map[string]any{"plan_type": "team", "secret": "private", "rate_limit": map[string]any{"primary_window": map[string]any{"used_percent": float64(4), "access_token": "private"}}}}
	raw, _ := json.Marshal(publicAccount(value))
	if strings.Contains(string(raw), "private") || strings.Contains(string(raw), "access_token") {
		t.Fatal("quota projection leaked unknown secret")
	}
}
func TestGUIConcurrencyIsBounded(t *testing.T) {
	s, _ := fixtureServer(t)
	for i := 0; i < cap(s.slots); i++ {
		s.slots <- struct{}{}
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
	if w.Code != 429 || w.Header().Get("Retry-After") != "1" {
		t.Fatal("concurrency bound not enforced")
	}
}
