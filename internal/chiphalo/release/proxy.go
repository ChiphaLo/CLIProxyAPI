package release

import (
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
)

func newProxy(endpoint *url.URL, transport http.RoundTripper, ingress ingressPolicy) http.Handler {
	proxy := &httputil.ReverseProxy{
		Rewrite: func(request *httputil.ProxyRequest) {
			request.SetURL(endpoint)
			// Rewrite drops inbound forwarding headers. Publish only a validated
			// canonical client address, never the caller's arbitrary header chain.
			request.SetXForwarded()
			client, _ := ingress.client(request.In)
			request.Out.Header.Set("X-Forwarded-For", client)
			for _, header := range []string{"Forwarded", "X-Real-IP", "CF-Connecting-IP", "True-Client-IP"} {
				request.Out.Header.Del(header)
			}
			if ingress.trusts(request.In.RemoteAddr) {
				proto := request.In.Header.Get("X-Forwarded-Proto")
				if proto == "https" || proto == "http" {
					request.Out.Header.Set("X-Forwarded-Proto", proto)
				}
			}
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
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := ingress.client(r); err != nil {
			http.Error(w, "invalid CPA ingress address", http.StatusBadRequest)
			return
		}
		proxy.ServeHTTP(w, r)
	})
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
