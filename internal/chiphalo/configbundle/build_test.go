package configbundle

import (
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func fixture() Input {
	return Input{
		Config: []byte(`api-keys: [inner-fixture]
proxy-url: socks5h://172.17.0.1:10808
oauth-settings:
  codex2: []
codex-api-key:
  - {api-key: other-fixture, prefix: foreign-codex, models: [{name: external, alias: external}]}
custom-root: {keep: true}
openai-compatibility:
  - name: cpa-gui-provider-chat
    prefix: provider
    base-url: https://provider.example/v1
    custom-provider: keep
    api-key-entries: [{api-key: previous-fixture, custom-key: keep}]
    models: [{name: old-upstream, alias: old, custom-model: keep}]
  - name: preserved
    prefix: manual
    base-url: https://manual.example/v1
    api-key-entries: [{api-key: manual-fixture}]
    models: [{name: manual-upstream, alias: manual}]
`),
		Generated: []byte("custom: keep\nchat_models: [{id: provider/old}, {id: manual/manual}]\nresponses_models: [{id: provider/old}, {id: manual/manual}]\nfailed_or_unavailable: []\n"),
		Context:   []byte(`{"custom":"keep","models":{"provider/old":{"extra":"keep"},"manual/manual":{"keep":true},"codex2/gpt-6-astra":{"keep":true}}}`),
		Accounts:  []byte(`{"accounts":[{"prefix":"codex"},{"prefix":"codex2"}]}`),
		Vault:     []byte(`{"provider":"new-fixture"}`),
		State:     []byte(`{"managedPrefixes":["provider"],"providers":[{"id":"provider","prefix":"provider","baseUrl":"https://provider.example/v1","models":[{"clientName":"old","upstreamName":"new-upstream","thinking":["high","ultra"],"inputModalities":["text","image"]}]}]}`),
		Resolve:   func(context.Context, string) ([]netip.Addr, error) { return nil, errors.New("offline") }, Now: time.Unix(1780000000, 0),
	}
}

func TestBuildPreservesUnknownFieldsAndForeignCredentials(t *testing.T) {
	in := fixture()
	out, err := Build(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"custom-root", "custom-provider", "custom-key", "custom-model", "foreign-codex", "other-fixture", "manual-fixture", "codex2"} {
		if !strings.Contains(string(out.Config), value) {
			t.Fatalf("lost preserved field %s", value)
		}
	}
	if strings.Contains(string(out.Config), "previous-fixture") || !strings.Contains(string(out.Config), "new-fixture") || len(out.Models) != 1 || out.Models[0] != "provider/old" {
		t.Fatal("managed mapping not applied")
	}
	var contexts map[string]any
	_ = json.Unmarshal(out.Context, &contexts)
	models := contexts["models"].(map[string]any)
	if models["manual/manual"] == nil || models["codex2/gpt-6-astra"] == nil || models["provider/old"].(map[string]any)["extra"] != "keep" {
		t.Fatal("context metadata lost")
	}
	cfg, err := config.ParseConfigBytes(out.Config)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.OpenAICompatibility[0].Models[0].Thinking.Levels[1] != "ultra" || len(cfg.OpenAICompatibility[0].Models[0].InputModalities) != 2 {
		t.Fatal("reasoning/image capabilities lost")
	}
}

func TestModernV8LayoutAndMuseNativeResponses(t *testing.T) {
	in := fixture()
	in.Config = []byte(`server: {host: 127.0.0.1, port: 8318}
access: {api-keys: [inner-fixture]}
api-keys:
  openai-compatibility:
    - name: cpa-gui-opencode-chat
      prefix: opencode
      base-url: https://opencode.ai/zen/go/v1
      keys: [{api-key: previous-fixture}]
      models: []
`)
	in.State = []byte(`{"providers":[{"id":"opencode","baseUrl":"https://opencode.ai/zen/go/v1","models":[{"clientName":"muse","upstreamName":"muse-spark-1.3-contributor"}]}],"managedPrefixes":["opencode"]}`)
	in.Vault = []byte(`{"opencode":"native-fixture"}`)
	out, err := Build(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.ParseConfigBytes(out.Config)
	if err != nil || len(cfg.OpenAICompatibility) != 1 || cfg.OpenAICompatibility[0].Prefix != "opencode" {
		t.Fatal("v8 layout not preserved")
	}
	gen, _ := parseYAML(out.Generated)
	chat, _ := seq(get(gen, "chat_models"))
	for _, row := range chat {
		if scalar(row, "id") == "opencode/muse" {
			t.Fatal("Muse routed through chat bridge")
		}
	}
	responses, _ := seq(get(gen, "responses_models"))
	found := false
	for _, row := range responses {
		if scalar(row, "id") == "opencode/muse" {
			found = scalar(row, "protocol") == "responses"
		}
	}
	if !found {
		t.Fatal("native Responses route missing")
	}
}

func TestPrivateParsersWithSyntheticInputs(t *testing.T) {
	in := fixture()
	if _, err := parseYAML(in.Config); err != nil {
		t.Fatal("config YAML parser", err)
	}
	if _, err := parseYAML(in.Generated); err != nil {
		t.Fatal("generated YAML parser", err)
	}
	if _, err := config.ParseConfigBytes(in.Config); err != nil {
		t.Fatal("upstream synthetic config parser", err)
	}
	var d draft
	if err := decodeJSON(in.State, &d); err != nil {
		t.Fatal("draft parser", err)
	}
	var m map[string]json.RawMessage
	for _, raw := range [][]byte{in.Vault, in.Context, in.Accounts} {
		if err := decodeJSON(raw, &m); err != nil {
			t.Fatal("JSON parser", err)
		}
	}
	if node(map[string]any{}).Kind != 4 {
		t.Fatalf("encoded map node kind %d", node(map[string]any{}).Kind)
	}
}

func TestBuildRejectsAmbiguousOrUnsafeDrafts(t *testing.T) {
	for name, change := range map[string]func(*Input){
		"OAuth scope":    func(in *Input) { in.State = []byte(`{"providers":[],"managedPrefixes":["codex2"]}`) },
		"foreign scope":  func(in *Input) { in.State = []byte(`{"providers":[],"managedPrefixes":["manual"]}`) },
		"duplicate JSON": func(in *Input) { in.State = []byte(`{"providers":[],"providers":[]}`) },
		"duplicate YAML": func(in *Input) { in.Config = append(in.Config, []byte("api-keys: [second]\n")...) },
		"new origin offline": func(in *Input) {
			in.State = []byte(strings.ReplaceAll(string(in.State), "provider.example", "new.example"))
		},
		"private DNS": func(in *Input) {
			in.Resolve = func(context.Context, string) ([]netip.Addr, error) {
				return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
			}
		},
		"changed proxy offline": func(in *Input) {
			in.State = []byte(strings.Replace(string(in.State), `"models":`, `"proxyUrl":"socks5h://172.17.0.1:10808","models":`, 1))
		},
		"missing key": func(in *Input) { in.Vault = []byte(`{}`) },
		"bad context": func(in *Input) { in.Context = []byte(`{"models":null}`) },
		"bad model": func(in *Input) {
			in.State = []byte(strings.ReplaceAll(string(in.State), `"clientName":"old"`, `"clientName":"../old"`))
		},
	} {
		t.Run(name, func(t *testing.T) {
			in := fixture()
			change(&in)
			if _, err := Build(context.Background(), in); err == nil {
				t.Fatal("unsafe bundle accepted")
			}
		})
	}
}

func TestDisabledProvidersAreRemovedWithoutChangingOAuth(t *testing.T) {
	in := fixture()
	in.State = []byte(`{"providers":[{"id":"provider","baseUrl":"https://provider.example/v1","enabled":false,"models":[]}],"managedPrefixes":[]}`)
	out, err := Build(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Models) != 0 || len(out.ReplacedModels) != 1 || strings.Contains(string(out.Config), "cpa-gui-provider") {
		t.Fatal("disabled provider not removed")
	}
	if !strings.Contains(string(out.Config), "foreign-codex") || !strings.Contains(string(out.Context), "codex2/gpt-6-astra") {
		t.Fatal("unrelated account configuration changed")
	}
	gen, _ := parseYAML(out.Generated)
	rows, _ := seq(get(gen, "responses_models"))
	for _, row := range rows {
		if scalar(row, "id") == "provider/old" {
			t.Fatal("removed route remains")
		}
	}
}

func TestPublicAddressPolicy(t *testing.T) {
	for _, ip := range []string{"127.0.0.1", "10.0.0.1", "100.64.0.1", "192.0.2.1", "169.254.169.254", "::1", "::ffff:127.0.0.1", "fc00::1", "2001:db8::1"} {
		if publicAddress(netip.MustParseAddr(ip)) {
			t.Fatalf("unsafe DNS address accepted: %s", ip)
		}
	}
	for _, ip := range []string{"8.8.8.8", "1.1.1.1", "2606:4700:4700::1111"} {
		if !publicAddress(netip.MustParseAddr(ip)) {
			t.Fatal("public DNS address rejected")
		}
	}
}
