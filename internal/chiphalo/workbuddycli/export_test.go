package workbuddycli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAccessProjectionNeverCopiesRefreshTokenAndRejectsAccountSwitch(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	source, dest := filepath.Join(dir, "original.json"), filepath.Join(dir, "access.json")
	credential := map[string]any{"type": "codebuddy", "domain": "www.workbuddy.ai", "uid": "expected-account", "accessToken": "access-fixture", "refreshToken": "never-export-refresh-fixture", "expiresAt": time.Now().Unix() + 86400}
	write := func() {
		raw, _ := json.Marshal(credential)
		if err := os.WriteFile(source, raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	write()
	if err := ExportAccess(source, dest, "expected-account"); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "refresh") || !strings.Contains(string(raw), "access-fixture") {
		t.Fatalf("unsafe projection: %s", raw)
	}
	credential["uid"] = "other-account"
	write()
	if err := ExportAccess(source, dest, "expected-account"); err == nil {
		t.Fatal("silently switched source account")
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatal("stale access projection survived account mismatch")
	}
}

func TestAccessProjectionRejectsExpiredAndDisabledCredentials(t *testing.T) {
	for _, disabled := range []bool{true, false} {
		dir := t.TempDir()
		source, dest := filepath.Join(dir, "original.json"), filepath.Join(dir, "access.json")
		raw, _ := json.Marshal(map[string]any{"type": "codebuddy", "domain": "www.workbuddy.ai", "uid": "expected-account", "accessToken": "fixture", "disabled": disabled, "expiresAt": time.Now().Unix() - 1})
		if err := os.WriteFile(source, raw, 0600); err != nil {
			t.Fatal(err)
		}
		if err := ExportAccess(source, dest, "expected-account"); err == nil {
			t.Fatal("exported unavailable credential")
		}
	}
}
