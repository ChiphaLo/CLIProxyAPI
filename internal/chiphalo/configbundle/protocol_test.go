package configbundle

import (
	"context"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestVerifiedProviderProtocolSurvivesApply(t *testing.T) {
	in := fixture()
	in.State = []byte(strings.Replace(string(in.State), `"id":"provider"`, `"id":"provider","upstreamProtocol":"responses"`, 1))
	out, err := Build(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.ParseConfigBytes(out.Config)
	if err != nil || cfg.OpenAICompatibility[0].UpstreamProtocol != "responses" {
		t.Fatal("native protocol lost")
	}
	gen, _ := parseYAML(out.Generated)
	for _, kind := range []string{"chat_models", "responses_models"} {
		rows, _ := seq(get(gen, kind))
		found := false
		for _, row := range rows {
			if scalar(row, "id") == "provider/old" {
				found = scalar(row, "protocol") == "responses"
			}
		}
		if !found {
			t.Fatalf("native protocol lost in %s", kind)
		}
	}
	if err := Verify(out); err != nil {
		t.Fatal(err)
	}
	in.State = []byte(strings.Replace(string(in.State), `"upstreamProtocol":"responses"`, `"upstreamProtocol":"unknown"`, 1))
	if _, err := Build(context.Background(), in); err == nil {
		t.Fatal("unknown protocol accepted")
	}
}
