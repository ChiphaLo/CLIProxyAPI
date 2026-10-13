package release

import (
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
)

type ingressPolicy map[netip.Addr]bool

func parseIngress(peers []string) (ingressPolicy, error) {
	policy := ingressPolicy{}
	for _, peer := range peers {
		address, err := netip.ParseAddr(strings.TrimSpace(peer))
		if err != nil || address.Zone() != "" || !address.IsLoopback() {
			return nil, fmt.Errorf("trusted ingress must contain exact loopback IP addresses")
		}
		policy[address.Unmap()] = true
	}
	return policy, nil
}

func peerIP(remote string) (netip.Addr, error) {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("invalid remote peer")
	}
	address, err := netip.ParseAddr(host)
	if err != nil || address.Zone() != "" {
		return netip.Addr{}, fmt.Errorf("invalid remote peer")
	}
	return address.Unmap(), nil
}

func (p ingressPolicy) trusts(remote string) bool {
	address, err := peerIP(remote)
	return err == nil && p[address]
}

func (p ingressPolicy) client(r *http.Request) (string, error) {
	peer, err := peerIP(r.RemoteAddr)
	if err != nil {
		return "", err
	}
	if !p[peer] {
		return peer.String(), nil
	}
	header := strings.Join(r.Header.Values("X-Forwarded-For"), ",")
	if header == "" {
		return peer.String(), nil
	}
	if len(header) > 2048 {
		return "", fmt.Errorf("forwarding chain too large")
	}
	parts := strings.Split(header, ",")
	if len(parts) > 16 {
		return "", fmt.Errorf("forwarding chain too long")
	}
	addresses := make([]netip.Addr, len(parts))
	for i, part := range parts {
		address, err := netip.ParseAddr(strings.TrimSpace(part))
		if err != nil || address.Zone() != "" || address.IsUnspecified() || address.IsMulticast() {
			return "", fmt.Errorf("invalid forwarding chain")
		}
		addresses[i] = address.Unmap()
	}
	// The nearest untrusted hop is authoritative. A spoofed address to its
	// left cannot override Caddy's appended connection peer.
	for i := len(addresses) - 1; i >= 0; i-- {
		if !p[addresses[i]] || i == 0 {
			return addresses[i].String(), nil
		}
	}
	return peer.String(), nil
}
