package cliproxy

import (
	"context"
	"errors"
)

// ReloadReviewedModelCatalog re-registers OAuth capabilities and scheduler
// entries after an explicit local publication. It does not reload credentials,
// run refresh, change provider payloads or enable this behavior by default.
func (s *Service) ReloadReviewedModelCatalog(ctx context.Context) error {
	if s == nil || s.modelCatalogTransform == nil || s.coreManager == nil || ctx == nil {
		return errors.New("reviewed catalog adapter unavailable")
	}
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	for _, auth := range s.coreManager.List() {
		if ctx.Err() != nil {
			return errors.New("reviewed catalog registration canceled")
		}
		if auth == nil || auth.Provider != "codex" || auth.AuthKind() == "apikey" {
			continue
		}
		s.registerModelsForAuth(ctx, auth)
		s.coreManager.RefreshSchedulerEntry(auth.ID)
	}
	return nil
}
