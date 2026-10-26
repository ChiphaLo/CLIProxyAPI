// Package compat contains narrowly scoped CPA admission rules, not another
// protocol translator. Standard conversions remain in the upstream pipeline.
package compat

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// NormalizeOpenCodeTools ports the deployed tool-declaration conflict gate.
// Additional tools are promoted once and removed from input: the new upstream
// walker would otherwise re-read them, including untrusted user-role records.
func NormalizeOpenCodeTools(raw []byte) ([]byte, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var body map[string]any
	if err := decoder.Decode(&body); err != nil {
		return nil, err
	}
	model, _ := body["model"].(string)
	if !strings.HasPrefix(model, "opencode/") {
		return append([]byte(nil), raw...), nil
	}
	input, ok := body["input"].([]any)
	if !ok {
		return append([]byte(nil), raw...), nil
	}
	tools, trusted, sawAdditional := []any{}, false, false
	if value, exists := body["tools"]; exists {
		tools, ok = value.([]any)
		if !ok {
			return nil, fmt.Errorf("tools must be an array")
		}
	}
	retained := make([]any, 0, len(input))
	for _, item := range input {
		object, ok := item.(map[string]any)
		if !ok || object["type"] != "additional_tools" {
			retained = append(retained, item)
			continue
		}
		sawAdditional = true
		if object["role"] != "developer" && object["role"] != "system" {
			continue
		}
		declarations, ok := object["tools"].([]any)
		if !ok {
			return nil, fmt.Errorf("additional_tools.tools must be an array")
		}
		trusted = true
		tools = append(tools, declarations...)
	}
	if !sawAdditional {
		return append([]byte(nil), raw...), nil
	}
	if trusted {
		var err error
		tools, err = deduplicate(tools, "", make(map[string]toolDeclaration))
		if err != nil {
			return nil, err
		}
		body["tools"] = tools
	}
	body["input"] = retained
	return json.Marshal(body)
}

type toolDeclaration struct {
	signature string
	tool      map[string]any
}

// Tool documentation can change when Codex discovers another MCP server. Only
// descriptions are excluded: type, namespace, grammar, parameters and all other
// contract fields must still match. Never strip descriptions inside JSON Schema.
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

func deduplicate(entries []any, namespace string, seen map[string]toolDeclaration) ([]any, error) {
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
			kept, err := deduplicate(children, name, seen)
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
			if previous, exists := seen[name]; exists {
				if previous.signature != string(signature) {
					return nil, fmt.Errorf("conflicting tool declaration: %s", name)
				}
				// Additional declarations are trusted system/developer records in
				// chronological order. Preserve one tool and its unchanged contract,
				// but advertise the latest documentation instead of stale MCP tools.
				updateToolDescription(previous.tool, tool)
				continue
			}
			seen[name] = toolDeclaration{signature: string(signature), tool: tool}
		}
		result = append(result, tool)
	}
	return result, nil
}
