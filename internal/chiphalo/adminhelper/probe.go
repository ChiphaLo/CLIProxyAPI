package adminhelper

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/privatefile"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/providerprobe"
)

func (s *Service) probeModels(ctx context.Context, payload map[string]json.RawMessage) Result {
	if s.probe == nil {
		return failure("Go provider probe is not enabled")
	}
	if err := allowed(payload, "provider_id", "base_url", "proxy_policy", "proxy_url", "api_key", "api_format"); err != nil {
		return failure(err.Error())
	}
	id, e1 := text(payload, "provider_id", 64)
	base, e2 := text(payload, "base_url", 4096)
	policy, e3 := text(payload, "proxy_policy", 32)
	proxy, e4 := text(payload, "proxy_url", 256)
	secret, e5 := text(payload, "api_key", 4096)
	format, e6 := text(payload, "api_format", 32)
	if e1 != nil || e2 != nil || e3 != nil || e4 != nil || e5 != nil || e6 != nil || !identity.MatchString(id) || (format != "" && format != "chat" && format != "responses" && format != "messages") {
		return failure("invalid provider probe request")
	}
	if policy == "" {
		policy = "direct"
	}
	if policy != "direct" && policy != "cpa-egress" || policy == "direct" && proxy != "" {
		return failure("invalid proxy policy")
	}
	select {
	case s.probeSlots <- struct{}{}:
		defer func() { <-s.probeSlots }()
	default:
		return failure("provider probe busy")
	}
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if secret == "" {
		raw, err := privatefile.Read(s.paths.Vault, 2*1024*1024, true)
		if err != nil {
			return failure("provider vault unavailable")
		}
		doc, err := object(raw)
		if err != nil || json.Unmarshal(doc[id], &secret) != nil || secret == "" {
			return failure("provider API Key is missing")
		}
		bindings, err := readProbeBindings(doc)
		if err != nil {
			return failure("invalid provider credential bindings")
		}
		// Explicit key saves authorize this exact tuple before a provider is
		// published. Never use a mutable GUI draft as credential authority.
		binding, matched := bindings[id]
		if !matched {
			// Existing vaults have no binding metadata; retain the published-only
			// rule for these keys instead of silently trusting a new draft host.
			if s.inventory == nil {
				return failure("published provider inventory unavailable")
			}
			seed, err := s.inventory.Read(ctx, "/seed")
			if err != nil || seed["ok"] != true {
				return failure("published provider inventory unavailable")
			}
			rows, _ := seed["providers"].([]any)
			for _, raw := range rows {
				p, ok := raw.(map[string]any)
				if ok && p["id"] == id && strings.TrimRight(stringField(p, "baseUrl"), "/") == base {
					binding = providerProbeBinding{Base: base, Proxy: stringField(p, "proxyUrl"), Format: stringField(p, "upstreamProtocol")}
					matched = true
					break
				}
			}
		}
		if !matched || binding.Base != base || policy == "direct" && binding.Proxy != "" || policy == "cpa-egress" && binding.Proxy == "" || proxy != "" && proxy != binding.Proxy {
			return failure("stored credential origin or proxy mismatch; provide an explicit test key")
		}
		if format == "" {
			format = binding.Format
		}
		if format != binding.Format && (format == "messages" || binding.Format == "messages") {
			return failure("stored credential protocol mismatch; provide an explicit test key")
		}
		proxy = binding.Proxy
	} else if policy == "cpa-egress" && proxy == "" {
		proxy = "socks5h://172.17.0.1:10808"
	}
	result := s.probe.Fetch(ctx, providerprobe.Input{Base: base, Proxy: proxy, Secret: secret, Format: format})
	value := Result{"ok": result.OK, "status": result.Status}
	if !result.OK {
		value["error"] = result.Error
		value["provider_id"] = id
		return value
	}
	value["models"], value["count"], value["truncated"] = result.Models, result.Count, result.Truncated
	return value
}

func stringField(row map[string]any, key string) string {
	value, _ := row[key].(string)
	return value
}
