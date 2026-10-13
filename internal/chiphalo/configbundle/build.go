// Package configbundle compiles GUI drafts without resetting published business
// configuration, OAuth identities, pricing or historical usage.
package configbundle

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/netip"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"gopkg.in/yaml.v3"
)

const FenceName = ".cpa-config-apply-pending"

var slug = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

type Resolver func(context.Context, string) ([]netip.Addr, error)
type Input struct {
	Config, Generated, Context, Accounts, Vault, State []byte
	Resolve                                            Resolver
	Now                                                time.Time
}
type Output struct {
	Config, Generated, Context []byte
	Models, ReplacedModels     []string
	Providers                  int
}
type draft struct {
	Providers []provider `json:"providers"`
	Managed   []string   `json:"managedPrefixes"`
}
type provider struct {
	ID      string  `json:"id"`
	Name    string  `json:"name"`
	Prefix  string  `json:"prefix"`
	Base    string  `json:"baseUrl"`
	Proxy   string  `json:"proxyUrl"`
	Enabled *bool   `json:"enabled"`
	Models  []model `json:"models"`
}
type model struct {
	Client   string   `json:"clientName"`
	Upstream string   `json:"upstreamName"`
	Enabled  *bool    `json:"enabled"`
	Context  int      `json:"contextWindow"`
	Thinking []string `json:"thinking"`
	Input    []string `json:"inputModalities"`
}

func enabled(value *bool) bool { return value == nil || *value }
func invalid() error {
	return errors.New("invalid configuration bundle; published files were not changed")
}

