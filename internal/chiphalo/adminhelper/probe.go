package adminhelper

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/privatefile"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/providerprobe"
)

func (s *Service) probeModels(ctx context.Context, payload map[string]json.RawMessage) Result {
	if s.inventory == nil || s.probe == nil {
		return failure("Go provider probe is not enabled")
	}
	if err := allowed(payload, "provider_id", "base_url", "proxy_policy", "proxy_url", "api_key"); err != nil {
		return failure(err.Error())
	}
	id, e1 := text(payload, "provider_id", 64)
	base, e2 := text(payload, "base_url", 4096)
	policy, e3 := text(payload, "proxy_policy", 32)
	proxy, e4 := text(payload, "proxy_url", 256)
	secret, e5 := text(payload, "api_key", 4096)
	if e1 != nil || e2 != nil || e3 != nil || e4 != nil || e5 != nil || !identity.MatchString(id) {
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
		// A GUI draft is not authorization to send an existing vault key to a
		// different public host. Only the published tuple can use that key.
		seed, err := s.inventory.Read(ctx, "/seed")
		if err != nil || seed["ok"] != true {
			return failure("published provider inventory unavailable")
		}
		rows, _ := seed["providers"].([]any)
		matched := false
		for _, raw := range rows {
			p, ok := raw.(map[string]any)
			if !ok || p["id"] != id || strings.TrimRight(stringField(p, "baseUrl"), "/") != base {
				continue
			}
			published := stringField(p, "proxyUrl")
			if policy == "direct" && published != "" || policy == "cpa-egress" && published == "" || proxy != "" && proxy != published {
				return failure("stored credential origin or proxy mismatch; provide an explicit test key")
			}
			proxy, matched = published, true
			break
		}
		if !matched {
			return failure("stored credential origin or proxy mismatch; provide an explicit test key")
		}
		raw, err := privatefile.Read(s.paths.Vault, 2*1024*1024, true)
		if err != nil {
			return failure("provider vault unavailable")
		}
		doc, err := object(raw)
		if err != nil || json.Unmarshal(doc[id], &secret) != nil || secret == "" {
			return failure("provider API Key is missing")
		}
	} else if policy == "cpa-egress" && proxy == "" {
		proxy = "socks5h://172.17.0.1:10808"
	}
	result := s.probe.Fetch(ctx, providerprobe.Input{Base: base, Proxy: proxy, Secret: secret})
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
