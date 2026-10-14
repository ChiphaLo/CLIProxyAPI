package configbundle

import (
	"context"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestBuildOpenCodeGoSessionHeader(t *testing.T) {
	for _, tc := range []struct {
		name, headers, base, want string
	}{
		{"missing", "", "https://opencode.ai/zen/go/v1", "$CPA-SESSION-ID"},
		{"null", "    headers: null\n", "https://opencode.ai/zen/go/v1", "$CPA-SESSION-ID"},
		{"preserve_other", "    headers: {X-Custom: retained}\n", "https://opencode.ai/zen/go/v1", "$CPA-SESSION-ID"},
		{"preserve_explicit", "    headers: {X-OpenCode-Session: explicit-session}\n", "https://opencode.ai/zen/go/v1", "explicit-session"},
		{"unrelated", "", "https://provider.example/v1", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := fixture()
			in.Config = []byte(strings.Replace(string(in.Config), "    custom-provider: keep\n", "    custom-provider: keep\n"+tc.headers, 1))
			in.Config = []byte(strings.Replace(string(in.Config), "https://provider.example/v1", tc.base, 1))
			in.State = []byte(strings.Replace(string(in.State), "https://provider.example/v1", tc.base, 1))
			out, err := Build(context.Background(), in)
			if err != nil {
				t.Fatal(err)
			}
			cfg, err := config.ParseConfigBytes(out.Config)
			if err != nil {
				t.Fatal(err)
			}
			headers := cfg.OpenAICompatibility[0].Headers
			got := ""
			count := 0
			for name, value := range headers {
				if strings.EqualFold(name, "x-opencode-session") {
					got = value
					count++
				}
			}
			if got != tc.want || count > 1 || tc.name == "preserve_other" && headers["X-Custom"] != "retained" {
				t.Fatalf("unexpected headers: %#v", headers)
			}
			in.Config, in.Generated, in.Context = out.Config, out.Generated, out.Context
			next, err := Build(context.Background(), in)
			if err != nil || string(next.Config) != string(out.Config) {
				t.Fatal("session default is not stable across repeated apply")
			}
		})
	}
}