// decodeJSON rejects duplicate keys at every depth without quoting private data.
func decodeJSON(raw []byte, target any) error {
	if len(raw) > 4*1024*1024 {
		return invalid()
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	var walk func() error
	walk = func() error {
		token, err := d.Token()
		if err != nil {
			return invalid()
		}
		delim, ok := token.(json.Delim)
		if !ok {
			return nil
		}
		switch delim {
		case '{':
			seen := map[string]bool{}
			for d.More() {
				key, err := d.Token()
				name, ok := key.(string)
				if err != nil || !ok || seen[name] {
					return invalid()
				}
				seen[name] = true
				if err := walk(); err != nil {
					return err
				}
			}
		case '[':
			for d.More() {
				if err := walk(); err != nil {
					return err
				}
			}
		default:
			return invalid()
		}
		_, err = d.Token()
		return err
	}
	if err := walk(); err != nil {
		return invalid()
	}
	if _, err := d.Token(); err != io.EOF {
		return invalid()
	}
	if json.Unmarshal(raw, target) != nil {
		return invalid()
	}
	return nil
}

// DecodeDraft rejects duplicate fields at every depth before management input
// is normalized or rewritten. It does not deserialize any credential files.
func DecodeDraft(raw []byte, target any) error { return decodeJSON(raw, target) }

func parseYAML(raw []byte) (*yaml.Node, error) {
	if len(raw) > 4*1024*1024 {
		return nil, invalid()
	}
	d := yaml.NewDecoder(bytes.NewReader(raw))
	var doc yaml.Node
	if d.Decode(&doc) != nil || len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, invalid()
	}
	var tail yaml.Node
	if d.Decode(&tail) != io.EOF {
		return nil, invalid()
	}
	var validate func(*yaml.Node) error
	validate = func(n *yaml.Node) error {
		// Shared mutable aliases are ambiguous when GUI-managed nodes change.
		if n.Kind == yaml.AliasNode || n.Anchor != "" {
			return invalid()
		}
		if n.Kind == yaml.MappingNode {
			seen := map[string]bool{}
			for i := 0; i < len(n.Content); i += 2 {
				k := n.Content[i]
				if k.Kind != yaml.ScalarNode || seen[k.Value] {
					return invalid()
				}
				seen[k.Value] = true
			}
		}
		for _, child := range n.Content {
			if err := validate(child); err != nil {
				return err
			}
		}
		return nil
	}
	if err := validate(&doc); err != nil {
		return nil, err
	}
	return doc.Content[0], nil
}
func get(n *yaml.Node, key string) *yaml.Node {
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return n.Content[i+1]
		}
	}
	return nil
}
func put(n *yaml.Node, key string, value *yaml.Node) {
	for i := 0; i < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			n.Content[i+1] = value
			return
		}
	}
	n.Content = append(n.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, value)
}
func node(value any) *yaml.Node { n := &yaml.Node{}; _ = n.Encode(value); return n }
func scalar(n *yaml.Node, key string) string {
	v := get(n, key)
	if v == nil || v.Kind != yaml.ScalarNode {
		return ""
	}
	return v.Value
}
func clone(n *yaml.Node) *yaml.Node {
	if n == nil {
		return node(map[string]any{})
	}
	copy := *n
	copy.Content = nil
	for _, child := range n.Content {
		copy.Content = append(copy.Content, clone(child))
	}
	return &copy
}
func seq(n *yaml.Node) ([]*yaml.Node, error) {
	if n == nil || n.Tag == "!!null" {
		return nil, nil
	}
	if n.Kind != yaml.SequenceNode {
		return nil, invalid()
	}
	for _, item := range n.Content {
		if item.Kind != yaml.MappingNode {
			return nil, invalid()
		}
	}
	return n.Content, nil
}
func family(root *yaml.Node, old, current string) (*yaml.Node, string, error) {
	if keys := get(root, "api-keys"); keys != nil && keys.Kind == yaml.SequenceNode {
		return root, old, nil
	}
	if get(root, "api-keys") == nil && (get(root, old) != nil || get(root, "host") != nil) {
		return root, old, nil
	}
	parent := get(root, "api-keys")
	if parent == nil {
		parent = node(map[string]any{})
		put(root, "api-keys", parent)
	}
	if parent.Kind != yaml.MappingNode {
		return nil, "", invalid()
	}
	return parent, current, nil
}
func owned(n *yaml.Node) bool {
	name := scalar(n, "name")
	return strings.HasPrefix(name, "cpa-gui-") || name == "opencode-go-chat"
}
func publicID(entry, model *yaml.Node) string {
	name := scalar(model, "alias")
	if name == "" {
		name = scalar(model, "name")
	}
	prefix := scalar(entry, "prefix")
	if prefix != "" {
		return prefix + "/" + name
	}
	return name
}
func normalize(m *model) error {
	m.Client = strings.TrimSpace(m.Client)
	m.Upstream = strings.TrimSpace(m.Upstream)
	if m.Client == "" || m.Upstream == "" || len(m.Client) > 200 || len(m.Upstream) > 300 || strings.ContainsAny(m.Client, "/\r\n\x00") || strings.ContainsAny(m.Upstream, "\r\n\x00") {
		return invalid()
	}
	if m.Context == 0 {
		m.Context = 272000
	}
	if m.Context < 1024 || m.Context > 10000000 {
		return invalid()
	}
	if m.Thinking == nil {
		m.Thinking = []string{"low", "medium", "high", "max"}
	}
	if len(m.Thinking) > 32 || len(m.Input) > 8 {
		return invalid()
	}
	seen := map[string]bool{}
	for _, effort := range m.Thinking {
		if effort == "" || len(effort) > 32 || seen[effort] || strings.ContainsAny(effort, "\r\n\x00") {
			return invalid()
		}
		seen[effort] = true
	}
	inputs := []string{"text"}
	for _, input := range m.Input {
		if input != "text" && input != "image" {
			return invalid()
		}
		if input == "image" && len(inputs) == 1 {
			inputs = append(inputs, "image")
		}
	}
	m.Input = inputs
	return nil
}

// NormalizeModelDraft shares model validation between GUI draft persistence and
// the final trusted compiler. Unknown JSON fields are not authorization inputs.
func NormalizeModelDraft(raw []byte) ([]byte, error) {
	var value model
	if decodeJSON(raw, &value) != nil {
		return nil, invalid()
	}
	if err := normalize(&value); err != nil {
		return nil, err
	}
	value.Enabled = new(enabled(value.Enabled))
	return json.Marshal(value)
}

