package configbundle

import (
	"errors"
	"strings"

	"gopkg.in/yaml.v3"
)

func normalizeProviderKind(p *provider) error {
	p.Kind = strings.TrimSpace(p.Kind)
	if p.Kind != "" && p.Kind != "openai-compatible" && p.Kind != "anthropic" {
		return invalid()
	}
	if p.Kind == "anthropic" && p.Protocol == "" {
		p.Protocol = "messages"
	}
	if p.Protocol == "messages" {
		if p.Kind == "openai-compatible" {
			return invalid()
		}
		p.Kind = "anthropic"
		p.Base = NormalizeMessagesBase(p.Base)
	} else if p.Kind == "anthropic" {
		return invalid()
	}
	if p.Protocol != "" && p.Protocol != "chat" && p.Protocol != "responses" && p.Protocol != "messages" {
		return invalid()
	}
	return nil
}

// Native Claude executors append /v1/messages to the configured gateway root.
func NormalizeMessagesBase(base string) string {
	return strings.TrimSuffix(strings.TrimRight(strings.TrimSpace(base), "/"), "/v1")
}

// NormalizeProviderProtocol is shared by the GUI draft writer and compiler.
func NormalizeProviderProtocol(kind, protocol string) (string, string, error) {
	p := provider{Kind: kind, Protocol: strings.TrimSpace(protocol)}
	if err := normalizeProviderKind(&p); err != nil {
		return "", "", err
	}
	return p.Kind, p.Protocol, nil
}

func claudeID(entry *yaml.Node) (string, error) {
	name := scalar(entry, "name")
	id := strings.TrimSuffix(strings.TrimPrefix(name, "cpa-gui-"), "-messages")
	if !strings.HasPrefix(name, "cpa-gui-") || !strings.HasSuffix(name, "-messages") || !slug.MatchString(id) {
		return "", invalid()
	}
	return id, nil
}

// This projection lets the common model compiler reuse its preservation rules.
// It never publishes Claude credentials into the OpenAI family.
func claudeProjection(entry *yaml.Node, id, credentialField string) (*yaml.Node, error) {
	n := node(map[string]any{})
	for _, field := range []string{"prefix", "base-url", "models", "headers", "disable-cooling", "request-retry", "request-scoped-errors", "priority"} {
		if v := get(entry, field); v != nil {
			put(n, field, clone(v))
		}
	}
	put(n, "name", node("cpa-gui-"+id+"-chat"))
	keys, err := seq(get(entry, "keys"))
	if err != nil {
		return nil, err
	}
	if len(keys) > 1 {
		return nil, errors.New("multi-key Claude provider requires an explicit migration")
	}
	key := node(map[string]any{})
	if len(keys) == 1 {
		key = clone(keys[0])
	} else {
		for _, field := range []string{"api-key", "proxy-url", "weight"} {
			if v := get(entry, field); v != nil {
				put(key, field, clone(v))
			}
		}
	}
	put(n, credentialField, &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq", Content: []*yaml.Node{key}})
	return n, nil
}

func buildClaudeEntry(compiled, previous *yaml.Node, credentialField string, modern bool) (*yaml.Node, error) {
	keys, err := seq(get(compiled, credentialField))
	if err != nil || len(keys) != 1 {
		return nil, invalid()
	}
	// Preserve the published Claude extensions in place. In particular, a GUI
	// edit does not enable, disable, expose or regenerate fingerprint profiles.
	out := clone(previous)
	for _, field := range []string{"name", "prefix", "base-url", "models", "headers", "disable-cooling", "request-retry", "request-scoped-errors", "priority"} {
		if v := get(compiled, field); v != nil {
			put(out, field, clone(v))
		}
	}
	name := scalar(compiled, "name")
	put(out, "name", node(strings.TrimSuffix(name, "-chat")+"-messages"))
	if modern {
		oldKeys, err := seq(get(previous, "keys"))
		if err != nil || len(oldKeys) > 1 {
			return nil, invalid()
		}
		key := node(map[string]any{})
		if len(oldKeys) == 1 {
			key = clone(oldKeys[0])
		}
		for _, field := range []string{"api-key", "proxy-url"} {
			put(key, field, clone(get(keys[0], field)))
		}
		put(out, "keys", &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq", Content: []*yaml.Node{key}})
	} else {
		for _, field := range []string{"api-key", "proxy-url"} {
			put(out, field, clone(get(keys[0], field)))
		}
	}
	return out, nil
}

func validateFamilyMigration(entry *yaml.Node) error {
	if entry == nil {
		return nil
	}
	allowed := map[string]bool{}
	for _, field := range []string{"name", "prefix", "base-url", "models", "headers", "disable-cooling", "request-retry", "request-scoped-errors", "priority", "api-key-entries", "keys", "upstream-protocol", "disabled"} {
		allowed[field] = true
	}
	for i := 0; i < len(entry.Content); i += 2 {
		if !allowed[entry.Content[i].Value] {
			return errors.New("provider family migration contains extensions requiring explicit review")
		}
	}
	for _, field := range []string{"api-key-entries", "keys"} {
		rows, err := seq(get(entry, field))
		if err != nil {
			return err
		}
		for _, row := range rows {
			for i := 0; i < len(row.Content); i += 2 {
				key := row.Content[i].Value
				if key != "api-key" && key != "proxy-url" {
					return errors.New("provider credential extensions require explicit family migration")
				}
			}
		}
	}
	return nil
}

func validateClaudeMigration(entry *yaml.Node) error {
	copy := clone(entry)
	allowed := map[string]bool{}
	for _, field := range []string{"name", "prefix", "base-url", "models", "headers", "disable-cooling", "request-retry", "request-scoped-errors", "priority", "api-key", "proxy-url", "weight", "keys"} {
		allowed[field] = true
	}
	for i := 0; i < len(copy.Content); i += 2 {
		if !allowed[copy.Content[i].Value] {
			return errors.New("Claude credential extensions require explicit family migration")
		}
	}
	keys, err := seq(get(entry, "keys"))
	if err != nil {
		return err
	}
	for _, key := range keys {
		for i := 0; i < len(key.Content); i += 2 {
			f := key.Content[i].Value
			if f != "api-key" && f != "proxy-url" && f != "weight" {
				return errors.New("Claude fingerprint or credential extensions require explicit migration")
			}
		}
	}
	return nil
}
