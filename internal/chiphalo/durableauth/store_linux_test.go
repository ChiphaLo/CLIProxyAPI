//go:build linux

package durableauth

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	sdkauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func fixtureAuth(t *testing.T, dir string) *coreauth.Auth {
	t.Helper()
	path := filepath.Join(dir, "test.json")
	meta := map[string]any{"type": "codex", "prefix": "codex2", "account_id": "test-team", "access_token": "test-access-old", "refresh_token": "test-refresh-old", "unknown_business_field": "keep"}
	raw, _ := json.Marshal(meta)
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	return &coreauth.Auth{ID: "test.json", FileName: "test.json", Provider: "codex", Prefix: "codex2", Metadata: meta, Attributes: map[string]string{coreauth.AttributePath: path}}
}

func TestNativeCodexSaveConfirmedWithoutLosingBusinessFieldsOrMode(t *testing.T) {
	dir, health := privateRoots(t)
	auth := fixtureAuth(t, dir)
	s, err := Open(dir, health, sdkauth.NewFileTokenStore(), false)
	if err != nil {
		t.Fatal(err)
	}
	auth.Metadata["access_token"] = "test-access-new"
	auth.Metadata["refresh_token"] = "test-refresh-new"
	path, err := s.Save(coreauth.WithAuthCreationIntent(context.Background()), auth)
	if err != nil || path != filepath.Join(dir, "test.json") || s.Err() != nil || s.Pending() {
		t.Fatalf("native confirmation failed: %v", err)
	}
	raw, _ := os.ReadFile(path)
	var disk map[string]any
	_ = json.Unmarshal(raw, &disk)
	if disk["unknown_business_field"] != "keep" || disk["prefix"] != "codex2" || disk["refresh_token"] != "test-refresh-new" {
		t.Fatal("upstream merge/identity/billing metadata was replaced")
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0600 {
		t.Fatal("credential permissions changed")
	}
	if _, err := os.Lstat(filepath.Join(health, markerName)); !os.IsNotExist(err) {
		t.Fatal("confirmed save left a restart fence")
	}
}

func TestAntigravityMetadataAndRefreshSaveRemainDurable(t *testing.T) {
	dir, health := privateRoots(t)
	auth := fixtureAuth(t, dir)
	auth.Provider, auth.Prefix = "antigravity", "antigravity"
	auth.Metadata["type"], auth.Metadata["prefix"] = "antigravity", "antigravity"
	auth.Metadata["project_id"], auth.Metadata["proxy_url"] = "project-fixture", "socks5h://127.0.0.1:10808"
	delete(auth.Metadata, "account_id")
	initial, _ := json.Marshal(auth.Metadata)
	path := filepath.Join(dir, "test.json")
	if err := os.WriteFile(path, initial, 0600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir, health, sdkauth.NewFileTokenStore(), false)
	if err != nil {
		t.Fatal(err)
	}
	auth.Metadata["antigravity_capability_hints"] = map[string]any{"project_id": "project-fixture", "endpoint": "https://daily-cloudcode-pa.googleapis.com", "model_ids": []any{"gemini-fixture"}}
	for _, refresh := range []bool{false, true} {
		if refresh {
			auth.Metadata["access_token"], auth.Metadata["refresh_token"] = "access-new", "refresh-new"
		}
		if saved, err := s.Save(context.Background(), auth); err != nil || saved != path || s.Err() != nil || s.Pending() {
			t.Fatalf("Antigravity save failed (refresh=%t): %v", refresh, err)
		}
		raw, err := os.ReadFile(path)
		var actual map[string]any
		if err != nil || json.Unmarshal(raw, &actual) != nil || !reflect.DeepEqual(actual, auth.Metadata) {
			t.Fatal("metadata, routing, project or rotated tokens changed during save")
		}
		info, _ := os.Stat(path)
		if info.Mode().Perm() != 0600 {
			t.Fatal("credential mode changed")
		}
		if _, err := os.Lstat(filepath.Join(health, markerName)); !os.IsNotExist(err) {
			t.Fatal("confirmed Antigravity save left a restart fence")
		}
	}
	if _, err := Open(dir, health, sdkauth.NewFileTokenStore(), false); err != nil {
		t.Fatalf("confirmed Antigravity save blocked restart: %v", err)
	}
}

func TestAntigravityConfirmationChecksProjectAndCapabilityHints(t *testing.T) {
	for _, key := range []string{"project_id", "proxy_url", "antigravity_capability_hints"} {
		t.Run(key, func(t *testing.T) {
			dir, health := privateRoots(t)
			auth := fixtureAuth(t, dir)
			auth.Provider, auth.Metadata["type"] = "antigravity", "antigravity"
			auth.Metadata[key] = "expected-fixture"
			inner := &testStore{save: func(_ context.Context, a *coreauth.Auth) (string, error) {
				wrong := maps.Clone(a.Metadata)
				wrong[key] = "wrong-fixture"
				raw, _ := json.Marshal(wrong)
				path := filepath.Join(dir, "test.json")
				return path, os.WriteFile(path, raw, 0600)
			}}
			s, err := Open(dir, health, inner, false)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.Save(context.Background(), auth); err != ErrUnconfirmed || s.Err() != ErrUnconfirmed {
				t.Fatal("wrong Antigravity postimage was accepted")
			}
		})
	}
}

func TestStoreFailedSaveAndRestartRemainFenced(t *testing.T) {
	dir, health := privateRoots(t)
	auth := fixtureAuth(t, dir)
	inner := &testStore{save: func(context.Context, *coreauth.Auth) (string, error) {
		return "", errors.New("unsafe backend diagnostic test-refresh-secret")
	}}
	s, err := Open(dir, health, inner, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Save(context.Background(), auth); err != ErrUnconfirmed || strings.Contains(err.Error(), "test-refresh-secret") {
		t.Fatal("failed save was not fenced or leaked backend details")
	}
	if s.Err() != ErrUnconfirmed {
		t.Fatal("a caller swallowing Save errors can still report ready")
	}
	if _, err := s.List(context.Background()); err != ErrUnconfirmed {
		t.Fatal("ambiguous credentials reloaded")
	}
	if _, err := Open(dir, health, inner, false); err != ErrUnconfirmed {
		t.Fatal("new process ignored the persistent failure fence")
	}
}

func TestStoreDetectsSilentWrongTokenAndWrongReturnedPath(t *testing.T) {
	for _, test := range []string{"wrong-token", "wrong-path"} {
		t.Run(test, func(t *testing.T) {
			dir, health := privateRoots(t)
			auth := fixtureAuth(t, dir)
			path := filepath.Join(dir, "test.json")
			inner := &testStore{save: func(context.Context, *coreauth.Auth) (string, error) {
				if test == "wrong-path" {
					return path + ".other", nil
				}
				return path, os.WriteFile(path, []byte(`{"type":"codex","refresh_token":"stale"}`), 0600)
			}}
			s, err := Open(dir, health, inner, false)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.Save(context.Background(), auth); err != ErrUnconfirmed || s.Err() != ErrUnconfirmed {
				t.Fatal("backend's success hid an unconfirmed save")
			}
		})
	}
}

func TestStoreArmsFenceBeforeMutationAndRejectsUnsafePath(t *testing.T) {
	dir, health := privateRoots(t)
	auth := fixtureAuth(t, dir)
	called := false
	inner := &testStore{save: func(context.Context, *coreauth.Auth) (string, error) {
		called = true
		if _, err := os.Stat(filepath.Join(health, markerName)); err != nil {
			t.Error("mutation began without a restart fence")
		}
		return "", errors.New("simulated crash during mutation")
	}}
	s, _ := Open(dir, health, inner, false)
	_, _ = s.Save(context.Background(), auth)
	if !called || s.Err() != ErrUnconfirmed {
		t.Fatal("mutation was not fenced")
	}
	dir, health = privateRoots(t)
	s, _ = Open(dir, health, inner, false)
	called = false
	auth.Attributes[coreauth.AttributePath] = filepath.Join(t.TempDir(), "outside.json")
	if _, err := s.Save(context.Background(), auth); err != ErrUnconfirmed || called {
		t.Fatal("a path outside the auth store reached the backend")
	}
}

func TestStoreRefusesNonPrivateHealthRoot(t *testing.T) {
	dir, health := privateRoots(t)
	_ = os.Chmod(health, 0777)
	if _, err := Open(dir, health, sdkauth.NewFileTokenStore(), false); err != ErrUnsafePath {
		t.Fatal("untrusted user can clear a persistence fence")
	}
}

func TestStoreHealthSurvivesSDKNonFatalRefreshPersistenceError(t *testing.T) {
	dir, health := privateRoots(t)
	auth := fixtureAuth(t, dir)
	inner := &testStore{save: func(context.Context, *coreauth.Auth) (string, error) { return "", errors.New("simulated disk failure") }}
	s, err := Open(dir, health, inner, false)
	if err != nil {
		t.Fatal(err)
	}
	manager := coreauth.NewManager(s, nil, nil)
	if _, err := manager.Register(coreauth.WithSkipPersist(context.Background()), auth); err != nil {
		t.Fatal(err)
	}
	base, _ := manager.GetByID(auth.ID)
	updated := base.Clone()
	updated.Metadata = maps.Clone(base.Metadata)
	updated.Metadata["access_token"] = "test-rotated-but-not-saved"
	saved, err := manager.UpdateRefreshedAuth(context.Background(), base, updated)
	if err != nil || saved == nil {
		t.Fatal("upstream non-fatal persistence policy changed", err)
	}
	if s.Err() != ErrUnconfirmed || saved.Metadata["access_token"] != "test-rotated-but-not-saved" {
		t.Fatal("SDK success masked the durable store health failure")
	}
	if _, err := Open(dir, health, inner, false); err != ErrUnconfirmed {
		t.Fatal("restart can silently load the stale disk token")
	}
}
