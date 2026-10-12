package config

import "fmt"

func (cfg *Config) ValidateRequestCompression() error {
	for i, provider := range cfg.OpenAICompatibility {
		for j, key := range provider.APIKeyEntries {
			for field, value := range map[string]string{"request-compression": key.RequestCompression, "response-compression": key.ResponseCompression} {
				switch value {
				case "", "identity", "gzip", "br", "zstd":
				default:
					return fmt.Errorf("invalid %s at openai-compatibility[%d].keys[%d]", field, i, j)
				}
			}
		}
	}
	return nil
}
