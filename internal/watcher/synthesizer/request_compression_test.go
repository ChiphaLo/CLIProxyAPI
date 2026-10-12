package synthesizer

import (
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"testing"
	"time"
)

func TestRequestCompressionIsCredentialScoped(t *testing.T) {
	ctx := &SynthesisContext{Config: &config.Config{OpenAICompatibility: []config.OpenAICompatibility{{Name: "fixture", BaseURL: "http://fixture.invalid/v1", APIKeyEntries: []config.OpenAICompatibilityAPIKey{{APIKey: "synthetic-first", RequestCompression: "gzip", ResponseCompression: "br"}, {APIKey: "synthetic-second"}}}}}, Now: time.Now(), IDGenerator: NewStableIDGenerator()}
	auths, err := NewConfigSynthesizer().Synthesize(ctx)
	if err != nil || len(auths) != 2 {
		t.Fatalf("%v %d", err, len(auths))
	}
	if auths[0].Attributes["request_compression"] != "gzip" || auths[1].Attributes["request_compression"] != "" {
		t.Fatal("request compression leaked across credentials")
	}
	if auths[0].Attributes["response_compression"] != "br" || auths[1].Attributes["response_compression"] != "" {
		t.Fatal("response compression leaked across credentials")
	}
}
