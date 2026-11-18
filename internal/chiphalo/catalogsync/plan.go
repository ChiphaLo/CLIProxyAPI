package catalogsync

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/configbundle"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/quota"
)

type Rejected struct {
	Account string `json:"account_id"`
	Model   string `json:"model"`
	Reason  string `json:"reason"`
	Status  int    `json:"status,omitempty"`
}
type Plan struct {
	Exposed, Context []byte
	Added            []string
	Removed          []string
	Rejected         []Rejected
	Changed          bool
	MorePending      bool
}

func publicID(prefix, name string) string {
	if prefix == "codex" {
		return name
	}
	return prefix + "/" + name
}
func contextValid(value any) bool {
	m, _ := value.(map[string]any)
	if m == nil || window(m["context_window"]) == 0 {
		return false
	}
	levels, ok := m["supported_reasoning_levels"].([]any)
	return ok && len(levels) > 0
}
func mapValue(value any) map[string]any {
	m, _ := value.(map[string]any)
	if m == nil {
		return map[string]any{}
	}
	return m
}

// BuildPlan performs no file publication, credential lookup, pricing or refresh.
// Model membership and capabilities come from each account's official /models
// catalog; an offline/failed discovery cannot erase an account's existing catalog
// or borrow another member's result.
func BuildPlan(ctx context.Context, bindings []quota.Binding, discovery Snapshot, exposedRaw, contextRaw []byte) (Plan, error) {
	return BuildScopedPlan(ctx, bindings, discovery, exposedRaw, contextRaw, nil)
}

