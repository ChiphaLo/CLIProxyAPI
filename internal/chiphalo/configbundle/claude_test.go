package configbundle

import (
	"context"
	"encoding/json"
	"net/netip"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"gopkg.in/yaml.v3"
)

func nativeFixture() Input {
	in := fixture()
	in.Config = []byte(strings.ReplaceAll(strings.ReplaceAll(string(in.Config), "    custom-provider: keep\n", ""), ", custom-key: keep", ""))
	in.State = []byte(strings.Replace(string(in.State), `"id":"provider"`, `"id":"provider","type":"anthropic","upstreamProtocol":"messages"`, 1))
	in.Resolve = func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
	}
	return in
}

func TestNormalizeProviderProtocol(t *testing.T) {
	for _, tc := range []struct {
		kind, protocol, wantKind, wantProtocol string
		invalid                                bool
	}{
		{"", "", "", "", false},
		{"openai-compatible", "responses", "openai-compatible", "responses", false},
		{"", "messages", "anthropic", "messages", false},
		{"anthropic", "", "anthropic", "messages", false},
		{"anthropic", "messages", "anthropic", "messages", false},
		{"anthropic", "chat", "", "", true},
		{"openai-compatible", "messages", "", "", true},
		{"unknown", "responses", "", "", true},
		{"", "unknown", "", "", true},
	} {
		kind, protocol, err := NormalizeProviderProtocol(tc.kind, tc.protocol)
		if (err != nil) != tc.invalid || !tc.invalid && (kind != tc.wantKind || protocol != tc.wantProtocol) {
			t.Fatalf("protocol normalization failed for %q / %q", tc.kind, tc.protocol)
		}
	}
}

func TestNativeClaudeMigrationAndInventory(t *testing.T) {
	for _, modern := range []bool{false, true} {
		t.Run(map[bool]string{false: "legacy", true: "v8"}[modern], func(t *testing.T) {
			in := nativeFixture()
			if modern {
				in.Config = []byte(`access: {api-keys: [inert]}
api-keys:
  openai-compatibility:
    - name: cpa-gui-provider-chat
      prefix: provider
      base-url: https://provider.example/v1
      keys: [{api-key: previous-fixture}]
      models: [{name: previous, alias: old}]
  claude:
    - name: foreign
      prefix: foreign-cld
      base-url: https://foreign.example
      models: [{name: foreign, alias: foreign}]
      keys: [{api-key: foreign-secret, cloak: {mode: never}}]
`)
			}
			out, err := Build(context.Background(), in)
			if err != nil {
				t.Fatal(err)
			}
			cfg, err := config.ParseConfigBytes(out.Config)
			if err != nil {
				t.Fatal(err)
			}
			var owned *config.ClaudeKey
			for i := range cfg.ClaudeKey {
				if cfg.ClaudeKey[i].Prefix == "provider" {
					owned = &cfg.ClaudeKey[i]
				}
			}
			if owned == nil || owned.APIKey != "new-fixture" || owned.BaseURL != "https://provider.example" || owned.FingerprintProfile != "" || owned.Cloak != nil || owned.Models[0].Name != "new-upstream" || owned.Models[0].Alias != "old" {
				t.Fatal("Claude identity, default policy or model mapping changed")
			}
			for _, p := range cfg.OpenAICompatibility {
				if p.Prefix == "provider" {
					t.Fatal("migration left duplicate OpenAI provider")
				}
			}
			if modern && (!strings.Contains(string(out.Config), "foreign-secret") || len(cfg.ClaudeKey) != 2) {
				t.Fatal("foreign Claude credential lost")
			}
			if err := Verify(out); err != nil {
				t.Fatal(err)
			}
			seed, err := Inventory(out.Config, out.Context, []byte(`{"models":[]}`))
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(seed)
			if strings.Contains(string(raw), "new-fixture") || strings.Contains(string(raw), "foreign-secret") {
				t.Fatal("inventory exposed a credential")
			}
			var decoded struct {
				Providers []provider `json:"providers"`
			}
			_ = json.Unmarshal(raw, &decoded)
			if len(decoded.Providers) != 1 || decoded.Providers[0].Kind != "anthropic" || decoded.Providers[0].Protocol != "messages" || len(decoded.Providers[0].Models[0].Input) != 2 {
				t.Fatal("native provider recovery lost protocol or modalities")
			}
			in.Config, in.Generated, in.Context = out.Config, out.Generated, out.Context
			out2, err := Build(context.Background(), in)
			if err != nil || Verify(out2) != nil {
				t.Fatalf("re-apply failed: %v", err)
			}
		})
	}
}

func TestNativeClaudeMigrationPreservesExtensionBoundary(t *testing.T) {
	in := fixture()
	in.State = nativeFixture().State
	in.Resolve = nativeFixture().Resolve
	if _, err := Build(context.Background(), in); err == nil {
		t.Fatal("unknown cross-family extensions were discarded")
	}
	in = nativeFixture()
	out, err := Build(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	root, _ := parseYAML(out.Config)
	parent, key, _ := family(root, "claude-api-key", "claude")
	rows, _ := seq(get(parent, key))
	put(rows[0], "cloak", node(map[string]any{"mode": "never", "strict-mode": false}))
	put(rows[0], "headers", node(map[string]any{"X-Fixture": "preserve"}))
	out.Config, _ = yaml.Marshal(root)
	in.Config, in.Generated, in.Context = out.Config, out.Generated, out.Context
	out, err = Build(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.ParseConfigBytes(out.Config)
	if err != nil || cfg.ClaudeKey[0].Cloak == nil || cfg.ClaudeKey[0].Cloak.Mode != "never" || cfg.ClaudeKey[0].Headers["X-Fixture"] != "preserve" {
		t.Fatal("published native extensions were lost")
	}
}

func TestNativeClaudeExplicitClientProfileSurvivesReapply(t *testing.T) {
	for _, modern := range []bool{false, true} {
		in := nativeFixture()
		if modern {
			in.Config = []byte("access: {api-keys: [fixture]}\napi-keys: {openai-compatibility: [], claude: []}\n")
		}
		out, err := Build(context.Background(), in)
		if err != nil {
			t.Fatal(err)
		}
		root, _ := parseYAML(out.Config)
		parent, key, _ := family(root, "claude-api-key", "claude")
		rows, _ := seq(get(parent, key))
		credential := rows[0]
		if modern {
			keys, _ := seq(get(credential, "keys"))
			credential = keys[0]
		}
		put(credential, "fingerprint-profile", node("claude-code-cli"))
		put(credential, "cloak", node(map[string]any{"mode": "always", "strict-mode": false, "cache-user-id": true}))
		out.Config, _ = yaml.Marshal(root)
		for range 2 {
			in.Config, in.Generated, in.Context = out.Config, out.Generated, out.Context
			out, err = Build(context.Background(), in)
			if err != nil {
				t.Fatal(err)
			}
			cfg, err := config.ParseConfigBytes(out.Config)
			if err != nil || len(cfg.ClaudeKey) != 1 {
				t.Fatal("native credential was lost")
			}
			credential := cfg.ClaudeKey[0]
			if credential.FingerprintProfile != "claude-code-cli" || credential.Cloak == nil || credential.Cloak.Mode != "always" || credential.Cloak.StrictMode || credential.Cloak.CacheUserID == nil || !*credential.Cloak.CacheUserID {
				t.Fatal("explicit client compatibility configuration did not survive re-apply")
			}
			if Verify(out) != nil {
				t.Fatal("profile configuration invalidated generated routes")
			}
		}
	}
}
