package clientkeys

import (
	"fmt"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/privatefile"
)

// ReadLegacyAdminKey reads only the deployed variable as literal data. It never
// executes a shell, expands variables, or falls back to the private backend key.
func ReadLegacyAdminKey(path string) (string, error) {
	raw, err := privatefile.Read(path, 64*1024, true)
	if err != nil {
		return "", err
	}
	found, key := false, ""
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, value, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(name) != "UNIFIED_GATEWAY_API_KEY" {
			continue
		}
		if found {
			return "", fmt.Errorf("duplicate CPA admin key variable")
		}
		found = true
		key = strings.TrimSpace(value)
		if len(key) >= 2 && (key[0] == '\'' && key[len(key)-1] == '\'' || key[0] == '"' && key[len(key)-1] == '"') {
			key = key[1 : len(key)-1]
		}
	}
	if key == "" || len(key) > 4096 || strings.ContainsAny(key, "\r\n\x00") {
		return "", fmt.Errorf("existing CPA public admin key is missing or invalid")
	}
	return key, nil
}
