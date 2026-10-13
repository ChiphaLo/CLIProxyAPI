package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/configbundle"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/privatefile"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// Match the SDK's stock tier selection for readiness; actual registered models
// must not be their own baseline proof for a newly added, unreviewed name.
func stockCodexCatalog(auth *coreauth.Auth) []*registry.ModelInfo {
	switch strings.ToLower(strings.TrimSpace(auth.Attributes["plan_type"])) {
	case "free":
		return registry.GetCodexFreeModels()
	case "team", "business", "go":
		return registry.GetCodexTeamModels()
	case "plus":
		return registry.GetCodexPlusModels()
	default:
		return registry.GetCodexProModels()
	}
}

// ModelHints feeds reviewed metadata into stock SDK registration, not an
// independent routing/translation implementation or a request-time writer.
func (l *LiveProfile) ModelHints(ctx context.Context, auth *coreauth.Auth, baseline []*registry.ModelInfo) ([]*registry.ModelInfo, error) {
	if auth == nil || auth.Provider != "codex" || auth.AuthKind() == "apikey" {
		return baseline, nil
	}
	if ctx.Err() != nil {
		return nil, errors.New("model hints canceled")
	}
	profile, err := l.Refresh()
	if err != nil || profile.Accounts.ValidateAuth(auth) != nil {
		return nil, errors.New("registered model owner unavailable")
	}
	var metadata struct {
		Models map[string]json.RawMessage `json:"models"`
	}
	raw, err := privatefile.Read(filepath.Join(l.paths.ConfigDir, "context-windows.json"), 4*1024*1024, false)
	if err != nil || configbundle.DecodeDraft(raw, &metadata) != nil || metadata.Models == nil {
		return nil, errors.New("reviewed model metadata unavailable")
	}
	byName := map[string]*registry.ModelInfo{}
	for _, info := range baseline {
		if info != nil {
			byName[info.ID] = info
		}
	}
	owner := ""
	for _, binding := range profile.Accounts.Bindings() {
		if binding.AuthID == auth.ID {
			owner = binding.ID
		}
	}
	result := []*registry.ModelInfo{}
	for _, name := range profile.Accounts.ModelNamesForAuth(auth.ID) {
		key := auth.Prefix + "/" + name
		if auth.Prefix == "codex" && metadata.Models[name] != nil {
			key = name
		}
		var hint struct {
			Window   int             `json:"context_window"`
			Max      int             `json:"max_context_window"`
			Default  string          `json:"default_reasoning_level"`
			Levels   json.RawMessage `json:"supported_reasoning_levels"`
			Inputs   []string        `json:"input_modalities"`
			Owner    string          `json:"source_account_id"`
			Kind     string          `json:"source_kind"`
			Verified string          `json:"verified_at"`
		}
		if data := metadata.Models[key]; data != nil && configbundle.DecodeDraft(data, &hint) != nil {
			return nil, errors.New("reviewed model capability invalid")
		}
		if hint.Owner != "" && hint.Owner != owner {
			return nil, errors.New("reviewed model capability belongs to a different member")
		}
		info := &registry.ModelInfo{ID: name, Object: "model", OwnedBy: "openai", Type: "codex", MetadataModelID: name}
		if source := byName[name]; source != nil {
			cloned := *source
			info = &cloned
		} else if hint.Owner != owner || hint.Kind != "live OAuth probe" || hint.Verified == "" || hint.Window < 1024 {
			if !profile.Accounts.HasAccountCatalog() && hint.Owner == "" {
				// Legacy exposure is an allowlist, not proof that every historical
				// name was routable. Keep the stock intersection without activating
				// unreviewed names or withholding the member's other valid models.
				continue
			}
			return nil, errors.New("new model lacks an account-local live review")
		}
		if byName[name] == nil {
			if _, err := time.Parse(time.RFC3339Nano, hint.Verified); err != nil {
				return nil, errors.New("new model live review timestamp invalid")
			}
			if len(hint.Levels) == 0 || string(hint.Levels) == "null" {
				return nil, errors.New("new model lacks reviewed reasoning capabilities")
			}
		}
		if hint.Window > 0 {
			if hint.Window < 1024 || hint.Window > 100000000 || hint.Max != 0 && hint.Max < hint.Window || hint.Max > 100000000 {
				return nil, errors.New("reviewed context bounds invalid")
			}
			info.ContextLength, info.MaxContextLength = hint.Window, hint.Window
			info.ClientContextWindow, info.ClientMaxContextWindow = hint.Window, hint.Max
		}
		if len(hint.Levels) > 0 && string(hint.Levels) != "null" {
			var levels []json.RawMessage
			if json.Unmarshal(hint.Levels, &levels) != nil || len(levels) > 16 {
				return nil, errors.New("reviewed reasoning capabilities invalid")
			}
			if byName[name] == nil && len(levels) == 0 {
				return nil, errors.New("new model lacks reviewed reasoning capabilities")
			}
			thinking := &registry.ThinkingSupport{Levels: []string{}}
			for _, level := range levels {
				var name string
				if json.Unmarshal(level, &name) != nil {
					var entry struct {
						Effort string `json:"effort"`
					}
					if json.Unmarshal(level, &entry) != nil {
						return nil, errors.New("reviewed reasoning capability invalid")
					}
					name = entry.Effort
				}
				if !slices.Contains([]string{"none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra"}, name) || slices.Contains(thinking.Levels, name) {
					return nil, errors.New("reviewed reasoning level invalid")
				}
				thinking.Levels = append(thinking.Levels, name)
			}
			info.Thinking, info.ExplicitThinking = thinking, true
			if hint.Default != "" {
				if !slices.Contains(thinking.Levels, hint.Default) {
					return nil, errors.New("reviewed default reasoning level not supported")
				}
				info.ClientDefaultReasoningLevel = hint.Default
			}
		}
		if len(hint.Inputs) > 0 {
			inputs := []string{}
			for _, input := range hint.Inputs {
				input = strings.TrimSpace(strings.ToLower(input))
				if input != "text" && input != "image" {
					return nil, errors.New("reviewed input modality invalid")
				}
				if !slices.Contains(inputs, input) {
					inputs = append(inputs, input)
				}
			}
			info.SupportedInputModalities, info.ExplicitInputModalities = inputs, true
		}
		result = append(result, info)
	}
	return result, nil
}
