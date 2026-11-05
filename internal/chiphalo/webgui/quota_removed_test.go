package webgui

import (
	"bytes"
	"crypto/sha256"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

func TestQuotaModelRemovedFromGUIWithoutRemovingAccountBilling(t *testing.T) {
	for _, removed := range []string{"Quota Model", "quotaModelPanel", "loadQuotaModel", "/quota-model", ".qm-"} {
		if bytes.Contains(index, []byte(removed)) {
			t.Fatalf("removed fitting UI or polling remains: %s", removed)
		}
	}
	for _, retained := range []string{"loadOAuthAccounts(accounts)", "loadOAuthResetCredits", "当前额度周期内的 CPA 估算费用", "renderUsage"} {
		if !bytes.Contains(index, []byte(retained)) {
			t.Fatalf("independent account or usage feature removed: %s", retained)
		}
	}
}

func TestRetiredQuotaModelReturnsGoneAndPreservesStatistics(t *testing.T) {
	s, _ := fixtureServer(t)
	before := map[string][32]byte{}
	for _, p := range []string{s.options.Usage, s.options.Quota, s.auth.config} {
		raw, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		before[p] = sha256.Sum256(raw)
	}
	token, err := s.auth.login("local", "fixture-password")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/gui/api/quota-model", "/gui/api/quota-model?window=all&cycles=24&fresh=1", "/gui/api/oauth-accounts?include_reset=0", "/gui/api/usage"} {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.AddCookie(&http.Cookie{Name: "cpa_session", Value: token})
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		want := http.StatusOK
		if bytes.Contains([]byte(path), []byte("quota-model")) {
			want = http.StatusGone
		}
		if w.Code != want {
			t.Fatalf("unexpected status for %s: %d %s", path, w.Code, w.Body.String())
		}
	}
	for p, hash := range before {
		raw, err := os.ReadFile(p)
		if err != nil || sha256.Sum256(raw) != hash {
			t.Fatalf("feature removal changed accounting or quota data: %s", p)
		}
	}
}
