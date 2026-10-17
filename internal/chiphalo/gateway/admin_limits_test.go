package gateway

import (
	"net/http"
	"path/filepath"
	"testing"
	"time"
)

func TestAdminHTTPAndWebsocketTurnsHaveNoKeyLimits(t *testing.T) {
	l := NewAdmissionLimits(32)
	l.now = func() time.Time { return time.Unix(1800000000, 0) }
	var releases []func()
	for i := 0; i < 300; i++ {
		release, ok := l.AcquireKey("admin", 0, 0, true)
		if !ok || !l.ChargeTurn("admin", "192.0.2.1", 0, 0) {
			t.Fatal("admin key was rate/concurrency limited")
		}
		releases = append(releases, release)
	}
	if l.samples != 0 || len(l.ips) != 0 || len(l.keys["admin"].times) != 0 {
		t.Fatal("unlimited admin allocated per-minute samples")
	}
	for _, release := range releases {
		release()
		release()
	}
	if l.keys["admin"].active != 0 {
		t.Fatal("admin in-flight counter leaked")
	}
	for _, limits := range [][2]int{{0, 8}, {120, 0}, {0, 0}, {-1, 8}, {120, -1}} {
		if _, ok := l.AcquireKey("issued", limits[0], limits[1], true); ok {
			t.Fatal("issued key acquired an unlimited/invalid policy")
		}
		if l.ChargeTurn("issued", "192.0.2.1", limits[0], limits[1]) {
			t.Fatal("issued turn acquired an unlimited/invalid policy")
		}
	}
}

func TestOnlyFreshlyAuthenticatedAdminBypassesIPRPM(t *testing.T) {
	l, _, engine := liveFixture(t)
	l.limits.now = func() time.Time { return time.Unix(1800000000, 0) }
	for i := 0; i < 180; i++ {
		if w := liveRequest(engine, "admin-test-key", ""); w.Code != http.StatusOK {
			t.Fatalf("admin request %d failed: %d", i, w.Code)
		}
	}
	if len(l.limits.ips) != 0 || l.limits.samples != 0 {
		t.Fatal("admin requests consumed the shared IP budget")
	}
	for i := 0; i < 121; i++ {
		want := http.StatusUnauthorized
		if i == 120 {
			want = http.StatusTooManyRequests
		}
		if w := liveRequest(engine, "invalid-token", ""); w.Code != want {
			t.Fatalf("invalid request %d: %d, want %d", i, w.Code, want)
		}
	}
	if w := liveRequest(engine, "admin-test-key", ""); w.Code != http.StatusOK {
		t.Fatal("authenticated admin inherited an exhausted IP budget")
	}
	if w := liveRequest(engine, "issued-test-key", ""); w.Code != http.StatusTooManyRequests {
		t.Fatal("issued key bypassed shared IP protection")
	}
	atomicIdentityWrite(t, l.paths.AdminEnv, []byte("UNIFIED_GATEWAY_API_KEY=new-admin-test-key\n"))
	if w := liveRequest(engine, "admin-test-key", ""); w.Code != http.StatusTooManyRequests {
		t.Fatal("rotated old admin retained an RPM exemption")
	}
	if w := liveRequest(engine, "new-admin-test-key", ""); w.Code != http.StatusOK {
		t.Fatal("rotated current admin lost its exemption")
	}
	atomicIdentityWrite(t, filepath.Join(l.paths.ConfigDir, "oauth-accounts.json"), []byte(`{"accounts":`))
	if w := liveRequest(engine, "new-admin-test-key", ""); w.Code == http.StatusOK {
		t.Fatal("invalid identity state left admin authenticated")
	}
}

func TestAdminStillHonorsGlobalConcurrency(t *testing.T) {
	l, _, engine := liveFixture(t)
	for i := 0; i < cap(l.limits.slots); i++ {
		l.limits.slots <- struct{}{}
	}
	if w := liveRequest(engine, "admin-test-key", ""); w.Code != http.StatusTooManyRequests {
		t.Fatal("admin bypassed the global resource budget")
	}
	for len(l.limits.slots) > 0 {
		<-l.limits.slots
	}
	if w := liveRequest(engine, "admin-test-key", ""); w.Code != http.StatusOK {
		t.Fatal("admin did not recover after global slots were released")
	}
}