func Build(ctx context.Context, in Input) (*Output, error) {
	root, err := parseYAML(in.Config)
	if err != nil {
		return nil, err
	}
	gen, err := parseYAML(in.Generated)
	if err != nil {
		return nil, err
	}
	var state draft
	var stateObject map[string]json.RawMessage
	if decodeJSON(in.State, &state) != nil || decodeJSON(in.State, &stateObject) != nil || state.Providers == nil || len(state.Providers) > 128 || len(state.Managed) > 256 {
		return nil, invalid()
	}
	for field := range stateObject {
		if field != "providers" && field != "managedPrefixes" {
			return nil, invalid()
		}
	}
	var accounts struct {
		Accounts []struct {
			Prefix string `json:"prefix"`
		} `json:"accounts"`
	}
	if decodeJSON(in.Accounts, &accounts) != nil || accounts.Accounts == nil {
		return nil, invalid()
	}
	reserved := map[string]bool{"codex": true, "codex2": true}
	for _, account := range accounts.Accounts {
		if !slug.MatchString(account.Prefix) {
			return nil, invalid()
		}
		reserved[account.Prefix] = true
	}
	var vault map[string]json.RawMessage
	var contexts map[string]json.RawMessage
	if decodeJSON(in.Vault, &vault) != nil || vault == nil || decodeJSON(in.Context, &contexts) != nil || contexts == nil {
		return nil, invalid()
	}
	contextModels := map[string]json.RawMessage{}
	if raw, ok := contexts["models"]; ok {
		if decodeJSON(raw, &contextModels) != nil || contextModels == nil {
			return nil, invalid()
		}
	}
	previousContext := map[string]json.RawMessage{}
	for id, value := range contextModels {
		previousContext[id] = value
	}
	parent, key, err := family(root, "openai-compatibility", "openai-compatibility")
	if err != nil {
		return nil, err
	}
	credentialField := "api-key-entries"
	if parent != root {
		credentialField = "keys"
	}
	entries, err := seq(get(parent, key))
	if err != nil {
		return nil, err
	}
	old := map[string]*yaml.Node{}
	replacement := map[string]bool{}
	preserved := []*yaml.Node{}
	used := map[string]bool{}
	for _, entry := range entries {
		if owned(entry) {
			old[scalar(entry, "name")] = entry
			models, err := seq(get(entry, "models"))
			if err != nil {
				return nil, err
			}
			for _, m := range models {
				replacement[publicID(entry, m)] = true
			}
		} else {
			preserved = append(preserved, entry)
			if prefix := scalar(entry, "prefix"); prefix != "" {
				used[prefix] = true
			}
		}
	}
	// Other API credential families are never deleted by a GUI bundle.
	parsedOriginal, err := config.ParseConfigBytes(in.Config)
	if err != nil {
		return nil, invalid()
	}
	for _, entry := range parsedOriginal.CodexKey {
		if entry.Prefix != "" {
			used[entry.Prefix] = true
		}
	}
	managed := map[string]bool{}
	for _, entry := range old {
		prefix := scalar(entry, "prefix")
		if !slug.MatchString(prefix) || reserved[prefix] {
			return nil, invalid()
		}
		managed[prefix] = true
	}
	for _, prefix := range state.Managed {
		if !slug.MatchString(prefix) || reserved[prefix] {
			return nil, errors.New("managed prefix conflicts with a registered OAuth account")
		}
		managed[prefix] = true
	}
	for prefix := range used {
		if managed[prefix] {
			return nil, errors.New("managed scope overlaps preserved provider configuration")
		}
	}
	providersSeen := map[string]bool{}
	allPrefixes := map[string]bool{}
	totalModels := 0
	for i := range state.Providers {
		p := &state.Providers[i]
		p.ID = strings.TrimSpace(p.ID)
		p.Prefix = strings.TrimSpace(p.Prefix)
		p.Base = strings.TrimRight(strings.TrimSpace(p.Base), "/")
		p.Proxy = strings.TrimSpace(p.Proxy)
		if p.Prefix == "" {
			p.Prefix = p.ID
		}
		if p.Name == "" {
			p.Name = p.ID
		}
		if !slug.MatchString(p.ID) || !slug.MatchString(p.Prefix) || providersSeen[p.ID] || allPrefixes[p.Prefix] || reserved[p.Prefix] || len(p.Name) > 120 {
			return nil, invalid()
		}
		providersSeen[p.ID] = true
		allPrefixes[p.Prefix] = true
		managed[p.Prefix] = true
		totalModels += len(p.Models)
		if totalModels > 4096 {
			return nil, invalid()
		}
		if err := validateOrigin(ctx, *p, old["cpa-gui-"+p.ID+"-chat"], in.Resolve); err != nil {
			return nil, err
		}
	}
	filterRows := func(section string) ([]*yaml.Node, error) {
		rows, err := seq(get(gen, section))
		if err != nil {
			return nil, err
		}
		out := []*yaml.Node{}
		for _, row := range rows {
			prefix, _, found := strings.Cut(scalar(row, "id"), "/")
			if !found || !managed[prefix] {
				out = append(out, row)
			}
		}
		return out, nil
	}
	chat, err := filterRows("chat_models")
	if err != nil {
		return nil, err
	}
	responses, err := filterRows("responses_models")
	if err != nil {
		return nil, err
	}
	for id := range contextModels {
		prefix, _, found := strings.Cut(id, "/")
		if found && managed[prefix] {
			delete(contextModels, id)
		}
	}
	compiled := []*yaml.Node{}
	modelsSet := map[string]bool{}
	count := 0
	for _, p := range state.Providers {
		if !enabled(p.Enabled) {
			continue
		}
		if used[p.Prefix] {
			return nil, errors.New("provider prefix collides with preserved published configuration")
		}
		used[p.Prefix] = true
		count++
		var secret string
		if json.Unmarshal(vault[p.ID], &secret) != nil || strings.TrimSpace(secret) == "" || len(secret) > 4096 {
			return nil, errors.New("enabled provider has no valid API key in the trusted vault")
		}
		secret = strings.TrimSpace(secret)
		name := "cpa-gui-" + p.ID + "-chat"
		previous := old[name]
		entry := clone(previous)
		put(entry, "name", node(name))
		put(entry, "prefix", node(p.Prefix))
		put(entry, "base-url", node(p.Base))
		if get(entry, "disable-cooling") == nil {
			put(entry, "disable-cooling", node(true))
		}
		if get(entry, "disabled") != nil {
			put(entry, "disabled", node(false))
		}
		keyEntry := node(map[string]any{})
		if previousKeys, _ := seq(get(previous, credentialField)); len(previousKeys) > 1 {
			return nil, errors.New("multi-key published provider requires an explicit migration")
		} else if len(previousKeys) > 0 {
			keyEntry = clone(previousKeys[0])
		}
		put(keyEntry, "api-key", node(secret))
		put(keyEntry, "proxy-url", node(p.Proxy))
		put(entry, credentialField, &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq", Content: []*yaml.Node{keyEntry}})
		previousModels := map[string]*yaml.Node{}
		if rows, _ := seq(get(previous, "models")); rows != nil {
			for _, row := range rows {
				previousModels[scalar(row, "alias")] = row
			}
		}
		modelNodes := []*yaml.Node{}
		aliases := map[string]bool{}
		for _, m := range p.Models {
			if err := normalize(&m); err != nil {
				return nil, err
			}
			if aliases[m.Client] {
				return nil, invalid()
			}
			aliases[m.Client] = true
			if !enabled(m.Enabled) {
				continue
			}
			id := p.Prefix + "/" + m.Client
			if modelsSet[id] {
				return nil, invalid()
			}
			modelsSet[id] = true
			n := clone(previousModels[m.Client])
			put(n, "name", node(m.Upstream))
			put(n, "alias", node(m.Client))
			put(n, "display-name", node(p.Name+" · "+m.Client))
			put(n, "max-context-length", node(m.Context))
			put(n, "input-modalities", node(m.Input))
			put(n, "output-modalities", node([]string{"text"}))
			thinking := clone(get(n, "thinking"))
			if thinking.Kind != yaml.MappingNode {
				return nil, invalid()
			}
			put(thinking, "levels", node(m.Thinking))
			put(n, "thinking", thinking)
			modelNodes = append(modelNodes, n)
			protocol := "chat"
			if p.ID == "opencode" && p.Prefix == "opencode" && p.Base == "https://opencode.ai/zen/go/v1" && strings.HasPrefix(m.Upstream, "muse-spark-") {
				protocol = "responses"
			}
			if protocol == "chat" {
				chat = append(chat, node(map[string]any{"id": id, "upstream": m.Upstream, "provider": p.ID, "protocol": "chat", "endpoint": "/chat/completions", "status": 200}))
			}
			responses = append(responses, node(map[string]any{"id": id, "upstream": m.Upstream, "provider": p.ID, "protocol": protocol, "endpoint": "/responses", "status": 200}))
			levels := []map[string]string{}
			for _, effort := range m.Thinking {
				levels = append(levels, map[string]string{"effort": effort, "description": effort})
			}
			value := map[string]any{"context_length": m.Context, "context_window": m.Context, "max_context_window": m.Context, "default_reasoning_level": "medium", "supported_reasoning_levels": levels, "input_modalities": m.Input, "upstream_slug": m.Upstream, "source": "cpa-gui managed · " + p.Name}
			if len(m.Thinking) == 0 {
				value["default_reasoning_level"] = "none"
			}
			merged := map[string]json.RawMessage{}
			if existing := previousContext[id]; existing != nil {
				if decodeJSON(existing, &merged) != nil || merged == nil {
					return nil, invalid()
				}
			}
			for field, item := range value {
				merged[field], _ = json.Marshal(item)
			}
			raw, _ := json.Marshal(merged)
			contextModels[id] = raw
		}
		put(entry, "models", &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq", Content: modelNodes})
		compiled = append(compiled, entry)
	}
	put(parent, key, &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq", Content: append(compiled, preserved...)})
	put(gen, "chat_models", &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq", Content: chat})
	put(gen, "responses_models", &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq", Content: responses})
	put(gen, "generated_at", node(in.Now.UTC().Format(time.RFC3339)))
	put(gen, "source", node("cpa-admin-helper-go"))
	contexts["models"], _ = json.Marshal(contextModels)
	if contexts["schema_version"] == nil {
		contexts["schema_version"] = json.RawMessage("2")
	}
	output := &Output{Providers: count}
	output.Config, err = yaml.Marshal(root)
	if err != nil {
		return nil, invalid()
	}
	output.Generated, err = yaml.Marshal(gen)
	if err != nil {
		return nil, invalid()
	}
	output.Context, err = json.MarshalIndent(contexts, "", "  ")
	if err != nil {
		return nil, invalid()
	}
	output.Context = append(output.Context, '\n')
	for id := range modelsSet {
		output.Models = append(output.Models, id)
	}
	for id := range replacement {
		output.ReplacedModels = append(output.ReplacedModels, id)
	}
	sort.Strings(output.Models)
	sort.Strings(output.ReplacedModels)
	if err := Verify(output); err != nil {
		return nil, err
	}
	return output, nil
}

// Verify checks generated route/metadata consistency using the upstream YAML
// parser. It never calls an external provider or mutates the published files.
func Verify(out *Output) error {
	cfg, err := config.ParseConfigBytes(out.Config)
	if err != nil {
		return invalid()
	}
	declared := map[string]string{}
	for _, p := range cfg.OpenAICompatibility {
		if !strings.HasPrefix(p.Name, "cpa-gui-") {
			continue
		}
		for _, m := range p.Models {
			id := p.Prefix + "/" + m.Alias
			if m.Alias == "" || declared[id] != "" {
				return invalid()
			}
			declared[id] = m.Name
		}
	}
	gen, err := parseYAML(out.Generated)
	if err != nil {
		return err
	}
	routes, err := seq(get(gen, "responses_models"))
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, row := range routes {
		id := scalar(row, "id")
		if upstream := declared[id]; upstream != "" {
			if seen[id] || scalar(row, "upstream") != upstream || scalar(row, "endpoint") != "/responses" {
				return invalid()
			}
			seen[id] = true
		}
	}
	var contexts struct {
		Models map[string]struct {
			Context  int    `json:"context_window"`
			Upstream string `json:"upstream_slug"`
		} `json:"models"`
	}
	if decodeJSON(out.Context, &contexts) != nil {
		return invalid()
	}
	for _, id := range out.Models {
		if declared[id] == "" || !seen[id] || contexts.Models[id].Upstream != declared[id] || contexts.Models[id].Context < 1024 {
			return invalid()
		}
	}
	return nil
}