// BuildScopedPlan adds only the selected models of one account when scope is
// set and refreshes that account's listed models; other accounts are unchanged.
func BuildScopedPlan(ctx context.Context, bindings []quota.Binding, discovery Snapshot, exposedRaw, contextRaw []byte, scope *Scope) (Plan, error) {

	plan := Plan{Added: []string{}, Removed: []string{}, Rejected: []Rejected{}}
	if ctx == nil || !discovery.OK || len(bindings) == 0 || len(bindings) > 128 {
		return plan, errors.New("explicit account catalog discovery and review required")
	}
	if scope.Validate() != nil {
		return plan, errors.New("invalid scoped catalog review")
	}
	if ctx.Err() != nil {
		return plan, errors.New("catalog review canceled before publication")
	}
	var exposedValue any
	var metadata map[string]any
	if configbundle.DecodeDraft(exposedRaw, &exposedValue) != nil || configbundle.DecodeDraft(contextRaw, &metadata) != nil || metadata == nil {
		return plan, errors.New("existing OAuth catalogs unavailable; no reset was planned")
	}
	exposed, object := exposedValue.(map[string]any)
	if !object {
		if legacy, ok := exposedValue.([]any); ok {
			exposed = map[string]any{"models": legacy}
		} else {
			return plan, errors.New("existing exposed OAuth catalog invalid")
		}
	}
	names, ok := exposed["models"].([]any)
	if !ok || len(names) > 512 {
		return plan, errors.New("existing exposed OAuth catalog invalid")
	}
	models, ok := metadata["models"].(map[string]any)
	if !ok {
		return plan, errors.New("existing context catalog invalid")
	}
	beforeNames := []string{}
	for _, item := range names {
		if object, ok := item.(map[string]any); ok {
			item = object["id"]
		}
		name, ok := item.(string)
		name = strings.TrimPrefix(strings.TrimSpace(name), "codex/")
		if !ok || name == "" || strings.ContainsAny(name, "/\r\n\x00") {
			return plan, errors.New("existing public OAuth model identity invalid")
		}
		beforeNames = append(beforeNames, name)
	}
	accountRows := mapValue(exposed["account_models"])
	if reviewed, ok := exposed["reviewed_account_models"].(map[string]any); ok {
		accountRows = reviewed
	}
	_, accountCatalogPresent := exposed["account_models"]
	known, found := map[string]bool{}, map[string]Account{}
	successfulDiscovery := false
	for _, binding := range bindings {
		if binding.ID == "" || binding.Prefix == "" || known[binding.ID] {
			return plan, errors.New("ambiguous registered catalog account")
		}
		known[binding.ID] = true
	}
	if scope != nil && !known[scope.AccountID] {
		return plan, errors.New("scoped catalog account is not registered")
	}
	for _, account := range discovery.Accounts {
		if !known[account.ID] || found[account.ID].ID != "" {
			return plan, errors.New("unregistered or duplicate discovery account")
		}
		found[account.ID] = account
		successfulDiscovery = successfulDiscovery || account.Status == "ok"
	}
	if !successfulDiscovery {
		return plan, errors.New("official catalog discovery unavailable; no publication was planned")
	}
	if len(found) != len(bindings) || accountCatalogPresent && len(accountRows) != len(bindings) {
		return plan, errors.New("account catalog inventory incomplete")
	}
	accountModels, union := map[string][]string{}, map[string]bool{}
	for _, binding := range bindings {
		account := found[binding.ID]
		if account.Prefix != binding.Prefix {
			return plan, errors.New("discovery changed a registered prefix")
		}
		if len(account.Models) > 512 {
			return plan, errors.New("discovered account catalog exceeds bound")
		}
		seen := map[string]bool{}
		for _, model := range account.Models {
			if seen[model.Name] {
				return plan, errors.New("ambiguous discovered model identity")
			}
			seen[model.Name] = true
		}
		current := slices.Clone(beforeNames)
		if accountCatalogPresent {
			items, valid := accountRows[binding.ID].([]any)
			if !valid {
				return plan, errors.New("account-local published catalog invalid")
			}
			current = []string{}
			for _, item := range items {
				name, valid := item.(string)
				if !valid || !slices.Contains(beforeNames, name) || slices.Contains(current, name) {
					return plan, errors.New("account-local model identity invalid")
				}
				current = append(current, name)
			}
		}
		kept := []string{}
		for _, name := range current {
			if !Eligible(name) {
				plan.Removed = append(plan.Removed, publicID(binding.Prefix, name))
				delete(models, publicID(binding.Prefix, name))
				if binding.Prefix == "codex" {
					delete(models, "codex/"+name)
				}
				continue
			}
			kept = append(kept, name)
		}
		if account.Status != "ok" {
			plan.Rejected = append(plan.Rejected, Rejected{Account: binding.ID, Reason: "account discovery unavailable"})
		} else {
			if scope != nil && scope.AccountID == binding.ID {
				listed := map[string]string{}
				for _, model := range account.Models {
					listed[model.Name] = model.Visibility
				}
				for _, name := range scope.Models {
					visibility, ok := listed[name]
					switch {
					case !ok:
						plan.Rejected = append(plan.Rejected, Rejected{Account: binding.ID, Model: publicID(binding.Prefix, name), Reason: "model is not listed by the upstream account catalog"})
					case visibility == "hide" || visibility == "internal":
						plan.Rejected = append(plan.Rejected, Rejected{Account: binding.ID, Model: publicID(binding.Prefix, name), Reason: "model is hidden by the upstream account catalog"})
					}
				}
			}
			for _, model := range account.Models {
				if !Eligible(model.Name) || model.Visibility == "hide" || model.Visibility == "internal" {
					continue
				}
				id := publicID(binding.Prefix, model.Name)
				contextID := id
				if binding.Prefix == "codex" && models[id] == nil {
					contextID = "codex/" + model.Name
				}
				listed := slices.Contains(kept, model.Name)
				if scope != nil && (scope.AccountID != binding.ID || !listed && !scope.allows(binding.ID, model.Name)) {
					continue
				}
				if ctx.Err() != nil {
					return Plan{}, errors.New("catalog review canceled before publication")
				}
				levels := []map[string]string{}
				for _, level := range model.Thinking {
					if thinkingLevel(level) {
						levels = append(levels, map[string]string{"effort": level})
					}
				}
				old := mapValue(models[contextID])
				if len(levels) == 0 {
					// Keep an existing published entry instead of erasing usable
					// capabilities when the official row omits reasoning levels.
					if !listed || !contextValid(old) {
						plan.Rejected = append(plan.Rejected, Rejected{Account: binding.ID, Model: id, Reason: "official catalog lists no reasoning levels"})
					}
					continue
				}
				defaultLevel := levels[0]["effort"]
				for _, level := range levels {
					if level["effort"] == "medium" {
						defaultLevel = level["effort"]
					}
				}
				for _, level := range levels {
					if level["effort"] == model.DefaultThinking {
						defaultLevel = level["effort"]
					}
				}
				limit, maxLimit := model.Window, model.MaxWindow
				if limit == 0 {
					limit = window(old["context_window"])
				}
				if limit == 0 {
					limit = 272000
				}
				if maxLimit < limit {
					maxLimit = limit
				}
				inputs := model.Inputs
				if len(inputs) == 0 {
					inputs = []string{"text"}
				}
				entry := maps.Clone(old)
				delete(entry, "verified_at")
				for key, value := range map[string]any{"context_length": limit, "context_window": limit, "max_context_window": maxLimit, "default_reasoning_level": defaultLevel, "supported_reasoning_levels": levels, "input_modalities": inputs, "upstream_slug": binding.Prefix + "/" + model.Name, "source": "official Codex /models catalog", "source_kind": "official catalog", "source_account_id": binding.ID} {
					entry[key] = value
				}
				if model.MultiAgent != "" {
					entry["multi_agent_version"] = model.MultiAgent
				}
				models[id] = entry

				if !slices.Contains(kept, model.Name) {
					kept = append(kept, model.Name)
					plan.Added = append(plan.Added, id)
				}
			}
		}
		accountModels[binding.ID] = kept
		for _, name := range kept {
			union[name] = true
		}
	}
	afterNames := []string{}
	for _, name := range beforeNames {
		if union[name] && !slices.Contains(afterNames, name) {
			afterNames = append(afterNames, name)
		}
	}
	newNames := []string{}
	for name := range union {
		if !slices.Contains(afterNames, name) {
			newNames = append(newNames, name)
		}
	}
	slices.Sort(newNames)
	afterNames = append(afterNames, newNames...)
	if len(afterNames) > 512 || ctx.Err() != nil {
		return Plan{}, errors.New("catalog plan exceeds bounds or was canceled")
	}
	exposed["models"], exposed["account_models"] = afterNames, accountModels
	if rawSettings, exists := exposed["account_settings"]; exists {
		raw, _ := json.Marshal(rawSettings)
		var settings map[string]AccountSetting
		if configbundle.DecodeDraft(raw, &settings) != nil {
			return Plan{}, errors.New("invalid Codex account settings")
		}
		exposed["reviewed_account_models"] = accountModels
		exposed["account_models"] = SelectedAccountModels(accountModels, settings)
	}
	metadata["models"] = models
	plan.Exposed, _ = json.MarshalIndent(exposed, "", "  ")
	plan.Context, _ = json.MarshalIndent(metadata, "", "  ")
	if len(plan.Exposed)+1 > 1024*1024 || len(plan.Context)+1 > 4*1024*1024 {
		return Plan{}, errors.New("catalog publication documents exceed bounds")
	}
	var before, after any
	_ = json.Unmarshal(exposedRaw, &before)
	_ = json.Unmarshal(plan.Exposed, &after)
	b1, _ := json.Marshal(before)
	a1, _ := json.Marshal(after)
	var oldContext any
	_ = json.Unmarshal(contextRaw, &oldContext)
	b2, _ := json.Marshal(oldContext)
	a2, _ := json.Marshal(metadata)
	plan.Changed = string(b1) != string(a1) || string(b2) != string(a2)
	plan.Exposed, plan.Context = append(plan.Exposed, '\n'), append(plan.Context, '\n')
	return plan, nil
}
