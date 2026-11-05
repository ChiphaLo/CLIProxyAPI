package configbundle

import "testing"

func TestDevinLocalOriginIsExactAndDirect(t *testing.T) {
	if err := ValidateProviderOrigin(t.Context(), "http://127.0.0.1:8333/v1", "direct"); err != nil {
		t.Fatal(err)
	}
	for _, base := range []string{"http://127.0.0.1:8335/v1", "http://localhost:8333/v1", "http://127.0.0.1:8333/v1/", "http://127.0.0.1:8333/v1?x=1", "http://127.0.0.1:8333/admin", "http://127.0.0.1:8333/v1/../admin"} {
		if ValidateProviderOrigin(t.Context(), base, "direct") == nil {
			t.Fatalf("unapproved Devin origin accepted: %s", base)
		}
	}
	for _, proxy := range []string{"", "DIRECT", "socks5h://172.17.0.1:10808"} {
		if ValidateProviderOrigin(t.Context(), "http://127.0.0.1:8333/v1", proxy) == nil {
			t.Fatal("Devin adapter allowed via an implicit or alternate proxy")
		}
	}
}
