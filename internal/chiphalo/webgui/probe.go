package webgui

import (
	"net/http"
	"strings"
)

// A new provider can be probed without persisting a draft or a vault secret.
// Explicit credentials are required: never fall back to a stored provider key.
func (s *Server) draftProviderProbe(w http.ResponseWriter, r *http.Request, body map[string]any) {
	key := strings.TrimSpace(stringValue(body["apiKey"]))
	if key == "" || len(key) > 4096 {
		reject(w, 400, "enter an API Key to test an unsaved provider")
		return
	}
	p := map[string]any{"id": "draft-probe", "prefix": "draft-probe", "models": []any{}}
	for _, field := range []string{"baseUrl", "proxyUrl", "upstreamProtocol", "type"} {
		if value, ok := body[field]; ok {
			p[field] = value
		}
	}
	if err := s.normalizeProvider(r.Context(), p, nil); err != nil {
		reject(w, 400, err.Error())
		return
	}
	proxy := stringValue(p["proxyUrl"])
	policy := "direct"
	if proxy != "" {
		policy = "cpa-egress"
	}
	s.providerProbe(w, r, map[string]any{
		"provider_id": "draft-probe", "base_url": p["baseUrl"],
		"proxy_policy": policy, "proxy_url": proxy,
		"api_format": p["upstreamProtocol"], "api_key": key,
	})
}

func (s *Server) providerProbe(w http.ResponseWriter, r *http.Request, payload map[string]any) {
	value, err := s.call(r.Context(), "probe_provider_models", payload)
	if err != nil {
		reject(w, 503, "provider probe helper unavailable")
		return
	}
	status := 200
	if value["ok"] != true {
		status = 400
		switch stringValue(value["error"]) {
		case "provider probe busy":
			status = 429
		case "upstream_transport_error", "invalid_upstream_response", "provider_response_too_large_or_incomplete", "provider_auth_rejected", "provider_rate_limited", "provider_server_error", "provider_request_rejected":
			status = 502
		case "published provider inventory unavailable", "provider vault unavailable", "Go provider probe is not enabled":
			status = 503
		}
	}
	respond(w, status, value)
}
