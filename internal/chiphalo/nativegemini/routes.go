// Package nativegemini defines the deliberately small public Gemini surface.
package nativegemini

import (
	"net/http"
	"regexp"
	"strings"
)

var modelName = regexp.MustCompile(`^[a-zA-Z0-9_-][a-zA-Z0-9._-]*$`)

// Parse accepts public model resources and the three audited generation actions.
// Prefixes are preserved for upstream account routing; no path normalization is
// performed that could change the identity used for authorization or receipts.
func Parse(method, path string) (model, action string, ok bool) {
	if method == http.MethodGet && path == "/v1beta/models" {
		return "", "list", true
	}
	const prefix = "/v1beta/models/"
	if !strings.HasPrefix(path, prefix) {
		return "", "", false
	}
	model = strings.TrimPrefix(path, prefix)
	switch method {
	case http.MethodGet:
		action = "get"
	case http.MethodPost:
		var found bool
		model, action, found = strings.Cut(model, ":")
		if !found || (action != "generateContent" && action != "streamGenerateContent" && action != "countTokens") {
			return "", "", false
		}
	default:
		return "", "", false
	}
	if len(model) == 0 || len(model) > 1024 {
		return "", "", false
	}
	parts := strings.Split(model, "/")
	if len(parts) > 8 {
		return "", "", false
	}
	for _, part := range parts {
		if !modelName.MatchString(part) || part == "." || part == ".." {
			return "", "", false
		}
	}
	return model, action, true
}

// Provider reports native Gemini execution backends, not models that merely
// happen to appear in the upstream's protocol-agnostic global catalog.
func Provider(provider string) bool {
	switch provider {
	case "gemini", "gemini-cli", "vertex", "antigravity":
		return true
	default:
		return false
	}
}
