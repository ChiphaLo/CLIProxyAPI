package quota

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestOfficialProjectionAndWeeklyAnchor(t *testing.T) {
	u, err := Project([]byte(`{"plan_type":"team","access_token":"secret-fixture","rate_limit":{"allowed":true,"primary_window":{"used_percent":4,"limit_window_seconds":604800,"reset_at":2000000,"token":"secret-fixture"}},"credits":{"has_credits":false,"unlimited":true,"balance":"0","token":"secret-fixture"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if u.Rate.Primary.Used == nil || *u.Rate.Primary.Remaining != 96 {
		t.Fatal("wrong official quota projection")
	}
	start, end, ok := u.Weekly(2000001)
	if !ok || start != 2000000 || end != 2604800 {
		t.Fatal("wrong anchored cycle")
	}
	raw, _ := json.Marshal(u)
	if strings.Contains(string(raw), "secret-fixture") || strings.Contains(string(raw), "access_token") {
		t.Fatal("unknown field leaked")
	}
}
func TestUnknownQuotaIsNotZero(t *testing.T) {
	u, err := Project([]byte(`{"rate_limit":{"secondary_window":{"reset_at":2000000}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if u.Rate.Secondary.Used != nil || u.Rate.Secondary.Remaining != nil {
		t.Fatal("unknown quota turned into zero")
	}
	if _, _, ok := u.Weekly(1000000); ok {
		t.Fatal("unknown duration guessed to be weekly")
	}
	if _, err := Project([]byte(`{"plan_type":"team"}`)); err == nil {
		t.Fatal("missing official schema accepted")
	}
}
