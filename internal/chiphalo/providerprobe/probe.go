// Package providerprobe retrieves a bounded catalog from an explicitly approved
// public origin. It never changes routing, prices, credentials or subscriptions.
package providerprobe

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"unicode"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/configbundle"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/proxyutil"
)

type Input struct{ Base, Proxy, Secret string }
type Result struct {
	OK        bool     `json:"ok"`
	Models    []string `json:"models,omitempty"`
	Count     int      `json:"count,omitempty"`
	Truncated bool     `json:"truncated"`
	Status    int      `json:"status,omitempty"`
	Error     string   `json:"error,omitempty"`
}
type TransportFactory func(Input, []netip.Addr) (http.RoundTripper, error)
type Fetcher struct {
	Resolve   configbundle.Resolver
	Transport TransportFactory
}

func pinnedTransport(input Input, addresses []netip.Addr) (http.RoundTripper, error) {
	u, err := url.Parse(input.Base)
	if err != nil || len(addresses) == 0 {
		return nil, errors.New("approved origin unavailable")
	}
	port := u.Port()
	if port == "" {
		port = "443"
	}
	setting := input.Proxy
	if setting == "" {
		setting = "direct"
	}
	dialer, _, err := proxyutil.BuildDialer(setting)
	if err != nil {
		return nil, errors.New("proxy unavailable")
	}
	dial, ok := dialer.(interface {
		DialContext(context.Context, string, string) (net.Conn, error)
	})
	if !ok {
		return nil, errors.New("cancelable acquisition required")
	}
	transport := proxyutil.NewDirectTransport()
	transport.Proxy = nil
	transport.DisableKeepAlives = true
	transport.TLSHandshakeTimeout = 0
	transport.ResponseHeaderTimeout = 0
	transport.TLSClientConfig = &tls.Config{ServerName: u.Hostname(), MinVersion: tls.VersionTLS12}
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		if address != net.JoinHostPort(u.Hostname(), port) {
			return nil, errors.New("unexpected provider target")
		}
		for _, ip := range addresses {
			// Both direct and SOCKS paths use the already validated destination
			// IP; a second DNS resolution must not bypass the public-origin check.
			conn, err := dial.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
			if err == nil {
				return conn, nil
			}
			if ctx.Err() != nil {
				break
			}
		}
		return nil, errors.New("provider acquisition unavailable")
	}
	return transport, nil
}

func (f Fetcher) Fetch(ctx context.Context, input Input) Result {
	fail := func(message string, status int) Result { return Result{Error: message, Status: status} }
	if input.Secret == "" || len(input.Secret) > 4096 || strings.IndexFunc(input.Secret, unicode.IsControl) >= 0 {
		return fail("invalid provider secret", 0)
	}
	u, err := url.Parse(input.Base)
	if err != nil || u.RawQuery != "" || u.ForceQuery || u.Opaque != "" {
		return fail("provider_url_rejected", 0)
	}
	resolve := f.Resolve
	if resolve == nil {
		resolve = func(ctx context.Context, host string) ([]netip.Addr, error) {
			return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		}
	}
	var addresses []netip.Addr
	validateResolve := func(ctx context.Context, host string) ([]netip.Addr, error) {
		ips, err := resolve(ctx, host)
		if err != nil || len(ips) == 0 || len(ips) > 16 {
			return nil, errors.New("bounded origin resolution unavailable")
		}
		addresses = append([]netip.Addr{}, ips...)
		return addresses, nil
	}
	if configbundle.ValidateProviderOriginWithResolver(ctx, input.Base, input.Proxy, validateResolve) != nil {
		return fail("provider_url_rejected", 0)
	}
	factory := f.Transport
	if factory == nil {
		factory = pinnedTransport
	}
	transport, err := factory(input, addresses)
	if err != nil {
		return fail("upstream_transport_error", 0)
	}
	if idle, ok := transport.(interface{ CloseIdleConnections() }); ok {
		defer idle.CloseIdleConnections()
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/models"
	u.RawPath = ""
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return fail("provider_url_rejected", 0)
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Authorization", "Bearer "+input.Secret)
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	if err != nil {
		return fail("upstream_transport_error", 0)
	}
	defer func() { _ = response.Body.Close() }()
	status := response.StatusCode
	if status < 200 || status >= 300 {
		message := "provider_request_rejected"
		switch {
		case status == 401 || status == 403:
			message = "provider_auth_rejected"
		case status == 429:
			message = "provider_rate_limited"
		case status >= 500:
			message = "provider_server_error"
		}
		// Error bodies/headers may contain secrets; never relay them to the GUI.
		return fail(message, status)
	}
	const maxBody = 4 * 1024 * 1024
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxBody+1))
	if err != nil || len(raw) > maxBody {
		return fail("provider_response_too_large_or_incomplete", status)
	}
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return fail("invalid_upstream_response", status)
	}
	if object, ok := value.(map[string]any); ok {
		value = object["data"]
		if value == nil {
			value = object["models"]
		}
	}
	items, ok := value.([]any)
	if !ok {
		return fail("invalid_upstream_response", status)
	}
	models, seen := []string{}, map[string]bool{}
	count := 0
	encodedBytes := 0
	for _, item := range items {
		if object, ok := item.(map[string]any); ok {
			item = object["id"]
			if item == nil || item == "" {
				item = object["name"]
			}
			if item == nil || item == "" {
				item = object["model"]
			}
		}
		model, ok := item.(string)
		model = strings.TrimSpace(model)
		if !ok || model == "" || len(model) > 256 || strings.IndexFunc(model, unicode.IsControl) >= 0 || strings.Contains(model, input.Secret) || seen[model] {
			continue
		}
		seen[model] = true
		count++
		encoded, _ := json.Marshal(model)
		if len(models) < 500 && encodedBytes+len(encoded)+1 <= 58*1024 {
			models = append(models, model)
			encodedBytes += len(encoded) + 1
		}
	}
	return Result{OK: true, Models: models, Count: count, Truncated: count > len(models), Status: status}
}
