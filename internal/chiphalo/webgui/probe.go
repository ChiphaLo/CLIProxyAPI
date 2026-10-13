package webgui

import "net/http"

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
