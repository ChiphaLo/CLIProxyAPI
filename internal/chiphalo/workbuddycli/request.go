package workbuddycli

import (
	"encoding/json"
	"errors"
	"regexp"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

type Message struct {
	Role             string          `json:"role"`
	Content          json.RawMessage `json:"content"`
	ToolCallID       string          `json:"tool_call_id,omitempty"`
	ToolCalls        []ToolCall      `json:"tool_calls,omitempty"`
	ReasoningContent string          `json:"reasoning_content,omitempty"`
}

type ToolCall struct {
	ID       string   `json:"id"`
	Type     string   `json:"type"`
	Function Function `json:"function"`
}

type Function struct {
	Name        string          `json:"name"`
	Arguments   string          `json:"arguments,omitempty"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type Tool struct {
	Type     string   `json:"type"`
	Function Function `json:"function"`
}

type Request struct {
	Model           string          `json:"model"`
	Messages        []Message       `json:"messages"`
	Tools           []Tool          `json:"tools,omitempty"`
	ToolChoice      json.RawMessage `json:"tool_choice,omitempty"`
	Stream          bool            `json:"stream"`
	ReasoningEffort string          `json:"reasoning_effort,omitempty"`
	MaxTokens       int             `json:"max_tokens,omitempty"`
}

type prepared struct {
	system   string
	history  []map[string]any
	prompt   string
	tools    []map[string]any
	schemas  map[string]*jsonschema.Schema
	required bool
}

var toolName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

func text(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s, nil
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) != nil {
		return "", errors.New("message content must be text")
	}
	var values []string
	for _, part := range parts {
		if part.Type != "text" {
			return "", errors.New("this official-client adapter has not enabled non-text input")
		}
		values = append(values, part.Text)
	}
	return strings.Join(values, "\n"), nil
}

func prepare(r Request) (*prepared, error) {
	if r.MaxTokens < 0 || r.MaxTokens > 100000 {
		return nil, errors.New("output token limit must be between 1 and 100000 when specified")
	}
	if r.Model != "deepseek-v4.1-flash" {
		return nil, errors.New("model is not enabled in this isolated official-client adapter")
	}
	if r.ReasoningEffort != "" && r.ReasoningEffort != "high" {
		return nil, errors.New("this WorkBuddy model currently declares high reasoning only")
	}
	if len(r.Messages) == 0 || len(r.Messages) > 8192 || len(r.Tools) > 128 {
		return nil, errors.New("invalid message or tool count")
	}
	p := &prepared{schemas: map[string]*jsonschema.Schema{}}
	choice, selected := "auto", ""
	if len(r.ToolChoice) != 0 && string(r.ToolChoice) != "null" {
		if json.Unmarshal(r.ToolChoice, &choice) != nil {
			var object struct {
				Type     string   `json:"type"`
				Function Function `json:"function"`
			}
			if json.Unmarshal(r.ToolChoice, &object) != nil || object.Type != "function" || object.Function.Name == "" {
				return nil, errors.New("unsupported tool_choice")
			}
			choice, selected = "required", object.Function.Name
		}
	}
	if choice != "auto" && choice != "none" && choice != "required" {
		return nil, errors.New("unsupported tool_choice")
	}
	p.required = choice == "required"
	seen := map[string]bool{}
	for _, tool := range r.Tools {
		name := tool.Function.Name
		if tool.Type != "function" || !toolName.MatchString(name) || seen[name] {
			return nil, errors.New("invalid or duplicate function declaration")
		}
		seen[name] = true
		if choice == "none" || selected != "" && selected != name {
			continue
		}
		var schema any
		if json.Unmarshal(tool.Function.Parameters, &schema) != nil {
			return nil, errors.New("invalid function parameter schema")
		}
		compiler := jsonschema.NewCompiler()
		// External schema resolution is intentionally unavailable.
		compiler.UseLoader(jsonschema.SchemeURLLoader{})
		uri := "urn:cpa:tool:" + name
		if err := compiler.AddResource(uri, schema); err != nil {
			return nil, errors.New("invalid function parameter schema")
		}
		compiled, err := compiler.Compile(uri)
		if err != nil {
			return nil, errors.New("invalid or externally referenced function parameter schema")
		}
		p.schemas[name] = compiled
		p.tools = append(p.tools, map[string]any{"id": name, "name": name, "description": tool.Function.Description, "requiresApproval": true, "inputSchema": schema})
	}
	if selected != "" && !seen[selected] || p.required && len(p.tools) == 0 {
		return nil, errors.New("required function is not declared")
	}
	var conversation []Message
	var instructions []string
	for _, m := range r.Messages {
		if value := strings.TrimSpace(m.ReasoningContent); value != "" && value != "[reasoning unavailable]" {
			return nil, errors.New("this ACP history adapter cannot safely import external reasoning_content")
		}
		if m.Role == "system" || m.Role == "developer" {
			s, err := text(m.Content)
			if err != nil {
				return nil, err
			}
			instructions = append(instructions, s)
		} else {
			conversation = append(conversation, m)
		}
	}
	if len(conversation) == 0 {
		return nil, errors.New("conversation has no user or tool input")
	}
	last := conversation[len(conversation)-1]
	if last.Role == "user" && len(last.ToolCalls) == 0 {
		var err error
		p.prompt, err = text(last.Content)
		if err != nil || p.prompt == "" {
			return nil, errors.New("final user prompt must contain text")
		}
		conversation = conversation[:len(conversation)-1]
	} else if last.Role == "tool" {
		// ACP requires a prompt to advance after injected historical tool results.
		p.prompt = "Continue from the tool results above."
	} else {
		return nil, errors.New("conversation must end in user input or tool results")
	}
	ids := map[string]bool{}
	for i := 0; i < len(conversation); i++ {
		m := conversation[i]
		s, err := text(m.Content)
		if err != nil {
			return nil, err
		}
		switch m.Role {
		case "user":
			p.history = append(p.history, map[string]any{"role": "user", "content": s})
		case "assistant":
			if s != "" {
				p.history = append(p.history, map[string]any{"role": "assistant", "content": s})
			}
			if len(m.ToolCalls) == 0 {
				continue
			}
			outputs := map[string]string{}
			for j := 0; j < len(m.ToolCalls); j++ {
				if i+1+j >= len(conversation) || conversation[i+1+j].Role != "tool" {
					return nil, errors.New("assistant tool batch has incomplete results")
				}
				output := conversation[i+1+j]
				if _, duplicate := outputs[output.ToolCallID]; duplicate {
					return nil, errors.New("duplicate tool result")
				}
				value, errText := text(output.Content)
				if errText != nil {
					return nil, errText
				}
				outputs[output.ToolCallID] = value
			}
			for _, call := range m.ToolCalls {
				value, exists := outputs[call.ID]
				if !exists || call.ID == "" || ids[call.ID] || call.Type != "function" || !toolName.MatchString(call.Function.Name) {
					return nil, errors.New("invalid tool history identity")
				}
				ids[call.ID] = true
				var input map[string]any
				if json.Unmarshal([]byte(call.Function.Arguments), &input) != nil || input == nil {
					return nil, errors.New("historical function arguments must be a JSON object")
				}
				p.history = append(p.history,
					map[string]any{"role": "tool_call", "name": "DelegateTool", "arguments": map[string]any{"toolId": call.Function.Name, "input": input}},
					map[string]any{"role": "tool_result", "output": value})
			}
			i += len(m.ToolCalls)
		default:
			return nil, errors.New("conversation contains an unsupported or orphaned role")
		}
	}
	p.system = strings.Join(instructions, "\n\n") + "\nUse only declared tools via DelegateTool. Tool execution belongs to the API caller, never this client."
	if p.required {
		p.system += "\nThis turn requires a call to a declared delegated tool."
	}
	return p, nil
}
