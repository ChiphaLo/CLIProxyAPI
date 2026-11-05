package adminhelper

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/configbundle"
)

// The reserved metadata key cannot collide with a valid provider identity.
const probeBindingsKey = "__cpa_probe_bindings"

type providerProbeBinding struct {
	Base   string `json:"base_url"`
	Proxy  string `json:"proxy_url"`
	Format string `json:"api_format"`
}

func readProbeBindings(doc map[string]json.RawMessage) (map[string]providerProbeBinding, error) {
	bindings := map[string]providerProbeBinding{}
	if raw, exists := doc[probeBindingsKey]; exists {
		if err := json.Unmarshal(raw, &bindings); err != nil || bindings == nil {
			return nil, errors.New("invalid provider credential bindings")
		}
	}
	return bindings, nil
}

func (s *Service) probeBinding(ctx context.Context, payload map[string]json.RawMessage) (*providerProbeBinding, error) {
	_, hasBase := payload["base_url"]
	_, hasProxy := payload["proxy_url"]
	_, hasFormat := payload["api_format"]
	if !hasBase && !hasProxy && !hasFormat {
		return nil, nil // Legacy writers remain compatible, without draft-key reuse.
	}
	base, e1 := text(payload, "base_url", 4097)
	proxy, e2 := text(payload, "proxy_url", 257)
	format, e3 := text(payload, "api_format", 33)
	if e1 != nil || e2 != nil || e3 != nil || !hasBase || len(base) > 4096 || len(proxy) > 256 || (format != "" && format != "chat" && format != "responses" && format != "messages") {
		return nil, errors.New("invalid provider credential origin")
	}
	if format == "" {
		format = "chat"
	}
	base = strings.TrimRight(base, "/")
	if format == "messages" {
		base = configbundle.NormalizeMessagesBase(base)
	}
	var resolve configbundle.Resolver
	if s.probe != nil {
		resolve = s.probe.Resolve
	}
	if err := configbundle.ValidateProviderOriginWithResolver(ctx, base, proxy, resolve); err != nil {
		return nil, errors.New("provider credential URL or proxy rejected")
	}
	return &providerProbeBinding{Base: base, Proxy: proxy, Format: format}, nil
}
