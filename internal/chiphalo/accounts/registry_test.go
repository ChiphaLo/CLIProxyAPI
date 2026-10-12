package accounts

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func fixture(t *testing.T) (string, []Account, map[string]map[string]any) {
	t.Helper()
	dir := t.TempDir()
	rows := []Account{{ID: "primary-member", Prefix: "codex", AuthFile: "primary.json", WorkspaceID: "shared-team"}, {ID: "second-member", Prefix: "codex2", AuthFile: "second.json", WorkspaceID: "shared-team"}}
	auths := map[string]map[string]any{}
	for i, row := range rows {
		claims, _ := json.Marshal(map[string]any{"sub": row.ID, "email": row.ID + "@example.invalid"})
		auths[row.AuthFile] = map[string]any{"type": "codex", "prefix": row.Prefix, "account_id": "shared-team", "email": row.ID + "@example.invalid", "id_token": "e30." + base64.RawURLEncoding.EncodeToString(claims) + ".test", "access_token": "test-only", "disabled": false}
		if i == 0 {
			stamp := int64(100)
			rows[i].LegacySince = &stamp
		}
	}
	writeAuths(t, dir, auths)
	return dir, rows, auths
}

func writeAuths(t *testing.T, dir string, auths map[string]map[string]any) {
	t.Helper()
	for name, auth := range auths {
		raw, _ := json.Marshal(auth)
		if err := os.WriteFile(filepath.Join(dir, name), raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func parseFixture(t *testing.T, dir string, rows []Account) (*Registry, error) {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"accounts": rows})
	return Parse(raw, dir, []byte(`{"models":["gpt-6-astra",{"id":"codex/gpt-6-luna"}]}`))
}

func TestDifferentMembersSharingWorkspaceRemainSeparate(t *testing.T) {
	dir, rows, auths := fixture(t)
	r, err := parseFixture(t, dir, rows)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(r.PublicModelIDs(), []string{"codex2/gpt-6-astra", "codex2/gpt-6-luna", "gpt-6-astra", "gpt-6-luna"}) {
		t.Fatal("public account model separation changed")
	}
	for _, tc := range []struct{ model, wantModel, id string }{{"gpt-6-astra", "codex/gpt-6-astra", "primary.json"}, {"gpt-6-astra(max)", "codex/gpt-6-astra(max)", "primary.json"}, {"codex2/gpt-6-luna", "codex2/gpt-6-luna", "second.json"}} {
		model, id, err := r.Resolve(tc.model, "/v1/responses")
		if err != nil || model != tc.wantModel || id != tc.id {
			t.Fatalf("account routing changed: %s %s %v", model, id, err)
		}
	}
	for _, row := range rows {
		if err := r.ValidateAuth(&coreauth.Auth{ID: row.AuthFile, Provider: "codex", Prefix: row.Prefix, Metadata: auths[row.AuthFile]}); err != nil {
			t.Fatal(err)
		}
	}
	if model, id, err := r.Resolve("opencode/deepseek-v4.1-flash", "/v1/responses"); err != nil || id != "" || model != "opencode/deepseek-v4.1-flash" {
		t.Fatal("unrelated provider route changed")
	}
	if _, _, err := r.Resolve("codex2/gpt-6-luna", "/v1/chat/completions"); err == nil {
		t.Fatal("legacy OAuth endpoint policy widened")
	}
	if _, _, err := r.Resolve("codex2/unpublished", "/v1/responses"); err == nil {
		t.Fatal("unknown model authorized")
	}
}

func TestRegistryRejectsAmbiguousAndMisboundCredentials(t *testing.T) {
	for _, name := range []string{"duplicate-id", "duplicate-prefix", "shared-file", "wrong-workspace", "wrong-prefix", "same-member", "unregistered", "traversal", "missing-file", "missing-primary"} {
		t.Run(name, func(t *testing.T) {
			dir, rows, auths := fixture(t)
			switch name {
			case "duplicate-id":
				rows[1].ID = rows[0].ID
			case "duplicate-prefix":
				rows[1].Prefix = rows[0].Prefix
			case "shared-file":
				rows[1].AuthFile = rows[0].AuthFile
			case "wrong-workspace":
				auths["primary.json"]["account_id"] = "wrong-team"
			case "wrong-prefix":
				auths["second.json"]["prefix"] = "codex"
			case "same-member":
				auths["second.json"]["email"] = auths["primary.json"]["email"]
				auths["second.json"]["id_token"] = auths["primary.json"]["id_token"]
			case "unregistered":
				auths["extra.json"] = map[string]any{"type": "codex", "prefix": "codex3", "email": "extra@example.invalid", "account_id": "shared-team"}
			case "traversal":
				rows[1].AuthFile = "../second.json"
			case "missing-file":
				rows[1].AuthFile = "missing.json"
			case "missing-primary":
				rows[0].Prefix = "other"
				auths["primary.json"]["prefix"] = "other"
			}
			writeAuths(t, dir, auths)
			if _, err := parseFixture(t, dir, rows); err == nil {
				t.Fatal("unsafe account registry accepted")
			}
		})
	}
}

func TestCredentialReplacementAndDisableCannotCrossAccounts(t *testing.T) {
	dir, rows, auths := fixture(t)
	r, err := parseFixture(t, dir, rows)
	if err != nil {
		t.Fatal(err)
	}
	auth := &coreauth.Auth{ID: "primary.json", Provider: "codex", Prefix: "codex", Metadata: auths["primary.json"]}
	auth.Metadata["email"] = "replaced@example.invalid"
	if r.ValidateAuth(auth) == nil {
		t.Fatal("credential replaced with another member was accepted")
	}
	auth.Metadata = auths["second.json"]
	if r.ValidateAuth(auth) == nil {
		t.Fatal("same workspace authorized another member")
	}
	// Disable is independent of the intentionally invalid replacement above.
	dir, rows, auths = fixture(t)
	auths["primary.json"]["disabled"] = true
	writeAuths(t, dir, auths)
	r, err = parseFixture(t, dir, rows)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.Resolve("gpt-6-astra", "/v1/responses"); err == nil {
		t.Fatal("disabled primary silently fell back")
	}
	if _, id, err := r.Resolve("codex2/gpt-6-astra", "/v1/responses"); err != nil || id != "second.json" {
		t.Fatal("disabling primary disabled unrelated member")
	}
}
