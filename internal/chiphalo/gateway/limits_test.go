package gateway

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func TestLimitsApplyLoweredPolicyWithoutResettingLiveCalls(t *testing.T) {
	l := NewAdmissionLimits(32)
	now := time.Unix(1800000000, 0)
	l.now = func() time.Time { return now }
	a, ok := l.AcquireKey("fixture", 120, 2, true)
	if !ok {
		t.Fatal("first admission failed")
	}
	b, ok := l.AcquireKey("fixture", 120, 2, true)
	if !ok {
		t.Fatal("second admission failed")
	}
	if _, ok := l.AcquireKey("fixture", 120, 1, true); ok {
		t.Fatal("lower limit replaced active counter")
	}
	a()
	a()
	if _, ok := l.AcquireKey("fixture", 120, 1, true); ok {
		t.Fatal("release counter decremented twice")
	}
	b()
	if _, ok := l.AcquireKey("fixture", 2, 1, true); ok {
		t.Fatal("changing RPM reset previous usage")
	}
	now = now.Add(time.Minute)
	c, ok := l.AcquireKey("fixture", 2, 1, true)
	if !ok {
		t.Fatal("minute boundary did not expire old admissions")
	}
	c()
}

func TestIPLimitsUseTrustedClientAddressAndExpireExactly(t *testing.T) {
	l := NewAdmissionLimits(32)
	now := time.Unix(1800000000, 0)
	l.now = func() time.Time { return now }
	engine := gin.New()
	_ = engine.SetTrustedProxies(nil)
	engine.Use(l.Global())
	engine.GET("/fixture", func(c *gin.Context) { c.Status(200) })
	for i := 0; i < 121; i++ {
		r := httptest.NewRequest("GET", "/fixture", nil)
		r.RemoteAddr = "192.0.2.1:1234"
		r.Header.Set("X-Forwarded-For", "198.51.100.99")
		w := httptest.NewRecorder()
		engine.ServeHTTP(w, r)
		want := 200
		if i == 120 {
			want = 429
		}
		if w.Code != want {
			t.Fatalf("IP boundary %d: %d", i, w.Code)
		}
	}
	now = now.Add(time.Minute)
	if !l.chargeIP("192.0.2.1") {
		t.Fatal("old window not expired")
	}
}

func TestWebsocketTurnsAndTrackingHaveFiniteBudgets(t *testing.T) {
	l := NewAdmissionLimits(32)
	if !l.ChargeTurn("fixture", "192.0.2.1", 1, 8) {
		t.Fatal("first turn failed")
	}
	if l.ChargeTurn("fixture", "192.0.2.1", 1, 8) {
		t.Fatal("WS frame bypassed per-key RPM")
	}
	l.samples = maxLimitSamples
	if l.chargeIP("192.0.2.2") {
		t.Fatal("bookkeeping exhausted but admission failed open")
	}
	if _, ok := l.AcquireKey("other", 120, 8, true); ok {
		t.Fatal("key admission bypassed tracking cap")
	}
}
