package webgui

import (
	"compress/gzip"
	"net/http"
	"strconv"
	"strings"
)

type gzipJSONWriter struct {
	http.ResponseWriter
	writer *gzip.Writer
}

func (w gzipJSONWriter) Write(p []byte) (int, error) { return w.writer.Write(p) }

func compressedReadRoute(path string) bool {
	switch path {
	case "/api/state", "/api/usage", "/api/oauth-accounts", "/api/claude-account":
		return true
	default:
		return false
	}
}

func acceptsGzip(value string) bool {
	for _, coding := range strings.Split(value, ",") {
		parts := strings.Split(coding, ";")
		if !strings.EqualFold(strings.TrimSpace(parts[0]), "gzip") {
			continue
		}
		quality := 1.0
		for _, p := range parts[1:] {
			key, v, ok := strings.Cut(strings.TrimSpace(p), "=")
			if ok && strings.EqualFold(key, "q") {
				q, err := strconv.ParseFloat(v, 64)
				if err != nil {
					return false
				}
				quality = q
			}
		}
		return quality > 0
	}
	return false
}
