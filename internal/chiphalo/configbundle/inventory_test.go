package configbundle

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestInventoryProjectsOnlyPublicProviderMetadata(t *testing.T) {
	in := fixture()
	out, err := Build(t.Context(), in)
	if err != nil {
		t.Fatal(err)
	}
	for _, v8 := range []bool{false, true} {
		t.Run(map[bool]string{false: "legacy", true: "v8"}[v8], func(t *testing.T) {
			raw := out.Config
			if v8 {
				raw = []byte(`access: {api-keys: [internal-secret]}
api-keys:
  openai-compatibility:
    - name: cpa-gui-provider-chat
      prefix: provider
      base-url: https://provider.example/v1
      headers: {Authorization: private-header}
      keys: [{api-key: upstream-private, proxy-url: socks5h://172.17.0.1:10808}]
      models: [{name: upstream, alias: client, input-modalities: [text, image], thinking: {levels: [high, ultra]}}]
`)
			}
			value, err := Inventory(raw, out.Context, []byte(`{"models":["gpt-6-astra","codex2/gpt-6-astra"]}`))
			if err != nil {
				t.Fatal(err)
			}
			encoded, _ := json.Marshal(value)
			for _, secret := range []string{"new-fixture", "manual-fixture", "upstream-private", "internal-secret", "private-header", "other-fixture"} {
				if strings.Contains(string(encoded), secret) {
					t.Fatal("inventory leaked a secret")
				}
			}
			providers := value["providers"].([]provider)
			if len(providers) != 1 || providers[0].ID != "provider" || len(providers[0].Models) != 1 {
				t.Fatal("wrong inventory")
			}
			if len(providers[0].Models[0].Thinking) != 2 || providers[0].Models[0].Thinking[1] != "ultra" {
				t.Fatal("thinking metadata lost")
			}
		})
	}
}

func TestInventoryRejectsCorruption(t *testing.T) {
	in := fixture()
	if _, err := Inventory(in.Config, []byte(`{"models":{},"models":{}}`), []byte(`{"models":[]}`)); err == nil {
		t.Fatal("duplicate fields accepted")
	}
	if _, err := Inventory([]byte("broken: ["), in.Context, []byte(`{"models":[]}`)); err == nil {
		t.Fatal("broken config accepted")
	}
	if _, err := Inventory(in.Config, in.Context, nil); err == nil {
		t.Fatal("missing exposed models accepted")
	}
}
