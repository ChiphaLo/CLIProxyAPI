package clientkeys

import "testing"

func TestAdminLimitsAreUnlimitedWithoutChangingIssuedDefaults(t *testing.T) {
	p, err := New("admin-test-only", keyRaw(t, "issued-test-only", nil))
	if err != nil {
		t.Fatal(err)
	}
	if rpm, concurrent, ok := p.Limits("admin"); !ok || rpm != 0 || concurrent != 0 {
		t.Fatal("admin limits are not unlimited")
	}
	if rpm, concurrent, ok := p.Limits("client-1"); !ok || rpm != 120 || concurrent != 8 {
		t.Fatal("issued-key defaults changed")
	}
	if _, _, ok := p.Limits("missing"); ok {
		t.Fatal("unknown key became active")
	}
}
