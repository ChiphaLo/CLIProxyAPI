//go:build linux

package adminhelper

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/catalogsync"
)

type codexSettingsBackend struct {
	dir     string
	reloads int
	failNew bool
}

func (b *codexSettingsBackend) Catalog(context.Context) ([]string, error) {
	raw, err := os.ReadFile(filepath.Join(b.dir, catalogFiles[1]))
	if err != nil {
		return nil, err
	}
	var d codexExposureDocument
	if json.Unmarshal(raw, &d) != nil {
		return nil, errors.New("invalid fixture")
	}
	out := []string{"provider/old"}
	for id, names := range d.Accounts {
		prefix := "codex"
		if id == "two" {
			prefix = "codex2"
		}
		for _, name := range names {
			out = append(out, codexPublicModel(prefix, name))
		}
	}
	slices.Sort(out)
	return out, nil
}
func (b *codexSettingsBackend) Await(ctx context.Context, digest string, models []string) error {
	raw, _ := os.ReadFile(filepath.Join(b.dir, configFiles[0]))
	actual, err := b.Catalog(ctx)
	if err != nil || digest != hash(raw) || !slices.Equal(actual, models) {
		return errors.New("fixture not ready")
	}
	return nil
}
func (b *codexSettingsBackend) ReloadReviewed(context.Context) error {
	b.reloads++
	raw, _ := os.ReadFile(filepath.Join(b.dir, catalogFiles[1]))
	if b.failNew && strings.Contains(string(raw), "account_settings") {
		return errors.New("registration fault")
	}
	return nil
}
func codexSettingsFixture(t *testing.T) (*Service, ApplyOptions, *codexSettingsBackend, map[string][]byte) {
	s, opts, old := applyFixture(t)
	docs := map[string]string{
		"oauth-accounts.json": `{"accounts":[{"id":"one","prefix":"codex","label":"Main","auth_file":"private-credential.json"},{"id":"two","prefix":"codex2","label":"Second","auth_file":"other-credential.json"}]}`,
		catalogFiles[1]:       `{"models":["gpt-6-astra","gpt-6-luna"],"account_models":{"one":["gpt-6-astra","gpt-6-luna"],"two":["gpt-6-astra"]},"extension":"keep"}`,
		catalogFiles[0]:       `{"models":{"gpt-6-astra":{"context_window":272000,"supported_reasoning_levels":[{"effort":"high"}],"access_token":"do-not-export"},"gpt-6-luna":{"context_window":272000,"supported_reasoning_levels":["low","high"]},"codex2/gpt-6-astra":{"context_window":200000,"supported_reasoning_levels":["high"]},"provider/old":{"keep":true}}}`,
	}
	for n, v := range docs {
		if err := os.WriteFile(filepath.Join(opts.ConfigDir, n), []byte(v), 0640); err != nil {
			t.Fatal(err)
		}
		old[n] = []byte(v)
	}
	b := &codexSettingsBackend{dir: opts.ConfigDir}
	opts.Backend = b
	if err := s.EnableApply(t.Context(), opts); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/accounts" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(400)
			return
		}
		_, _ = w.Write([]byte(`{"ok":true,"accounts":[{"id":"one","prefix":"codex","enabled":true},{"id":"two","prefix":"codex2","enabled":true}]}`))
	}))
	t.Cleanup(server.Close)
	s.inventory = &HTTPBackend{private: true, origin: server.URL, client: server.Client()}
	return s, opts, b, old
}
func codexPayload(s *Service, t *testing.T, id string, enabled bool, disabled []string) map[string]json.RawMessage {
	t.Helper()
	view := s.dispatch("codex_accounts", nil)
	requireOK(t, view)
	values := map[string]any{"account_id": id, "revision": view["revision"], "name": "Edited account", "enabled": enabled, "disabled_models": disabled}
	out := map[string]json.RawMessage{}
	for k, v := range values {
		out[k], _ = json.Marshal(v)
	}
	return out
}
func TestCodexManagementKeepsSeparateAccountsAndReviewedInventory(t *testing.T) {
	s, opts, b, old := codexSettingsFixture(t)
	view := s.dispatch("codex_accounts", nil)
	requireOK(t, view)
	raw, _ := json.Marshal(view)
	for _, secret := range []string{"access_token", "do-not-export", "auth_file", "private-credential.json"} {
		if strings.Contains(string(raw), secret) {
			t.Fatal("private data exported")
		}
	}
	requireOK(t, s.dispatch("update_codex_account", codexPayload(s, t, "one", true, []string{"gpt-6-astra"})))
	actual, _ := b.Catalog(t.Context())
	if !slices.Equal(actual, []string{"codex2/gpt-6-astra", "gpt-6-luna", "provider/old"}) {
		t.Fatal(actual)
	}
	requireOK(t, s.dispatch("update_codex_account", codexPayload(s, t, "one", false, []string{"gpt-6-astra"})))
	actual, _ = b.Catalog(t.Context())
	if !slices.Equal(actual, []string{"codex2/gpt-6-astra", "provider/old"}) {
		t.Fatal(actual)
	}
	requireOK(t, s.dispatch("update_codex_account", codexPayload(s, t, "one", true, []string{})))
	actual, _ = b.Catalog(t.Context())
	if !slices.Equal(actual, []string{"codex2/gpt-6-astra", "gpt-6-astra", "gpt-6-luna", "provider/old"}) {
		t.Fatal(actual)
	}
	d, err := s.apply.codexDocuments()
	if err != nil || len(d.exposed.Reviewed["one"]) != 2 || d.exposed.Settings["two"].Name != "" {
		t.Fatal("reviewed inventory or other account changed", err)
	}
	for _, n := range []string{configFiles[0], configFiles[1], catalogFiles[0], "oauth-accounts.json"} {
		raw, _ := os.ReadFile(filepath.Join(opts.ConfigDir, n))
		if string(raw) != string(old[n]) {
			t.Fatalf("unrelated file changed: %s", n)
		}
	}
	if b.reloads != 3 {
		t.Fatalf("SDK reloads %d", b.reloads)
	}
}
func TestCodexManagementRejectsStaleUnreviewedCrossAccountAndBindingWrites(t *testing.T) {
	for _, kind := range []string{"stale", "other-account-model", "prefix", "api-key", "bad-name", "null-enabled", "duplicate-model", "unknown-account"} {
		t.Run(kind, func(t *testing.T) {
			s, opts, b, old := codexSettingsFixture(t)
			payload := codexPayload(s, t, "two", true, []string{})
			switch kind {
			case "stale":
				payload["revision"] = json.RawMessage(`"` + strings.Repeat("0", 64) + `"`)
			case "other-account-model":
				payload["disabled_models"] = json.RawMessage(`["gpt-6-luna"]`)
			case "prefix":
				payload["prefix"] = json.RawMessage(`"other"`)
			case "api-key":
				payload["access_token"] = json.RawMessage(`"secret"`)
			case "bad-name":
				payload["name"] = json.RawMessage(`"a\nb"`)
			case "null-enabled":
				payload["enabled"] = json.RawMessage(`null`)
			case "duplicate-model":
				payload["disabled_models"] = json.RawMessage(`["gpt-6-astra","gpt-6-astra"]`)
			case "unknown-account":
				payload["account_id"] = json.RawMessage(`"unknown"`)
			}
			if s.dispatch("update_codex_account", payload)["ok"] == true {
				t.Fatal("invalid edit accepted")
			}
			for n, want := range old {
				raw, _ := os.ReadFile(filepath.Join(opts.ConfigDir, n))
				if string(raw) != string(want) {
					t.Fatal("rejected edit wrote file", n)
				}
			}
			if b.reloads != 0 {
				t.Fatal("rejected edit reloaded SDK")
			}
		})
	}
}
func TestCodexManagementSDKFailureRollsBack(t *testing.T) {
	s, opts, b, old := codexSettingsFixture(t)
	b.failNew = true
	result := s.dispatch("update_codex_account", codexPayload(s, t, "one", false, []string{}))
	if result["ok"] == true || result["rollback"] != true {
		t.Fatal(result)
	}
	for n, want := range old {
		raw, _ := os.ReadFile(filepath.Join(opts.ConfigDir, n))
		if string(raw) != string(want) {
			t.Fatal("rollback did not preserve original", n)
		}
	}
	pending(t, opts, false)
}
func TestCodexAccountSettingValidation(t *testing.T) {
	if catalogsync.ValidateAccountSetting(catalogsync.AccountSetting{Name: "Name", Enabled: true, DisabledModels: []string{}}, []string{"known"}) != nil {
		t.Fatal("valid selection rejected")
	}
}
