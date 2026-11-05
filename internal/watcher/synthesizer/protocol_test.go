package synthesizer

import (
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"testing"
	"time"
)

func TestConfiguredNativeProtocolFollowsCredential(t *testing.T) {
	cfg := &config.Config{OpenAICompatibility: []config.OpenAICompatibility{{Name: "fixture", Prefix: "arbitrary", BaseURL: "https://fixture.example/v1", UpstreamProtocol: "responses", APIKeyEntries: []config.OpenAICompatibilityAPIKey{{APIKey: "inert", ProxyURL: "socks5h://proxy.example:1080"}}}}}
	rows, err := NewConfigSynthesizer().Synthesize(&SynthesisContext{Config: cfg, Now: time.Unix(1, 0), IDGenerator: NewStableIDGenerator()})
	if err != nil || len(rows) != 1 {
		t.Fatalf("synthesis %v", err)
	}
	if rows[0].Attributes["upstream_protocol"] != "responses" || rows[0].ProxyURL != "socks5h://proxy.example:1080" {
		t.Fatal("selected credential lost transport capability")
	}
}
