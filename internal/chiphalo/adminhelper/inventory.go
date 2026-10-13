package adminhelper

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/privatefile"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/providerprobe"
)

// EnableInventory must run before Serve; the HTTP-only shadow adapter cannot
// claim trusted private management identity.
func (s *Service) EnableInventory(backend *HTTPBackend) error {
	if backend == nil || !backend.private {
		return errors.New("authenticated private inventory required")
	}
	s.inventory = backend
	s.probe = &providerprobe.Fetcher{}
	return nil
}

func (s *Service) readInventory(ctx context.Context, op string) Result {
	if s.inventory == nil {
		return failure("Go private inventory is not enabled")
	}
	if op == "gateway_models" {
		ids, err := s.inventory.Catalog(ctx)
		if err != nil {
			return failure("gateway catalog unavailable")
		}
		count := len(ids)
		if len(ids) > 500 {
			ids = ids[:500]
		}
		return Result{"ok": true, "models": ids, "count": count, "truncated": count > 500}
	}
	if op == "gui_provider_seed" {
		value, err := s.inventory.Read(ctx, "/seed")
		if err != nil {
			return failure("provider inventory unavailable")
		}
		value["generated_at"] = s.now().Unix()
		return value
	}
	ready, err := s.inventory.Read(ctx, "/__cpa_release/ready")
	if err != nil {
		return failure("gateway readiness unavailable")
	}
	raw, err := privatefile.Read(s.paths.Vault, 2*1024*1024, true)
	if err != nil {
		return failure("provider vault unavailable")
	}
	doc, err := object(raw)
	if err != nil {
		return failure("provider vault invalid")
	}
	secrets := map[string]bool{}
	for id, raw := range doc {
		var value string
		if identity.MatchString(id) && json.Unmarshal(raw, &value) == nil {
			secrets[id] = value != ""
		}
	}
	state := "inactive"
	if ready["ready"] == true {
		state = "active"
	}
	return Result{"ok": true, "status": map[string]any{"gateway": state, "docker": "embedded Go SDK", "runtime": "embedded_go_sdk", "sdk_ready": ready["configuration_ready"] == true, "configuration_ready": ready["configuration_ready"], "config_apply_pending": ready["config_apply_pending"]}, "provider_secrets": secrets}
}
