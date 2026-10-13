package quota

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/proxyutil"
)

const OfficialUsageURL = "https://chatgpt.com/backend-api/wham/usage"

// Credential is ephemeral SDK-owned material. It is never serialized into a
// collector response, file, audit event, command argument or GUI request.
type Credential struct{ Token, Workspace, Proxy string }
type TransportFactory func(string) (http.RoundTripper, error)
type Fetcher struct{ Transport TransportFactory }

func defaultTransport(setting string) (http.RoundTripper, error) {
	transport, _, err := proxyutil.BuildHTTPTransport(setting)
	if err != nil {
		return nil, errors.New("configured proxy unavailable")
	}
	if transport == nil {
		transport = proxyutil.NewDirectTransport()
	}
	parsed, err := proxyutil.Parse(setting)
	if err != nil {
		return nil, errors.New("configured proxy unavailable")
	}
	if parsed.URL != nil && (parsed.URL.Scheme == "socks5" || parsed.URL.Scheme == "socks5h") {
		dialer, _, err := proxyutil.BuildDialer(setting)
		if err != nil {
			return nil, errors.New("configured proxy unavailable")
		}
		if contextual, ok := dialer.(interface {
			DialContext(context.Context, string, string) (net.Conn, error)
		}); ok {
			transport.DialContext = contextual.DialContext
		} else {
			return nil, errors.New("cancelable proxy acquisition required")
		}
	}
	return transport, nil
}
func (f Fetcher) Fetch(ctx context.Context, credential Credential) (Usage, error) {
	var empty Usage
	if credential.Token == "" || len(credential.Token) > 65536 || credential.Workspace == "" || len(credential.Workspace) > 512 || strings.ContainsAny(credential.Token+credential.Workspace, "\r\n\x00") {
		return empty, errors.New("official credential unavailable")
	}
	factory := f.Transport
	if factory == nil {
		factory = defaultTransport
	}
	transport, err := factory(credential.Proxy)
	if err != nil {
		return empty, errors.New("official transport unavailable")
	}
	if idle, ok := transport.(interface{ CloseIdleConnections() }); ok {
		defer idle.CloseIdleConnections()
	}
	// No connected-upstream timeout is introduced. Cancellation follows the
	// original caller; transport acquisition retains upstream's existing policy.
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, OfficialUsageURL, nil)
	if err != nil {
		return empty, errors.New("official request unavailable")
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Authorization", "Bearer "+credential.Token)
	request.Header.Set("ChatGPT-Account-Id", credential.Workspace)
	response, err := client.Do(request)
	if err != nil {
		return empty, errors.New("official transport unavailable")
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return empty, fmt.Errorf("official_http_%d", response.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, 262145))
	if err != nil {
		return empty, errors.New("official response unavailable")
	}
	return Project(raw)
}
