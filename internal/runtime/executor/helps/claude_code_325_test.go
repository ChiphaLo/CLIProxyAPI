package helps

import (
	"context"
	"net/http"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestClaude325UpgradeCachedSoftwareKeepsIdentity(t *testing.T) {
	ResetClaudeDeviceProfileCache()
	t.Cleanup(ResetClaudeDeviceProfileCache)
	auth := &coreauth.Auth{ID: "same-account"}
	old := claudeClientCompatibilityProfile(nil)
	key := claudeDeviceProfileCacheKey(auth, "same-key", ClaudeDeviceProfile{})
	claudeDeviceProfileCache[key] = claudeDeviceProfileCacheEntry{profile: old, expire: time.Now().Add(time.Hour)}
	got, err := ResolveClaudeDeviceProfileRequired(context.Background(), auth, "same-key", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.UserAgent != "claude-cli/2.1.325 (external, cli)" || got.PackageVersion != "0.128.0" || got.RuntimeVersion != "v26.12.1" {
		t.Fatalf("stale software tuple survived: %+v", got)
	}
	if got.OS != old.OS || got.Arch != old.Arch || key != claudeDeviceProfileCacheKey(auth, "same-key", ClaudeDeviceProfile{}) {
		t.Fatal("account/platform scope changed")
	}
	r, _ := http.NewRequest("POST", "http://127.0.0.1", nil)
	ApplyClaudeDeviceProfileHeaders(r, got)
	if r.Header.Get("User-Agent") != got.UserAgent || DefaultClaudeVersion(nil) != "2.1.325" {
		t.Fatal("version fields disagree")
	}
}
