package config

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestRequestCompressionConfiguration(t *testing.T) {
	for _, coding := range []string{"", "identity", "gzip", "br", "zstd", "deflate", "GZIP"} {
		t.Run(coding, func(t *testing.T) {
			raw := []byte(fmt.Sprintf("config-version: 8\napi-keys:\n  openai-compatibility:\n    - name: fixture\n      base-url: http://fixture.invalid/v1\n      keys:\n        - api-key: synthetic-key\n          request-compression: %q\n          response-compression: %q\n", coding, coding))
			valid := coding != "deflate" && coding != "GZIP"
			if err := ValidateV8Config(raw); (err == nil) != valid {
				t.Fatalf("v8 validation %v", err)
			}
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			cfg, err := LoadConfig(path)
			if (err == nil) != valid {
				t.Fatalf("load %v", err)
			}
			if valid && cfg.OpenAICompatibility[0].APIKeyEntries[0].RequestCompression != coding {
				t.Fatal("capability lost during layout conversion")
			}
			if valid && cfg.OpenAICompatibility[0].APIKeyEntries[0].ResponseCompression != coding {
				t.Fatal("response capability lost")
			}
		})
	}
}
