// Package compat contains narrowly scoped CPA admission rules, not another
// protocol translator. Standard conversions remain in the upstream pipeline.
package compat

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
)

// NormalizeOpenCodeTools ports the deployed tool-declaration conflict gate.
// Additional tools are promoted once and removed from input: the new upstream
// walker would otherwise re-read them, including untrusted user-role records.
// raw must be valid JSON; unrelated models are returned without a copy.
func NormalizeOpenCodeTools(raw []byte) ([]byte, error) {
	model := util.GetGJSONBytesNoCopy(raw, "model")
	if model.Exists() && model.Type != ojson.String && model.Type != ojson.Null {
		return nil, fmt.Errorf("model must be a string")
	}
	if !strings.HasPrefix(model.String(), "opencode/") {
		return raw, nil
	}
	return NormalizeResponsesTools(raw)
}

// NormalizeResponsesTools promotes trusted Codex tool discovery records while
// preserving native namespaces, custom tools and replay items. Only the tool
// declarations are decoded; every other input item is copied verbatim, so the
// cost does not depend on image or history size. raw must be valid JSON.
func NormalizeResponsesTools(raw []byte) ([]byte, error) {
	var input, current ojson.Result
	util.ParseGJSONBytesNoCopy(raw).ForEach(func(key, value ojson.Result) bool {
		switch key.String() {
		case "input":
			input = value
		case "tools":
			current = value
		}
		return true
	})
	if !input.IsArray() {
		return raw, nil
	}
	if current.Exists() && !current.IsArray() {
		return nil, fmt.Errorf("tools must be an array")
	}
	trusted, sawAdditional := false, false
	var snapshots []string
	retained := make([]string, 0, 64)
	retainedBytes := 0
	var failure error
	input.ForEach(func(_, item ojson.Result) bool {
		kind := item.Get("type")
		if !item.IsObject() || kind.Type != ojson.String || kind.String() != "additional_tools" {
			retained = append(retained, item.Raw)
			retainedBytes += len(item.Raw) + 1
			return true
		}
		sawAdditional = true
		role := item.Get("role")
		if role.Type != ojson.String || role.String() != "developer" && role.String() != "system" {
			return true
		}
		declarations := item.Get("tools")
		if !declarations.IsArray() {
			failure = fmt.Errorf("additional_tools.tools must be an array")
			return false
		}
		trusted = true
		snapshots = append(snapshots, declarations.Raw)
		return true
	})
	if failure != nil {
		return nil, failure
	}
	if !sawAdditional {
		return raw, nil
	}
	doc := ojson.NewDoc(raw)
	if trusted {
		tools := []any{}
		if current.Exists() {
			if err := decodeDeclarations(current.Raw, &tools); err != nil {
				return nil, err
			}
		}
		// Top-level tools describe the current runtime. Historical discovery
		// snapshots fill missing identities, newest first, as tools evolve.
		selected := make(map[string]bool)
		tools, err := deduplicate(tools, "", make(map[string]toolDeclaration), selected)
		if err != nil {
			return nil, err
		}
		for i := len(snapshots) - 1; i >= 0; i-- {
			var declarations []any
			if err := decodeDeclarations(snapshots[i], &declarations); err != nil {
				return nil, err
			}
			additional, err := deduplicate(declarations, "", make(map[string]toolDeclaration), selected)
			if err != nil {
				return nil, err
			}
			tools = append(tools, additional...)
		}
		encoded, err := json.Marshal(tools)
		if err != nil {
			return nil, err
		}
		if err := doc.SetRaw("tools", string(encoded)); err != nil {
			return nil, err
		}
	}
	var rebuilt strings.Builder
	rebuilt.Grow(retainedBytes + 2)
	rebuilt.WriteByte('[')
	for i, item := range retained {
		if i > 0 {
			rebuilt.WriteByte(',')
		}
		rebuilt.WriteString(item)
	}
	rebuilt.WriteByte(']')
	if err := doc.SetRaw("input", rebuilt.String()); err != nil {
		return nil, err
	}
	return doc.Bytes(), nil
}

// decodeDeclarations keeps numbers exact so schemas round-trip unchanged.
func decodeDeclarations(raw string, target *[]any) error {
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.UseNumber()
	return decoder.Decode(target)
}

type toolDeclaration struct {
	signature string
	tool      map[string]any
}

// Within one snapshot, only tool documentation may differ. Type, namespace,
// grammar, parameters and other contract fields must match. Never strip schema
// descriptions when validating duplicate declarations in that snapshot.
func toolContract(tool map[string]any) map[string]any {
	contract := make(map[string]any, len(tool))
	for key, value := range tool {
		if key == "description" {
			continue
		}
		if key == "function" {
			if function, ok := value.(map[string]any); ok {
				value = toolContract(function)
			}
		}
		contract[key] = value
	}
	return contract
}

func updateToolDescription(previous, current map[string]any) {
	if description, exists := current["description"]; exists {
		previous["description"] = description
	} else {
		delete(previous, "description")
	}
	if function, ok := previous["function"].(map[string]any); ok {
		if updated, ok := current["function"].(map[string]any); ok {
			updateToolDescription(function, updated)
		}
	}
}

func deduplicate(entries []any, namespace string, seen map[string]toolDeclaration, selected map[string]bool) ([]any, error) {
	result := make([]any, 0, len(entries))
	for _, value := range entries {
		tool, ok := value.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("tool declarations must be objects")
		}
		if tool["type"] == "namespace" {
			children, ok := tool["tools"].([]any)
			if !ok {
				return nil, fmt.Errorf("namespace.tools must be an array")
			}
			name, _ := tool["name"].(string)
			if name == "" {
				name, _ = tool["namespace"].(string)
			}
			if namespace != "" {
				name = namespace + "." + name
			}
			kept, err := deduplicate(children, name, seen, selected)
			if err != nil {
				return nil, err
			}
			if len(kept) > 0 {
				tool["tools"] = kept
				result = append(result, tool)
			}
			continue
		}
		function := tool
		if nested, ok := tool["function"].(map[string]any); ok {
			function = nested
		}
		name, _ := function["name"].(string)
		if name != "" {
			ns, _ := tool["namespace"].(string)
			if ns == "" {
				ns = namespace
			}
			signature, err := json.Marshal([]any{ns, toolContract(tool)})
			if err != nil {
				return nil, err
			}
			identity := ns + "\x00" + name
			if previous, exists := seen[identity]; exists {
				if previous.signature != string(signature) {
					return nil, fmt.Errorf("conflicting tool declaration: %s", name)
				}
				// Identical declarations within one snapshot share one contract.
				updateToolDescription(previous.tool, tool)
				continue
			}
			seen[identity] = toolDeclaration{signature: string(signature), tool: tool}
			if selected[identity] {
				continue
			}
			selected[identity] = true
		}
		result = append(result, tool)
	}
	return result, nil
}
