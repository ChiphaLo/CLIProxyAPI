package release

import (
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
)

func newProxy(endpoint *url.URL, transport http.RoundTripper) http.Handler {
	return &httputil.ReverseProxy{
		Rewrite: func(request *httputil.ProxyRequest) {
			request.SetURL(endpoint)
			// Untrusted forwarding headers must not bypass the gateway IP policy.
			request.SetXForwarded()
		},
		Transport: transport,
		// Flush SSE immediately; never buffer an upstream model turn to switch.
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`{"error":{"message":"selected CPA release is unavailable","type":"gateway_error"}}`))
		},
	}
}

func (c *Controller) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// The atomic selection is loaded once per request, including upgraded and
	// long-lived streams. Switching never mutates that selection afterward.
	current := c.active.Load()
	if current == nil {
		http.Error(w, "no CPA release selected", http.StatusServiceUnavailable)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/__cpa_release/") {
		http.NotFound(w, r)
		return
	}
	current.handler.ServeHTTP(w, r)
}
