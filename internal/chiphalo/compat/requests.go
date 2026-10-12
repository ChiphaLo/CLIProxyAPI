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
		tools, err = deduplicate(tools, "", make(map[string]string))
		if err != nil {
			return nil, err
		}
		body["tools"] = tools
	}
	body["input"] = retained
	return json.Marshal(body)
}

func deduplicate(entries []any, namespace string, seen map[string]string) ([]any, error) {
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
			signature, err := json.Marshal([]any{ns, tool})
			if err != nil {
				return nil, err
			}
			if previous, exists := seen[name]; exists {
				if previous != string(signature) {
					return nil, fmt.Errorf("conflicting tool declaration: %s", name)
				}
				continue
			}
			seen[name] = string(signature)
		}
		result = append(result, tool)
	}
	return result, nil
}
