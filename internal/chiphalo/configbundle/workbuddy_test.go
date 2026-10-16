package configbundle

import (
	"context"
	"net/netip"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestWorkBuddyLocalOriginsAreExactDirectOnly(t *testing.T) {
	for _, base := range []string{"http://127.0.0.1:8331/v1", "http://127.0.0.1:8332/v1"} {
		resolve := func(context.Context, string) ([]netip.Addr, error) {
			t.Fatal("fixed local worker must not perform DNS")
			return nil, nil
		}
		if ValidateProviderOriginWithResolver(t.Context(), base, "direct", resolve) != nil {
			t.Fatal("fixed worker rejected")
		}
	}
	for _, base := range []string{"http://127.0.0.1:8331", "http://127.0.0.1:8331/v1/", "http://127.0.0.1:8331/v1?x=1", "http://127.0.0.1:8331/v1#x", "http://127.0.0.1:8331/v1/../admin", "http://127.0.0.1:8333/v1", "http://localhost:8331/v1", "http://[::1]:8331/v1", "http://127.0.0.1:8320/v1", "http://user@127.0.0.1:8331/v1", "https://example.com/v1"} {
		if WorkBuddyLocalOrigin(base, "direct") || ValidateProviderOrigin(t.Context(), base, "direct") == nil {
			t.Fatalf("arbitrary origin/proxy accepted: %s", base)
		}
	}
	for _, proxy := range []string{"", "socks5h://172.17.0.1:10808", "none", "DIRECT"} {
		if ValidateProviderOrigin(t.Context(), "http://127.0.0.1:8331/v1", proxy) == nil {
			t.Fatal("worker was allowed through an implicit/alternate proxy")
		}
	}
}

func TestWorkBuddyBundleKeepsRealmsIndependentAndOtherProviders(t *testing.T) {
	in := fixture()
	in.State = []byte(`{"managedPrefixes":["workbuddy-cn","workbuddy-intl"],"providers":[{"id":"workbuddy_cn","prefix":"workbuddy-cn","baseUrl":"http://127.0.0.1:8331/v1","proxyUrl":"direct","models":[{"clientName":"glm-5.2","upstreamName":"glm-5.2","inputModalities":["text"]}]},{"id":"workbuddy_intl","prefix":"workbuddy-intl","baseUrl":"http://127.0.0.1:8332/v1","proxyUrl":"direct","models":[{"clientName":"glm-5.2","upstreamName":"glm-5.2","inputModalities":["text"]}]}]}`)
	in.Vault = []byte(`{"workbuddy_cn":"domestic-fixture","workbuddy_intl":"international-fixture"}`)
	out, err := Build(t.Context(), in)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.ParseConfigBytes(out.Config)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"workbuddy-cn": "domestic-fixture", "workbuddy-intl": "international-fixture"}
	for _, p := range cfg.OpenAICompatibility {
		secret, ok := want[p.Prefix]
		if !ok {
			continue
		}
		if len(p.APIKeyEntries) != 1 || p.APIKeyEntries[0].APIKey != secret || p.APIKeyEntries[0].ProxyURL != "direct" || len(p.Models) != 1 || p.Models[0].Name != "glm-5.2" {
			t.Fatal("worker credentials, direct transport or models were mixed")
		}
		delete(want, p.Prefix)
	}
	if len(want) != 0 || len(cfg.OpenAICompatibility) != 3 || !strings.Contains(string(out.Config), "foreign-codex") || !strings.Contains(string(out.Config), "manual-fixture") {
		t.Fatal("worker or unrelated provider inventory changed")
	}
}
