package configbundle

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

var rejectedRanges = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("100.64.0.0/10"), netip.MustParsePrefix("192.0.0.0/24"), netip.MustParsePrefix("192.0.2.0/24"), netip.MustParsePrefix("192.88.99.0/24"), netip.MustParsePrefix("198.18.0.0/15"), netip.MustParsePrefix("198.51.100.0/24"), netip.MustParsePrefix("203.0.113.0/24"), netip.MustParsePrefix("240.0.0.0/4"), netip.MustParsePrefix("2001:db8::/32"),
}

func publicAddress(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsValid() || !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
		return false
	}
	for _, prefix := range rejectedRanges {
		if prefix.Contains(ip) {
			return false
		}
	}
	return true
}
func permittedProxy(value string) bool {
	if value == "" {
		return true
	}
	for _, scheme := range []string{"socks5://", "socks5h://"} {
		for _, host := range []string{"127.0.0.1", "172.17.0.1", "host.docker.internal"} {
			if value == scheme+host+":10808" {
				return true
			}
		}
	}
	return false
}
func samePublished(p provider, previous *yaml.Node) bool {
	if previous == nil || scalar(previous, "name") != "cpa-gui-"+p.ID+"-chat" || scalar(previous, "prefix") != p.Prefix || strings.TrimRight(strings.TrimSpace(scalar(previous, "base-url")), "/") != p.Base {
		return false
	}
	keyNode := get(previous, "keys")
	if keyNode == nil {
		keyNode = get(previous, "api-key-entries")
	}
	keys, err := seq(keyNode)
	if err != nil || len(keys) == 0 {
		return false
	}
	for _, key := range keys {
		if strings.TrimSpace(scalar(key, "proxy-url")) != p.Proxy {
			return false
		}
	}
	return true
}
func validateOrigin(ctx context.Context, p provider, previous *yaml.Node, resolve Resolver) error {
	if !permittedProxy(p.Proxy) {
		return errors.New("provider proxy is outside the configured allowlist")
	}
	if p.Base == "" || len(p.Base) > 4096 || strings.ContainsAny(p.Base, "\r\n\x00") {
		return errors.New("provider URL rejected")
	}
	u, err := url.Parse(p.Base)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Fragment != "" {
		return errors.New("provider URL must be an HTTPS hostname without credentials or fragments")
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if _, err := netip.ParseAddr(host); err == nil {
		return errors.New("literal-IP provider URLs are rejected")
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return errors.New("provider URL rejected")
		}
	}
	if resolve == nil {
		resolve = func(ctx context.Context, host string) ([]netip.Addr, error) {
			return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		}
	}
	ips, err := resolve(ctx, host)
	if ctx.Err() != nil {
		return errors.New("provider validation canceled")
	}
	if err != nil || len(ips) == 0 {
		// Only the trusted currently published connection tuple may survive DNS
		// unavailability. GUI drafts never authorize a new or changed origin.
		if samePublished(p, previous) {
			return nil
		}
		return errors.New("new or changed provider hostname could not be validated")
	}
	for _, ip := range ips {
		if !publicAddress(ip) {
			return errors.New("provider resolves to a non-public address")
		}
	}
	return nil
}

// ValidateProviderOrigin validates a new management URL without trusting GUI drafts.
func ValidateProviderOrigin(ctx context.Context, base, proxy string) error {
	return validateOrigin(ctx, provider{Base: base, Proxy: proxy}, nil, nil)
}

func ValidateProviderOriginWithResolver(ctx context.Context, base, proxy string, resolve Resolver) error {
	return validateOrigin(ctx, provider{Base: base, Proxy: proxy}, nil, resolve)
}
