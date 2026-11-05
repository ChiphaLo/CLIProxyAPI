package configbundle

import "testing"

func TestClaudeLocalOriginIsExactAndDirect(t *testing.T) {
	const base = "http://127.0.0.1:8334/v1"
	if err := ValidateProviderOrigin(t.Context(), base, "direct"); err != nil {
		t.Fatal(err)
	}
	for _, candidate := range []string{
		"http://127.0.0.1:8335/v1", "http://localhost:8334/v1", "http://[::1]:8334/v1",
		base + "/", base + "?x=1", base + "#fragment", base + "/../admin",
		"http://user:secret@127.0.0.1:8334/v1", "http://127.0.0.1:8334/admin",
	} {
		if ValidateProviderOrigin(t.Context(), candidate, "direct") == nil {
			t.Fatalf("unapproved Claude origin accepted: %s", candidate)
		}
	}
	for _, proxy := range []string{"", "DIRECT", "socks5h://172.17.0.1:10808"} {
		if ValidateProviderOrigin(t.Context(), base, proxy) == nil {
			t.Fatal("Claude core allowed via an implicit or alternate proxy")
		}
	}
}
