package configbundle

import (
	"encoding/json"
	"slices"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

// Inventory projects only GUI-owned routes. Credential/header/transport
// extensions are deliberately omitted, not returned through a generic map.
func Inventory(raw, contextRaw, exposedRaw []byte) (map[string]any, error) {
	root, err := parseYAML(raw)
	if err != nil {
		return nil, err
	}
	cfg, err := config.ParseConfigBytes(raw)
	if err != nil {
		return nil, invalid()
	}
	var ctx struct {
		Models map[string]json.RawMessage `json:"models"`
	}
	var exposed struct {
		Models []string `json:"models"`
	}
	if decodeJSON(contextRaw, &ctx) != nil || ctx.Models == nil || decodeJSON(exposedRaw, &exposed) != nil || exposed.Models == nil {
		return nil, invalid()
	}
	providers := []provider{}
	byID := map[string]int{}
	prefixes := []string{}
	ids := map[string]bool{}
	for _, id := range exposed.Models {
		ids[id] = true
	}
	for _, source := range cfg.OpenAICompatibility {
		if !strings.HasPrefix(source.Name, "cpa-gui-") {
			continue
		}
		id := strings.TrimSuffix(strings.TrimPrefix(source.Name, "cpa-gui-"), "-chat")
		if !slug.MatchString(id) {
			return nil, invalid()
		}
		proxy := ""
		if len(source.APIKeyEntries) > 0 {
			proxy = source.APIKeyEntries[0].ProxyURL
		}
		p := provider{Kind: "openai-compatible", Protocol: source.UpstreamProtocol, ID: id, Name: id, Prefix: source.Prefix, Base: source.BaseURL, Proxy: proxy, Enabled: new(!source.Disabled), Models: []model{}}
		for _, m := range source.Models {
			client := m.Alias
			if client == "" {
				client = m.Name
			}
			levels := []string{}
			if m.Thinking != nil {
				levels = append(levels, m.Thinking.Levels...)
			}
			inputs := slices.Clone(m.InputModalities)
			if len(inputs) == 0 {
				inputs = []string{"text"}
			}
			p.Models = append(p.Models, model{Client: client, Upstream: m.Name, Enabled: new(true), Context: m.MaxContextLength, Thinking: levels, Input: inputs})
			ids[p.Prefix+"/"+client] = true
		}
		if i, ok := byID[id]; ok {
			if providers[i].Prefix != p.Prefix || providers[i].Base != p.Base || providers[i].Proxy != p.Proxy {
				return nil, invalid()
			}
			providers[i].Models = append(providers[i].Models, p.Models...)
		} else {
			byID[id] = len(providers)
			providers = append(providers, p)
			prefixes = append(prefixes, p.Prefix)
		}
	}
	claudeParent, claudeKey, err := family(root, "claude-api-key", "claude")
	if err != nil {
		return nil, err
	}
	claudeRows, err := seq(get(claudeParent, claudeKey))
	if err != nil {
		return nil, err
	}
	claudeIDs := map[string]string{}
	for _, row := range claudeRows {
		if !owned(row) {
			continue
		}
		id, err := claudeID(row)
		if err != nil {
			return nil, err
		}
		prefix := scalar(row, "prefix")
		if prefix == "" || claudeIDs[prefix] != "" {
			return nil, invalid()
		}
		claudeIDs[prefix] = id
	}
	for _, source := range cfg.ClaudeKey {
		id := claudeIDs[source.Prefix]
		if id == "" {
			continue
		}
		if _, exists := byID[id]; exists {
			return nil, invalid()
		}
		p := provider{Kind: "anthropic", Protocol: "messages", ID: id, Name: id, Prefix: source.Prefix, Base: source.BaseURL, Proxy: source.ProxyURL, Enabled: new(true), Models: []model{}}
		for _, m := range source.Models {
			client := m.Alias
			if client == "" {
				client = m.Name
			}
			levels := []string{}
			if m.Thinking != nil {
				levels = append(levels, m.Thinking.Levels...)
			}
			inputs := []string{"text"}
			var meta struct {
				Input []string `json:"input_modalities"`
			}
			if raw, exists := ctx.Models[p.Prefix+"/"+client]; exists {
				if decodeJSON(raw, &meta) != nil {
					return nil, invalid()
				}
				if len(meta.Input) > 0 {
					inputs = meta.Input
				}
			}
			p.Models = append(p.Models, model{Client: client, Upstream: m.Name, Enabled: new(true), Context: m.MaxContextLength, Thinking: levels, Input: inputs})
			ids[p.Prefix+"/"+client] = true
		}
		byID[id] = len(providers)
		providers = append(providers, p)
		prefixes = append(prefixes, p.Prefix)
	}
	slices.Sort(prefixes)
	prefixes = slices.Compact(prefixes)
	metadata := map[string]any{}
	for id := range ids {
		var m struct {
			Window  int             `json:"context_window"`
			Length  int             `json:"context_length"`
			Levels  json.RawMessage `json:"supported_reasoning_levels"`
			Default string          `json:"default_reasoning_level"`
			Inputs  []string        `json:"input_modalities"`
		}
		if raw, ok := ctx.Models[id]; ok && decodeJSON(raw, &m) != nil {
			return nil, invalid()
		}
		if m.Window == 0 {
			m.Window = m.Length
		}
		if len(m.Inputs) == 0 {
			m.Inputs = []string{"text"}
		}
		levels := []any{}
		if len(m.Levels) > 0 && string(m.Levels) != "null" {
			var rawLevels []json.RawMessage
			if decodeJSON(m.Levels, &rawLevels) != nil {
				return nil, invalid()
			}
			for _, raw := range rawLevels {
				var level string
				if json.Unmarshal(raw, &level) == nil {
					levels = append(levels, level)
					continue
				}
				var value struct {
					Effort      string `json:"effort"`
					Description string `json:"description"`
				}
				if decodeJSON(raw, &value) != nil || value.Effort == "" {
					return nil, invalid()
				}
				levels = append(levels, map[string]string{"effort": value.Effort, "description": value.Description})
			}
		}
		metadata[id] = map[string]any{"context_window": m.Window, "supported_reasoning_levels": levels, "default_reasoning_level": m.Default, "input_modalities": m.Inputs}
	}
	return map[string]any{"ok": true, "providers": providers, "managed_prefixes": prefixes, "codex_context": metadata}, nil
}
